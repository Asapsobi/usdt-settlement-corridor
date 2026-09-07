package reservations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"energybroker/internal/db"
	"energybroker/internal/money"
)

// errDuplicateIdempotencyKey is insertPending's own signal that a
// concurrent Create for the same Idempotency-Key already won the race --
// never returned to a caller directly, Create translates it into
// fetching and returning that winning row instead.
var errDuplicateIdempotencyKey = errors.New("reservations: duplicate idempotency key")

type scannable interface {
	Scan(dest ...any) error
}

func scanReservation(row scannable) (*Reservation, error) {
	var r Reservation
	var status string
	var vendor *string
	var costUnits *int64
	if err := row.Scan(&r.ID, &r.IdempotencyKey, &r.ExternalID, &r.OrderID, &r.TargetAddress,
		&r.EnergyUnits, &r.Tier, &status, &vendor, &costUnits, &r.ConfirmedAt, &r.Deadline, &r.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("reservations: scanning row: %w", err)
	}
	r.Status = Status(status)
	r.Vendor = vendor
	if costUnits != nil {
		amount := money.Amount(*costUnits)
		r.CostTRX = &amount
	}
	return &r, nil
}

const selectColumns = `id, idempotency_key, external_id, order_id, target_address, energy_units, tier, status, vendor, cost_trx, confirmed_at, deadline, created_at`

// insertPending creates a new PENDING reservation row for req against
// orderID -- the row this whole Create call revolves around, inserted
// BEFORE either path (fast or slow) is attempted, so a concurrent
// duplicate request racing on the same Idempotency-Key fails the unique
// index rather than both proceeding to reserve/delegate independently.
func insertPending(ctx context.Context, q db.Queryer, req Request, orderID int64) (*Reservation, error) {
	row := q.QueryRow(ctx, `
		INSERT INTO reservations (idempotency_key, external_id, order_id, target_address, energy_units, tier, status, deadline)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+selectColumns+`
	`, req.IdempotencyKey, req.ExternalID, orderID, req.TargetAddress, req.EnergyUnits, req.Tier, string(StatusPending), req.Deadline)

	r, err := scanReservation(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, errDuplicateIdempotencyKey
		}
		return nil, fmt.Errorf("reservations: inserting pending reservation: %w", err)
	}
	return r, nil
}

func getByIdempotencyKey(ctx context.Context, q db.Queryer, key string) (*Reservation, error) {
	row := q.QueryRow(ctx, `SELECT `+selectColumns+` FROM reservations WHERE idempotency_key = $1`, key)
	return scanReservation(row)
}

func getByID(ctx context.Context, q db.Queryer, id int64) (*Reservation, error) {
	row := q.QueryRow(ctx, `SELECT `+selectColumns+` FROM reservations WHERE id = $1`, id)
	return scanReservation(row)
}

// markConfirmed transitions id to CONFIRMED, recording which vendor
// ultimately serviced it and at what cost. cost is 0 (not null) for a
// fast-path confirmation -- see Reservation.CostTRX's own doc comment
// on why that is correct, not a bug.
func markConfirmed(ctx context.Context, q db.Queryer, id int64, vendor string, cost money.Amount) (*Reservation, error) {
	row := q.QueryRow(ctx, `
		UPDATE reservations
		SET status = $1, vendor = $2, cost_trx = $3, confirmed_at = $4
		WHERE id = $5
		RETURNING `+selectColumns+`
	`, string(StatusConfirmed), vendor, int64(cost), time.Now().UTC(), id)
	r, err := scanReservation(row)
	if err != nil {
		return nil, fmt.Errorf("reservations: marking %d confirmed: %w", id, err)
	}
	return r, nil
}

func markFailed(ctx context.Context, q db.Queryer, id int64) (*Reservation, error) {
	row := q.QueryRow(ctx, `
		UPDATE reservations SET status = $1 WHERE id = $2
		RETURNING `+selectColumns+`
	`, string(StatusFailed), id)
	r, err := scanReservation(row)
	if err != nil {
		return nil, fmt.Errorf("reservations: marking %d failed: %w", id, err)
	}
	return r, nil
}

// recentReservedUnits sums energy_units for every non-FAILED reservation
// created within the last window -- internal/buffer's own DemandObserver
// interface (C4.3), finally given a real implementation now that this
// chunk's own reservations table exists.
func recentReservedUnits(ctx context.Context, q db.Queryer, window time.Duration) (int64, error) {
	var total int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(energy_units), 0) FROM reservations
		WHERE created_at > now() - $1::interval AND status <> $2
	`, window.String(), string(StatusFailed)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("reservations: summing recent reserved units: %w", err)
	}
	return total, nil
}
