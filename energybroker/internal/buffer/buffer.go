// Package buffer is C4.3: "Read this third" made real -- a standing,
// continuously-replenished pool of delegatable TRON energy, reconciled
// against on-chain truth rather than trusted as bookkeeping alone.
package buffer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"energybroker/internal/db"
	"energybroker/internal/provider"
	"energybroker/internal/routing"
)

// DemandObserver is the one thing TargetLevel needs that this chunk
// itself does not own: a rolling window of actual reservation volume.
// C4.4 (the next chunk) is what will actually create reservations and
// therefore be able to answer this -- this package is built against the
// interface now, with a fake implementation this chunk's own tests use
// meanwhile, the same posture C3.0 took toward its own C1 sender-address
// gap (SenderAddressLookup) before C1 actually exposed one.
type DemandObserver interface {
	// RecentReservedUnits reports how many energy units were reserved
	// across the last window of wall-clock time.
	RecentReservedUnits(ctx context.Context, window time.Duration) (int64, error)
}

// Config scopes how this Buffer sizes and replenishes itself.
type Config struct {
	// MinimumFloor is the target level regardless of recent demand, so a
	// quiet period doesn't let the buffer drain to zero right before a
	// burst -- this chunk's own build spec names this requirement
	// explicitly.
	MinimumFloor int64
	// LookbackWindow is how far back TargetLevel samples DemandObserver
	// for recent volume.
	LookbackWindow time.Duration
	// LookaheadWindow is how far ahead the buffer should cover, at the
	// rate observed over LookbackWindow -- e.g. a 2-hour lookback
	// projected across a 30-minute lookahead means "hold half an hour's
	// worth of whatever rate we've actually been seeing."
	LookaheadWindow time.Duration
	// Weights and Ceiling are passed straight through to
	// routing.SelectProvider on every replenish chunk -- see C4.2.
	Weights routing.RoutingWeights
	Ceiling float64
	// DelegationDuration is how long a single Delegate call's own lease
	// covers, and therefore how far in the future a freshly-replenished
	// row's own expires_at is set.
	DelegationDuration time.Duration
	// ReconcileLookahead is how close to a row's own expires_at counts
	// as "nearing" it for Reconcile's own purposes (DefaultReconcileLookahead
	// if unset) -- see verify.go.
	ReconcileLookahead time.Duration
	// StagingAddress is the broker-controlled TRON address buffer
	// capacity is delegated to during Replenish, before any specific
	// order is known to need it. This is a real, flagged design
	// decision this chunk makes, not something the build spec states
	// outright: TRON energy delegation always targets a specific
	// address, but a standing buffer is deliberately order-independent
	// (the whole point of "Read this third" is acquiring capacity
	// BEFORE a payout needs it) -- so this component needs an address of
	// its own to hold that capacity against, pending re-delegation to
	// the actual payout slot once C4.4 exists to do it. Whoever owns S1
	// should confirm this is the right address to use in production;
	// see this chunk's own commit message.
	StagingAddress string
}

func (cfg Config) lookback() time.Duration {
	if cfg.LookbackWindow <= 0 {
		return DefaultLookbackWindow
	}
	return cfg.LookbackWindow
}

func (cfg Config) lookahead() time.Duration {
	if cfg.LookaheadWindow <= 0 {
		return DefaultLookaheadWindow
	}
	return cfg.LookaheadWindow
}

func (cfg Config) delegationDuration() time.Duration {
	if cfg.DelegationDuration <= 0 {
		return DefaultDelegationDuration
	}
	return cfg.DelegationDuration
}

// DefaultLookbackWindow, DefaultLookaheadWindow: a 2-hour sample
// projected across a 30-minute lookahead -- at component-map's own
// ~4/hour peak, this covers roughly 2 upcoming payouts' worth of energy
// at the recently observed rate, without needing a hardcoded volume
// constant that would go stale the moment real traffic differs from
// today's estimate (this chunk's own build spec explicitly warns
// against that).
const (
	DefaultLookbackWindow  = 2 * time.Hour
	DefaultLookaheadWindow = 30 * time.Minute
)

// DefaultDelegationDuration is how long a single Replenish chunk's own
// Delegate call leases capacity for, when Config.DelegationDuration is
// unset.
const DefaultDelegationDuration = 24 * time.Hour

// DefaultReplenishInterval is how often RunReplenishLoop checks the
// buffer against TargetLevel, when Interval is unset.
const DefaultReplenishInterval = 60 * time.Second

// Buffer is C4.3's own standing pool: it reads prices and health from
// C4.1 (via the Router it holds), makes provider choices via C4.2's own
// Router, and calls Delegate directly on whichever EnergyProvider the
// Router selected.
type Buffer struct {
	pool      *db.Pool
	providers map[string]provider.EnergyProvider
	router    *routing.Router
	demand    DemandObserver
	reader    TronEnergyReader
	alerter   Alerter
	cfg       Config
}

