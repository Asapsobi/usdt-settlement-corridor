package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestParseOutagePolicy(t *testing.T) {
	tests := []struct {
		in      string
		want    OutagePolicy
		wantErr bool
	}{
		{"", FailClosed, false},
		{"fail_closed", FailClosed, false},
		{"fail_open", FailOpen, false},
		{"FAIL_OPEN", FailClosed, true},
		{"bogus", FailClosed, true},
	}
	for _, tt := range tests {
		got, err := ParseOutagePolicy(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseOutagePolicy(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseOutagePolicy(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestOutagePolicy_StringRoundTrip(t *testing.T) {
	for _, p := range []OutagePolicy{FailClosed, FailOpen} {
		parsed, err := ParseOutagePolicy(p.String())
		if err != nil {
			t.Fatalf("ParseOutagePolicy(%q): %v", p.String(), err)
		}
		if parsed != p {
			t.Fatalf("round-trip: %v -> %q -> %v", p, p.String(), parsed)
		}
	}
}

// alwaysFailProvider is a ScreeningProvider whose every call fails
// (after a tiny delay, to exercise the per-attempt timeout without
// slowing the test suite down).
type alwaysFailProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *alwaysFailProvider) Screen(ctx context.Context, address string) (Verdict, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return Verdict{}, ctx.Err()
	case <-time.After(5 * time.Millisecond):
		return Verdict{}, errors.New("alwaysFailProvider: deliberate failure")
	}
}

func (p *alwaysFailProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// flakyThenSucceedProvider fails its first N calls, then succeeds with a
// fixed, recognizable Verdict on every call after that.
type flakyThenSucceedProvider struct {
	mu           sync.Mutex
	failuresLeft int
	calls        int
}

func (p *flakyThenSucceedProvider) Screen(ctx context.Context, address string) (Verdict, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.failuresLeft > 0 {
		p.failuresLeft--
		return Verdict{}, errors.New("flakyThenSucceedProvider: still failing")
	}
	return Verdict{RiskScore: 0.01, Flagged: false, ProviderName: "flaky", CheckedAt: time.Now().UTC()}, nil
}

func TestScreenWithPolicy_FailClosed_ExhaustedReturnsErrorAndTaggedVerdict(t *testing.T) {
	prov := &alwaysFailProvider{}
	v, err := ScreenWithPolicy(context.Background(), prov, "0xAddr", 10*time.Millisecond, 3, FailClosed)
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want a wrapped ErrProviderUnavailable", err)
	}
	if !IsProviderUnavailable(v) {
		t.Fatalf("IsProviderUnavailable(%+v) = false, want true", v)
	}
	if prov.callCount() != 3 {
		t.Fatalf("provider called %d times, want exactly 3 (retries)", prov.callCount())
	}
}

func TestScreenWithPolicy_FailOpen_ExhaustedReturnsNilErrorAndTaggedVerdict(t *testing.T) {
	prov := &alwaysFailProvider{}
	v, err := ScreenWithPolicy(context.Background(), prov, "0xAddr", 10*time.Millisecond, 3, FailOpen)
	if err != nil {
		t.Fatalf("err = %v, want nil (FailOpen never errors on exhaustion)", err)
	}
	if !IsProviderUnavailable(v) {
		t.Fatalf("IsProviderUnavailable(%+v) = false, want true", v)
	}
	if prov.callCount() != 3 {
		t.Fatalf("provider called %d times, want exactly 3 (retries)", prov.callCount())
	}
}

// TestScreenWithPolicy_SucceedsOnLastRetryUsesRealVerdict is the build
// spec's own named acceptance criterion: a provider that fails N-1 times
// then succeeds on the last retry uses the real verdict from that
// success, not the outage path -- for BOTH policies, since outage
// handling should never even be consulted when an attempt succeeds.
func TestScreenWithPolicy_SucceedsOnLastRetryUsesRealVerdict(t *testing.T) {
	for _, policy := range []OutagePolicy{FailClosed, FailOpen} {
		t.Run(policy.String(), func(t *testing.T) {
			prov := &flakyThenSucceedProvider{failuresLeft: 2}
			v, err := ScreenWithPolicy(context.Background(), prov, "0xAddr", time.Second, 3, policy)
			if err != nil {
				t.Fatalf("ScreenWithPolicy: %v", err)
			}
			if IsProviderUnavailable(v) {
				t.Fatalf("got the outage placeholder verdict, want the real one from the successful attempt: %+v", v)
			}
			if v.RiskScore != 0.01 || v.ProviderName != "flaky" {
				t.Fatalf("v = %+v, want the real verdict {RiskScore: 0.01, ProviderName: flaky}", v)
			}
			if prov.calls != 3 {
				t.Fatalf("provider called %d times, want exactly 3 (2 failures + the succeeding attempt)", prov.calls)
			}
		})
	}
}

func TestScreenWithPolicy_ContextCancelledDuringBackoffStopsRetrying(t *testing.T) {
	prov := &alwaysFailProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond) // after the 1st attempt fails, during backoff before the 2nd
		cancel()
	}()

	_, err := ScreenWithPolicy(ctx, prov, "0xAddr", 10*time.Millisecond, 10, FailClosed)
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want a wrapped ErrProviderUnavailable", err)
	}
	if prov.callCount() >= 10 {
		t.Fatalf("provider called %d times, want fewer than the full 10 retries (context cancellation should cut backoff short)", prov.callCount())
	}
}

func TestScreenWithPolicy_RetriesLessThanOneMeansAtLeastOneAttempt(t *testing.T) {
	prov := &alwaysFailProvider{}
	_, err := ScreenWithPolicy(context.Background(), prov, "0xAddr", 10*time.Millisecond, 0, FailClosed)
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("err = %v, want a wrapped ErrProviderUnavailable", err)
	}
	if prov.callCount() != 1 {
		t.Fatalf("provider called %d times, want exactly 1 (retries<=0 treated as 1)", prov.callCount())
	}
}
