// Package pricing is C4.1: continuous price awareness for the three
// live-priced providers. It turns raw vendor quotes into a trustworthy,
// fresh price feed -- routing decisions (C4.2) are a later chunk's job,
// this one only answers "what did we last see, and is it still good
// enough to trust."
package pricing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"energybroker/internal/db"
	"energybroker/internal/provider"
)

// DefaultInterval is how often RunLoop polls every configured provider,
// when Interval is unset. This chunk's own build spec names 30-60s as
// the right order of magnitude ("energy prices don't move fast enough
// at this volume to need sub-minute polling") -- 60s is the end of that
// range this package picks, erring toward fewer vendor calls rather than
// more, the same cost-consciousness component-map.md flags for C3's own
// per-call vendor cost.
const DefaultInterval = 60 * time.Second

// DefaultStaleness is CurrentPrice's own staleness window, when Staleness
// is unset: twice DefaultInterval, so a single missed or slow tick does
// not flip every provider unhealthy on its own, while a provider that
// misses two ticks in a row (something is actually wrong, not just
// ordinary jitter) does. Invariant 2 requires picking a number and
// defending it, not leaving it open-ended -- this is that number.
const DefaultStaleness = 2 * DefaultInterval

// DefaultPollTimeout bounds a single provider.Quote call during one
// poll tick, matching C3's own DefaultTimeout for a vendor call.
const DefaultPollTimeout = 10 * time.Second

// ErrPriceStale is CurrentPrice's own result when the freshest
// observation on record for a provider (if any) is older than the
// configured staleness window. Distinct from ErrProviderUnhealthy
// (below): this is a DURABLE, database-only signal -- it holds even
// across a process restart that lost every in-memory health flag this
// package's own Poller tracked, the same "cache vs. computed, they must
// agree" discipline C1.4 established for balance reconciliation. A
// caller must never fall back to the last-known value on this error;
// see this chunk's own invariant 2.
var ErrPriceStale = errors.New("pricing: price is stale")

// ErrProviderUnhealthy is CurrentPrice's own result when this Poller's
// own last poll attempt for a provider errored -- reflected starting
// from that exact tick, not retroactively, and not conflated with
// ErrPriceStale: a provider that is actively erroring right now is a
// different fact from one whose last known-good price merely aged past
// the staleness window, the same way C3's own outage handling keeps
// "vendor timed out" and "no verdict at all" as distinct, auditable
// reasons rather than collapsing them into one generic failure.
var ErrProviderUnhealthy = errors.New("pricing: provider is unhealthy")

// Poller polls a fixed set of named providers and answers "what's the
// current, trustworthy price" for each. It is the one place this
// component holds the two facts CurrentPrice needs together: the
// database's own observation history, and this process's own live
// health state per provider -- see CurrentPrice's own doc comment for
// why both matter.
type Poller struct {
	pool      *db.Pool
	providers map[string]provider.EnergyProvider
	staleness time.Duration
	timeout   time.Duration

	mu     sync.RWMutex
	status map[string]error // nil/absent = healthy; non-nil = the most recent Quote() attempt's own error
}

// NewPoller returns a Poller for providers (keyed by provider name --
// see internal/provider's own name constants), recording observations
// into pool and treating an observation older than staleness as stale
// (DefaultStaleness if staleness <= 0).
func NewPoller(pool *db.Pool, providers map[string]provider.EnergyProvider, staleness time.Duration) *Poller {
	if staleness <= 0 {
		staleness = DefaultStaleness
	}
	return &Poller{
		pool:      pool,
		providers: providers,
		staleness: staleness,
		timeout:   DefaultPollTimeout,
		status:    make(map[string]error),
	}
}

// RunLoop polls every configured provider on an interval. Blocks until
// ctx is cancelled, returning ctx.Err().
func (p *Poller) RunLoop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := p.PollAll(ctx); err != nil {
		slog.Error("pricing: initial poll failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := p.PollAll(ctx); err != nil {
				slog.Error("pricing: poll failed", "error", err)
			}
		}
	}
}

