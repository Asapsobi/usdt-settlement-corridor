// Package rescreen is C3.7: the mechanism for
// `c1-scenario-catalog.md`'s open item -- a sender address gets flagged
// after an order already passed screening. It re-checks the provider
// (bypassing internal/cache's own trust window on purpose) for every
// distinct sender behind an order currently `screened` or `dispatching`,
// and records a rescreen_flags row plus a loud log alert when the fresh
// verdict disagrees with the one that let the order through.
//
// This package NEVER calls a C1 transition. There is no screened->held
// or dispatching->held pair in C1.5's own transition table for a
// provenance re-flag (the only dispatching->held path requires a
// reversal, for a dispatch failure, not this) -- inventing one would be
// exactly the kind of unilateral C1 change this system's own build docs
// have deliberately avoided elsewhere. A human decides what happens to
// an order already mid-flight; this package only ever surfaces the fact.
package rescreen

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"screening/internal/cache"
	"screening/internal/db"
	"screening/internal/ledgerclient"
	"screening/internal/provider"
	"screening/internal/verdict"
)

// DefaultInterval is how often RunLoop re-checks, when Interval is
// unset. component-map.md's own volume figure (~100 screenings/day)
// makes even an hourly full re-check cheap -- this chunk's own build
// spec names the same default.
const DefaultInterval = time.Hour

// DefaultTimeout bounds a single provider.Screen call.
const DefaultTimeout = 10 * time.Second

// relevantStates are the only two states this job ever re-screens:
// already past C3's own gate (funded/held are not yet through it) but
// not yet settled (terminal states -- settled, refunded, expired -- are
// out of scope; a post-hoc flag on one is a different, unspecified
// problem this chunk does not attempt to solve).
var relevantStates = []string{"screened", "dispatching"}

// OrderLister is the one call this job needs from C1 --
// ledgerclient.Client's real implementation, or a fake for testing.
type OrderLister interface {
	ListOrdersByState(ctx context.Context, state, cursor string) (refs []ledgerclient.OrderRef, newCursor string, err error)
}

// Config scopes what this job re-screens against and how.
type Config struct {
	// ProviderName must match internal/cache's own key for this
	// provider -- see pipeline.Config's identical field for why a
	// mismatch would silently break comparisons.
	ProviderName string
	// Timeout bounds a single provider.Screen call. Defaults to
	// DefaultTimeout if <= 0.
	Timeout time.Duration
	// Thresholds must match what actually classified the order the
	// first time (verdict.DefaultThresholds if unset, same zero-value
	// safety net as pipeline.Config's own Thresholds field) -- using a
	// DIFFERENT threshold here than C3.4 used would make "disagrees with
	// the cached verdict" meaningless, since the same raw score could
	// classify differently under different thresholds for no real
	// change in risk.
	Thresholds verdict.Thresholds
	// TTL controls how long the freshly re-screened result stays
	// cached, same as pipeline.Config's own TTL field -- a re-screen's
	// fresh result becomes the new latest, trusted row for this address,
	// benefiting any future order from the same sender too.
	TTL cache.TTLConfig
	// Metrics is optional -- nil means no metrics are recorded, never a
	// panic, the same convention every other pluggable dependency in
	// this module uses. Backs C3.8's rescreen_flags_total.
	Metrics MetricsRecorder
}

// MetricsRecorder is how this job reports rescreen_flags_total (C3.8's
// own build spec): one call per flag actually recorded, per affected
// order -- matching ListUnresolved/List's own per-row granularity.
type MetricsRecorder interface {
	FlagRecorded()
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

func (cfg Config) recordFlag() {
	if cfg.Metrics != nil {
		cfg.Metrics.FlagRecorded()
	}
}

// RunLoop re-checks on an interval. Blocks until ctx is cancelled,
// returning ctx.Err().
func RunLoop(ctx context.Context, pool *db.Pool, lister OrderLister, prov provider.ScreeningProvider, cfg Config, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := RunTick(ctx, pool, lister, prov, cfg); err != nil {
		slog.Error("rescreen: initial tick failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := RunTick(ctx, pool, lister, prov, cfg); err != nil {
				slog.Error("rescreen: tick failed", "error", err)
			}
		}
	}
}

// RunTick walks every page of every relevant state, groups the results
// by DISTINCT sender address (one provider.Screen call per address, not
// per order -- several orders can legitimately share a sender), and
// flags every affected order when a fresh check disagrees with the
// verdict that let it through. Silent (no rescreen_flags row, no log
// beyond routine tracing) when nothing has changed -- the expected,
// overwhelming majority case, same posture as C2.5's own
// silent-on-routine-pre-final-reorg behavior.
//
// A single address's failure (a provider timeout, a caching error) is
// logged and skipped, never allowed to stop the rest of the batch --
// this is a background job on an hourly cadence, not a blocking gate;
// the next tick tries again naturally.
func RunTick(ctx context.Context, pool *db.Pool, lister OrderLister, prov provider.ScreeningProvider, cfg Config) error {
	orders, err := collectOrders(ctx, lister)
	if err != nil {
		return fmt.Errorf("rescreen: collecting orders to re-check: %w", err)
	}

	bySender := make(map[string][]orderAtState)
	for _, o := range orders {
		if o.ref.SenderAddress == nil || *o.ref.SenderAddress == "" {
			// Should not happen for screened/dispatching orders --
			// sender_address is set atomically with funded, which both
			// states are always downstream of -- but this job's job is
			// to re-check known senders, not to chase down how one went
			// missing.
			slog.Warn("rescreen: order has no sender_address, skipping",
				"order_id", o.ref.OrderID, "external_id", o.ref.ExternalID, "state", o.state)
			continue
		}
		address := *o.ref.SenderAddress
		bySender[address] = append(bySender[address], o)
	}

	for address, affected := range bySender {
		if err := rescreenAddress(ctx, pool, prov, cfg, address, affected); err != nil {
			slog.Error("rescreen: re-screening address failed, will retry next tick", "address", address, "error", err)
		}
	}
	return nil
}

