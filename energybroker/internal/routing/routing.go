// Package routing is C4.2: turning live, fresh, under-ceiling prices
// (C4.1) into an actual provider selection. It never calls Delegate on
// anything -- PriceSource's own narrow shape (Quote only) makes that
// structurally impossible, not just a convention this package happens
// to follow, matching the "zero attempts to call any primary provider's
// Delegate" acceptance criterion by construction rather than by
// discipline.
package routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"sync"

	"energybroker/internal/db"
	"energybroker/internal/pricing"
	"energybroker/internal/provider"
)

// PriceSource is the one call this package needs from C4.1 --
// pricing.Poller's real implementation, or a fake for testing. Defined
// here (the consumer), not in internal/pricing, matching this module's
// own established convention (e.g. pipeline.Reporter in the sibling
// screening module).
type PriceSource interface {
	CurrentPrice(ctx context.Context, providerName string) (provider.Quote, error)
}

// RoutingWeights maps a primary provider's name (provider.Tronsell,
// provider.Netts, or provider.Catfee -- never provider.JustLendManual,
// which is a fallback OUTCOME this package returns, never a weighted
// candidate it selects among) to its configured share of traffic, per
// §B's own `routing_weights` config block. Weights need not sum to
// exactly 1 -- SelectProvider always renormalizes over whichever subset
// actually survives filtering.
type RoutingWeights map[string]float64

// ErrInvalidRoutingWeights guards SelectProvider against a config
// mistake that would otherwise fail silently or panic: an empty weight
// set, a negative weight, a zero-sum weight set, or a weight entry for
// a provider name this package doesn't recognize as one of the three
// primaries.
var ErrInvalidRoutingWeights = errors.New("routing: invalid routing weights")

// ErrInvalidCeiling guards SelectProvider (and, via each their own
// constructor, internal/buffer.Buffer and internal/reservations.Service)
// against a misconfigured ceiling of zero or negative -- C4.7's own
// adversarial scenario. A non-positive ceiling must refuse to start
// (or, here, refuse the call outright) rather than silently treating
// every real price as over ceiling.
var ErrInvalidCeiling = errors.New("routing: invalid ceiling")

func (w RoutingWeights) validate() error {
	if len(w) == 0 {
		return fmt.Errorf("%w: empty", ErrInvalidRoutingWeights)
	}
	var sum float64
	for name, weight := range w {
		if name != provider.Tronsell && name != provider.Netts && name != provider.Catfee {
			return fmt.Errorf("%w: %q is not one of the three primary providers", ErrInvalidRoutingWeights, name)
		}
		if weight < 0 {
			return fmt.Errorf("%w: %q has a negative weight %v", ErrInvalidRoutingWeights, name, weight)
		}
		sum += weight
	}
	if sum <= 0 {
		return fmt.Errorf("%w: weights sum to %v, want > 0", ErrInvalidRoutingWeights, sum)
	}
	return nil
}

// Reason values Selection.Reason can take.
const (
	// ReasonWeighted is a normal weighted-random pick among one or more
	// healthy, under-ceiling primaries.
	ReasonWeighted = "weighted"
	// ReasonFallbackLadder means every primary is unreachable right
	// now (unhealthy per C4.1, or its price has gone stale) -- an
	// outage, not a price event. Triggers C4.6's manual runbook path.
	ReasonFallbackLadder = "fallback_ladder"
	// ReasonManualRequired means every primary DID answer with a
	// current, healthy price, but every one of them is priced above
	// the configured ceiling -- a genuine market-wide spike, invariant
	// 3 forbids paying through. Distinguished from ReasonFallbackLadder
	// so C4.6's alerting can tell "vendors are down" from "vendors are
	// just expensive right now" apart, per this chunk's own acceptance
	// criteria.
	ReasonManualRequired = "manual_required"
)

