package routing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"energybroker/internal/provider"
)

// fakePriceSource is a PriceSource whose per-provider outcome is
// scripted by the test -- this chunk's own acceptance criteria are
// about SelectProvider's own filtering/renormalization logic, not about
// a real price feed, so it never touches internal/pricing's own
// database-backed Poller.
type fakePriceSource struct {
	mu     sync.Mutex
	quotes map[string]provider.Quote
	errs   map[string]error
}

func newFakePriceSource() *fakePriceSource {
	return &fakePriceSource{quotes: make(map[string]provider.Quote), errs: make(map[string]error)}
}

func (f *fakePriceSource) setPrice(name string, sun float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.errs, name)
	f.quotes[name] = provider.Quote{ProviderName: name, PricePerUnitSun: sun, QuotedAt: time.Now().UTC(), MaxUnitsAvailable: 1_000_000}
}

func (f *fakePriceSource) setUnhealthy(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.quotes, name)
	f.errs[name] = err
}

func (f *fakePriceSource) CurrentPrice(ctx context.Context, name string) (provider.Quote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errs[name]; ok {
		return provider.Quote{}, err
	}
	q, ok := f.quotes[name]
	if !ok {
		return provider.Quote{}, errors.New("fakePriceSource: no price configured")
	}
	return q, nil
}

func defaultWeights() RoutingWeights {
	return RoutingWeights{
		provider.Tronsell: 0.60,
		provider.Netts:    0.35,
		provider.Catfee:   0.05,
	}
}

const ceiling = 25.7

// TestSelectProvider_AllHealthyMatchesConfiguredWeights is this chunk's
// own 10,000-call acceptance criterion: the empirical distribution over
// all three primaries, all healthy and under ceiling, matches the
// configured weights within a documented, bounded tolerance.
func TestSelectProvider_AllHealthyMatchesConfiguredWeights(t *testing.T) {
	prices := newFakePriceSource()
	prices.setPrice(provider.Tronsell, 24.0)
	prices.setPrice(provider.Netts, 24.0)
	prices.setPrice(provider.Catfee, 24.0)

	r := NewRouter(prices, nil, 1)
	counts := runSelections(t, r, defaultWeights(), ceiling, 10000)

	assertWithinTolerance(t, counts, 10000, map[string]float64{
		provider.Tronsell: 0.60,
		provider.Netts:    0.35,
		provider.Catfee:   0.05,
	}, 0.03)
}

// TestSelectProvider_UnhealthyProviderIsRenormalizedNotZeroed is this
// chunk's own CatFee-down acceptance criterion: the Tronsell:Netts
// empirical ratio matches 60:35 renormalized between just the two of
// them, not 60:35 against a phantom 5% with nowhere to go.
func TestSelectProvider_UnhealthyProviderIsRenormalizedNotZeroed(t *testing.T) {
	prices := newFakePriceSource()
	prices.setPrice(provider.Tronsell, 24.0)
	prices.setPrice(provider.Netts, 24.0)
	prices.setUnhealthy(provider.Catfee, errors.New("catfee: simulated outage"))

	r := NewRouter(prices, nil, 2)
	counts := runSelections(t, r, defaultWeights(), ceiling, 10000)

	if counts[provider.Catfee] != 0 {
		t.Fatalf("catfee was selected %d times while unhealthy, want 0", counts[provider.Catfee])
	}
	if counts[provider.JustLendManual] != 0 {
		t.Fatalf("fallback was selected %d times even though two primaries are healthy, want 0", counts[provider.JustLendManual])
	}

	// 60:35 renormalized over just {tronsell, netts} is 60/95 : 35/95.
	assertWithinTolerance(t, counts, 10000, map[string]float64{
		provider.Tronsell: 60.0 / 95.0,
		provider.Netts:    35.0 / 95.0,
	}, 0.03)
}

// TestSelectProvider_AllUnhealthyAlwaysFallsBackToManualLadder is this
// chunk's own all-down acceptance criterion.
func TestSelectProvider_AllUnhealthyAlwaysFallsBackToManualLadder(t *testing.T) {
	prices := newFakePriceSource()
	prices.setUnhealthy(provider.Tronsell, errors.New("tronsell: simulated outage"))
	prices.setUnhealthy(provider.Netts, errors.New("netts: simulated outage"))
	prices.setUnhealthy(provider.Catfee, errors.New("catfee: simulated outage"))

	r := NewRouter(prices, nil, 3)
	for i := 0; i < 1000; i++ {
		sel, err := r.SelectProvider(context.Background(), defaultWeights(), ceiling)
		if err != nil {
			t.Fatalf("SelectProvider: %v", err)
		}
		if sel.Provider != provider.JustLendManual {
			t.Fatalf("call %d: Provider = %q, want %q", i, sel.Provider, provider.JustLendManual)
		}
		if sel.Reason != ReasonFallbackLadder {
			t.Fatalf("call %d: Reason = %q, want %q (an outage, not a price event)", i, sel.Reason, ReasonFallbackLadder)
		}
	}
	// PriceSource exposes no Delegate method at all -- routing calling it
	// is not merely undesired, it does not typecheck. This assertion
	// documents that structural guarantee rather than re-proving it.
	var _ PriceSource = prices
}