type orderAtState struct {
	ref   ledgerclient.OrderRef
	state string
}

// collectOrders walks every page of every relevant state. There is no
// persisted cursor between ticks (unlike C3.3's discovery loop): the
// set of screened/dispatching orders isn't monotonically growing
// forward by updated_at, since an order LEAVES the set (moving on to
// settled) without necessarily being the most recently updated one --
// this job needs "everything currently in these states", re-collected
// fully, every tick.
func collectOrders(ctx context.Context, lister OrderLister) ([]orderAtState, error) {
	var out []orderAtState
	for _, state := range relevantStates {
		cursor := ""
		for {
			refs, newCursor, err := lister.ListOrdersByState(ctx, state, cursor)
			if err != nil {
				return nil, fmt.Errorf("listing %s orders: %w", state, err)
			}
			for _, ref := range refs {
				out = append(out, orderAtState{ref: ref, state: state})
			}
			if len(refs) == 0 || newCursor == cursor {
				break
			}
			cursor = newCursor
		}
	}
	return out, nil
}

// rescreenAddress re-checks one distinct sender address and flags every
// order in affected if the fresh verdict disagrees with the one that
// let them through.
//
// Caching the fresh verdict and recording every affected order's flag
// happen inside ONE transaction, not as separate writes: if recording a
// flag failed after the fresh verdict had already been committed to
// screening_results, the NEXT tick's own LatestAny lookup would see that
// fresh (already Hold-classified) row as "previous" and silently take
// the routine no-disagreement branch -- permanently losing a flag that
// was never actually recorded. Wrapping both in db.Tx means a failure
// anywhere in this address's batch rolls the fresh verdict back too, so
// the next tick re-detects the same disagreement from the same,
// unchanged previous verdict instead of silently skipping it.
func rescreenAddress(ctx context.Context, pool *db.Pool, prov provider.ScreeningProvider, cfg Config, address string, affected []orderAtState) error {
	previous, err := cache.LatestAny(ctx, pool, cfg.ProviderName, address)
	if err != nil {
		return fmt.Errorf("fetching previous verdict for %s: %w", address, err)
	}
	if previous == nil {
		// No prior screening_results row at all for an address behind
		// an already-screened order should not normally happen, but
		// isn't this job's problem to diagnose -- nothing to diff
		// against, so there is nothing safe to conclude either way.
		slog.Warn("rescreen: no previous screening result found for an already-screened address, skipping",
			"address", address, "affected_orders", len(affected))
		return nil
	}

	screenCtx, cancel := context.WithTimeout(ctx, cfg.timeout())
	fresh, err := prov.Screen(screenCtx, address)
	cancel()
	if err != nil {
		return fmt.Errorf("re-screening %s: %w", address, err)
	}

	thresholds := cfg.thresholds()
	previousDecision := verdict.Classify(previous.Verdict, thresholds)
	freshDecision := verdict.Classify(fresh, thresholds)

	return db.Tx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		newID, err := cache.Put(ctx, tx, cfg.ProviderName, address, fresh, cfg.TTL.For(fresh))
		if err != nil {
			return fmt.Errorf("caching fresh verdict for %s: %w", address, err)
		}

		if previousDecision.Classification != verdict.Pass || freshDecision.Classification != verdict.Hold {
			// Either it wasn't clean before (not this job's concern --
			// C3.6 already owns anything currently held) or it's still
			// clean now: the routine, silent case. The fresh verdict is
			// still committed above -- it's a genuinely new, real
			// result, worth keeping as the latest cached row regardless
			// of whether it triggers a flag.
			return nil
		}

		for _, o := range affected {
			slog.Error("rescreen: RESCREEN FLAG -- a sender's verdict changed from clean to flagged after its order already passed screening; needs manual review",
				"order_id", o.ref.OrderID, "external_id", o.ref.ExternalID, "order_state", o.state,
				"sender_address", address, "previous_verdict_id", previous.ID, "new_verdict_id", newID)
			if _, err := recordFlag(ctx, tx, o.ref.OrderID, o.ref.ExternalID, o.state, previous.ID, newID); err != nil {
				return fmt.Errorf("recording flag for order %d (%s): %w", o.ref.OrderID, o.ref.ExternalID, err)
			}
			cfg.recordFlag()
		}
		return nil
	})
}