// Selection is SelectProvider's own result: which provider to use, and
// why -- Reason is never decorative, C4.6's own alerting reads it to
// decide whether an outage or a price event triggered the fallback.
type Selection struct {
	Provider string
	Reason   string
}

// Router selects a provider for one reservation attempt, and (C4.6)
// records/de-duplicates manual_fallback_events when it can't. Stateful
// (an injected PriceSource, its own seeded PRNG, and now a database
// connection for fallback.go's own table) for the same reason
// pricing.Poller is stateful rather than a bag of free functions: a
// live routing decision needs a live price feed, and a reproducible-in-
// tests random draw needs its own owned generator, never math/rand's
// shared global source (which every other package's own test could also
// be perturbing concurrently).
type Router struct {
	prices PriceSource
	pool   *db.Pool

	mu      sync.Mutex
	rng     *rand.Rand
	metrics MetricsRecorder
}

// MetricsRecorder is how this package reports the two Prometheus
// metrics C4.8's own build spec names that originate here:
// ceiling_rejections_total{provider} and manual_fallback_events_total.
// Optional -- nil (the default) means no metrics are recorded, never a
// panic, the same convention every other pluggable dependency in this
// project's own sibling modules uses. Set via SetMetrics rather than a
// NewRouter parameter: every existing caller across this module already
// constructs a Router before any Metrics implementation exists to give
// it, and metrics are the one dependency truly optional enough not to
// warrant another constructor signature change.
type MetricsRecorder interface {
	CeilingRejected(providerName string)
	ManualFallbackEventTriggered()
}

// SetMetrics wires m into this Router -- see MetricsRecorder's own doc
// comment. Not safe to call concurrently with SelectProvider/
// OnFallbackTriggered; call it once, right after NewRouter, before the
// Router is shared with anything else.
func (r *Router) SetMetrics(m MetricsRecorder) {
	r.metrics = m
}

func (r *Router) recordCeilingRejected(providerName string) {
	if r.metrics != nil {
		r.metrics.CeilingRejected(providerName)
	}
}

func (r *Router) recordManualFallbackEventTriggered() {
	if r.metrics != nil {
		r.metrics.ManualFallbackEventTriggered()
	}
}

// NewRouter returns a Router reading prices from prices, recording
// fallback events (C4.6) into pool, with its own PRNG seeded by seed --
// the same seeded, reproducible-sequence discipline provider.MockProvider
// itself already uses.
func NewRouter(prices PriceSource, pool *db.Pool, seed int64) *Router {
	return &Router{prices: prices, pool: pool, rng: rand.New(rand.NewSource(seed))}
}

