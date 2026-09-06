// Package pipeline is C3.4 (extended by C3.6): it ties C3.1 (cache),
// C3.2 (classification), C3.3 (discovery), and C3.6 (the hold queue)
// together into the thing that actually calls a screening provider and
// reports the result to C1.
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
	"screening/internal/holds"
	"screening/internal/ledgerclient"
	"screening/internal/provider"
	"screening/internal/verdict"
)

// DefaultInterval is how often RunLoop checks screening_queue for
// PENDING rows ready to screen, when Interval is unset. Matched to
// discovery.DefaultInterval -- there is no value in checking for
// screenable orders faster than discovery can possibly produce them.
const DefaultInterval = 5 * time.Second

// DefaultTimeout bounds a single provider.Screen attempt. Config, not a
// hardcoded constant used directly -- see Config's own doc comment.
const DefaultTimeout = 10 * time.Second

// DefaultRetries is how many attempts provider.ScreenWithPolicy makes
// before giving up, when Retries is unset.
const DefaultRetries = 3

// Reporter is the one call this pipeline needs from C1 --
// ledgerclient.Client's real implementation, or a fake for testing.
// Defined here (the consumer), not in ledgerclient, matching this
// module's existing convention (e.g. discovery.FundedOrderPoller).
type Reporter interface {
	ReportVerdict(ctx context.Context, externalID string, decision verdict.Decision) error
}

