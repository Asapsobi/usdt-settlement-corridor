package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// OutagePolicy decides what ScreenWithPolicy reports once every retry
// against a vendor is exhausted. FailClosed is the zero value and the
// shipped default -- see "Read this third" in
// docs/03-build/c3-screening-build-prompts.md: a vendor outage must
// never silently become a pass. FailOpen exists as an explicit,
// deliberate, config-only opt-in -- never a code-level default -- for
// whoever owns AML/compliance sign-off to choose in writing, trading
// tier-SLA continuity for screening a window of orders on zero vendor
// signal.
type OutagePolicy int

const (
	FailClosed OutagePolicy = iota
	FailOpen
)

// String renders the config-file spelling of p ("fail_closed" /
// "fail_open"), the inverse of ParseOutagePolicy.
func (p OutagePolicy) String() string {
	if p == FailOpen {
		return "fail_open"
	}
	return "fail_closed"
}

// ParseOutagePolicy parses the CONFIG section's own string values --
// "fail_closed" or "fail_open" (case-sensitive, matching every other
// config value convention in this codebase). An empty string defaults
// to FailClosed, matching "default fail_closed" -- config that never
// mentions this key at all gets the safe default, not an error.
func ParseOutagePolicy(s string) (OutagePolicy, error) {
	switch s {
	case "", "fail_closed":
		return FailClosed, nil
	case "fail_open":
		return FailOpen, nil
	default:
		return FailClosed, fmt.Errorf("provider: unknown outage_policy %q, want \"fail_closed\" or \"fail_open\"", s)
	}
}

// ReasonProviderUnavailable tags a Verdict ScreenWithPolicy synthesized
// after exhausting every retry -- never a real vendor response. It is
// deliberately NOT something internal/verdict.Classify is ever run
// against: Classify has no way to distinguish a genuinely clean
// Verdict{RiskScore: 0} from this placeholder, so callers must check
// IsProviderUnavailable and route to verdict.Unavailable() or
// verdict.PassVendorUnavailable() directly instead of classifying it.
const ReasonProviderUnavailable = "provider_unavailable"

// IsProviderUnavailable reports whether v is ScreenWithPolicy's own
// exhausted-retries placeholder (the FailOpen case, where it is
// returned alongside a nil error -- see ScreenWithPolicy's own doc
// comment for why FailClosed instead signals this via ErrProviderUnavailable).
func IsProviderUnavailable(v Verdict) bool {
	for _, code := range v.ReasonCodes {
		if code == ReasonProviderUnavailable {
			return true
		}
	}
	return false
}

// ErrProviderUnavailable is returned by ScreenWithPolicy when every
// retry is exhausted under FailClosed. Wraps the last underlying
// attempt's own error via %w, so a caller that needs it can still get
// at the original cause.
var ErrProviderUnavailable = errors.New("provider: unavailable after exhausting every retry")

// DefaultBackoffBase and DefaultBackoffMax bound ScreenWithPolicy's
// between-attempt backoff: base, doubling each attempt, capped at max.
// Not configurable via ScreenWithPolicy's own parameters -- the build
// spec's signature is fixed to (ctx, provider, address, timeout,
// retries, policy) -- so these are package constants, not a Config
// field, the same way MockProvider's own thresholds aren't parameters.
const (
	DefaultBackoffBase = 200 * time.Millisecond
	DefaultBackoffMax  = 5 * time.Second
)

// ScreenWithPolicy calls prov.Screen up to retries times, each attempt
// bounded by timeout and (after the first) preceded by an exponential
// backoff sleep. The first attempt that succeeds wins outright -- its
// real Verdict is returned immediately, untouched by outage handling,
// matching the build spec's own acceptance criterion that a provider
// failing N-1 times then succeeding on the last retry uses that real
// verdict, never the outage path.
//
// Once every attempt is exhausted:
//   - FailClosed (the default): returns a non-nil error wrapping
//     ErrProviderUnavailable. Callers should treat this exactly like
//     C3.4's plain provider.Screen error case -- report
//     verdict.Unavailable(), never classify the returned Verdict (it is
//     a placeholder, tagged via ReasonProviderUnavailable, not a real
//     result).
//   - FailOpen: returns a nil error alongside that SAME tagged
//     placeholder Verdict -- callers must check IsProviderUnavailable
//     before classifying, and report verdict.PassVendorUnavailable()
//     instead of running it through Classify (which would produce a
//     plain, un-auditable ReasonPass for a check that never actually
//     happened).
//
// retries <= 0 is treated as 1 (at least one attempt is always made).
func ScreenWithPolicy(ctx context.Context, prov ScreeningProvider, address string, timeout time.Duration, retries int, policy OutagePolicy) (Verdict, error) {
	if retries <= 0 {
		retries = 1
	}

	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				lastErr = err
				break
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		v, err := prov.Screen(attemptCtx, address)
		cancel()
		if err == nil {
			return v, nil
		}
		lastErr = err
		slog.Warn("provider: screen attempt failed",
			"address", address, "attempt", attempt+1, "retries", retries, "error", err)
	}

	placeholder := Verdict{
		RiskScore:   0,
		Flagged:     false,
		ReasonCodes: []string{ReasonProviderUnavailable},
		CheckedAt:   time.Now().UTC(),
	}

	if policy == FailOpen {
		slog.Error("provider: every retry exhausted, fail_open policy in effect -- reporting a pass despite vendor unavailability",
			"address", address, "retries", retries, "last_error", lastErr)
		return placeholder, nil
	}

	slog.Error("provider: every retry exhausted, failing closed",
		"address", address, "retries", retries, "last_error", lastErr)
	return placeholder, fmt.Errorf("%w: %v", ErrProviderUnavailable, lastErr)
}

// sleepBackoff waits attempt's exponential backoff (base * 2^(attempt-1),
// capped at DefaultBackoffMax) or returns ctx's error if it's cancelled
// first -- backoff is never allowed to outlive the caller's own context.
func sleepBackoff(ctx context.Context, attempt int) error {
	delay := DefaultBackoffBase << (attempt - 1)
	if delay > DefaultBackoffMax || delay <= 0 {
		delay = DefaultBackoffMax
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}
