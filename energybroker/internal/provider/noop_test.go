package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestNoOpProvider_AlwaysReturnsErrManualFallbackRequired proves the
// justlend_manual slot is reachable through the same EnergyProvider
// interface as a real vendor but never silently succeeds -- this
// chunk's own acceptance criterion.
func TestNoOpProvider_AlwaysReturnsErrManualFallbackRequired(t *testing.T) {
	var p EnergyProvider = NoOpProvider{}

	q, err := p.Quote(context.Background())
	if !errors.Is(err, ErrManualFallbackRequired) {
		t.Fatalf("Quote() error = %v, want ErrManualFallbackRequired", err)
	}
	if q != (Quote{}) {
		t.Fatalf("Quote() = %+v, want the zero value", q)
	}

	d, err := p.Delegate(context.Background(), "TFakeTargetAddress000000000000001", 1000, time.Hour)
	if !errors.Is(err, ErrManualFallbackRequired) {
		t.Fatalf("Delegate() error = %v, want ErrManualFallbackRequired", err)
	}
	if d != (Delegation{}) {
		t.Fatalf("Delegate() = %+v, want the zero value", d)
	}

	rd, err := p.Redelegate(context.Background(), "some-delegation-id", "TFakeTargetAddress000000000000001", 1000)
	if !errors.Is(err, ErrManualFallbackRequired) {
		t.Fatalf("Redelegate() error = %v, want ErrManualFallbackRequired", err)
	}
	if rd != (Delegation{}) {
		t.Fatalf("Redelegate() = %+v, want the zero value", rd)
	}
}