// TestSelectProvider_AllHealthyButAllOverCeilingIsDistinguishedFromOutage
// is this chunk's own market-wide-spike acceptance criterion: every
// primary DID answer, but all three are priced above a deliberately low
// test ceiling -- the fallback Selection's Reason must differ from the
// all-unhealthy case above.
func TestSelectProvider_AllHealthyButAllOverCeilingIsDistinguishedFromOutage(t *testing.T) {
	prices := newFakePriceSource()
	prices.setPrice(provider.Tronsell, 100.0)
	prices.setPrice(provider.Netts, 100.0)
	prices.setPrice(provider.Catfee, 100.0)

	r := NewRouter(prices, nil, 4)
	sel, err := r.SelectProvider(context.Background(), defaultWeights(), ceiling)
	if err != nil {
		t.Fatalf("SelectProvider: %v", err)
	}
	if sel.Provider != provider.JustLendManual {
		t.Fatalf("Provider = %q, want %q", sel.Provider, provider.JustLendManual)
	}
	if sel.Reason != ReasonManualRequired {
		t.Fatalf("Reason = %q, want %q (a price event, not an outage)", sel.Reason, ReasonManualRequired)
	}
	if sel.Reason == ReasonFallbackLadder {
		t.Fatal("a market-wide price spike must not report the same Reason as an all-providers-down outage")
	}
}

func TestSelectProvider_MixedUnhealthyAndOverCeilingStillReportsManualRequired(t *testing.T) {
	// At least one primary is reachable (over ceiling, not down) --
	// this is a pricing problem the operator can see a real number for,
	// so it should read as ReasonManualRequired, not ReasonFallbackLadder.
	prices := newFakePriceSource()
	prices.setUnhealthy(provider.Tronsell, errors.New("tronsell: simulated outage"))
	prices.setPrice(provider.Netts, 100.0)
	prices.setPrice(provider.Catfee, 100.0)

	r := NewRouter(prices, nil, 5)
	sel, err := r.SelectProvider(context.Background(), defaultWeights(), ceiling)
	if err != nil {
		t.Fatalf("SelectProvider: %v", err)
	}
	if sel.Reason != ReasonManualRequired {
		t.Fatalf("Reason = %q, want %q", sel.Reason, ReasonManualRequired)
	}
}

func TestSelectProvider_RejectsInvalidWeights(t *testing.T) {
	prices := newFakePriceSource()
	r := NewRouter(prices, nil, 1)

	tests := []struct {
		name    string
		weights RoutingWeights
	}{
		{"empty", RoutingWeights{}},
		{"negative weight", RoutingWeights{provider.Tronsell: -0.1, provider.Netts: 1.1}},
		{"zero sum", RoutingWeights{provider.Tronsell: 0, provider.Netts: 0}},
		{"unknown provider", RoutingWeights{"unknown-vendor": 1.0}},
		{"justlend_manual is never a weighted candidate", RoutingWeights{provider.JustLendManual: 1.0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.SelectProvider(context.Background(), tc.weights, ceiling)
			if !errors.Is(err, ErrInvalidRoutingWeights) {
				t.Fatalf("SelectProvider(%v) error = %v, want ErrInvalidRoutingWeights", tc.weights, err)
			}
		})
	}
}

// runSelections calls SelectProvider n times and returns the count of
// each Selection.Provider seen.
func runSelections(t *testing.T, r *Router, weights RoutingWeights, ceiling float64, n int) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for i := 0; i < n; i++ {
		sel, err := r.SelectProvider(context.Background(), weights, ceiling)
		if err != nil {
			t.Fatalf("SelectProvider (call %d): %v", i, err)
		}
		counts[sel.Provider]++
	}
	return counts
}

// assertWithinTolerance checks each expected provider's empirical share
// of n calls against its expected share, within +/- tolerance -- bounded
// and asserted, never eyeballed, per this chunk's own acceptance
// criterion. tolerance=0.03 (3 percentage points) is generous relative
// to the actual sampling noise at n=10000 for every weight this chunk's
// own config uses (even the smallest, 5%, has a standard error under
// 0.22 percentage points), so this is not a flaky test.
func assertWithinTolerance(t *testing.T, counts map[string]int, n int, expected map[string]float64, tolerance float64) {
	t.Helper()
	for name, wantShare := range expected {
		gotShare := float64(counts[name]) / float64(n)
		diff := gotShare - wantShare
		if diff < 0 {
			diff = -diff
		}
		if diff > tolerance {
			t.Errorf("provider %s: empirical share %.4f, want %.4f +/- %.4f (got %d/%d selections)",
				name, gotShare, wantShare, tolerance, counts[name], n)
		}
	}
}

// TestSelectProvider_RejectsNonPositiveCeiling is C4.7's own adversarial
// scenario: a ceiling misconfigured to zero or negative must refuse
// outright, never silently evaluate to "everything is over ceiling"
// (safe but undiagnosable) or, worse, "everything is under ceiling" (a
// broken comparison that would pay anything -- UnderCeiling's own `<=`
// can't actually do that, but this guards the call site regardless of
// how UnderCeiling itself is implemented).
func TestSelectProvider_RejectsNonPositiveCeiling(t *testing.T) {
	prices := newFakePriceSource()
	prices.setPrice(provider.Tronsell, 24.0)
	prices.setPrice(provider.Netts, 24.0)
	prices.setPrice(provider.Catfee, 24.0)
	r := NewRouter(prices, nil, 1)

	for _, badCeiling := range []float64{0, -1, -25.7} {
		_, err := r.SelectProvider(context.Background(), defaultWeights(), badCeiling)
		if !errors.Is(err, ErrInvalidCeiling) {
			t.Fatalf("SelectProvider(ceiling=%v) error = %v, want ErrInvalidCeiling", badCeiling, err)
		}
	}
}
