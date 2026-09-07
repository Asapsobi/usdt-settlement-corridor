package buffer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"energybroker/internal/provider"
)

// TronEnergyReader is the one piece of TRON chain-read this component
// needs -- read-only, no signing, the same posture C2 took toward BSC
// (watch, never write). DelegationUnits reports how many energy units
// delegationID currently shows as actively delegated to address
// on-chain -- 0 if fully revoked or expired, less than the originally
// requested amount if partially reduced.
//
// This chunk ships no real TRON RPC client: a real client is a
// substantial, separate piece of infrastructure (an HTTP/gRPC connection
// to a TRON full node or an indexer, request signing NOT required since
// this is read-only, but still a new external dependency this chunk's
// own acceptance criteria do not require -- "against a forked/simulated
// TRON node" names a fake, not a live testnet). FakeTronReader
// (faketron.go) is the only implementation here, mirroring
// internal/provider's own posture of shipping the fake first and a real
// vendor/chain client later, behind the same interface.
type TronEnergyReader interface {
	DelegationUnits(ctx context.Context, address, delegationID string) (int64, error)
}

// Alerter is how Reconcile surfaces a correction the next Replenish
// cycle must not be left to silently absorb. Optional -- nil means no
// alert is sent, never a panic, the same convention every other
// pluggable dependency in this project's own sibling modules uses (e.g.
// screening's pipeline.MetricsRecorder).
type Alerter interface {
	AlertBufferShortfall(ctx context.Context, row Row, expectedUnits, actualUnits int64)
}

// VerifyOnChain queries d's own delegation on-chain and confirms the
// expected units are actually present. A vendor's Delegate response
// claiming success is never, on its own, sufficient to mark capacity
// AVAILABLE -- invariant 1 -- this is the independent check that makes
// that claim trustworthy. Returns false (not an error) for both "fully
// absent" and "partially present" -- a partial delegation is not safe
// to treat as the full amount this component believes it has.
func (b *Buffer) VerifyOnChain(ctx context.Context, d provider.Delegation) (bool, error) {
	total, err := b.reader.DelegationUnits(ctx, d.TargetAddress, d.ID)
	if err != nil {
		return false, fmt.Errorf("buffer: querying on-chain delegation %s for %s: %w", d.ID, d.TargetAddress, err)
	}
	return total >= d.EnergyUnits, nil
}

// DefaultReconcileInterval is how often RunReconcileLoop re-checks, when
// Interval is unset -- "a ticker, default every few minutes" per this
// chunk's own build spec.
const DefaultReconcileInterval = 3 * time.Minute

// DefaultReconcileLookahead is how close to expires_at a row must be to
// count as "nearing" it, when Config's own value (added below) is
// unset. Wider than one poll interval so a row is never checked for the
// first time only after it's already too late to alert usefully.
const DefaultReconcileLookahead = 15 * time.Minute

// RunReconcileLoop re-verifies on an interval. Blocks until ctx is
// cancelled, returning ctx.Err().
func (b *Buffer) RunReconcileLoop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := b.Reconcile(ctx); err != nil {
		slog.Error("buffer: initial reconcile failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := b.Reconcile(ctx); err != nil {
				slog.Error("buffer: reconcile failed", "error", err)
			}
		}
	}
}

// Reconcile re-verifies every AVAILABLE or RESERVED row nearing its own
// expires_at against on-chain truth. A row whose on-chain reality no
// longer matches bookkeeping (the delegation expired early, or a
// vendor's own dashboard revoked it -- this happens in the real
// energy-rental market) is corrected immediately: marked EXPIRED, and
// alerted -- a RESERVED row's shortfall is never silently left for the
// next Replenish cycle to absorb, since Replenish only ever looks at
// the AVAILABLE total, not at what a specific order was already
// promised.
func (b *Buffer) Reconcile(ctx context.Context) error {
	lookahead := b.cfg.ReconcileLookahead
	if lookahead <= 0 {
		lookahead = DefaultReconcileLookahead
	}

	rows, err := rowsNearingExpiry(ctx, b.pool, lookahead)
	if err != nil {
		return fmt.Errorf("buffer: reconcile: %w", err)
	}

	var toExpire []int64
	for _, row := range rows {
		actual, err := b.reader.DelegationUnits(ctx, b.cfg.StagingAddress, row.DelegationID)
		if err != nil {
			slog.Error("buffer: reconcile: on-chain query failed, leaving row as-is for next cycle",
				"row_id", row.ID, "delegation_id", row.DelegationID, "error", err)
			continue
		}
		if actual >= row.Units {
			continue // still fully present, nothing to correct
		}

		slog.Error("buffer: reconcile: on-chain delegation no longer matches bookkeeping, correcting",
			"row_id", row.ID, "delegation_id", row.DelegationID, "provider", row.ProviderName,
			"status", row.Status, "expected_units", row.Units, "actual_units", actual)
		toExpire = append(toExpire, row.ID)

		if b.alerter != nil {
			b.alerter.AlertBufferShortfall(ctx, row, row.Units, actual)
		}
	}

	if err := markExpired(ctx, b.pool, toExpire); err != nil {
		return fmt.Errorf("buffer: reconcile: %w", err)
	}

	b.mu.Lock()
	b.lastReconciledAt = time.Now().UTC()
	b.mu.Unlock()
	return nil
}
