package chain

import "time"

// health is one provider's rolling outcome record. Guarded by Pool.mu
// rather than its own lock -- every update happens from within a round
// Pool already serializes access for, so a second lock here would only
// add contention without adding safety.
type health struct {
	consecutiveFailures int
	totalRounds         int
	totalFailures       int
	lastError           string
	lastSuccessAt       time.Time
}

// ProviderHealth is the read-only snapshot GET /system/providers will
// eventually serve (C2.9's job -- this chunk only tracks and exposes the
// data, modeled after C1.7's GET /system/invariants: make the health of
// the thing visible, don't wait for an operator to go looking for it).
type ProviderHealth struct {
	Name                string
	Healthy             bool
	ConsecutiveFailures int
	TotalRounds         int
	TotalFailures       int
	LastError           string
	LastSuccessAt       time.Time
}

// recordSuccess resets a provider's consecutive-failure streak. Called
// once per round for every provider that agreed with the winning group,
// from LatestFinalized and LogsAt alike -- both are "rounds" in the sense
// that matters here: did this provider's answer this time match its
// peers'.
func (p *Pool) recordSuccess(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.health[name]
	h.consecutiveFailures = 0
	h.totalRounds++
	h.lastSuccessAt = time.Now()
}

// recordFailure increments a provider's consecutive-failure streak.
// Called for a provider that timed out, errored, or reported a result
// that disagreed with the round's winning group -- all three count
// identically toward the streak that eventually flags it unhealthy,
// matching the spec's own framing ("a provider that has disagreed OR
// failed").
func (p *Pool) recordFailure(name string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.health[name]
	h.consecutiveFailures++
	h.totalRounds++
	h.totalFailures++
	if err != nil {
		h.lastError = err.Error()
	}
}

// ProviderHealthSnapshot returns the current health of every configured
// provider, in configuration order, for a deterministic response.
func (p *Pool) ProviderHealthSnapshot() []ProviderHealth {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ProviderHealth, 0, len(p.providers))
	for _, prov := range p.providers {
		h := p.health[prov.Name]
		out = append(out, ProviderHealth{
			Name:                prov.Name,
			Healthy:             h.consecutiveFailures < p.unhealthyAfter,
			ConsecutiveFailures: h.consecutiveFailures,
			TotalRounds:         h.totalRounds,
			TotalFailures:       h.totalFailures,
			LastError:           h.lastError,
			LastSuccessAt:       h.lastSuccessAt,
		})
	}
	return out
}
