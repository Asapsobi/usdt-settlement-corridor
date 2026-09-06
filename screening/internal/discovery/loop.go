package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"screening/internal/db"
	"screening/internal/ledgerclient"
	"screening/internal/provider"
)

// DefaultInterval is how often RunLoop polls C1 for newly-funded orders,
// when Interval is unset. §2 of docs/03-build/c3-screening-build-prompts.md's
// "Read this first" names 5-10s as "not a real cost yet" at this
// system's actual volume (~4 deposits/hour peak, component-map.md).
const DefaultInterval = 5 * time.Second

// FundedOrderPoller is the one call this loop needs from C1 --
// ledgerclient.Client's real implementation, or a fake for testing (the
// C3.3 build spec's own acceptance criterion: "PollFundedOrders against
// a fake C1 client").
type FundedOrderPoller interface {
	PollFundedOrders(ctx context.Context, cursor string) (refs []ledgerclient.OrderRef, newCursor string, err error)
}

// RunLoop polls FundedOrderPoller on an interval, enqueueing every newly
// discovered funded order into screening_queue (idempotent: an order
// already queued is left untouched, never re-inserted or reset), and
// separately retries resolving sender_address for any queued order that
// doesn't have one yet.
//
// The two are deliberately independent passes, not one gated on the
// other: discovery advances the cursor and records an order the moment
// C1 reports it funded, whether or not that same tick's GetSenderAddress
// call happened to succeed. Gating enqueue on a successful lookup would
// mean a single transient failure could permanently lose an order --
// C1's own PollFundedOrders cursor only ever moves forward, so an order
// not enqueued this tick because its lookup failed would never be seen
// again once the cursor passes it. Recording it immediately, with a NULL
// sender_address, and resolving that address via a separate retry pass
// against screening_queue itself (never against C1's list-by-state
// pagination) is what makes the retry safe and bounded instead of
// requiring cursor rewind logic.
//
// Blocks until ctx is cancelled, returning ctx.Err().
func RunLoop(ctx context.Context, pool *db.Pool, poller FundedOrderPoller, senderLookup provider.SenderAddressLookup, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := RunTick(ctx, pool, poller, senderLookup); err != nil {
		slog.Error("discovery: initial tick failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := RunTick(ctx, pool, poller, senderLookup); err != nil {
				slog.Error("discovery: tick failed", "error", err)
			}
		}
	}
}

// RunTick runs one discovery pass: poll, enqueue, retry unresolved
// addresses. Exported (matching pipeline.RunTick and rescreen.RunTick's
// own convention in this module) so a caller that needs to drive a
// single, deterministic tick -- a test, or C3.9's replay harness --
// doesn't have to wait on RunLoop's own ticker.
func RunTick(ctx context.Context, q Queryer, poller FundedOrderPoller, senderLookup provider.SenderAddressLookup) error {
	cursor, err := getCursor(ctx, q)
	if err != nil {
		return err
	}

	refs, newCursor, err := poller.PollFundedOrders(ctx, cursor)
	if err != nil {
		return fmt.Errorf("discovery: polling funded orders: %w", err)
	}

	for _, ref := range refs {
		var senderAddr *string
		if addr, err := senderLookup.GetSenderAddress(ctx, ref.ExternalID); err != nil {
			slog.Warn("discovery: sender_address not yet resolved for a newly discovered order, will retry",
				"order_id", ref.OrderID, "external_id", ref.ExternalID, "error", err)
		} else {
			senderAddr = &addr
		}
		if err := enqueueIfNew(ctx, q, ref.OrderID, ref.ExternalID, senderAddr); err != nil {
			return err
		}
	}

	if newCursor != "" && newCursor != cursor {
		if err := setCursor(ctx, q, newCursor); err != nil {
			return err
		}
	}

	if err := retryUnresolvedSenderAddresses(ctx, q, senderLookup); err != nil {
		return fmt.Errorf("discovery: retrying unresolved sender addresses: %w", err)
	}
	return nil
}

// retryUnresolvedSenderAddresses is the second, independent pass RunLoop
// runs every tick: every screening_queue row still missing a
// sender_address gets one more GetSenderAddress attempt, updated in
// place the moment it succeeds. A row that fails again is left exactly
// as it was -- picked up again next tick -- never dropped, never marked
// with a placeholder value.
func retryUnresolvedSenderAddresses(ctx context.Context, q Queryer, senderLookup provider.SenderAddressLookup) error {
	pending, err := listMissingSenderAddress(ctx, q)
	if err != nil {
		return err
	}
	for _, entry := range pending {
		addr, err := senderLookup.GetSenderAddress(ctx, entry.ExternalID)
		if err != nil {
			slog.Warn("discovery: sender_address still not resolved, will retry next tick",
				"order_id", entry.OrderID, "external_id", entry.ExternalID, "error", err)
			continue
		}
		if err := setSenderAddress(ctx, q, entry.OrderID, addr); err != nil {
			return err
		}
	}
	return nil
}
