// Package ratelimit is C6.1's own per-customer request throttle: a
// classic token bucket, in-memory (this project's every component runs
// as a single process, same posture C4's own reservation locking and
// C5's own slot selection already assume -- no distributed rate-limit
// store is needed here either).
package ratelimit

import (
	"sync"
	"time"
)

// DefaultPerMinute is the rate limit a customer with no explicit
// override (customers.RateLimitPerMinute == nil) gets.
const DefaultPerMinute = 300

type bucket struct {
	tokens     float64
	lastRefill time.Time
	perMinute  int
}

// Limiter holds one bucket per customer, created lazily on first use.
type Limiter struct {
	mu      sync.Mutex
	buckets map[int64]*bucket
	now     func() time.Time // overridable in tests
}

// New returns a Limiter.
func New() *Limiter {
	return &Limiter{buckets: make(map[int64]*bucket), now: time.Now}
}

// Allow reports whether customerID may make one more request right now,
// given perMinute (nil -> DefaultPerMinute), consuming one token if so.
// A bucket's own capacity equals perMinute -- a customer can burst up to
// a full minute's allowance, then refills continuously at
// perMinute/60 tokens per second.
func (l *Limiter) Allow(customerID int64, perMinute *int) bool {
	rate := DefaultPerMinute
	if perMinute != nil {
		rate = *perMinute
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[customerID]
	now := l.now()
	if !ok {
		b = &bucket{tokens: float64(rate), lastRefill: now, perMinute: rate}
		l.buckets[customerID] = b
	} else if b.perMinute != rate {
		// A rate-limit override changed since this bucket was created
		// (an operator edited the customer row) -- resize the bucket's
		// own capacity/refill rate to match, capping tokens at the new
		// (possibly smaller) capacity rather than letting a stale,
		// larger balance carry over.
		b.perMinute = rate
		if b.tokens > float64(rate) {
			b.tokens = float64(rate)
		}
	}

	elapsed := now.Sub(b.lastRefill).Seconds()
	b.lastRefill = now
	b.tokens += elapsed * (float64(rate) / 60)
	if b.tokens > float64(rate) {
		b.tokens = float64(rate)
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// RetryAfter reports how long customerID should wait before its next
// request would likely be allowed -- one token's own refill time at its
// configured rate, for the Retry-After header C6.1's own acceptance
// criterion requires on a 429.
func RetryAfter(perMinute *int) time.Duration {
	rate := DefaultPerMinute
	if perMinute != nil {
		rate = *perMinute
	}
	if rate <= 0 {
		rate = 1
	}
	return time.Minute / time.Duration(rate)
}
