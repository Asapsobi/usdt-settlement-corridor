// Package pipeline is C3.4: it ties C3.1 (cache), C3.2 (classification),
// and C3.3 (discovery) together into the thing that actually calls a
// screening provider and reports the result to C1.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"screening/internal/cache"
	"screening/internal/db"
	"screening/internal/discovery"
	"screening/internal/ledgerclient"
	"screening/internal/provider"
	"screening/internal/verdict"
)

// DefaultInterval is how often RunLoop checks screening_queue for
// PENDING rows ready to screen, when Interval is unset. Matched to
// discovery.DefaultInterval -- there is no value in checking for
// screenable orders faster than discovery can possibly produce them.
const DefaultInterval = 5 * time.Second

// DefaultTimeout bounds a single provider.Screen call. Config, not a
// hardcoded constant used directly -- see Config's own doc comment.
const DefaultTimeout = 10 * time.Second

// Reporter is the one call this pipeline needs from C1 --
// ledgerclient.Client's real implementation, or a fake for testing.
// Defined here (the consumer), not in ledgerclient, matching this
// module's existing convention (e.g. discovery.FundedOrderPoller).
type Reporter interface {
	ReportVerdict(ctx context.Context, externalID string, decision verdict.Decision) error
}

// Config scopes what this pipeline screens against and how.
type Config struct {
	// ProviderName must match the name internal/cache stores results
	// under for this provider (provider.Verdict.ProviderName) -- cache
	// lookups are keyed by (provider name, address), so a mismatch here
	// would silently never hit the cache at all.
	ProviderName string
	// Timeout bounds a single provider.Screen call. Defaults to
	// DefaultTimeout if <= 0.
	Timeout time.Duration
	// Thresholds are verdict.Classify's own risk-score cutoffs -- see
	// verdict.Thresholds' own doc comment for why these are config, not
	// a constant. The zero value (PassBelow: 0, FlaggedAtOrAbove: 0)
	// defaults to verdict.DefaultThresholds rather than being used
	// as-is: taken literally, it would classify every single score
	// (always >= 0) straight into Hold/screening_hold_flagged, silently
	// holding every order this pipeline ever screens. There is no
	// legitimate config that actually wants both cutoffs at exactly 0,
	// so treating it as "unset" is safe.
	Thresholds verdict.Thresholds
	// TTL controls how long a fresh screening result stays cached,
	// split by outcome -- see cache.TTLConfig's own doc comment.
	TTL cache.TTLConfig
}

func (cfg Config) timeout() time.Duration {
	if cfg.Timeout <= 0 {
		return DefaultTimeout
	}
	return cfg.Timeout
}

func (cfg Config) thresholds() verdict.Thresholds {
	if cfg.Thresholds == (verdict.Thresholds{}) {
		return verdict.DefaultThresholds
	}
	return cfg.Thresholds
}

// RunLoop checks screening_queue on an interval and screens every row
// ListReadyForScreening returns. Blocks until ctx is cancelled,
// returning ctx.Err().
func RunLoop(ctx context.Context, pool *db.Pool, prov provider.ScreeningProvider, reporter Reporter, cfg Config, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := RunTick(ctx, pool, prov, reporter, cfg); err != nil {
		slog.Error("pipeline: initial tick failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := RunTick(ctx, pool, prov, reporter, cfg); err != nil {
				slog.Error("pipeline: tick failed", "error", err)
			}
		}
	}
}

// RunTick screens every PENDING, address-resolved queue row once. A
// single row's failure is logged and left PENDING for the next tick --
// never allowed to stop the rest of the batch.
func RunTick(ctx context.Context, pool db.Queryer, prov provider.ScreeningProvider, reporter Reporter, cfg Config) error {
	pending, err := discovery.ListReadyForScreening(ctx, pool)
	if err != nil {
		return fmt.Errorf("pipeline: listing orders ready for screening: %w", err)
	}

	for _, entry := range pending {
		if err := ScreenAndReport(ctx, pool, prov, reporter, cfg, entry); err != nil {
			slog.Error("pipeline: screening order failed, left PENDING for next tick",
				"order_id", entry.OrderID, "external_id", entry.ExternalID, "error", err)
		}
	}
	return nil
}

