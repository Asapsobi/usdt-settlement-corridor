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
		&r.EnergyUnits, &r.Tier, &status, &vendor, &costUnits, &r.ConfirmedAt, &r.Deadline, &r.CreatedAt, &r.FastPath); err != nil {
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

const selectColumns = `id, idempotency_key, external_id, order_id, target_address, energy_units, tier, status, vendor, cost_trx, confirmed_at, deadline, created_at, via_fast_path`

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

// listByStatus lists reservations matching any of statuses, newest first,
// bounded by limit -- the ops console's own OC.5 (see
// docs/03-build/ops-console-build-prompts.md), closing the gap
// GET /v1/reservations/{id} being get-by-id-only left: finding a stuck
// FAILED/PENDING row required a direct SELECT against this table before
// this route existed.
func listByStatus(ctx context.Context, q db.Queryer, statuses []string, limit int) ([]Reservation, error) {
	rows, err := q.Query(ctx, `SELECT `+selectColumns+` FROM reservations WHERE status = ANY($1) ORDER BY created_at DESC LIMIT $2`,
		statuses, limit)
	if err != nil {
		return nil, fmt.Errorf("reservations: listing by status: %w", err)
	}
	defer rows.Close()

	var out []Reservation
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reservations: listing by status: %w", err)
	}
	return out, nil
}

// markConfirmed transitions id to CONFIRMED, recording which vendor
// ultimately serviced it, at what cost, and via which path -- the last
// of these is what GET /v1/system/invariants' own "fast-path vs slow-
// path ratio" and the reservations_total{fast_path,slow_path,failed}
// metric (C4.8) both read.
func markConfirmed(ctx context.Context, q db.Queryer, id int64, vendor string, cost money.Amount, viaFastPath bool) (*Reservation, error) {
	row := q.QueryRow(ctx, `
		UPDATE reservations
		SET status = $1, vendor = $2, cost_trx = $3, confirmed_at = $4, via_fast_path = $5
		WHERE id = $6
		RETURNING `+selectColumns+`
	`, string(StatusConfirmed), vendor, int64(cost), time.Now().UTC(), viaFastPath, id)
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

// PathCounts reports how many CONFIRMED reservations resolved via the
// fast path versus the slow path -- GET /v1/system/invariants' own
// "fast-path vs slow-path ratio" (C4.8).
func PathCounts(ctx context.Context, q db.Queryer) (fastCount, slowCount int64, err error) {
	row := q.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE via_fast_path = true),
			COUNT(*) FILTER (WHERE via_fast_path = false)
		FROM reservations
		WHERE status = $1
	`, string(StatusConfirmed))
	if err := row.Scan(&fastCount, &slowCount); err != nil {
		return 0, 0, fmt.Errorf("reservations: counting path totals: %w", err)
	}
	return fastCount, slowCount, nil
}

// recentReservedUnits sums energy_units for every non-FAILED reservation
// created within the last window against targetAddress -- internal/buffer's
// own DemandObserver interface (C4.3), scoped per slot address now that
// each slot maintains its own independent target level (design (a) --
// see this package's own doc comment).
func recentReservedUnits(ctx context.Context, q db.Queryer, window time.Duration, targetAddress string) (int64, error) {
	var total int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(energy_units), 0) FROM reservations
		WHERE created_at > now() - $1::interval AND status <> $2 AND target_address = $3
	`, window.String(), string(StatusFailed), targetAddress).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("reservations: summing recent reserved units: %w", err)
	}
	return total, nil
}
