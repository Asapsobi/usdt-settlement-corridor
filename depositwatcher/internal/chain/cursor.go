package chain

import (
	"context"
	"fmt"

	"depositwatcher/internal/db"
)

// getLastScanned reads the single ingestion_cursor row. Migration
// 0003 seeds it at (1, 0), so this never has to handle a missing row --
// the Go side can always assume it exists.
func getLastScanned(ctx context.Context, q db.Queryer) (uint64, error) {
	var last int64
	err := q.QueryRow(ctx, `SELECT last_scanned FROM ingestion_cursor WHERE id = 1`).Scan(&last)
	if err != nil {
		return 0, fmt.Errorf("chain: reading ingestion cursor: %w", err)
	}
	return uint64(last), nil
}

// LastScannedHeight is getLastScanned, exported for operator visibility
// (C2.9's GET /system/invariants, "cursor lag") -- the ingestion loop
// itself only ever needs the unexported form.
func LastScannedHeight(ctx context.Context, q db.Queryer) (uint64, error) {
	return getLastScanned(ctx, q)
}
