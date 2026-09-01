package recon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/journal"
)

var ErrInvalidSnapshotParams = errors.New("recon: invalid snapshot parameters")

// SnapshotParams is what a caller (C2 or C5, watching a chain) reports:
// what an account they own holds on-chain, at a stated reference, against
// the ledger watermark they believe corresponds to it. C1 never fetches
// anything itself and does not know what a block is -- chain_ref is
// opaque to this package, just a string it records and echoes back.
type SnapshotParams struct {
	AccountCode   string
	ObservedUnits int64
	ChainRef      string
	Watermark     int64
	ObservedAt    time.Time
}

// Result is what IngestSnapshot reports back about the snapshot it just
// processed.
type Result struct {
	DriftUnits int64
	Halted     bool
}

func (p SnapshotParams) validate() error {
	if p.AccountCode == "" {
		return fmt.Errorf("%w: empty account_code", ErrInvalidSnapshotParams)
	}
	if p.ChainRef == "" {
		return fmt.Errorf("%w: empty chain_ref", ErrInvalidSnapshotParams)
	}
	if p.Watermark <= 0 {
		return fmt.Errorf("%w: watermark must be a positive entry id, got %d", ErrInvalidSnapshotParams, p.Watermark)
	}
	if p.ObservedAt.IsZero() {
		return fmt.Errorf("%w: zero observed_at", ErrInvalidSnapshotParams)
	}
	return nil
}

// IngestSnapshot records a reconciliation snapshot and, if the drift
// between what the chain reports and what the ledger's own history says
// (BalanceAsOf, at the caller's watermark -- not the current balance,
// since a chain snapshot is a moment and the ledger keeps moving) exceeds
// the configured tolerance for that asset, halts the ledger with reason
// BALANCE_DRIFT.
//
// The snapshot is always recorded, halt or not -- ingestion itself is
// never blocked or skipped, per the build spec's HALT SEMANTICS ("does
// NOT block:... snapshot ingestion"). Recording and (conditionally)
// halting happen in the same tx, so if the halt write somehow failed the
// snapshot wouldn't silently exist without it -- the whole call rolls
// back together, same as everywhere else in this system.
//
// The result carries DriftUnits and Halted back to the caller -- C1.8's
// HTTP handler needs both: the drift value to observe on the
// recon_drift_units metric, and whether it halted to report in the
// response body, neither of which the caller could otherwise recover
// without recomputing the same query itself.
func IngestSnapshot(ctx context.Context, tx pgx.Tx, cfg Config, p SnapshotParams) (Result, error) {
	if err := p.validate(); err != nil {
		return Result{}, err
	}

	acc, err := accounts.GetByCode(ctx, tx, p.AccountCode)
	if err != nil {
		return Result{}, fmt.Errorf("recon: ingest snapshot for %q: %w", p.AccountCode, err)
	}

	ledgerAmt, err := journal.BalanceAsOf(ctx, tx, p.AccountCode, p.Watermark)
	if err != nil {
		return Result{}, fmt.Errorf("recon: ingest snapshot for %q: %w", p.AccountCode, err)
	}

	drift := p.ObservedUnits - ledgerAmt.Units
	result := Result{DriftUnits: drift}

	_, err = tx.Exec(ctx, `
		INSERT INTO recon_snapshots
			(account_id, asset, observed_units, chain_ref, ledger_units, watermark, drift_units, observed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, acc.ID, string(acc.Asset), p.ObservedUnits, p.ChainRef, ledgerAmt.Units, p.Watermark, drift, p.ObservedAt)
	if err != nil {
		return Result{}, fmt.Errorf("recon: ingest snapshot for %q: recording: %w", p.AccountCode, err)
	}

	tolerance := cfg.toleranceFor(acc.Asset)
	absDrift := drift
	if absDrift < 0 {
		absDrift = -absDrift
	}
	if absDrift <= tolerance {
		return result, nil
	}
	result.Halted = true

	return result, halt.Set(ctx, tx, halt.SetParams{
		Reason: halt.ReasonBalanceDrift,
		Detail: map[string]any{
			"account_code":   p.AccountCode,
			"asset":          string(acc.Asset),
			"observed_units": p.ObservedUnits,
			"ledger_units":   ledgerAmt.Units,
			"drift_units":    drift,
			"watermark":      p.Watermark,
			"chain_ref":      p.ChainRef,
		},
		Actor: "recon:snapshot_ingestion",
	})
}
