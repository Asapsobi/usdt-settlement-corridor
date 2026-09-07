package provider

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"energybroker/internal/money"
)

// ErrMalformedResponse is returned by MockProvider for a provider
// configured via ForceMalformed. It exists so a caller can distinguish
// "the vendor sent garbage" from every other error shape -- never
// silently coerced into a zero-value Quote/Delegation treated as valid.
var ErrMalformedResponse = errors.New("provider: malformed response")

// defaultMaxUnitsAvailable is what Quote reports absent a forced
// partial-fill -- large enough that no real MVP-scale reservation
// (component-map's own ~100 payouts/day) would ever be capped by it.
const defaultMaxUnitsAvailable = 10_000_000

// MockProvider is a deterministic, seeded EnergyProvider bound to one
// provider name: the only implementation this chunk ships, and the one
// this component's own eventual replay harness (mirroring C3.9) will
// exercise real routing/pricing logic against once those chunks exist.
//
// Determinism here is by SEEDED PRNG, not content-hash the way C3's own
// MockProvider is keyed by address: TRON energy pricing is a live,
// continuously-fluctuating number by nature (this component polls it
// forever, see C4.1), not a static per-key verdict -- so "same seed
// always yields the same quote sequence" means call order matters and
// is reproducible, not call-order-independent.
type MockProvider struct {
	name         string
	basePriceSun float64

	mu             sync.Mutex
	rng            *rand.Rand
	forcedPrice    *float64
	forceTimeout   bool
	forceMalformed bool
	maxUnits       int64
	delegationSeq  int64
}

// NewMockProvider returns a MockProvider named name (one of the
// Provider name constants), seeded with seed, quoting around
// basePriceSun absent any forced price. Two MockProviders constructed
// with the same name, seed, and basePriceSun, called the same number of
// times, produce an identical quote sequence.
func NewMockProvider(name string, seed int64, basePriceSun float64) *MockProvider {
	return &MockProvider{
		name:         name,
		basePriceSun: basePriceSun,
		rng:          rand.New(rand.NewSource(seed)),
		maxUnits:     defaultMaxUnitsAvailable,
	}
}

// ForcePrice pins every subsequent Quote/Delegate cost calculation to
// sun, including a value above any ceiling a caller might configure --
// this chunk's own acceptance criterion needs a way to exercise
// ceiling-rejection logic in a later chunk without that logic living in
// this package.
func (m *MockProvider) ForcePrice(sun float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forcedPrice = &sun
}

// ForceTimeout configures every subsequent call to never return --
// Quote/Delegate block until ctx is done and return ctx.Err(), exactly
// what a hung real vendor call would look like to a caller that applied
// its own timeout.
func (m *MockProvider) ForceTimeout() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceTimeout = true
}

// ForceMalformed configures every subsequent call to return
// ErrMalformedResponse, simulating a vendor response this client can't
// parse.
func (m *MockProvider) ForceMalformed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceMalformed = true
}

// ForcePartialFill caps every subsequent Quote's MaxUnitsAvailable at
// units, simulating a vendor that can't fill a full-size request in one
// call.
func (m *MockProvider) ForcePartialFill(units int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxUnits = units
}

// Quote implements EnergyProvider.
func (m *MockProvider) Quote(ctx context.Context) (Quote, error) {
	m.mu.Lock()
	forceTimeout := m.forceTimeout
	forceMalformed := m.forceMalformed
	forcedPrice := m.forcedPrice
	maxUnits := m.maxUnits
	m.mu.Unlock()

	if forceTimeout {
		// Block on ctx alone -- never a fixed sleep, so a caller's
		// deadline is respected exactly, not overshot.
		<-ctx.Done()
		return Quote{}, ctx.Err()
	}
	if forceMalformed {
		return Quote{}, fmt.Errorf("mock provider %s: %w", m.name, ErrMalformedResponse)
	}
	if err := ctx.Err(); err != nil {
		return Quote{}, err
	}

	return Quote{
		ProviderName:      m.name,
		PricePerUnitSun:   m.nextPrice(forcedPrice),
		QuotedAt:          time.Now().UTC(),
		MaxUnitsAvailable: maxUnits,
	}, nil
}

// nextPrice advances this provider's own seeded PRNG by exactly one
// draw per call, regardless of which branch below is taken -- so a
// forced price configured mid-sequence doesn't shift what every
// subsequent, un-forced call in the same run would have quoted, keeping
// "same seed, same sequence" true call-for-call.
func (m *MockProvider) nextPrice(forcedPrice *float64) float64 {
	m.mu.Lock()
	jitter := m.rng.Float64()*0.2 - 0.1 // +/-10% around basePriceSun
	m.mu.Unlock()
	if forcedPrice != nil {
		return *forcedPrice
	}
	return m.basePriceSun * (1 + jitter)
}

// Delegate implements EnergyProvider. It never sets ConfirmedAt -- that
// is deliberately not this interface's job, see EnergyProvider's own
// doc comment.
func (m *MockProvider) Delegate(ctx context.Context, target string, units int64, duration time.Duration) (Delegation, error) {
	m.mu.Lock()
	forceTimeout := m.forceTimeout
	forceMalformed := m.forceMalformed
	forcedPrice := m.forcedPrice
	m.mu.Unlock()

	if forceTimeout {
		<-ctx.Done()
		return Delegation{}, ctx.Err()
	}
	if forceMalformed {
		return Delegation{}, fmt.Errorf("mock provider %s: %w", m.name, ErrMalformedResponse)
	}
	if err := ctx.Err(); err != nil {
		return Delegation{}, err
	}

	price := m.nextPrice(forcedPrice)
	cost, err := money.ParseDecimal(fmt.Sprintf("%.6f", price*float64(units)/1_000000))
	if err != nil {
		return Delegation{}, fmt.Errorf("mock provider %s: computing cost: %w", m.name, err)
	}

	m.mu.Lock()
	m.delegationSeq++
	seq := m.delegationSeq
	m.mu.Unlock()

	return Delegation{
		ID:            fmt.Sprintf("mock-%s-%d", m.name, seq),
		ProviderName:  m.name,
		TargetAddress: target,
		EnergyUnits:   units,
		CostTRX:       cost,
		RequestedAt:   time.Now().UTC(),
		ConfirmedAt:   nil,
	}, nil
}