// PollAll calls Quote() on every configured provider concurrently. A
// provider that errors or times out is marked unhealthy for CurrentPrice
// callers starting from this call, not retroactively -- its price_
// observations rows (if any) are left exactly as they were; no failure
// row is ever written, only success rows, keeping that table an
// unambiguous record of "prices actually observed," never "attempts
// made." One provider's own failure never stops another's -- and never
// makes this method itself return a non-nil error -- the same "log and
// continue per item" discipline discovery.RunTick and pipeline.RunTick
// both use for their own per-item failures.
func (p *Poller) PollAll(ctx context.Context) error {
	var wg sync.WaitGroup
	for name, prov := range p.providers {
		wg.Add(1)
		go func(name string, prov provider.EnergyProvider) {
			defer wg.Done()

			attemptCtx, cancel := context.WithTimeout(ctx, p.timeout)
			quote, err := prov.Quote(attemptCtx)
			cancel()
			if err != nil {
				p.setStatus(name, err)
				slog.Warn("pricing: poll failed, provider marked unhealthy", "provider", name, "error", err)
				return
			}

			if err := insertObservation(ctx, p.pool, quote); err != nil {
				p.setStatus(name, err)
				slog.Error("pricing: recording observation failed, provider marked unhealthy", "provider", name, "error", err)
				return
			}
			p.setStatus(name, nil)
		}(name, prov)
	}
	wg.Wait()
	return nil
}

func (p *Poller) setStatus(name string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status[name] = err
}

func (p *Poller) lastError(name string) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status[name]
}

// CurrentPrice returns providerName's current, trustworthy price: an
// error if either (a) this Poller's own last poll attempt for
// providerName failed (ErrProviderUnhealthy, checked first -- the
// tick-immediate signal), or (b) the freshest observation on record is
// older than the configured staleness window, or there is none at all
// (ErrPriceStale -- the durable, database-only backstop that still
// works even against a freshly-constructed Poller with no in-memory
// health history, e.g. right after a process restart). Never returns a
// stale or zero-value Quote silently -- see invariant 2.
func (p *Poller) CurrentPrice(ctx context.Context, providerName string) (provider.Quote, error) {
	if err := p.lastError(providerName); err != nil {
		return provider.Quote{}, fmt.Errorf("%w: %s: %w", ErrProviderUnhealthy, providerName, err)
	}

	obs, err := latestObservation(ctx, p.pool, providerName)
	if err != nil {
		return provider.Quote{}, err
	}
	if obs == nil {
		return provider.Quote{}, fmt.Errorf("%w: %s: no observation recorded yet", ErrPriceStale, providerName)
	}
	if age := time.Since(obs.observedAt); age > p.staleness {
		return provider.Quote{}, fmt.Errorf("%w: %s: last observation is %s old (staleness window %s)",
			ErrPriceStale, providerName, age.Round(time.Second), p.staleness)
	}
	return obs.toQuote(), nil
}

// UnderCeiling reports whether quote's price is acceptable to pay under
// ceiling. A price exactly EQUAL to ceiling is under it: invariant 3
// forbids paying a price ABOVE the configured ceiling, and a price
// merely at the ceiling has not crossed that line -- the same boundary
// discipline C3.2's own Classify uses (documented and tested explicitly,
// never left to accidental float behavior). Pure and stateless so
// routing (C4.2) and the slow path (C4.4) both call this exact function
// rather than two implementations that could drift.
func UnderCeiling(quote provider.Quote, ceiling float64) bool {
	return quote.PricePerUnitSun <= ceiling
}

// observation is one price_observations row.
type observation struct {
	providerName string
	priceSun     float64
	observedAt   time.Time
	maxUnits     int64
}

func (o observation) toQuote() provider.Quote {
	return provider.Quote{
		ProviderName:      o.providerName,
		PricePerUnitSun:   o.priceSun,
		QuotedAt:          o.observedAt,
		MaxUnitsAvailable: o.maxUnits,
	}
}

// latestObservation returns the single most recent price_observations
// row for providerName, or nil if there is none at all.
func latestObservation(ctx context.Context, q db.Queryer, providerName string) (*observation, error) {
	row := q.QueryRow(ctx, `
		SELECT provider_name, price_sun, observed_at, max_units
		FROM price_observations
		WHERE provider_name = $1
		ORDER BY observed_at DESC
		LIMIT 1
	`, providerName)

	var o observation
	if err := row.Scan(&o.providerName, &o.priceSun, &o.observedAt, &o.maxUnits); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pricing: fetching latest observation for %s: %w", providerName, err)
	}
	return &o, nil
}

// insertObservation records quote as a new, append-only
// price_observations row -- never an update to an existing one.
func insertObservation(ctx context.Context, q db.Queryer, quote provider.Quote) error {
	_, err := q.Exec(ctx, `
		INSERT INTO price_observations (provider_name, price_sun, observed_at, max_units)
		VALUES ($1, $2, $3, $4)
	`, quote.ProviderName, quote.PricePerUnitSun, quote.QuotedAt, quote.MaxUnitsAvailable)
	if err != nil {
		return fmt.Errorf("pricing: recording observation for %s: %w", quote.ProviderName, err)
	}
	return nil
}
