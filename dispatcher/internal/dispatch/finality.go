package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dispatcher/internal/ledgerclient"
)

// FinalityReader is this package's only path to checking whether a
// broadcast transaction has reached TRON Super-Representative finality --
// narrow and consumer-defined, a single read-only chain query, no
// different in spirit from C2's own watch-only relationship to BSC or
// C4.3's VerifyOnChain.
type FinalityReader interface {
	IsFinal(ctx context.Context, tronTxID string) (bool, error)
}

// ErrFinalityStale means tronTxID has not reached finality within
// FinalityStaleCeiling of its own broadcast_at -- an alert-worthy
// condition (a transaction that broadcast successfully but is stuck, or
// was silently dropped by the network) distinct from "not yet final,
// keep polling," the same posture C4.1's price-staleness ceiling and
// C2.5's stale-pending-finality alert already take for their own
// analogous waits.
var ErrFinalityStale = errors.New("dispatch: broadcast attempt has not reached finality within the configured ceiling")

func customerTRC20AccountCode(customerID string) string {
	return customerAccountCode(customerID, "USDT_TRC20")
}

func slotAccountCode(slotID int) string {
	return fmt.Sprintf("asset:tron:slot:%d", slotID)
}

// buildSettlementLines builds the E3 entry per c1-ledger-build-prompts.md
// §B: the customer's TRC20 liability clears, the slot's own balance pays
// out -- exactly order.AmountOut on both sides, the same frozen value E2
// already used, never recomputed.
func buildSettlementLines(order ledgerclient.Order, slotID int) []ledgerclient.EntryLine {
	return []ledgerclient.EntryLine{
		{AccountCode: customerTRC20AccountCode(order.CustomerID), Asset: "USDT_TRC20", Amount: order.AmountOut},
		{AccountCode: slotAccountCode(slotID), Asset: "USDT_TRC20", Amount: -order.AmountOut},
	}
}

// ConfirmFinality checks whether attemptID's own tron_txid has reached SR
// finality, and if so, settles it: marks the dispatch_attempts row
// CONFIRMED and calls C1's dispatching -> settled transition with the E3
// entry. A single check per call, not an internal polling loop -- the
// caller (a ticker-driven background job) is what repeatedly invokes
// this, the same shape Broadcast itself takes toward ErrSignaturePending
// rather than blocking inside this package.
//
//   - Not yet final, within FinalityStaleCeiling of broadcast_at:
//     returns (false, attempt, nil) -- keep polling.
//   - Not yet final, past FinalityStaleCeiling: returns
//     (false, attempt, ErrFinalityStale) -- alert-worthy, but the caller
//     keeps polling regardless (a transaction that lands late is still
//     correctly settled; staleness is an operator signal, not a reason
//     to give up).
//   - Already CONFIRMED: returned as-is immediately, no chain call, no
//     C1 call -- idempotent replay short-circuited locally rather than
//     leaning on C1's own idempotency for every poll tick.
//   - Newly final: settles, exactly like EnterDispatching, with the same
//     refetch-once-on-version-conflict and back-off-on-halt handling
//     (dispatching -> settled is HaltBlocked too, per C1.5's transition
//     table).
func (d *Dispatcher) ConfirmFinality(ctx context.Context, order ledgerclient.Order, attemptID int64, occurredAt time.Time, chain FinalityReader) (bool, BroadcastAttempt, error) {
	attempt, err := d.Attempts.Get(ctx, attemptID)
	if err != nil {
		return false, BroadcastAttempt{}, err
	}
	if attempt.Status == BroadcastConfirmed {
		return true, attempt, nil
	}
	if attempt.Status != BroadcastBroadcast || attempt.TronTxID == nil {
		return false, BroadcastAttempt{}, fmt.Errorf("dispatch: attempt %d is not BROADCAST with a tron_txid (status %s)", attemptID, attempt.Status)
	}

	final, err := chain.IsFinal(ctx, *attempt.TronTxID)
	if err != nil {
		return false, BroadcastAttempt{}, fmt.Errorf("dispatch: checking finality for attempt %d: %w", attemptID, err)
	}
	if !final {
		if attempt.BroadcastAt != nil && d.FinalityStaleCeiling > 0 && time.Since(*attempt.BroadcastAt) > d.FinalityStaleCeiling {
			return false, attempt, ErrFinalityStale
		}
		return false, attempt, nil
	}

	settleKey := fmt.Sprintf("dispatcher:settle:%d:%d", order.ID, attempt.AttemptNumber)
	if err := d.settleOrder(ctx, order, attempt.SlotID, settleKey, occurredAt); err != nil {
		return false, BroadcastAttempt{}, err
	}
	// Mirrors HandlePartialSettlement's own d.Store.MarkSettled call for
	// the Sweep path -- dispatch_state's own status must reach a
	// terminal value too, not just dispatch_attempts', or a caller
	// scanning Store.ListDispatching (C5.7's reconciliation job, and
	// this proof run's own orchestration loop) would see this order as
	// still in flight forever after it has actually settled. Found while
	// wiring the MVP proof run's orchestration loop: this single-order
	// path never called it, unlike every other settlement/failure path
	// in this package.
	if err := d.Store.MarkSettled(ctx, order.ID); err != nil {
		return false, BroadcastAttempt{}, fmt.Errorf("dispatch: order %d settled in C1 but marking dispatch_state SETTLED failed: %w", order.ID, err)
	}

	updated, err := d.Attempts.markConfirmed(ctx, attemptID)
	if err != nil {
		return false, BroadcastAttempt{}, err
	}
	return true, updated, nil
}

// settleOrder posts the E3 settlement entry and transitions order to
// `settled`, with the same refetch-once-on-version-conflict and
// back-off-on-halt handling EnterDispatching itself uses. Shared between
// ConfirmFinality (single-order attempts) and HandlePartialSettlement
// (C5.8's own per-recipient settlement within a Sweep batch) -- the E3
// shape and the retry discipline are identical either way; only the
// idempotency key's own construction differs, so it is the caller's to
// supply, not derived here.
func (d *Dispatcher) settleOrder(ctx context.Context, order ledgerclient.Order, slotID int, settleKey string, occurredAt time.Time) error {
	lines := buildSettlementLines(order, slotID)

	ensureKey := "dispatcher:ensure-account:" + slotAccountCode(slotID)
	if err := d.Ledger.EnsureAccount(ctx, slotAccountCode(slotID), ledgerclient.AccountAsset, "USDT_TRC20", ensureKey); err != nil {
		return fmt.Errorf("dispatch: ensuring %s exists before settling: %w", slotAccountCode(slotID), err)
	}

	expectedVersion := order.Version
	versionConflictRetried := false
	for {
		_, err := d.Ledger.TransitionWithEntry(ctx, order.ExternalID, "settled", expectedVersion,
			"payout_sr_final", "payout_settled", occurredAt, lines, settleKey)
		if err == nil {
			return nil
		}

		if errors.Is(err, ledgerclient.ErrVersionConflict) && !versionConflictRetried {
			versionConflictRetried = true
			refetched, getErr := d.Ledger.GetOrder(ctx, order.ExternalID)
			if getErr != nil {
				return fmt.Errorf("dispatch: refetching order %s after version conflict: %w", order.ExternalID, getErr)
			}
			expectedVersion = refetched.Version
			continue
		}
		if errors.Is(err, ledgerclient.ErrSystemHalted) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d.HaltBackoff):
			}
			continue
		}

		return fmt.Errorf("dispatch: settling order %s: %w", order.ExternalID, err)
	}
}
