package dispatch

import (
	"context"
	"errors"
	"fmt"

	"dispatcher/internal/ledgerclient"
)

// ReconcileFailedDispatches scans every order still locally DISPATCHING
// and, for each whose most recent broadcast attempt has FAILED, reports
// the dispatch failure via reporter and marks it HELD locally. This is
// what actually closes the "crashed between reversing the conversion and
// transitioning to held" gap invariant 4 requires -- not a hope that
// whatever called Broadcast also happens to retry ReportDispatchFailure
// on its own. Meant to run on a ticker (a few minutes, per C5.7's own
// build spec); one call here is one pass, not a loop.
//
// An order whose latest attempt is still BUILT, SIGNED, or BROADCAST is
// left alone -- it may still succeed, or a fresh attempt may still be
// started for it; only a FAILED latest attempt with no successor means
// this order has genuinely given up.
//
// Returns how many orders this pass reported and closed, so a caller can
// log or alert on an unexpectedly large number.
func (d *Dispatcher) ReconcileFailedDispatches(ctx context.Context, reporter ledgerclient.DispatchFailureReporter, reason string) (int, error) {
	dispatching, err := d.Store.ListDispatching(ctx)
	if err != nil {
		return 0, fmt.Errorf("dispatch: listing dispatching orders for reconciliation: %w", err)
	}

	closed := 0
	for _, local := range dispatching {
		latest, err := d.Attempts.LatestForOrder(ctx, local.OrderID)
		if err != nil {
			if errors.Is(err, ErrBroadcastAttemptNotFound) {
				// Entered dispatching but never even built a first
				// attempt yet -- not this job's concern, still in
				// flight.
				continue
			}
			return closed, fmt.Errorf("dispatch: fetching latest attempt for order %d: %w", local.OrderID, err)
		}
		if latest.Status != BroadcastFailed {
			continue
		}

		order, err := d.Ledger.GetOrder(ctx, local.ExternalID)
		if err != nil {
			return closed, fmt.Errorf("dispatch: fetching order %s for reconciliation: %w", local.ExternalID, err)
		}
		// Another actor may have already moved this order past
		// dispatching (e.g. a concurrent reconciliation pass, or a
		// human) between ListDispatching's own read and here.
		if order.State != "dispatching" {
			continue
		}

		if err := reporter.ReportDispatchFailure(ctx, order, local.ConversionEntryKey, reason); err != nil {
			return closed, fmt.Errorf("dispatch: reporting dispatch failure for order %d: %w", local.OrderID, err)
		}
		if err := d.Store.MarkHeld(ctx, local.OrderID); err != nil {
			return closed, fmt.Errorf("dispatch: marking order %d HELD locally: %w", local.OrderID, err)
		}
		closed++
	}
	return closed, nil
}