// MetricsRecorder is how this pipeline reports the metrics C3.5's and
// C3.8's own build specs name: VendorUnavailable fires once per
// exhausted-retries event regardless of which OutagePolicy is
// configured (so an operator sees vendor degradation even when FailOpen
// is masking it from the order pipeline's own behavior);
// VerdictReported fires once per successfully-reported Decision, by
// classification, backing verdicts_total{classification}; HoldOpened
// fires once per new hold this pipeline opens, backing
// holds_opened_total. Optional -- nil means no metrics are recorded,
// never a panic, the same convention every other pluggable dependency
// in this module uses.
type MetricsRecorder interface {
	VendorUnavailable()
	VerdictReported(classification verdict.Classification)
	HoldOpened()
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
	// Retries is provider.ScreenWithPolicy's own attempt budget.
	// Defaults to DefaultRetries if <= 0.
	Retries int
	// OutagePolicy decides what happens once every retry is exhausted --
	// see provider.OutagePolicy's own doc comment. The zero value is
	// provider.FailClosed, which is both Go's natural zero value and
	// this system's own shipped default ("Read this third" in the C3
	// build spec) -- an uninitialized Config is always safe.
	OutagePolicy provider.OutagePolicy
	// Metrics is optional -- see MetricsRecorder's own doc comment.
	Metrics MetricsRecorder
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

func (cfg Config) retries() int {
	if cfg.Retries <= 0 {
		return DefaultRetries
	}
	return cfg.Retries
}

func (cfg Config) recordVendorUnavailable() {
	if cfg.Metrics != nil {
		cfg.Metrics.VendorUnavailable()
	}
}

func (cfg Config) recordVerdictReported(classification verdict.Classification) {
	if cfg.Metrics != nil {
		cfg.Metrics.VerdictReported(classification)
	}
}

func (cfg Config) recordHoldOpened() {
	if cfg.Metrics != nil {
		cfg.Metrics.HoldOpened()
	}
}

// LogOutagePolicy logs cfg.OutagePolicy once, loudly for the non-default
// FailOpen (Warn) and quietly for the default FailClosed (Info) --
// exactly C1.7's own posture on a non-default TRX reconciliation
// tolerance: a deliberate risk-tradeoff choice must be visible at
// startup, never silent. Intended to be called once by whichever
// process assembles this Config (cmd/screend, once its own engine
// wiring exists -- see this chunk's own commit for why that wiring is
// still out of scope here).
func (cfg Config) LogOutagePolicy() {
	if cfg.OutagePolicy == provider.FailOpen {
		slog.Warn("pipeline: outage_policy is fail_open -- an exhausted vendor retry budget will be reported to C1 as a clean pass (screening_pass_vendor_unavailable), never silently",
			"provider", cfg.ProviderName)
		return
	}
	slog.Info("pipeline: outage_policy is fail_closed (default)", "provider", cfg.ProviderName)
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
//  2. Otherwise, provider.ScreenWithPolicy (retries with backoff, up to
//     cfg.retries() attempts each bounded by cfg.timeout()). A real
//     success is cached and classified normally. Once every retry is
//     exhausted, cfg.OutagePolicy decides what gets reported instead of
//     ever running the exhausted-retries placeholder through Classify
//     (see provider.ScreenWithPolicy's own doc comment for why that
//     placeholder must never be classified): FailClosed reports
//     verdict.Unavailable() (Hold), FailOpen reports
//     verdict.PassVendorUnavailable() (Pass, but audibly not a real
//     clean result). Neither outcome gets a screening_results row --
//     there was no real vendor response to cache.
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

	if decision.Classification == verdict.Hold {
		// Opened BEFORE reporting to C1, not after, and unconditionally
		// on every attempt (idempotent via holds' own partial unique
		// index -- a retry just returns the existing OPEN hold): the
		// alternative order -- open only once ReportVerdict confirms
		// success -- has a real failure window where C1 already knows
		// the order is held but this call crashes or errors before
		// recording that locally. A retry would then hit
		// ledgerclient.ErrIllegalTransition (the order already left
		// funded) and this function's own handling below marks the
		// queue row DONE without ever creating a holds row -- a real
		// order stuck in held with no human ever able to see it. Opening
		// first means the worst case is a stale OPEN hold for an order
		// that actually left held some OTHER way (a genuine race, not
		// this pipeline's own report) -- self-correcting the moment a
		// reviewer tries to act on it, since Release/Reject then surface
		// the same ErrIllegalTransition instead of silently corrupting
		// anything.
		orderRef := ledgerclient.OrderRef{OrderID: entry.OrderID, ExternalID: entry.ExternalID}
		if _, err := holds.Open(ctx, pool, orderRef, decision); err != nil {
			return fmt.Errorf("pipeline: opening hold for order %d (%s): %w", entry.OrderID, entry.ExternalID, err)
		}
		cfg.recordHoldOpened()
	}

	if err := reporter.ReportVerdict(ctx, entry.ExternalID, decision); err != nil {
		if errors.Is(err, ledgerclient.ErrIllegalTransition) {
			slog.Info("pipeline: order left funded before its verdict could be reported, marking DONE without retry",
				"order_id", entry.OrderID, "external_id", entry.ExternalID)
			return discovery.MarkDone(ctx, pool, entry.OrderID)
		}
		return fmt.Errorf("pipeline: reporting verdict for order %d (%s): %w", entry.OrderID, entry.ExternalID, err)
	}
	cfg.recordVerdictReported(decision.Classification)

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

	v, err := provider.ScreenWithPolicy(ctx, prov, address, cfg.timeout(), cfg.retries(), cfg.OutagePolicy)
	if err != nil {
		if !errors.Is(err, provider.ErrProviderUnavailable) {
			return verdict.Decision{}, fmt.Errorf("pipeline: screening %s: %w", address, err)
		}
		// FailClosed exhaustion: ScreenWithPolicy already logged the
		// details (address, retries, last_error) -- nothing more to add
		// here beyond the metric.
		cfg.recordVendorUnavailable()
		return verdict.Unavailable(), nil
	}
	if provider.IsProviderUnavailable(v) {
		// FailOpen exhaustion: a nil error, but still an outage event --
		// same metric as the FailClosed branch above, since the metric's
		// whole point is visibility regardless of which policy is
		// masking it from the order pipeline's own behavior.
		cfg.recordVendorUnavailable()
		return verdict.PassVendorUnavailable(), nil
	}

	id, err := cache.Put(ctx, pool, cfg.ProviderName, address, v, cfg.TTL.For(v))
	if err != nil {
		return verdict.Decision{}, fmt.Errorf("pipeline: caching verdict for %s: %w", address, err)
	}
	decision := verdict.Classify(v, cfg.thresholds())
	decision.ScreeningResultID = id
	return decision, nil
}
