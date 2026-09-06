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

// LastCandidateScannedHeight reads how far internal/candidates has
// scanned for Transfer logs -- a separate cursor from last_scanned
// (this package's own header/pre-final-reorg bookkeeping), since the
// candidate pipeline is wired in independently (cmd/watcherd) and never
// needs to be exactly in step with it, only never ahead of it. found is
// false on a fresh database (the column is NULL until
// SetCandidateScannedHeight is called for the first time) -- the caller
// decides what "never scanned yet" should mean (cmd/watcherd starts from
// the current chain tip, not genesis).
func LastCandidateScannedHeight(ctx context.Context, q db.Queryer) (height uint64, found bool, err error) {
	var last *int64
	if err := q.QueryRow(ctx, `SELECT last_candidate_scanned FROM ingestion_cursor WHERE id = 1`).Scan(&last); err != nil {
		return 0, false, fmt.Errorf("chain: reading candidate scan cursor: %w", err)
	}
	if last == nil {
		return 0, false, nil
	}
	return uint64(*last), true, nil
}

// SetCandidateScannedHeight persists how far internal/candidates has
// scanned.
func SetCandidateScannedHeight(ctx context.Context, q db.Queryer, height uint64) error {
	if _, err := q.Exec(ctx, `UPDATE ingestion_cursor SET last_candidate_scanned = $1 WHERE id = 1`, int64(height)); err != nil {
		return fmt.Errorf("chain: setting candidate scan cursor: %w", err)
	}
	return nil
}