// SelectProvider filters weights' three primaries to those that are (a)
// healthy and priced within the current staleness window per C4.1, and
// (b) under ceiling per C4.1's own UnderCeiling, then makes a
// weighted-random pick among survivors, renormalized over just their
// own weights -- if catfee is down, tronsell:netts keeps its 60:35
// ratio between the two of them, never 60:35 against a phantom 5% that
// no longer has anywhere to go.
//
// Zero survivors always returns Selection{Provider: justlend_manual,
// ...} -- the trigger condition for C4.6's manual runbook, never an
// automatic delegation attempt against JustLendDAO (see this
// component's own "Read this second") -- with Reason distinguishing
// why: ReasonFallbackLadder if no primary could even be priced right
// now (an outage), ReasonManualRequired if at least one primary
// answered but every priced primary is above ceiling (a price event).
func (r *Router) SelectProvider(ctx context.Context, weights RoutingWeights, ceiling float64) (Selection, error) {
	if err := weights.validate(); err != nil {
		return Selection{}, err
	}
	// C4.7's own adversarial scenario: a ceiling misconfigured to zero or
	// negative must refuse outright here, not silently fall through to
	// UnderCeiling, which (being `price <= ceiling`) would just reject
	// every real, positive price and permanently fallback-ladder --
	// safe, but a silent, undiagnosable misconfiguration rather than a
	// loud one. This is deliberately checked here too, not only at
	// Buffer/Service construction time: a second, independent guard
	// costs nothing and protects any future caller that builds a Router
	// without going through either constructor's own validation.
	if ceiling <= 0 {
		return Selection{}, fmt.Errorf("%w: ceiling must be positive, got %v", ErrInvalidCeiling, ceiling)
	}

	survivors := make(map[string]float64, len(weights))
	var pricedButOverCeiling int

	names := make([]string, 0, len(weights))
	for name := range weights {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic iteration order for a reproducible draw given a fixed rng sequence

	for _, name := range names {
		quote, err := r.prices.CurrentPrice(ctx, name)
		if err != nil {
			// Not a ceiling check at all -- unhealthy or stale, never
			// reached comparing a price against ceiling -- so this is
			// deliberately not logged by auditCeilingCheck below, which
			// exists specifically to answer "did we ever overpay",
			// not "was every provider reachable".
			continue
		}
		underCeiling := pricing.UnderCeiling(quote, ceiling)
		auditCeilingCheck(name, quote.PricePerUnitSun, ceiling, underCeiling)
		if !underCeiling {
			pricedButOverCeiling++
			r.recordCeilingRejected(name)
			continue
		}
		survivors[name] = weights[name]
	}

	if len(survivors) == 0 {
		// At least one primary answered with a real, current price --
		// even if every one that did is over ceiling and the rest are
		// simply unreachable -- reads as a pricing problem an operator
		// can see a real number for, not a pure connectivity outage.
		// Only zero priced responses at all is a true outage.
		if pricedButOverCeiling > 0 {
			return Selection{Provider: provider.JustLendManual, Reason: ReasonManualRequired}, nil
		}
		return Selection{Provider: provider.JustLendManual, Reason: ReasonFallbackLadder}, nil
	}

	chosen := r.weightedPick(names, survivors)
	return Selection{Provider: chosen, Reason: ReasonWeighted}, nil
}

// weightedPick draws uniformly over the renormalized survivor weights.
// names is iterated in its already-sorted order so the mapping from one
// rng draw to a chosen provider is deterministic given a fixed PRNG
// sequence, not at the mercy of Go's randomized map iteration order.
func (r *Router) weightedPick(names []string, survivors map[string]float64) string {
	var total float64
	for _, name := range names {
		total += survivors[name] // zero for a name that didn't survive filtering
	}

	r.mu.Lock()
	draw := r.rng.Float64() * total
	r.mu.Unlock()

	var cumulative float64
	for _, name := range names {
		weight, ok := survivors[name]
		if !ok {
			continue
		}
		cumulative += weight
		if draw < cumulative {
			return name
		}
	}
	// Floating-point rounding at the very top of the range -- return the
	// last survivor in sorted order rather than an empty string.
	for i := len(names) - 1; i >= 0; i-- {
		if _, ok := survivors[names[i]]; ok {
			return names[i]
		}
	}
	panic("routing: weightedPick called with no survivors")
}

// auditCeilingCheck logs one line for every ceiling comparison
// SelectProvider actually performs, pass or fail -- C4.7's own
// acceptance criterion: "a full audit log line exists for every ceiling
// check performed... the artifact that lets someone answer 'did we ever
// overpay' definitively rather than by inference." Info-level on a pass
// (routine, high-volume, not worth an operator's attention on its own)
// and Warn-level on a rejection (still routine during a real price
// spike, but worth being loud about individually, distinct from the
// once-per-outage manual_fallback_events row C4.6 records separately).
func auditCeilingCheck(providerName string, priceSun, ceiling float64, underCeiling bool) {
	if underCeiling {
		slog.Info("routing: ceiling check", "provider", providerName, "price_sun", priceSun, "ceiling_sun", ceiling, "under_ceiling", true)
		return
	}
	slog.Warn("routing: ceiling check", "provider", providerName, "price_sun", priceSun, "ceiling_sun", ceiling, "under_ceiling", false)
}
