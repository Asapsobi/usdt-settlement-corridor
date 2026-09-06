package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

// ErrMalformedResponse is returned by MockProvider.Screen for an address
// configured via ForceMalformed. It exists so a caller can distinguish
// "the vendor sent garbage" from every other error shape, the same way a
// real vendor client would need a typed error for a response it can't
// parse -- never silently coerced into a zero-value Verdict, which would
// read as a clean pass to anything downstream that only checks err == nil
// carelessly.
var ErrMalformedResponse = errors.New("provider: malformed response")

// highRiskThreshold is the deterministic-hash cutoff above which an
// address not explicitly forced is still classified Flagged. Exists so
// tests can exercise "naturally flagged" without every hash needing to be
// force-configured by hand.
const highRiskThreshold = 0.95

// MockProvider is a deterministic, seeded ScreeningProvider: the only
// implementation this chunk ships, and the only one C3.9's replay
// harness exercises against (real-vendor integration is a later swap-in
// behind the same ScreeningProvider interface, not a prerequisite for C3
// shipping its own gate).
//
// Determinism is by (seed, address) content hash, not by consuming a
// shared PRNG stream per call -- call order and concurrency never affect
// the result, and the same address always yields the same verdict for
// the life of one MockProvider.
type MockProvider struct {
	seed int64

	mu         sync.RWMutex
	flagged    map[string]bool
	timeout    map[string]bool
	malformed  map[string]bool
	screenCall map[string]int
}

// NewMockProvider returns a MockProvider seeded with seed. Two
// MockProviders constructed with the same seed produce identical
// verdicts for identical addresses.
func NewMockProvider(seed int64) *MockProvider {
	return &MockProvider{
		seed:       seed,
		flagged:    make(map[string]bool),
		timeout:    make(map[string]bool),
		malformed:  make(map[string]bool),
		screenCall: make(map[string]int),
	}
}

// ScreenCallCount reports how many times Screen has been called for
// address -- exported so a caller (internal/pipeline's own tests,
// concretely) can assert a cache hit really did skip the vendor call
// entirely, not just that the outcome looked right.
func (m *MockProvider) ScreenCallCount(address string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.screenCall[address]
}

// ForceFlagged configures address to always classify as Flagged, on top
// of whatever its natural deterministic score would have been.
func (m *MockProvider) ForceFlagged(address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flagged[address] = true
}

// ForceTimeout configures address to never return -- Screen blocks until
// ctx is done and returns ctx.Err(), exactly what a hung real vendor call
// would look like to a caller that applied its own timeout.
func (m *MockProvider) ForceTimeout(address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timeout[address] = true
}

// ForceMalformed configures address to return ErrMalformedResponse and a
// zero-value Verdict, simulating a vendor response this client can't
// parse.
func (m *MockProvider) ForceMalformed(address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.malformed[address] = true
}

// Screen implements ScreeningProvider.
func (m *MockProvider) Screen(ctx context.Context, address string) (Verdict, error) {
	m.mu.Lock()
	m.screenCall[address]++
	forcedTimeout := m.timeout[address]
	forcedMalformed := m.malformed[address]
	forcedFlagged := m.flagged[address]
	m.mu.Unlock()

	if forcedTimeout {
		// Block on ctx alone -- never a fixed sleep, so a caller's
		// deadline is respected exactly, not overshot.
		<-ctx.Done()
		return Verdict{}, ctx.Err()
	}
	if forcedMalformed {
		return Verdict{}, fmt.Errorf("mock provider: address %s: %w", address, ErrMalformedResponse)
	}
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}

	score := m.deterministicScore(address)
	flagged := forcedFlagged || score >= highRiskThreshold

	var reasons []string
	switch {
	case forcedFlagged:
		reasons = []string{"mock_forced_flag"}
	case flagged:
		reasons = []string{"mock_high_risk_score"}
	}

	raw, err := json.Marshal(struct {
		Address string  `json:"address"`
		Score   float64 `json:"score"`
	}{Address: address, Score: score})
	if err != nil {
		// json.Marshal on this fixed, all-string/float struct cannot
		// realistically fail -- surfaced rather than ignored only so a
		// future field change that does break marshaling fails loudly.
		return Verdict{}, fmt.Errorf("mock provider: marshaling raw response: %w", err)
	}

	return Verdict{
		RiskScore:    score,
		Flagged:      flagged,
		ReasonCodes:  reasons,
		RawResponse:  raw,
		ProviderName: "mock",
		CheckedAt:    time.Now().UTC(),
	}, nil
}

// deterministicScore maps (seed, address) to a stable float in [0, 1)
// via FNV-1a -- content-addressed, not derived from any shared PRNG
// state, so it is safe under concurrent calls and independent of call
// order.
func (m *MockProvider) deterministicScore(address string) float64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%s", m.seed, address)
	sum := h.Sum64()
	return float64(sum) / float64(^uint64(0))
}
