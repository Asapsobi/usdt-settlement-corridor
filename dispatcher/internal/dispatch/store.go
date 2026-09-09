package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"dispatcher/internal/db"
)

// ErrAttemptNotFound means no dispatch_state row exists for the given order.
var ErrAttemptNotFound = errors.New("dispatch: no such attempt")

// Store is dispatch_state's own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// Create records a new attempt, idempotent on order_id: a second Create
// for an order that already has a row returns that row unchanged, never
// erroring and never overwriting it -- the same "crash between the C1
// call succeeding and this INSERT" recovery case EnterDispatching's own
// retry-on-version-conflict logic is built around, just at the local
// storage layer instead of C1's.
func (s *Store) Create(ctx context.Context, a Attempt) (Attempt, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO dispatch_state (order_id, external_id, slot_id, conversion_entry_key, status, entered_dispatching_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (order_id) DO NOTHING
		RETURNING order_id, external_id, slot_id, conversion_entry_key, status, entered_dispatching_at
	`, a.OrderID, a.ExternalID, a.SlotID, a.ConversionEntryKey, string(a.Status), a.EnteredDispatchingAt)

	attempt, err := scanAttempt(row)
	if err == nil {
		return attempt, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, fmt.Errorf("dispatch: creating attempt for order %d: %w", a.OrderID, err)
	}

	// ON CONFLICT DO NOTHING returned no row: an attempt for this order
	// already existed.
	return s.Get(ctx, a.OrderID)
}

// Get fetches the attempt recorded for orderID.
func (s *Store) Get(ctx context.Context, orderID int64) (Attempt, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT order_id, external_id, slot_id, conversion_entry_key, status, entered_dispatching_at
		FROM dispatch_state WHERE order_id = $1
	`, orderID)
	attempt, err := scanAttempt(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, ErrAttemptNotFound
		}
		return Attempt{}, fmt.Errorf("dispatch: fetching attempt for order %d: %w", orderID, err)
	}
	return attempt, nil
}

// MarkSettled transitions orderID's attempt from DISPATCHING to SETTLED.
// C5.6's own concern -- present here only so the Status enum and its
// legal transitions are defined in one place from the start, matching
// this project's convention of getting a lifecycle's shape right before
// the chunk that first needs every leg of it exists.
func (s *Store) MarkSettled(ctx context.Context, orderID int64) error {
	return s.markStatus(ctx, orderID, StatusSettled)
}

// MarkHeld transitions orderID's attempt from DISPATCHING to HELD --
// C5.7's own concern.
func (s *Store) MarkHeld(ctx context.Context, orderID int64) error {
	return s.markStatus(ctx, orderID, StatusHeld)
}

// ListDispatching returns every attempt still in DISPATCHING -- C5.7's
// own reconciliation job scans these to find orders whose most recent
// broadcast attempt has FAILED with no further action ever having been
// taken.
func (s *Store) ListDispatching(ctx context.Context) ([]Attempt, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT order_id, external_id, slot_id, conversion_entry_key, status, entered_dispatching_at
		FROM dispatch_state WHERE status = $1 ORDER BY order_id
	`, string(StatusDispatching))
	if err != nil {
		return nil, fmt.Errorf("dispatch: listing dispatching attempts: %w", err)
	}
	defer rows.Close()

	var out []Attempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// markStatus transitions orderID from DISPATCHING to status, idempotent
// on a retry that lands after its own prior call already succeeded:
// found while wiring ConfirmFinality into a caller (the MVP proof run's
// orchestration loop) that genuinely retries it on every tick until
// finality -- a crash between settleOrder committing and this call the
// first time around must not turn a legitimate replay into an error the
// second time around, the same "replay, don't fail" discipline every
// other idempotency key in this project already gets.
func (s *Store) markStatus(ctx context.Context, orderID int64, status Status) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE dispatch_state SET status = $1, updated_at = now()
		WHERE order_id = $2 AND status = $3
	`, string(status), orderID, string(StatusDispatching))
	if err != nil {
		return fmt.Errorf("dispatch: marking order %d %s: %w", orderID, status, err)
	}
	if tag.RowsAffected() == 0 {
		current, err := s.Get(ctx, orderID)
		if err != nil {
			return err
		}
		if current.Status == status {
			return nil
		}
		return fmt.Errorf("dispatch: order %d attempt is not in DISPATCHING, cannot mark %s", orderID, status)
	}
	return nil
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanAttempt(row scanRow) (Attempt, error) {
	var a Attempt
	var status string
	err := row.Scan(&a.OrderID, &a.ExternalID, &a.SlotID, &a.ConversionEntryKey, &status, &a.EnteredDispatchingAt)
	if err != nil {
		return Attempt{}, err
	}
	a.Status = Status(status)
	return a, nil
}
