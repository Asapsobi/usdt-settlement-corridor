package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMockProvider_SameSeedYieldsSameQuoteSequence(t *testing.T) {
	ctx := context.Background()
	const n = 20

	a := NewMockProvider(Tronsell, 42, 24.0)
	b := NewMockProvider(Tronsell, 42, 24.0)

	for i := 0; i < n; i++ {
		qa, err := a.Quote(ctx)
		if err != nil {
			t.Fatalf("a.Quote() call %d: %v", i, err)
		}
		qb, err := b.Quote(ctx)
		if err != nil {
			t.Fatalf("b.Quote() call %d: %v", i, err)
		}
		if qa.PricePerUnitSun != qb.PricePerUnitSun {
			t.Fatalf("call %d: prices diverged: %v vs %v", i, qa.PricePerUnitSun, qb.PricePerUnitSun)
		}
	}
}

func TestMockProvider_DifferentSeedsYieldDifferentSequences(t *testing.T) {
	ctx := context.Background()
	a := NewMockProvider(Tronsell, 1, 24.0)
	b := NewMockProvider(Tronsell, 2, 24.0)

	var identical = true
	for i := 0; i < 20; i++ {
		qa, err := a.Quote(ctx)
		if err != nil {
			t.Fatalf("a.Quote(): %v", err)
		}
		qb, err := b.Quote(ctx)
		if err != nil {
			t.Fatalf("b.Quote(): %v", err)
		}
		if qa.PricePerUnitSun != qb.PricePerUnitSun {
			identical = false
			break
		}
	}
	if identical {
		t.Fatal("two MockProviders with different seeds produced an identical 20-call quote sequence -- seeding is not actually varying the sequence")
	}
}

func TestMockProvider_ForceTimeoutRespectsContextDeadline(t *testing.T) {
	m := NewMockProvider(Netts, 1, 24.0)
	m.ForceTimeout()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := m.Quote(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Quote() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Quote() took %v to respect a 30ms deadline -- ForceTimeout must block on ctx alone, never a fixed sleep past it", elapsed)
	}
}

func TestMockProvider_ForceTimeout_AlsoAppliesToDelegate(t *testing.T) {
	m := NewMockProvider(Catfee, 1, 24.0)
	m.ForceTimeout()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := m.Delegate(ctx, "TFakeTargetAddress000000000000001", 1000, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Delegate() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestMockProvider_ForceMalformed_IsATypedError(t *testing.T) {
	m := NewMockProvider(Tronsell, 1, 24.0)
	m.ForceMalformed()

	q, err := m.Quote(context.Background())
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("Quote() error = %v, want ErrMalformedResponse", err)
	}
	if q != (Quote{}) {
		t.Fatalf("Quote() = %+v on a malformed response, want the zero value -- never a partially-valid Quote", q)
	}

	d, err := m.Delegate(context.Background(), "TFakeTargetAddress000000000000001", 1000, time.Hour)
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("Delegate() error = %v, want ErrMalformedResponse", err)
	}
	if d != (Delegation{}) {
		t.Fatalf("Delegate() = %+v on a malformed response, want the zero value -- never a partially-valid Delegation", d)
	}
}

func TestMockProvider_ForcePartialFill(t *testing.T) {
	m := NewMockProvider(Tronsell, 1, 24.0)
	m.ForcePartialFill(500)

	q, err := m.Quote(context.Background())
	if err != nil {
		t.Fatalf("Quote(): %v", err)
	}
	if q.MaxUnitsAvailable != 500 {
		t.Fatalf("MaxUnitsAvailable = %d, want 500 (forced partial fill)", q.MaxUnitsAvailable)
	}
}

func TestMockProvider_ForcePrice_CanExceedAnyCeiling(t *testing.T) {
	m := NewMockProvider(Tronsell, 1, 24.0)
	m.ForcePrice(9999.0) // no ceiling exists in this package -- routing (a later chunk) owns rejecting this

	q, err := m.Quote(context.Background())
	if err != nil {
		t.Fatalf("Quote(): %v", err)
	}
	if q.PricePerUnitSun != 9999.0 {
		t.Fatalf("PricePerUnitSun = %v, want the forced 9999.0", q.PricePerUnitSun)
	}
}

func TestMockProvider_DelegateNeverSetsConfirmedAt(t *testing.T) {
	m := NewMockProvider(Tronsell, 1, 24.0)
	d, err := m.Delegate(context.Background(), "TFakeTargetAddress000000000000001", 1000, time.Hour)
	if err != nil {
		t.Fatalf("Delegate(): %v", err)
	}
	if d.ConfirmedAt != nil {
		t.Fatalf("ConfirmedAt = %v, want nil -- on-chain verification is not this interface's job", d.ConfirmedAt)
	}
	if d.ID == "" {
		t.Fatal("Delegation.ID is empty")
	}
}

func TestMockProvider_RedelegateRetargetsAtZeroCost(t *testing.T) {
	m := NewMockProvider(Tronsell, 1, 24.0)
	d, err := m.Redelegate(context.Background(), "mock-tronsell-1", "TNewSlotAddress0000000000000001", 1000)
	if err != nil {
		t.Fatalf("Redelegate(): %v", err)
	}
	if d.TargetAddress != "TNewSlotAddress0000000000000001" {
		t.Fatalf("TargetAddress = %q, want the new target", d.TargetAddress)
	}
	if d.EnergyUnits != 1000 {
		t.Fatalf("EnergyUnits = %d, want 1000", d.EnergyUnits)
	}
	if d.CostTRX != 0 {
		t.Fatalf("CostTRX = %v, want 0 -- retargeting already-paid-for capacity costs nothing further", d.CostTRX)
	}
	if d.ConfirmedAt != nil {
		t.Fatal("ConfirmedAt must be nil -- on-chain verification is not this interface's job")
	}
}

func TestMockProvider_RedelegateRespectsForcedTimeoutAndMalformed(t *testing.T) {
	m := NewMockProvider(Tronsell, 1, 24.0)
	m.ForceTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.Redelegate(ctx, "d1", "TNew0000000000000000000000000001", 1000); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Redelegate() error = %v, want context.DeadlineExceeded", err)
	}

	m2 := NewMockProvider(Netts, 1, 28.0)
	m2.ForceMalformed()
	if _, err := m2.Redelegate(context.Background(), "d1", "TNew0000000000000000000000000001", 1000); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("Redelegate() error = %v, want ErrMalformedResponse", err)
	}
}
