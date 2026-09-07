package buffer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"energybroker/internal/db"
)

// ErrBufferExhausted is Reserve's own result when the buffer's current
// AVAILABLE capacity is insufficient -- the signal that triggers C4.4's
// slow path. Reserve touches zero rows when this is returned: the whole
// claim happens inside one transaction that is rolled back, never
// partially applied.
var ErrBufferExhausted = errors.New("buffer: insufficient available energy")

// Allocation is what Reserve hands back: which buffer_allocations row
// was created, and which underlying energy_buffer rows it claimed.
// Units is the ACTUAL total claimed, which can exceed the units
// requested -- Reserve claims whole rows (a delegation cannot be split
// without an on-chain re-delegation step this chunk does not build), so
// the last row claimed may overshoot; an order is never given less than
// it asked for, only possibly a little more.
type Allocation struct {
	ID         int64
	OrderID    int64
	Units      int64
	ReservedAt time.Time
	RowIDs     []int64
}

// Reserve atomically claims at least units worth of AVAILABLE
// energy_buffer rows already slotted under targetAddress, for orderID,
// oldest-expiry-first (so soon-to-expire capacity is used up rather than
// stranded), and marks them RESERVED. Only rows already pre-provisioned
// against targetAddress are eligible -- see internal/provider's own
// EnergyProvider doc comment for why a claimed row is never retargeted
// to a different address after the fact.
//
// Concurrency safety is `FOR UPDATE SKIP LOCKED`, not an application-
// level mutex -- but claimed ONE ROW PER QUERY, in a loop, not in a
// single bulk `SELECT ... LIMIT n FOR UPDATE SKIP LOCKED`. That
// distinction is load-bearing, not a style choice: `FOR UPDATE SKIP
// LOCKED` locks every row a query actually matches (up to any LIMIT)
// the instant it executes, server-side, before the caller has read or
// even wants most of them. A bulk query bounded only by "at most `units`
// rows could ever be needed" still over-locks badly whenever real rows
// are larger than 1 unit each (the common case, since Replenish chunks
// in the hundreds or thousands) -- one lucky transaction can walk away
// with every row in the table while needing only one of them, starving
// every other concurrent Reserve call with a false ErrBufferExhausted.
// Claiming exactly one row per query, excluding rows already claimed
// earlier in this same transaction, means this call never locks a
// single row more than it actually ends up using.
func (b *Buffer) Reserve(ctx context.Context, orderID int64, targetAddress string, units int64) (*Allocation, error) {
	if units <= 0 {
		return nil, fmt.Errorf("buffer: Reserve called with non-positive units %d", units)
	}
	if targetAddress == "" {
		return nil, fmt.Errorf("buffer: Reserve called with an empty targetAddress")
	}

	var alloc *Allocation
	err := db.Tx(ctx, b.pool, func(ctx context.Context, tx pgx.Tx) error {
		claimed := []int64{} // never nil -- see the `= ANY($3)` exclusion below
		var total int64
		for total < units {
			var id, rowUnits int64
			err := tx.QueryRow(ctx, `
				SELECT id, units FROM energy_buffer
				WHERE status = $1 AND slot_address = $2 AND expires_at > now() AND NOT (id = ANY($3))
				ORDER BY expires_at ASC
				LIMIT 1
				FOR UPDATE SKIP LOCKED
			`, string(StatusAvailable), targetAddress, claimed).Scan(&id, &rowUnits)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					break // nothing left anywhere -- exhausted
				}
				return fmt.Errorf("buffer: locking an available row: %w", err)
			}
			claimed = append(claimed, id)
			total += rowUnits
		}

		if total < units {
			return ErrBufferExhausted
		}

		var allocationID int64
		err := tx.QueryRow(ctx, `
			INSERT INTO buffer_allocations (order_id, units) VALUES ($1, $2) RETURNING id
		`, orderID, total).Scan(&allocationID)
		if err != nil {
			return fmt.Errorf("buffer: recording allocation for order %d: %w", orderID, err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE energy_buffer SET status = $1, allocation_id = $2 WHERE id = ANY($3)
		`, string(StatusReserved), allocationID, claimed); err != nil {
			return fmt.Errorf("buffer: marking %d row(s) reserved for order %d: %w", len(claimed), orderID, err)
		}

		alloc = &Allocation{ID: allocationID, OrderID: orderID, Units: total, ReservedAt: time.Now().UTC(), RowIDs: claimed}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrBufferExhausted) {
			return nil, ErrBufferExhausted
		}
		return nil, err
	}
	return alloc, nil
}

// RowsByIDs resolves an Allocation's own RowIDs back into full Row
// detail -- C4.4's fast path needs each row's provider_name and
// delegation_id to know which vendor to ask to redirect which
// delegation to the requested payout slot; Allocation itself carries
// only bare ids, deliberately (see Allocation's own doc comment).
func (b *Buffer) RowsByIDs(ctx context.Context, ids []int64) ([]Row, error) {
	return rowsByIDs(ctx, b.pool, ids)
}
