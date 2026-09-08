package dispatch

import (
	"context"
	"fmt"

	"dispatcher/internal/slots"
)

// HandleMidFlightFreeze reacts to a slot freeze detected between
// selecting it for attemptID and that attempt reaching finality --
// the scenario catalog's own "caught pre-settlement, should be
// recoverable without a loss entry" case, distinct from an
// already-settled slot freezing later (a terminal, insurable risk with
// no engineering fix, and not this function's concern).
//
// Always retires the slot (a fact independent of this specific attempt:
// a frozen slot must never be selected for a NEW dispatch again,
// confirmed or not), then branches on how far attemptID itself had
// gotten:
//
//   - BUILT or SIGNED (never broadcast): abandoned. Marked FAILED --
//     never re-signed, never broadcast -- so a fresh attempt (a new
//     attempt_number, a newly selected slot, a newly built transfer) is
//     legitimately a DIFFERENT attempt, not a retry of the frozen one
//     (invariant 1's exactly-once is about one attempt). Re-selecting a
//     slot and starting that new attempt is the CALLER's job, the same
//     boundary Broadcast itself draws around retrying on its own.
//   - BROADCAST (not yet CONFIRMED): genuinely ambiguous -- the payout
//     may have landed before the freeze, or not. needsAlert=true, and
//     nothing here (or anywhere in this call) touches C1 in either
//     direction; guessing which outcome happened is explicitly out of
//     scope, a human decides.
//   - CONFIRMED or FAILED: already resolved before the freeze was even
//     detected; nothing to do.
func (d *Dispatcher) HandleMidFlightFreeze(ctx context.Context, attemptID int64, slotStore *slots.Store) (needsAlert bool, err error) {
	attempt, err := d.Attempts.Get(ctx, attemptID)
	if err != nil {
		return false, err
	}

	switch attempt.Status {
	case BroadcastBuilt, BroadcastSigned:
		if _, err := d.Attempts.MarkFailed(ctx, attemptID); err != nil {
			return false, fmt.Errorf("dispatch: marking attempt %d FAILED after mid-flight freeze: %w", attemptID, err)
		}
		if err := slotStore.MarkRetired(ctx, attempt.SlotID); err != nil {
			return false, fmt.Errorf("dispatch: retiring frozen slot %d: %w", attempt.SlotID, err)
		}
		return false, nil

	case BroadcastBroadcast:
		if err := slotStore.MarkRetired(ctx, attempt.SlotID); err != nil {
			return false, fmt.Errorf("dispatch: retiring frozen slot %d: %w", attempt.SlotID, err)
		}
		return true, nil

	default: // CONFIRMED, FAILED
		return false, nil
	}
}
