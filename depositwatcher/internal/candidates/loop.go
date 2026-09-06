package candidates

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"depositwatcher/internal/chain"
	"depositwatcher/internal/db"
	"depositwatcher/internal/finality"
)

// DefaultInterval is how often RunLoop checks for newly-ingested blocks
// to scan for candidates and re-checks finality, when Interval is unset.
// Matched to chain.DefaultIngestionInterval: there is no point checking
// for candidates faster than new blocks can possibly have been ingested.
const DefaultInterval = 3 * time.Second

// RunLoop is cmd/watcherd's own engine: on each tick, it scans every
// block internal/chain's own ingestion loop has already vetted (never
// ahead of chain.LastScannedHeight -- ingestion's pre-final-reorg check
// for a height must have already happened before this package trusts a
// log found there) but that this package has not yet scanned for
// Transfer logs, then checks finality once. Blocks until ctx is
// cancelled, returning ctx.Err().
//
// Progress is persisted via chain.SetCandidateScannedHeight, so a
// restart resumes near where it left off rather than replaying the
// entire chain. A brand new cursor (first boot, column still NULL)
// starts from the CURRENT ingested height, not genesis -- there is no
// value in a fresh deployment trying to candidate-scan years of history
// it was never running to observe live.
func RunLoop(ctx context.Context, pool *chain.Pool, database *db.Pool, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := runTick(ctx, pool, database, quotes, tracker, cfg); err != nil {
		slog.Error("candidates: initial tick failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := runTick(ctx, pool, database, quotes, tracker, cfg); err != nil {
				slog.Error("candidates: tick failed", "error", err)
			}
		}
	}
}

func runTick(ctx context.Context, pool *chain.Pool, database *db.Pool, quotes QuotedAmountFetcher,
	tracker *finality.Tracker, cfg Config) error {
	ingestedHeight, err := chain.LastScannedHeight(ctx, database)
	if err != nil {
		return err
	}

	lastCandidate, found, err := chain.LastCandidateScannedHeight(ctx, database)
	if err != nil {
		return err
	}
	if !found {
		if err := chain.SetCandidateScannedHeight(ctx, database, ingestedHeight); err != nil {
			return err
		}
		lastCandidate = ingestedHeight
	}

	if ingestedHeight > lastCandidate {
		if err := ScanRange(ctx, pool, database, quotes, tracker, cfg, lastCandidate+1, ingestedHeight); err != nil {
			return fmt.Errorf("candidates: scanning [%d,%d]: %w", lastCandidate+1, ingestedHeight, err)
		}
		if err := chain.SetCandidateScannedHeight(ctx, database, ingestedHeight); err != nil {
			return err
		}
	}

	if err := tracker.CheckFinality(ctx, pool); err != nil {
		return fmt.Errorf("candidates: checking finality: %w", err)
	}
	return nil
}
