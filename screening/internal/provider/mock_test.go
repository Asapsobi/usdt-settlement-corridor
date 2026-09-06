package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMockProviderDeterministic(t *testing.T) {
	m1 := NewMockProvider(42)
	m2 := NewMockProvider(42)

	v1, err := m1.Screen(context.Background(), "0xabc")
	if err != nil {
		t.Fatalf("m1.Screen: %v", err)
	}
	v2, err := m2.Screen(context.Background(), "0xabc")
	if err != nil {
		t.Fatalf("m2.Screen: %v", err)
	}

	if v1.RiskScore != v2.RiskScore || v1.Flagged != v2.Flagged {
		t.Fatalf("same seed+address produced different verdicts: %+v vs %+v", v1, v2)
	}

	// Repeated calls against the same provider must also agree with
	// themselves -- determinism isn't just cross-instance.
	v3, err := m1.Screen(context.Background(), "0xabc")
	if err != nil {
		t.Fatalf("m1.Screen (again): %v", err)
	}
	if v1.RiskScore != v3.RiskScore || v1.Flagged != v3.Flagged {
		t.Fatalf("repeated call against same provider produced a different verdict: %+v vs %+v", v1, v3)
	}

	// A different seed is not required to agree with a different seed --
	// this just documents that the seed actually participates in the hash.
	m3 := NewMockProvider(7)
	v4, err := m3.Screen(context.Background(), "0xabc")
	if err != nil {
		t.Fatalf("m3.Screen: %v", err)
	}
	if v4.RiskScore == v1.RiskScore {
		t.Skip("different seeds happened to hash to the same score for this address -- not a failure, just uninformative")
	}
}

func TestMockProviderForceFlagged(t *testing.T) {
	m := NewMockProvider(1)
	const addr = "0xflagged"
	m.ForceFlagged(addr)

	v, err := m.Screen(context.Background(), addr)
	if err != nil {
		t.Fatalf("Screen: %v", err)
	}
	if !v.Flagged {
		t.Fatal("expected Flagged=true for a forced address")
	}
	if len(v.ReasonCodes) == 0 {
		t.Fatal("expected a non-empty reason code for a flagged verdict")
	}
}

func TestMockProviderForceTimeoutRespectsContextDeadline(t *testing.T) {
	m := NewMockProvider(1)
	const addr = "0xslow"
	m.ForceTimeout(addr)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := m.Screen(ctx, addr)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	// Generous upper bound -- this must return at (or very shortly after)
	// the deadline, never sleep past it on some unrelated fixed duration.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Screen took %v to respect a 50ms deadline -- it slept instead of watching ctx.Done()", elapsed)
	}
}

func TestMockProviderForceMalformed(t *testing.T) {
	m := NewMockProvider(1)
	const addr = "0xgarbage"
	m.ForceMalformed(addr)

	v, err := m.Screen(context.Background(), addr)
	if err == nil {
		t.Fatal("expected a non-nil error for a forced-malformed address")
	}
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("expected ErrMalformedResponse, got %v", err)
	}
	if v.RiskScore != 0 || v.Flagged || v.ReasonCodes != nil || v.RawResponse != nil || v.ProviderName != "" {
		t.Fatalf("expected a zero-value Verdict alongside the error, got %+v", v)
	}
}

func TestMockProviderConcurrentScreenIsSafe(t *testing.T) {
	m := NewMockProvider(9)
	const addr = "0xconcurrent"

	done := make(chan Verdict, 50)
	for i := 0; i < 50; i++ {
		go func() {
			v, err := m.Screen(context.Background(), addr)
			if err != nil {
				t.Errorf("concurrent Screen: %v", err)
			}
			done <- v
		}()
	}

	first := <-done
	for i := 1; i < 50; i++ {
		v := <-done
		if v.RiskScore != first.RiskScore || v.Flagged != first.Flagged {
			t.Fatalf("concurrent calls for the same address disagreed: %+v vs %+v", first, v)
		}
	}
}
