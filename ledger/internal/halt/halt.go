// Package halt owns system_state and halt_log: whether the ledger is
// currently halted, why, the full history of every halt and clear, and a
// short-lived in-memory cache so a halt-blocked transition can reject
// fast without paying a database round trip in the common case.
//
// C1.6 built a minimal slice of this early (Set/IsHalted/Reason only,
// no clearing, no audit log, no cache) because its post-settlement reorg
// scenario needed somewhere to set a halt and C1.7 didn't exist yet. This
// is the rest of it.
package halt

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrClearRequiresOperator = errors.New("halt: clearing requires both an operator identity and a note")

// Executor is satisfied by both *pgxpool.Pool and pgx.Tx. Set and Clear
// accept this rather than pgx.Tx specifically because they have two
// genuinely different callers: C1.6's reorg handling, which must commit
// a halt atomically with the loss entry it just posted (so it passes a
// tx), and the self-check ticker in internal/recon, which has no
// enclosing transaction to join and just passes the pool directly.
type Executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SetParams describes why the ledger is being halted.
type SetParams struct {
	Reason string
	Detail map[string]any
	Actor  string
}

// Set halts the ledger and appends a 'set' row to halt_log. It always
// writes both, even if the ledger is already halted: halt_log is the
// complete history, and a later trigger overwriting system_state's
// current reason/detail is exactly why that history matters -- without
// it, an earlier cause would be silently lost the moment a second one
// fires. Callers that poll on a schedule (internal/recon's self-check
// ticker) are expected to check IsHalted themselves and skip calling Set
// again for a condition that hasn't changed, rather than relying on Set
// to deduplicate -- Set has no way to know "this is the same problem as
// last time" from a reason and a detail blob alone.
func Set(ctx context.Context, q Executor, p SetParams) error {
	if _, err := q.Exec(ctx, `
		UPDATE system_state
		SET halted = true, halt_reason = $1, halt_detail = $2, halted_at = now(), halted_by = $3
		WHERE id = 1
	`, p.Reason, p.Detail, p.Actor); err != nil {
		return fmt.Errorf("halt: set: %w", err)
	}

	if _, err := q.Exec(ctx, `
		INSERT INTO halt_log (action, reason, detail, actor)
		VALUES ('set', $1, $2, $3)
	`, p.Reason, p.Detail, p.Actor); err != nil {
		return fmt.Errorf("halt: set: logging: %w", err)
	}
	return nil
}

// ClearParams is what an operator supplies to clear a halt. Both fields
// are required -- clearing is manual only, and an operator identity plus
// a free-text note are the minimum record of who decided the ledger was
// safe to resume and why.
type ClearParams struct {
	Actor string
	Note  string
}

// Clear un-halts the ledger and appends a 'clear' row to halt_log. There
// is no automatic clearing anywhere in this system; this is the only way
// halted ever becomes false again.
func Clear(ctx context.Context, q Executor, p ClearParams) error {
	if p.Actor == "" || p.Note == "" {
		return ErrClearRequiresOperator
	}

	if _, err := q.Exec(ctx, `
		UPDATE system_state
		SET halted = false, cleared_at = now(), cleared_by = $1
		WHERE id = 1
	`, p.Actor); err != nil {
		return fmt.Errorf("halt: clear: %w", err)
	}

	if _, err := q.Exec(ctx, `
		INSERT INTO halt_log (action, note, actor)
		VALUES ('clear', $1, $2)
	`, p.Note, p.Actor); err != nil {
		return fmt.Errorf("halt: clear: logging: %w", err)
	}
	return nil
}

// IsHalted reports whether the ledger is currently halted. This is the
// authoritative, uncached read -- callers on the hot path that need the
// 1-second-stale fast check should use Cache.IsHalted instead.
func IsHalted(ctx context.Context, q Executor) (bool, error) {
	var halted bool
	err := q.QueryRow(ctx, `SELECT halted FROM system_state WHERE id = 1`).Scan(&halted)
	return halted, err
}

// Reason returns the current halt_reason, or "" if not halted.
func Reason(ctx context.Context, q Executor) (string, error) {
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

// Cache is the in-memory, at-most-1-second-stale halt flag the build spec
// calls for: "cached in memory with a max staleness of 1 second... a
// cached read is for fast rejection, the transactional read is what is
// authoritative." It refreshes lazily on read rather than polling on its
// own ticker -- the staleness bound is the same either way, and a caller
// that never asks never causes a query.
type Cache struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	// halted/updatedAt are read and written only under mu.
	halted    bool
	updatedAt time.Time
}

const maxCacheAge = time.Second

// NewCache creates a Cache backed by pool. Every process that calls
// orders.Transition needs exactly one of these; internal/orders holds it
// via SetCache below rather than taking it as a parameter on every
// Transition call, since that signature is already fixed by C1.5 and
// every existing caller.
func NewCache(pool *pgxpool.Pool) *Cache {
	return &Cache{pool: pool}
}

// IsHalted returns the cached value if it is less than 1 second old,
// otherwise refreshes from the database first. The refreshed value is
// itself cached for the next caller, so under load this is at most one
// query per second regardless of call volume.
func (c *Cache) IsHalted(ctx context.Context) (bool, error) {
	c.mu.Lock()
	if time.Since(c.updatedAt) < maxCacheAge {
		halted := c.halted
		c.mu.Unlock()
		return halted, nil
	}
	c.mu.Unlock()

	halted, err := IsHalted(ctx, c.pool)
	if err != nil {
		return false, err
	}

	c.mu.Lock()
	c.halted = halted
	c.updatedAt = time.Now()
	c.mu.Unlock()
	return halted, nil
}
