package recon

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/halt"
	"ledger/internal/journal"
)

// Reconciler runs the periodic self-checks the build spec calls for,
// independent of any externally reported snapshot: TrialBalance,
// VerifyBalances, and the position:corridor ceiling check.
type Reconciler struct {
	pool *pgxpool.Pool
	cfg  Config
}

func NewReconciler(pool *pgxpool.Pool, cfg Config) *Reconciler {
	return &Reconciler{pool: pool, cfg: cfg}
}

// Run ticks every cfg.Interval until ctx is cancelled. Intended to be
// started with `go reconciler.Run(ctx)` alongside the HTTP server, using
// the same context cmd/ledgerd cancels on shutdown.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}

// RunOnce runs a single self-check cycle. Exported (rather than folded
// into Run as an unexported step) so tests can exercise the check logic
// directly, and so a test that wants to prove the *ticker itself* fires
// within one interval can still do that separately by calling Run with a
// short Config.Interval.
//
// If the ledger is already halted, RunOnce returns immediately without
// running any check. Nothing here can un-halt the system (there is no
// auto-clear anywhere in this codebase), so re-detecting and re-logging
// an already-known, unresolved condition every single cycle would just
// spam halt_log forever until an operator clears it -- this is the one
// place in the package that deliberately deduplicates, and it is a
// judgment call: not derived from the build spec's HALT SEMANTICS or
// ACCEPTANCE sections, which describe what triggers a halt but not what
// a *repeat* trigger while already halted should do.
func (r *Reconciler) RunOnce(ctx context.Context) {
	halted, err := halt.IsHalted(ctx, r.pool)
	if err != nil {
		slog.Error("recon: checking halt state", "error", err)
		return
	}
	if halted {
		return
	}

	if r.checkTrialBalance(ctx) {
		return
	}
	if r.checkCacheDivergence(ctx) {
		return
	}
	r.checkCorridorCeiling(ctx)
}

// checkTrialBalance is the second line of defence behind C1.2's deferred
// constraint trigger: that trigger should already make an unbalanced
// entry impossible to commit, but if it somehow didn't, TrialBalance
// summing every journal_lines row directly (not through the
// account_balances cache) is what catches it here.
func (r *Reconciler) checkTrialBalance(ctx context.Context) (halted bool) {
	trial, err := journal.TrialBalance(ctx, r.pool)
	if err != nil {
		slog.Error("recon: computing trial balance", "error", err)
		return false
	}
	for asset, total := range trial {
		if total != 0 {
			if err := halt.Set(ctx, r.pool, halt.SetParams{
				Reason: "TRIAL_BALANCE_BROKEN",
				Detail: map[string]any{"asset": string(asset), "total_units": total},
				Actor:  "recon:self_check",
			}); err != nil {
				slog.Error("recon: setting TRIAL_BALANCE_BROKEN halt", "error", err)
			}
			return true
		}
	}
	return false
}

func (r *Reconciler) checkCacheDivergence(ctx context.Context) (halted bool) {
	discrepancies, err := journal.VerifyBalances(ctx, r.pool)
	if err != nil {
		slog.Error("recon: verifying balances", "error", err)
		return false
	}
	if len(discrepancies) == 0 {
		return false
	}
	if err := halt.Set(ctx, r.pool, halt.SetParams{
		Reason: "CACHE_DIVERGENCE",
		Detail: map[string]any{"discrepancies": discrepancies},
		Actor:  "recon:self_check",
	}); err != nil {
		slog.Error("recon: setting CACHE_DIVERGENCE halt", "error", err)
	}
	return true
}

// checkCorridorCeiling raises an alert (a structured log line -- this
// system has no paging/notification integration of its own; whatever
// consumes these logs downstream owns that) when position:corridor's
// magnitude for a configured asset exceeds its ceiling. This is
// deliberately never a halt: exceeding the ceiling means the treasury
// rebalance loop is behind, which is slow, not broken, and halting on a
// slow rebalance would stop the business over a non-error.
func (r *Reconciler) checkCorridorCeiling(ctx context.Context) {
	for asset, ceiling := range r.cfg.CorridorCeilings {
		code := "position:corridor:" + string(asset)
		bal, err := journal.Balance(ctx, r.pool, code)
		if err != nil {
			slog.Error("recon: reading corridor balance", "account", code, "error", err)
			continue
		}
		magnitude := bal.Units
		if magnitude < 0 {
			magnitude = -magnitude
		}
		if magnitude > ceiling {
			slog.Warn("position:corridor exceeds configured ceiling -- treasury rebalance appears behind",
				"account", code, "magnitude_units", magnitude, "ceiling_units", ceiling)
		}
	}
}
