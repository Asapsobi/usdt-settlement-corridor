package ratelimit_test

import (
	"testing"
	"time"

	"gateway/internal/ratelimit"
)

func TestAllow_WithinLimit(t *testing.T) {
	l := ratelimit.New()
	rate := 5
	for i := 0; i < rate; i++ {
		if !l.Allow(1, &rate) {
			t.Fatalf("Allow request %d: want true (within burst capacity)", i)
		}
	}
}

func TestAllow_ExceedsLimit(t *testing.T) {
	l := ratelimit.New()
	rate := 3
	for i := 0; i < rate; i++ {
		if !l.Allow(1, &rate) {
			t.Fatalf("Allow request %d: want true", i)
		}
	}
	if l.Allow(1, &rate) {
		t.Fatal("Allow after exhausting burst capacity: want false")
	}
}

func TestAllow_DifferentCustomersIndependent(t *testing.T) {
	l := ratelimit.New()
	rate := 1
	if !l.Allow(1, &rate) {
		t.Fatal("customer 1's first request: want true")
	}
	if l.Allow(1, &rate) {
		t.Fatal("customer 1's second request: want false (exhausted)")
	}
	if !l.Allow(2, &rate) {
		t.Fatal("customer 2's first request: want true -- independent bucket from customer 1")
	}
}

func TestAllow_DefaultRateWhenNilOverride(t *testing.T) {
	l := ratelimit.New()
	for i := 0; i < ratelimit.DefaultPerMinute; i++ {
		if !l.Allow(1, nil) {
			t.Fatalf("Allow request %d with nil override: want true (within default burst)", i)
		}
	}
	if l.Allow(1, nil) {
		t.Fatal("Allow after exhausting the default burst: want false")
	}
}

func TestRetryAfter_MatchesRefillRate(t *testing.T) {
	rate := 60
	got := ratelimit.RetryAfter(&rate)
	if got != time.Second {
		t.Fatalf("RetryAfter(60/min) = %v, want 1s", got)
	}
}
