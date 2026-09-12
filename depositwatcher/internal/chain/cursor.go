package chain

import (
	"context"
	"fmt"
	"time"

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

// CursorState is the raw ingestion_cursor row, exported for operator
// visibility (the ops console's own GET /v1/system/cursor -- see
// docs/03-build/ops-console-build-prompts.md's OC.4) -- everything
// LastScannedHeight/LastCandidateScannedHeight already expose
// individually, just read together in one round trip plus UpdatedAt,
// which neither of those two functions needed before now.
type CursorState struct {
	LastScanned          uint64
	LastCandidateScanned *uint64
	UpdatedAt            time.Time
}

// GetCursorState reads the full ingestion_cursor row in one query.
func GetCursorState(ctx context.Context, q db.Queryer) (CursorState, error) {
	var lastScanned int64
	var lastCandidate *int64
	var updatedAt time.Time
	err := q.QueryRow(ctx, `SELECT last_scanned, last_candidate_scanned, updated_at FROM ingestion_cursor WHERE id = 1`).
		Scan(&lastScanned, &lastCandidate, &updatedAt)
	if err != nil {
		return CursorState{}, fmt.Errorf("chain: reading ingestion cursor: %w", err)
	}
	state := CursorState{LastScanned: uint64(lastScanned), UpdatedAt: updatedAt}
	if lastCandidate != nil {
		v := uint64(*lastCandidate)
		state.LastCandidateScanned = &v
	}
	return state, nil
}

// SetCursorState sets both cursor columns together, atomically, in one
// statement -- the ops console's own OC.4 write path. Unlike
// SetCandidateScannedHeight (used by the live candidate-scanning loop
// itself, which only ever advances its own column), this is an operator
// override of BOTH columns at once and is the only writer of
// last_scanned outside the ingestion loop's own internal setter.
func SetCursorState(ctx context.Context, q db.Queryer, lastScanned, lastCandidateScanned uint64) error {
	_, err := q.Exec(ctx,
		`UPDATE ingestion_cursor SET last_scanned = $1, last_candidate_scanned = $2, updated_at = now() WHERE id = 1`,
		int64(lastScanned), int64(lastCandidateScanned))
	if err != nil {
		return fmt.Errorf("chain: setting ingestion cursor: %w", err)
	}
	return nil
}
