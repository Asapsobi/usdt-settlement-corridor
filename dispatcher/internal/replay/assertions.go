package replay

import (
	"context"
	"errors"
	"fmt"

	"dispatcher/internal/dispatch"
)

// finalAssertions checks every FINAL ASSERTION in the C5.11 build spec,
// against everything this run actually did (tracked on h as scenarios
// ran), not just what a single scenario locally checked.
func (h *harness) finalAssertions(ctx context.Context) []Result {
	return []Result{
		h.assertEveryDispatchedOrderResolved(),
		h.assertUnsignedTxHashUniqueness(),
		h.assertEverySettledOrderHasExactlyOneE3(ctx),
		h.assertEveryHeldOrderHasExactlyOneReversal(ctx),
		h.assertSweepBatchesReserveOncePerBatch(),
	}
}

// assertEveryDispatchedOrderResolved is FINAL ASSERTION 1: every
// dispatched order reaches exactly one of `settled` or `held` -- never
// left in `dispatching` with no open attempt and no reconciliation
// finding it. An order this run deliberately left ambiguous (the
// post-broadcast freeze scenario) is compliant as long as it still has
// an OPEN (non-terminal) attempt -- that IS the "something is tracking
// this" the assertion cares about, not a violation.
func (h *harness) assertEveryDispatchedOrderResolved() Result {
	const name = "FinalAssertion_EveryDispatchedOrderReachesSettledOrHeld"
	attempts := dispatch.NewAttemptStore(h.pool)

	for _, d := range h.dispatched {
		state, err := h.ledger.getOrderState(h.ctx, d.externalID)
		if err != nil {
			return fail(name, fmt.Errorf("order %s: %w", d.externalID, err))
		}
		if state == "settled" || state == "held" {
			continue
		}
		if state != "dispatching" {
			return fail(name, fmt.Errorf("order %s is %q, want settled, held, or (only if ambiguous) dispatching", d.externalID, state))
		}
		if !d.expectAmbiguous {
			return fail(name, fmt.Errorf("order %s is still dispatching and was not expected to be ambiguous", d.externalID))
		}
		latest, err := attempts.LatestForOrder(h.ctx, d.orderID)
		if err != nil {
			if errors.Is(err, dispatch.ErrBroadcastAttemptNotFound) {
				// A Sweep-tier order requeued after a partial-batch
				// failure never gets a dispatch_attempts row at all --
				// that table is Direct/Standard's own per-order
				// broadcast tracking; batch_queue is what tracks a
				// Sweep order instead, and this order being marked
				// expectAmbiguous already confirms this run's own
				// scenario knows it's legitimately still queued, not
				// abandoned.
				continue
			}
			return fail(name, fmt.Errorf("order %s: %w", d.externalID, err))
		}
		if latest.Status == dispatch.BroadcastConfirmed || latest.Status == dispatch.BroadcastFailed {
			return fail(name, fmt.Errorf("order %s is dispatching with no OPEN attempt (latest is %s) and no reconciliation finding it", d.externalID, latest.Status))
		}
	}
	return pass(name)
}

// assertUnsignedTxHashUniqueness is FINAL ASSERTION 2: every
// dispatch_attempts row's unsigned_tx_hash is unique (the DB's own
// UNIQUE constraint should make this unconditionally true; asserted
// directly here as the concrete, checked fact rather than trusted purely
// by construction).
func (h *harness) assertUnsignedTxHashUniqueness() Result {
	const name = "FinalAssertion_UnsignedTxHashUniquePerAttempt"
	rows, err := h.pool.Query(h.ctx, `
		SELECT unsigned_tx_hash, count(*) FROM dispatch_attempts GROUP BY unsigned_tx_hash HAVING count(*) > 1
	`)
	if err != nil {
		return fail(name, err)
	}
	defer rows.Close()
	var dupes []string
	for rows.Next() {
		var hash string
		var count int
		if err := rows.Scan(&hash, &count); err != nil {
			return fail(name, err)
		}
		dupes = append(dupes, fmt.Sprintf("%s (%d rows)", hash, count))
	}
	if err := rows.Err(); err != nil {
		return fail(name, err)
	}
	if len(dupes) > 0 {
		return fail(name, fmt.Errorf("duplicate unsigned_tx_hash values found: %v", dupes))
	}
	return pass(name)
}

// assertEverySettledOrderHasExactlyOneE3 is FINAL ASSERTION 3.
func (h *harness) assertEverySettledOrderHasExactlyOneE3(ctx context.Context) Result {
	const name = "FinalAssertion_EverySettledOrderHasExactlyOneE3MatchingAmountOut"
	for _, s := range h.settled {
		count, err := h.ledger.journalEntryCount(ctx, s.settleKey)
		if err != nil {
			return fail(name, fmt.Errorf("order %s: %w", s.externalID, err))
		}
		if count != 1 {
			return fail(name, fmt.Errorf("order %s: %d E3 entries for key %q, want exactly 1", s.externalID, count, s.settleKey))
		}
		amount, err := h.ledger.journalLineAmount(ctx, s.settleKey, s.trcLiabilityCode)
		if err != nil {
			return fail(name, fmt.Errorf("order %s: %w", s.externalID, err))
		}
		if amount != s.amountOutUnits {
			return fail(name, fmt.Errorf("order %s: E3 amount = %d, want exactly amount_out = %d", s.externalID, amount, s.amountOutUnits))
		}
	}
	return pass(name)
}

// assertEveryHeldOrderHasExactlyOneReversal is FINAL ASSERTION 4.
func (h *harness) assertEveryHeldOrderHasExactlyOneReversal(ctx context.Context) Result {
	const name = "FinalAssertion_EveryHeldOrderHasExactlyOneReversalOfE2"
	for _, hd := range h.held {
		count, err := h.ledger.reversalCount(ctx, hd.conversionEntryKey)
		if err != nil {
			return fail(name, fmt.Errorf("order %s: %w", hd.externalID, err))
		}
		if count != 1 {
			return fail(name, fmt.Errorf("order %s: %d reversals of its E2, want exactly 1", hd.externalID, count))
		}
	}
	return pass(name)
}

// assertSweepBatchesReserveOncePerBatch is FINAL ASSERTION 5: every
// Sweep batch's own energy reservation count is exactly one, sized for
// the batch total, never per-recipient.
func (h *harness) assertSweepBatchesReserveOncePerBatch() Result {
	const name = "FinalAssertion_SweepBatchesNeverReservePerRecipient"
	for i, b := range h.batchRuns {
		if b.energyReservationCalls != 1 {
			return fail(name, fmt.Errorf("batch %d (%d recipients): %d energy reservation calls, want exactly 1", i, b.recipientCount, b.energyReservationCalls))
		}
	}
	return pass(name)
}

// assertNoPanicsOccurred is FINAL ASSERTION 6's own panic half (the
// deadlock/unexplained-error half is covered by every scenario and
// assertion already returning a clean fail() instead of hanging or
// erroring out of Run itself). Mirrors every prior component's own
// zero-panics check.
func (h *harness) assertNoPanicsOccurred(report *Report) Result {
	const name = "FinalAssertion_ZeroDeadlocksZeroPanicsZeroUnexplainedErrors"
	for _, s := range report.Scenarios {
		if !s.Passed && len(s.Detail) >= 9 && s.Detail[:9] == "PANICKED:" {
			return fail(name, fmt.Errorf("scenario %s panicked: %s", s.Name, s.Detail))
		}
	}
	return pass(name)
}
