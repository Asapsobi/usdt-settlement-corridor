// Package slots is C5's own slot-identity registry: which of the six
// payout slots (decision 4) exists, its real TRON address, its lifecycle
// status, and its own running transaction count. See this package's own
// migration for why this is a separate table (and, deliberately, a
// separate concern) from S1's own slot-custody registry.
package slots

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dispatcher/internal/db"
)

// Status is one slot's own lifecycle state.
type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusRetiring Status = "RETIRING"
	StatusRetired  Status = "RETIRED"
)

// Slot is one row of the slots table.
type Slot struct {
	ID             int
	TronAddress    string
	Status         Status
	ActivatedAt    time.Time
	RetiredAt      *time.Time
	TxCount        int64
	LastDispatchAt *time.Time
}

// ErrDuplicateSlot means id or tronAddress is already registered.
var ErrDuplicateSlot = errors.New("slots: slot id or TRON address is already registered")

// ErrSlotNotFound means no row exists for the given slot id.
var ErrSlotNotFound = errors.New("slots: no such slot")

// ErrIllegalTransition is the Go-level guard mirroring the DB trigger's
// own rejection, for the specific "can't skip backwards" cases this
// package itself catches before ever issuing the UPDATE -- see Retire
// and MarkRetiring's own doc comments for exactly which transitions this
// covers.
var ErrIllegalTransition = errors.New("slots: illegal status transition")

// Store is this package's own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// Create registers a new slot, ACTIVE from activatedAt. tronAddress
// comes from a real S1 SlotAddress call (this package never derives one
// itself -- see the migration's own doc comment).
func (s *Store) Create(ctx context.Context, id int, tronAddress string, activatedAt time.Time) (Slot, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO slots (id, tron_address, status, activated_at, tx_count)
		VALUES ($1, $2, $3, $4, 0)
		RETURNING id, tron_address, status, activated_at, retired_at, tx_count, last_dispatch_at
	`, id, tronAddress, string(StatusActive), activatedAt)
	slot, err := scanSlot(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Slot{}, ErrDuplicateSlot
		}
		return Slot{}, fmt.Errorf("slots: creating slot %d: %w", id, err)
	}
	return slot, nil
}

// Get fetches one slot by id.
func (s *Store) Get(ctx context.Context, id int) (Slot, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, tron_address, status, activated_at, retired_at, tx_count, last_dispatch_at FROM slots WHERE id = $1
	`, id)
	slot, err := scanSlot(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Slot{}, ErrSlotNotFound
		}
		return Slot{}, fmt.Errorf("slots: fetching slot %d: %w", id, err)
	}
	return slot, nil
}

// List returns every slot at status, ordered by id.
func (s *Store) List(ctx context.Context, status Status) ([]Slot, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, tron_address, status, activated_at, retired_at, tx_count, last_dispatch_at FROM slots WHERE status = $1 ORDER BY id
	`, string(status))
	if err != nil {
		return nil, fmt.Errorf("slots: listing slots at %s: %w", status, err)
	}
	defer rows.Close()

	var out []Slot
	for rows.Next() {
		slot, err := scanSlot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, slot)
	}
	return out, rows.Err()
}

// MarkRetiring transitions id from ACTIVE to RETIRING -- the normal
// cap-exhaustion path: accepts no new dispatches, finishes anything
// already in flight.
func (s *Store) MarkRetiring(ctx context.Context, id int) error {
	return s.transition(ctx, id, StatusActive, StatusRetiring, nil)
}

// MarkRetired transitions id to RETIRED -- from RETIRING (the normal
// drain-complete path) or directly from ACTIVE (C5.9's own mid-flight
// freeze path: "not just RETIRING -- a frozen slot is not coming back").
func (s *Store) MarkRetired(ctx context.Context, id int) error {
	slot, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if slot.Status != StatusActive && slot.Status != StatusRetiring {
		return fmt.Errorf("%w: slot %d is %s, not ACTIVE or RETIRING", ErrIllegalTransition, id, slot.Status)
	}
	return s.transition(ctx, id, slot.Status, StatusRetired, retiredAtNow)
}

func retiredAtNow(now time.Time) *time.Time { return &now }

func (s *Store) transition(ctx context.Context, id int, from, to Status, retiredAt func(time.Time) *time.Time) error {
	now := time.Now().UTC()
	var err error
	if retiredAt != nil {
		_, err = s.pool.Exec(ctx, `
			UPDATE slots SET status = $1, retired_at = $2 WHERE id = $3 AND status = $4
		`, string(to), retiredAt(now), id, string(from))
	} else {
		_, err = s.pool.Exec(ctx, `
			UPDATE slots SET status = $1 WHERE id = $2 AND status = $3
		`, string(to), id, string(from))
	}
	if err != nil {
		return fmt.Errorf("slots: transitioning slot %d %s -> %s: %w", id, from, to, err)
	}
	return nil
}

// IncrementTxCount bumps id's own running transaction count by delta
// (always positive in practice -- a batch adds len(recipients), a single
// transfer adds 1). Never reset on rotation start, per this package's
// own migration comment.
func (s *Store) IncrementTxCount(ctx context.Context, id int, delta int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE slots SET tx_count = tx_count + $1 WHERE id = $2`, delta, id)
	if err != nil {
		return fmt.Errorf("slots: incrementing tx_count for slot %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSlotNotFound
	}
	return nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanSlot(row scannable) (Slot, error) {
	var slot Slot
	var status string
	if err := row.Scan(&slot.ID, &slot.TronAddress, &status, &slot.ActivatedAt, &slot.RetiredAt, &slot.TxCount, &slot.LastDispatchAt); err != nil {
		return Slot{}, err
	}
	slot.Status = Status(status)
	return slot, nil
}
