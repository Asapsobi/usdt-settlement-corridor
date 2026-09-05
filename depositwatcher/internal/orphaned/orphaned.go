// Package orphaned stores deposits that finalized on-chain for an order
// C1 no longer considers open -- gap #3's mechanism (C2.8), not its
// policy. Nothing in this package decides what to DO about a row here;
// it exists purely to capture one and make it visible (C2.9's HTTP
// surface) until a human, or an eventual product decision, resolves it.
package orphaned

import (
	"context"
	"fmt"
	"time"

	"depositwatcher/internal/db"
)

// Deposit is an orphaned_deposits row.
type Deposit struct {
	ID                    int64
	OrderID               int64
	ExternalID            string
	TxHash                string
	LogIndex              int
	Amount                int64 // minor units, this service's own money.Decimals convention
	DetectedAt            time.Time
	OrderStateAtDetection string
	Resolution            *string
	ResolvedAt            *time.Time
}

// Queryer is db.Queryer under this package's own name, matching the
// convention internal/addresses and internal/chain already established.
type Queryer = db.Queryer

// Record inserts d, idempotent on (tx_hash, log_index) -- the same
// candidate detected as orphaned more than once (a retried recording
// attempt after a transient failure, or the surrounding process
// restarting) must never produce a second row for it.
func Record(ctx context.Context, q Queryer, d Deposit) error {
	_, err := q.Exec(ctx, `
		INSERT INTO orphaned_deposits
			(order_id, external_id, tx_hash, log_index, amount, detected_at, order_state_at_detection)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tx_hash, log_index) DO NOTHING
	`, d.OrderID, d.ExternalID, d.TxHash, d.LogIndex, d.Amount, d.DetectedAt, d.OrderStateAtDetection)
	if err != nil {
		return fmt.Errorf("orphaned: recording %s:%d: %w", d.TxHash, d.LogIndex, err)
	}
	return nil
}

const selectSQL = `
	SELECT id, order_id, external_id, tx_hash, log_index, amount, detected_at,
		order_state_at_detection, resolution, resolved_at
	FROM orphaned_deposits`

// List returns every orphaned deposit, most recently detected first --
// what C2.9's GET surface reads for operator visibility.
func List(ctx context.Context, q Queryer) ([]Deposit, error) {
	rows, err := q.Query(ctx, selectSQL+` ORDER BY detected_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("orphaned: list: %w", err)
	}
	defer rows.Close()

	var out []Deposit
	for rows.Next() {
		var d Deposit
		if err := rows.Scan(&d.ID, &d.OrderID, &d.ExternalID, &d.TxHash, &d.LogIndex, &d.Amount,
			&d.DetectedAt, &d.OrderStateAtDetection, &d.Resolution, &d.ResolvedAt); err != nil {
			return nil, fmt.Errorf("orphaned: list: scanning row: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orphaned: list: %w", err)
	}
	return out, nil
}