// NewBuffer wires a Buffer. providers must contain an entry for every
// provider name cfg.Weights names -- Replenish returns an error the
// first time the Router selects a name with no matching entry, rather
// than silently skipping it.
func NewBuffer(pool *db.Pool, providers map[string]provider.EnergyProvider, router *routing.Router, demand DemandObserver, reader TronEnergyReader, alerter Alerter, cfg Config) *Buffer {
	return &Buffer{
		pool:      pool,
		providers: providers,
		router:    router,
		demand:    demand,
		reader:    reader,
		alerter:   alerter,
		cfg:       cfg,
	}
}

// TargetLevel is how many units this buffer should hold right now: the
// recent demand rate (DemandObserver, over Config.LookbackWindow)
// projected across Config.LookaheadWindow, floored at
// Config.MinimumFloor. Never a hardcoded number -- see Config's own doc
// comment on why.
func (b *Buffer) TargetLevel(ctx context.Context) (int64, error) {
	recent, err := b.demand.RecentReservedUnits(ctx, b.cfg.lookback())
	if err != nil {
		return 0, fmt.Errorf("buffer: sampling recent demand: %w", err)
	}
	if recent < 0 {
		return 0, fmt.Errorf("buffer: DemandObserver reported a negative recent-units value %d", recent)
	}

	rate := float64(recent) / b.cfg.lookback().Seconds()
	projected := int64(rate * b.cfg.lookahead().Seconds())

	target := projected
	if target < b.cfg.MinimumFloor {
		target = b.cfg.MinimumFloor
	}
	return target, nil
}

// RunLoop replenishes on an interval. Blocks until ctx is cancelled,
// returning ctx.Err().
func (b *Buffer) RunLoop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultReplenishInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := b.Replenish(ctx); err != nil {
		slog.Error("buffer: initial replenish failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := b.Replenish(ctx); err != nil {
				slog.Error("buffer: replenish failed", "error", err)
			}
		}
	}
}

// Replenish tops the buffer up toward TargetLevel, one provider-chunk at
// a time via routing.SelectProvider, bounded by each provider's own
// MaxUnitsAvailable. A step that can't proceed right now -- routing
// falls back to justlend_manual, a re-quote fails, Delegate fails, or
// VerifyOnChain doesn't confirm -- logs and returns nil rather than
// erroring the whole tick: the next scheduled tick (RunLoop) or the next
// explicit caller tries again, the same "log and continue, never a
// retry storm within one call" discipline every other loop-owning
// package in this project uses.
func (b *Buffer) Replenish(ctx context.Context) error {
	target, err := b.TargetLevel(ctx)
	if err != nil {
		return err
	}
	current, err := availableTotal(ctx, b.pool)
	if err != nil {
		return err
	}

	shortfall := target - current
	for shortfall > 0 {
		sel, err := b.router.SelectProvider(ctx, b.cfg.Weights, b.cfg.Ceiling)
		if err != nil {
			return fmt.Errorf("buffer: selecting a provider to replenish: %w", err)
		}
		if sel.Provider == provider.JustLendManual {
			// orderID nil: this is the buffer's own background loop
			// detecting the outage before any specific order needed the
			// energy -- OnFallbackTriggered's own de-dup means a
			// prolonged outage spanning many ticks still records exactly
			// one open manual_fallback_events row, not one per tick.
			event, err := b.router.OnFallbackTriggered(ctx, sel.Reason, nil)
			if err != nil {
				slog.Error("buffer: recording the fallback event failed", "reason", sel.Reason, "error", err)
			}
			slog.Warn("buffer: replenish blocked -- every primary provider is unavailable or over ceiling, see docs/runbook-energy-fallback.md",
				"reason", sel.Reason, "shortfall_units", shortfall, "fallback_event_id", event.ID)
			return nil
		}

		prov, ok := b.providers[sel.Provider]
		if !ok {
			return fmt.Errorf("buffer: router selected %q, which has no wired EnergyProvider", sel.Provider)
		}

		quote, err := prov.Quote(ctx)
		if err != nil {
			slog.Warn("buffer: re-quoting the just-selected provider failed, will retry next cycle", "provider", sel.Provider, "error", err)
			return nil
		}

		chunk := shortfall
		if quote.MaxUnitsAvailable > 0 && chunk > quote.MaxUnitsAvailable {
			chunk = quote.MaxUnitsAvailable
		}

		delegation, err := prov.Delegate(ctx, b.cfg.StagingAddress, chunk, b.cfg.delegationDuration())
		if err != nil {
			slog.Error("buffer: delegate call failed", "provider", sel.Provider, "error", err)
			return nil
		}

		confirmed, err := b.VerifyOnChain(ctx, delegation)
		if err != nil {
			slog.Error("buffer: on-chain verification errored, NOT marking available", "provider", sel.Provider, "delegation_id", delegation.ID, "error", err)
			return nil
		}
		if !confirmed {
			slog.Error("buffer: delegation did not verify on-chain, NOT marking available -- a vendor's own 200 response is never trusted alone (invariant 1)",
				"provider", sel.Provider, "delegation_id", delegation.ID, "units", chunk)
			return nil
		}

		expiresAt := delegation.RequestedAt.Add(b.cfg.delegationDuration())
		if _, err := insertAvailable(ctx, b.pool, delegation, expiresAt); err != nil {
			return fmt.Errorf("buffer: recording replenished capacity: %w", err)
		}

		shortfall -= chunk
	}
	return nil
}
