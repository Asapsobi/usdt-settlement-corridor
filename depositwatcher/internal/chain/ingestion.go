package chain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"depositwatcher/internal/db"
)

// DefaultSeenBlocksWindow is how many recent block heights seen_blocks
// retains -- generous at BSC's current ~0.45s block time (per the C2
// build spec's "Read this second" section), still under 2 minutes of
// history, cheap to keep.
const DefaultSeenBlocksWindow = 200

// DefaultIngestionInterval is how often RunIngestionLoop checks for new
// blocks when IngestionConfig.Interval is unset.
const DefaultIngestionInterval = 3 * time.Second

// IngestionConfig controls RunIngestionLoop's cadence and retention.
type IngestionConfig struct {
	Interval time.Duration // zero means DefaultIngestionInterval
	Window   uint64        // zero means DefaultSeenBlocksWindow
}

func (c IngestionConfig) withDefaults() IngestionConfig {
	if c.Interval <= 0 {
		c.Interval = DefaultIngestionInterval
	}
	if c.Window == 0 {
		c.Window = DefaultSeenBlocksWindow
	}
	return c
}

// RunIngestionLoop walks the chain forward from wherever ingestion_cursor
// last left off, storing each block's (height, hash, parent_hash) in
// seen_blocks and logging a pre-final reorg the moment a stored parent
// hash stops matching the block actually observed at that parent's
// height. Advisory only, per §B: this is NOT the finality decision --
// that's LatestFinalized, C2.5's job -- it exists purely so a routine,
// expected pre-final reorg is visible for observability instead of
// silently invisible.
//
// Blocks until ctx is cancelled, returning ctx.Err(). Each block's work
// commits in one transaction: insert into seen_blocks, check the parent
// hash, advance the cursor, prune -- all together or none of them, so a
// process killed mid-scan resumes from exactly the last block that fully
// committed. It never skips a block and never leaves a half-applied one
// for the restart to reprocess with some different, inconsistent effect.
func RunIngestionLoop(ctx context.Context, rpcPool *Pool, database *db.Pool, cfg IngestionConfig) error {
	cfg = cfg.withDefaults()

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	// Run once immediately rather than waiting a full interval before the
	// first tick -- a freshly started watcher shouldn't sit idle for up
	// to cfg.Interval before doing any work.
	if err := runIngestionTick(ctx, rpcPool, database, cfg.Window); err != nil {
		slog.Error("ingestion: initial tick failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := runIngestionTick(ctx, rpcPool, database, cfg.Window); err != nil {
				slog.Error("ingestion: tick failed", "error", err)
			}
		}
	}
}

func runIngestionTick(ctx context.Context, rpcPool *Pool, database *db.Pool, window uint64) error {
	lastScanned, err := getLastScanned(ctx, database)
	if err != nil {
		return err
	}

	tipHeader, err := rpcPool.LatestBlockHeader(ctx)
	if err != nil {
		return fmt.Errorf("chain: fetching chain tip: %w", err)
	}
	tip := tipHeader.Number.Uint64()

	if tip <= lastScanned {
		return nil // nothing new since the last tick
	}

	for height := lastScanned + 1; height <= tip; height++ {
		if err := processOneBlock(ctx, rpcPool, database, height, window); err != nil {
			return fmt.Errorf("chain: processing block %d: %w", height, err)
		}
	}
	return nil
}

// processOneBlock fetches height's header, then atomically stores it,
// checks its parent hash, advances the cursor to height, and prunes --
// see RunIngestionLoop's doc comment for why all four happen in exactly
// one transaction.
func processOneBlock(ctx context.Context, rpcPool *Pool, database *db.Pool, height, window uint64) error {
	header, err := rpcPool.HeaderByNumber(ctx, height)
	if err != nil {
		return fmt.Errorf("fetching header: %w", err)
	}
	hash := header.Hash().Hex()
	parentHash := header.ParentHash.Hex()

	return db.Tx(ctx, database, func(ctx context.Context, tx pgx.Tx) error {
		if height > 0 {
			mismatch, expectedParent, err := detectParentMismatch(ctx, tx, height, parentHash)
			if err != nil {
				return err
			}
			if mismatch {
				// Advisory only: this is deliberately just a log line, per
				// §B ("it never by itself triggers a report to C1"). The
				// now-known-stale row at height-1 is not corrected here --
				// seen_blocks exists for observability, not as a source of
				// truth anything downstream reads for a decision, so
				// leaving it to age out via the window below is
				// sufficient; retroactively rewriting history this loop
				// has already walked past is out of this chunk's scope.
				slog.Warn("ingestion: pre-final reorg detected (advisory only, not a finality decision)",
					"height", height, "expected_parent_hash", expectedParent, "actual_parent_hash", parentHash)
			}
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO seen_blocks (height, hash, parent_hash)
			VALUES ($1, $2, $3)
			ON CONFLICT (height) DO UPDATE
			SET hash = excluded.hash, parent_hash = excluded.parent_hash, observed_at = now()
		`, int64(height), hash, parentHash); err != nil {
			return fmt.Errorf("storing seen block %d: %w", height, err)
		}

		if _, err := tx.Exec(ctx, `UPDATE ingestion_cursor SET last_scanned = $1, updated_at = now() WHERE id = 1`,
			int64(height)); err != nil {
			return fmt.Errorf("advancing cursor to %d: %w", height, err)
		}

		// Keep exactly the most recent `window` heights: [height-window+1,
		// height]. Guarded by height >= window, not just height > 0 --
		// height and window are both uint64, and height-window+1 would
		// underflow to a huge value (deleting everything) if height were
		// smaller than window-1.
		if height >= window {
			threshold := height - window + 1
			if _, err := tx.Exec(ctx, `DELETE FROM seen_blocks WHERE height < $1`, int64(threshold)); err != nil {
				return fmt.Errorf("pruning seen_blocks: %w", err)
			}
		}
		return nil
	})
}

// detectParentMismatch compares actualParentHash (the header just fetched
// for `height`) against whatever this package previously stored for
// height-1. Absence of a stored row at height-1 -- the very first block
// this process ever scans, or one that's already aged out of the window
// -- is not a mismatch: there is nothing to compare against, which is
// expected, not an error.
func detectParentMismatch(ctx context.Context, tx pgx.Tx, height uint64, actualParentHash string) (mismatch bool, storedHash string, err error) {
	err = tx.QueryRow(ctx, `SELECT hash FROM seen_blocks WHERE height = $1`, int64(height-1)).Scan(&storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("reading prior block %d: %w", height-1, err)
	}
	return storedHash != actualParentHash, storedHash, nil
}
