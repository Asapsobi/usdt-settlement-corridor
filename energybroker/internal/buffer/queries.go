package buffer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"energybroker/internal/db"
	"energybroker/internal/money"
	"energybroker/internal/provider"
)

// Status is one energy_buffer row's own lifecycle state.
type Status string

const (
	StatusAvailable Status = "AVAILABLE"
	StatusReserved  Status = "RESERVED"
	StatusSpent     Status = "SPENT"
	StatusExpired   Status = "EXPIRED"
)

// Row is one energy_buffer row.
type Row struct {
	ID           int64
	ProviderName string
	DelegationID string
	Units        int64
	AcquiredAt   time.Time
	CostTRX      money.Amount
	ExpiresAt    time.Time
	Status       Status
	AllocationID *int64
}

// availableTotal sums every currently-AVAILABLE row's units -- the
// "current" half of Replenish's own "compare against TargetLevel" check.
func availableTotal(ctx context.Context, q db.Queryer) (int64, error) {
	var total int64
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(units), 0) FROM energy_buffer WHERE status = $1
	`, string(StatusAvailable)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("buffer: summing available units: %w", err)
	}
	return total, nil
}

// totalsByProvider sums units by (provider_name, status) for the two
// statuses an operator cares about at a glance -- AVAILABLE (spendable
// now) and RESERVED (already earmarked for a specific order). SPENT and
// EXPIRED rows are history, not current inventory, so they're excluded
// here on purpose.
func totalsByProvider(ctx context.Context, q db.Queryer) ([]ProviderTotal, error) {
	rows, err := q.Query(ctx, `
		SELECT provider_name,
		       COALESCE(SUM(units) FILTER (WHERE status = $1), 0),
		       COALESCE(SUM(units) FILTER (WHERE status = $2), 0)
		FROM energy_buffer
		WHERE status IN ($1, $2)
		GROUP BY provider_name
		ORDER BY provider_name
	`, string(StatusAvailable), string(StatusReserved))
	if err != nil {
		return nil, fmt.Errorf("buffer: summing totals by provider: %w", err)
	}
	defer rows.Close()

	var out []ProviderTotal
	for rows.Next() {
		var t ProviderTotal
		if err := rows.Scan(&t.ProviderName, &t.Available, &t.Reserved); err != nil {
			return nil, fmt.Errorf("buffer: scanning provider total: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// insertAvailable records d as a new, AVAILABLE energy_buffer row --
// called only once VerifyOnChain has independently confirmed it, per
// invariant 1; a vendor's own 200 response alone never reaches this
// function.
func insertAvailable(ctx context.Context, q db.Queryer, d provider.Delegation, expiresAt time.Time) (Row, error) {
	row := q.QueryRow(ctx, `
		INSERT INTO energy_buffer (provider_name, delegation_id, units, acquired_at, cost_trx, expires_at, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, provider_name, delegation_id, units, acquired_at, cost_trx, expires_at, status, allocation_id
	`, d.ProviderName, d.ID, d.EnergyUnits, d.RequestedAt, int64(d.CostTRX), expiresAt, string(StatusAvailable))
	return scanRow(row)
}

// rowsNearingExpiry returns every AVAILABLE or RESERVED row whose
// expires_at falls within lookahead of now -- Reconcile's own working
// set.
func rowsNearingExpiry(ctx context.Context, q db.Queryer, lookahead time.Duration) ([]Row, error) {
	rows, err := q.Query(ctx, `
		SELECT id, provider_name, delegation_id, units, acquired_at, cost_trx, expires_at, status, allocation_id
		FROM energy_buffer
		WHERE status IN ($1, $2)
		  AND expires_at <= now() + $3::interval
		ORDER BY expires_at ASC
	`, string(StatusAvailable), string(StatusReserved), lookahead.String())
	if err != nil {
		return nil, fmt.Errorf("buffer: listing rows nearing expiry: %w", err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		r, err := scanRowFields(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rowsByIDs fetches energy_buffer rows by id, in no particular order --
// Reserve's own Allocation.RowIDs is the only caller, resolving a claim
// back into the provider_name/delegation_id each row needs for
// C4.4's own fast-path re-delegation.
func rowsByIDs(ctx context.Context, q db.Queryer, ids []int64) ([]Row, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT id, provider_name, delegation_id, units, acquired_at, cost_trx, expires_at, status, allocation_id
		FROM energy_buffer
		WHERE id = ANY($1)
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("buffer: fetching rows by id: %w", err)
	}
	defer rows.Close()

	var out []Row
	for rows.Next() {
		r, err := scanRowFields(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// markExpired transitions row ids to EXPIRED -- Reconcile's own
// correction when on-chain reality no longer matches bookkeeping.
func markExpired(ctx context.Context, q db.Queryer, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, `
		UPDATE energy_buffer SET status = $1 WHERE id = ANY($2)
	`, string(StatusExpired), ids)
	if err != nil {
		return fmt.Errorf("buffer: marking %d row(s) expired: %w", len(ids), err)
	}
	return nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanRow(row scannable) (Row, error) {
	r, err := scanRowFields(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Row{}, err
		}
		return Row{}, fmt.Errorf("buffer: scanning row: %w", err)
	}
	return r, nil
}

func scanRowFields(row scannable) (Row, error) {
	var r Row
	var costTRX int64
	if err := row.Scan(&r.ID, &r.ProviderName, &r.DelegationID, &r.Units, &r.AcquiredAt, &costTRX, &r.ExpiresAt, &r.Status, &r.AllocationID); err != nil {
		return Row{}, err
	}
	r.CostTRX = money.Amount(costTRX)
	return r, nil
}