// ScreenAndReport runs one queue entry through the full pipeline:
//
//  1. cache.Get(provider, address) -- an unexpired, non-invalidated hit
//     skips the vendor call entirely (the whole point of caching by
//     sender address).
//  2. Otherwise, prov.Screen(ctx, address) under cfg.timeout(). On
//     success, cache.Put persists it. On any error (including a
//     timeout), no row is cached -- verdict.Unavailable() is used
//     directly instead of classifying a fabricated Verdict, matching
//     what that function exists for: the fail-closed, no-backing-row
//     decision for exactly this case. (C3.5 replaces this single
//     attempt with retries and a configurable outage policy; until then
//     fail-closed, C1's own shipped default, is the only behavior this
//     chunk needs.)
//  3. reporter.ReportVerdict with the classified Decision.
//  4. The queue row is marked DONE -- including when C1 reports
//     ledgerclient.ErrIllegalTransition (the order left funded before
//     C3 got to it, a legitimate race, not an error here).
//
// Exported, not folded into RunTick's loop body, so a caller (tests,
// concretely) can invoke it twice for the same entry to simulate a C3
// restart mid-pipeline. That replay is safe, but NOT because C1
// recognizes the repeated Idempotency-Key as a duplicate: a real C1 was
// found, while building this chunk, to only replay-detect transitions
// that post a journal entry (orders.Transition's own
// replayIfAlreadyPosted, keyed by journal.GetEntryByIdempotencyKey) --
// funded->screened and funded->held both have RequiresEntry: false, so
// there is no entry for C1 to recognize a repeat by. A second
// ReportVerdict call for an order that already got screened/held instead
// fails with ledgerclient.ErrIllegalTransition, which is exactly the
// signal this function already treats as "done, no retry" for the
// legitimate-race case below -- so a replay is absorbed safely, just via
// that path, not via true idempotent dedup on C1's side. See
// TestReportVerdict_ReplaySurfacesIllegalTransitionSafely in
// internal/ledgerclient for the live proof.
func ScreenAndReport(ctx context.Context, pool db.Queryer, prov provider.ScreeningProvider, reporter Reporter, cfg Config, entry discovery.QueueEntry) error {
	if entry.SenderAddress == nil {
		return fmt.Errorf("pipeline: order %d (%s) has no resolved sender_address yet", entry.OrderID, entry.ExternalID)
	}
	address := *entry.SenderAddress

	// No intermediate SCREENING marking here: this pipeline processes
	// one row fully (screen, then report, then DONE) before moving to
	// the next, within a single sequential tick -- there is no
	// concurrent access within this process for SCREENING to guard
	// against, and marking it here without also reverting it on a
	// mid-step failure would risk stranding a row that never reaches
	// DONE. The SCREENING state stays defined (migration 0003) for
	// whenever a real need for it exists (e.g. multiple screend
	// instances), not exercised by this chunk.
	decision, err := screen(ctx, pool, prov, cfg, address)
	if err != nil {
		return err
	}

	if err := reporter.ReportVerdict(ctx, entry.ExternalID, decision); err != nil {
		if errors.Is(err, ledgerclient.ErrIllegalTransition) {
			slog.Info("pipeline: order left funded before its verdict could be reported, marking DONE without retry",
				"order_id", entry.OrderID, "external_id", entry.ExternalID)
			return discovery.MarkDone(ctx, pool, entry.OrderID)
		}
		return fmt.Errorf("pipeline: reporting verdict for order %d (%s): %w", entry.OrderID, entry.ExternalID, err)
	}

	return discovery.MarkDone(ctx, pool, entry.OrderID)
}

// screen produces a Decision for address: a cache hit if one exists,
// otherwise a fresh provider call, classified either way.
func screen(ctx context.Context, pool db.Queryer, prov provider.ScreeningProvider, cfg Config, address string) (verdict.Decision, error) {
	hit, err := cache.Get(ctx, pool, cfg.ProviderName, address)
	if err != nil {
		return verdict.Decision{}, fmt.Errorf("pipeline: checking cache for %s: %w", address, err)
	}
	if hit != nil {
		decision := verdict.Classify(hit.Verdict, cfg.thresholds())
		decision.ScreeningResultID = hit.ID
		return decision, nil
	}

	screenCtx, cancel := context.WithTimeout(ctx, cfg.timeout())
	v, err := prov.Screen(screenCtx, address)
	cancel()
	if err != nil {
		slog.Warn("pipeline: provider screen failed, failing closed (screening_hold_unavailable)",
			"address", address, "error", err)
		return verdict.Unavailable(), nil
	}

	id, err := cache.Put(ctx, pool, cfg.ProviderName, address, v, cfg.TTL.For(v))
	if err != nil {
		return verdict.Decision{}, fmt.Errorf("pipeline: caching verdict for %s: %w", address, err)
	}
	decision := verdict.Classify(v, cfg.thresholds())
	decision.ScreeningResultID = id
	return decision, nil
}
