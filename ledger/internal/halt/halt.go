// Package halt owns system_state: whether the ledger is currently halted,
// and why.
//
// This is deliberately a minimal slice of C1.7 (the reconciler), built
// early because C1.6's post-settlement reorg handling (the loss case)
// must set a halt and there is nowhere else for that to live. It does not
// implement C1.7's full halt lifecycle: no ClearHalt, no halt_log, no
// reconciliation snapshots, no self-check ticker, and nothing here is
// consulted by C1.5's Transition to actually block a halt-blocked
// transition -- that enforcement is explicitly C1.7's job, per the build
// spec's own HALT SEMANTICS section. This package only answers "is the
// ledger halted, and why," and lets C1.6 set that state once.
package halt

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Queryer is satisfied by both *pgxpool.Pool and pgx.Tx.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SetParams describes why the ledger is being halted.
type SetParams struct {
	Reason string
	Detail map[string]any
	Actor  string
}

// Set halts the ledger, in tx, so the halt commits atomically with
// whatever triggered it (for C1.6, the reorg-loss entry). It is a plain
// UPDATE, not conditional on the current halted value: this package does
// not yet define what should happen when a halt is set while the system
// is already halted for a different reason -- that is genuinely C1.7's
// design space, since C1.7 is the one with multiple possible halt
// triggers. For C1.6's single trigger (POST_SETTLEMENT_REORG), a plain
// overwrite is correct.
func Set(ctx context.Context, tx pgx.Tx, p SetParams) error {
	_, err := tx.Exec(ctx, `
		UPDATE system_state
		SET halted = true, halt_reason = $1, halt_detail = $2, halted_at = now(), halted_by = $3
		WHERE id = 1
	`, p.Reason, p.Detail, p.Actor)
	return err
}

// IsHalted reports whether the ledger is currently halted.
func IsHalted(ctx context.Context, q Queryer) (bool, error) {
	var halted bool
	err := q.QueryRow(ctx, `SELECT halted FROM system_state WHERE id = 1`).Scan(&halted)
	return halted, err
}

// Reason returns the current halt_reason, or "" if not halted.
func Reason(ctx context.Context, q Queryer) (string, error) {
	var reason *string
	err := q.QueryRow(ctx, `SELECT halt_reason FROM system_state WHERE id = 1`).Scan(&reason)
	if err != nil {
		return "", err
	}
	if reason == nil {
		return "", nil
	}
	return *reason, nil
}
