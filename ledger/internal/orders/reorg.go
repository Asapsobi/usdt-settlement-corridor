package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/money"
)

// ErrUnexpectedState is returned by HandleDepositReorg when the order is
// in a state neither reorg scenario is defined for. An unexpected state
// is an error, not a default branch -- guessing what a reorg should mean
// for, say, an already-refunded order is exactly the kind of silent
// plausible-sounding behavior this system refuses to have.
var ErrUnexpectedState = errors.New("orders: unexpected state for reorg handling")

// HandleDepositReorg reacts to a reported BEP20 deposit reorg by branching
// on the order's current state -- there are exactly two scenarios, and
// they are not the same problem:
//
//   - funded (scenario A, routine): the deposit is reversed and the order
//     returns to quoted, as if the deposit had never landed. Net effect on
//     the books is zero.
//   - dispatching or settled (scenario B, a real loss): the payout already
//     happened and cannot be undone, so the deposit is reversed but the
//     payout entry is not; the gap is booked as a realized loss, and the
//     ledger is halted, because a settlement that will not be paid for is
//     exactly the class of problem this system must never handle quietly.
//
// Any other state -- quoted, screened, held, refunded, expired -- returns
// ErrUnexpectedState naming the actual state, rather than doing nothing
// or doing something that merely looks plausible.
//
// originalEntryKey is the idempotency key of the deposit_final entry that
// funded this order -- the caller (C2, the deposit watcher, not built
// yet) knows this key because it is the same one it used when it first
// posted that entry; HandleDepositReorg has no other way to find it.
func HandleDepositReorg(ctx context.Context, tx pgx.Tx, orderID int64, originalEntryKey string, actor string) (Order, error) {
	order, err := Get(ctx, tx, orderID)
	if err != nil {
		return Order{}, err
	}

	switch order.State {
	case Funded:
		return reorgScenarioA(ctx, tx, order, originalEntryKey, actor)
	case Dispatching, Settled:
		return reorgScenarioB(ctx, tx, order, originalEntryKey, actor)
	default:
		return Order{}, fmt.Errorf("%w: order %d is in state %s", ErrUnexpectedState, orderID, order.State)
	}
}

// reorgScenarioA: reverse the deposit, return the order to quoted. If the
// quote has since expired, that is a separate, later transition
// (quoted -> expired) made by whatever next touches this order -- not
// something HandleDepositReorg does reactively here.
func reorgScenarioA(ctx context.Context, tx pgx.Tx, order Order, originalEntryKey, actor string) (Order, error) {
	original, err := journal.GetEntryByIdempotencyKey(ctx, tx, originalEntryKey)
	if err != nil {
		return Order{}, fmt.Errorf("orders: reorg scenario A: looking up %q: %w", originalEntryKey, err)
	}

	now := time.Now()
	reversal, err := journal.Reverse(ctx, tx, original.ID, actor, "deposit reorged out before dispatch", now)
	if err != nil {
		return Order{}, fmt.Errorf("orders: reorg scenario A: reversing entry %d: %w", original.ID, err)
	}

	updated, err := Transition(ctx, tx, order.ID, Quoted, order.Version, TransitionParams{
		Actor:      actor,
		Reason:     "deposit reorged out before dispatch",
		OccurredAt: now,
		EntryID:    &reversal.ID,
	})
	if err != nil {
		return Order{}, fmt.Errorf("orders: reorg scenario A: transitioning order %d to quoted: %w", order.ID, err)
	}
	return updated, nil
}

// reorgScenarioB: the payout already left. Reverse only the deposit (the
// inbound side), post the resulting gap as a realized loss, and halt.
// The order's state is not touched -- it really did settle (or is
// mid-dispatch); what changed is that we will not be paid for it.
//
// The loss entry's offsetting credit is position:corridor for
// amount_out's asset (USDT_TRC20), not the payout's slot account. The
// slot is already correct: that TRC20 genuinely left and E3 (the payout
// entry, never touched here) already recorded that truthfully. The actual
// gap is in position:corridor, which after the conversion entry (E2, also
// never touched here) is still carrying this amount as "pending
// rebalance" -- inventory the system believes will be topped up from the
// BEP20 side. Since that BEP20 side just evaporated, it never will be;
// crediting position:corridor is what converts "pending" into
// "permanently written off," which is what actually happened.
func reorgScenarioB(ctx context.Context, tx pgx.Tx, order Order, originalEntryKey, actor string) (Order, error) {
	original, err := journal.GetEntryByIdempotencyKey(ctx, tx, originalEntryKey)
	if err != nil {
		return Order{}, fmt.Errorf("orders: reorg scenario B: looking up %q: %w", originalEntryKey, err)
	}

	now := time.Now()
	if _, err := journal.Reverse(ctx, tx, original.ID, actor, "deposit reorged out post-dispatch", now); err != nil {
		return Order{}, fmt.Errorf("orders: reorg scenario B: reversing entry %d: %w", original.ID, err)
	}

	lossAsset := order.AmountOut.Asset
	lossUnits := order.AmountOut.Units
	_, err = journal.Post(ctx, tx, journal.EntryRequest{
		// Deterministic and order-scoped, not derived from originalEntryKey
		// with a fresh timestamp: a retried HandleDepositReorg call for the
		// same order must reach the exact same key, so this is idempotent
		// under retry the same way every other entry in this system is.
		IdempotencyKey: fmt.Sprintf("ledger:reorg_loss:%d", order.ID),
		EntryType:      "reorg_loss",
		Actor:          actor,
		OccurredAt:     now,
		OrderID:        &order.ID,
		Lines: []journal.Line{
			{AccountCode: "expense:loss:reorg", Amount: money.Amount{Asset: lossAsset, Units: lossUnits}},
			{AccountCode: corridorAccountFor(lossAsset), Amount: money.Amount{Asset: lossAsset, Units: -lossUnits}},
		},
	})
	if err != nil {
		return Order{}, fmt.Errorf("orders: reorg scenario B: posting loss entry for order %d: %w", order.ID, err)
	}

	err = halt.Set(ctx, tx, halt.SetParams{
		Reason: "POST_SETTLEMENT_REORG",
		Detail: map[string]any{
			"order_id":           order.ID,
			"order_external_id":  order.ExternalID,
			"original_entry_key": originalEntryKey,
			"loss_asset":         string(lossAsset),
			"loss_units":         lossUnits,
		},
		Actor: actor,
	})
	if err != nil {
		return Order{}, fmt.Errorf("orders: reorg scenario B: setting halt for order %d: %w", order.ID, err)
	}

	// No Transition call: the order's state is unchanged by design. It
	// really did settle; we just were not paid.
	return order, nil
}

func corridorAccountFor(asset money.Asset) string {
	return "position:corridor:" + string(asset)
}
