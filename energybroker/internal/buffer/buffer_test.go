package buffer

import (
	"context"
	"errors"
	"testing"
	"time"

	"energybroker/internal/provider"
)

type fakeDemandObserver struct {
	recent int64
	err    error
}

func (f fakeDemandObserver) RecentReservedUnits(ctx context.Context, window time.Duration, targetAddress string) (int64, error) {
	return f.recent, f.err
}

const testSlotAddress = "TSlot1"

// newTestBuffer builds a Buffer for TargetLevels/VerifyOnChain's own pure
// unit tests. Ceiling and SlotAddresses are defaulted here so every call
// site below doesn't need its own irrelevant Config.Ceiling/SlotAddresses
// just to satisfy NewBuffer's own validation.
func newTestBuffer(demand DemandObserver, reader TronEnergyReader, cfg Config) *Buffer {
	if cfg.Ceiling <= 0 {
		cfg.Ceiling = 1000
	}
	if len(cfg.SlotAddresses) == 0 {
		cfg.SlotAddresses = []string{testSlotAddress}
	}
	b, err := NewBuffer(nil, nil, nil, demand, reader, nil, cfg)
	if err != nil {
		panic(err) // unreachable given the defaults above; a panic here would mean this helper itself has a bug
	}
	return b
}

func TestTargetLevel_ProjectsRecentDemandAcrossLookahead(t *testing.T) {
	// 40 units over a 2h lookback is a rate of 40/7200s; projected across
	// a 30-minute (1800s) lookahead: 40 * (1800/7200) = 10.
	b := newTestBuffer(fakeDemandObserver{recent: 40}, nil, Config{
		MinimumFloor:    1,
		LookbackWindow:  2 * time.Hour,
		LookaheadWindow: 30 * time.Minute,
	})
	got, err := b.targetLevelFor(context.Background(), testSlotAddress)
	if err != nil {
		t.Fatalf("targetLevelFor: %v", err)
	}
	if got != 10 {
		t.Fatalf("targetLevelFor = %d, want 10", got)
	}
}

func TestTargetLevel_NeverBelowMinimumFloor(t *testing.T) {
	b := newTestBuffer(fakeDemandObserver{recent: 0}, nil, Config{
		MinimumFloor:    5000,
		LookbackWindow:  2 * time.Hour,
		LookaheadWindow: 30 * time.Minute,
	})
	got, err := b.targetLevelFor(context.Background(), testSlotAddress)
	if err != nil {
		t.Fatalf("targetLevelFor: %v", err)
	}
	if got != 5000 {
		t.Fatalf("targetLevelFor = %d, want the configured floor 5000 even with zero recent demand", got)
	}
}

func TestTargetLevel_DemandAboveFloorWins(t *testing.T) {
	b := newTestBuffer(fakeDemandObserver{recent: 72000}, nil, Config{ // 10 units/sec
		MinimumFloor:    1,
		LookbackWindow:  2 * time.Hour,
		LookaheadWindow: 30 * time.Minute,
	})
	got, err := b.targetLevelFor(context.Background(), testSlotAddress)
	if err != nil {
		t.Fatalf("targetLevelFor: %v", err)
	}
	if got != 18000 { // 10/sec * 1800s
		t.Fatalf("targetLevelFor = %d, want 18000 (demand-driven, above the floor)", got)
	}
}

func TestTargetLevel_PropagatesDemandObserverError(t *testing.T) {
	wantErr := errors.New("demand source unavailable")
	b := newTestBuffer(fakeDemandObserver{err: wantErr}, nil, Config{MinimumFloor: 1})
	_, err := b.targetLevelFor(context.Background(), testSlotAddress)
	if !errors.Is(err, wantErr) {
		t.Fatalf("targetLevelFor error = %v, want wrapping %v", err, wantErr)
	}
}

func TestTargetLevel_RejectsNegativeDemand(t *testing.T) {
	b := newTestBuffer(fakeDemandObserver{recent: -1}, nil, Config{MinimumFloor: 1})
	_, err := b.targetLevelFor(context.Background(), testSlotAddress)
	if err == nil {
		t.Fatal("expected an error for a negative recent-units value, got nil")
	}
}

func TestTargetLevels_OneEntryPerSlotAddress(t *testing.T) {
	b := newTestBuffer(fakeDemandObserver{recent: 0}, nil, Config{
		MinimumFloor:  50,
		SlotAddresses: []string{"TSlotA", "TSlotB", "TSlotC"},
	})
	got, err := b.TargetLevels(context.Background())
	if err != nil {
		t.Fatalf("TargetLevels: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("TargetLevels returned %d entries, want 3", len(got))
	}
	for _, addr := range []string{"TSlotA", "TSlotB", "TSlotC"} {
		if got[addr] != 50 {
			t.Fatalf("TargetLevels[%s] = %d, want the configured floor 50", addr, got[addr])
		}
	}
}

func TestVerifyOnChain_FullyPresent(t *testing.T) {
	reader := NewFakeTronReader()
	reader.SetUnits("TStaging1", "deleg-1", 5000)
	b := newTestBuffer(nil, reader, Config{})

	d := provider.Delegation{ID: "deleg-1", TargetAddress: "TStaging1", EnergyUnits: 5000}
	ok, err := b.VerifyOnChain(context.Background(), d)
	if err != nil {
		t.Fatalf("VerifyOnChain: %v", err)
	}
	if !ok {
		t.Fatal("VerifyOnChain = false, want true for a fully-present delegation")
	}
}

func TestVerifyOnChain_Absent(t *testing.T) {
	reader := NewFakeTronReader() // nothing configured -- defaults to 0, i.e. absent
	b := newTestBuffer(nil, reader, Config{})

	d := provider.Delegation{ID: "deleg-2", TargetAddress: "TStaging1", EnergyUnits: 5000}
	ok, err := b.VerifyOnChain(context.Background(), d)
	if err != nil {
		t.Fatalf("VerifyOnChain: %v", err)
	}
	if ok {
		t.Fatal("VerifyOnChain = true, want false for an absent delegation")
	}
}

func TestVerifyOnChain_PartiallyPresentIsNotConfirmed(t *testing.T) {
	reader := NewFakeTronReader()
	reader.SetUnits("TStaging1", "deleg-3", 2000) // less than the 5000 requested
	b := newTestBuffer(nil, reader, Config{})

	d := provider.Delegation{ID: "deleg-3", TargetAddress: "TStaging1", EnergyUnits: 5000}
	ok, err := b.VerifyOnChain(context.Background(), d)
	if err != nil {
		t.Fatalf("VerifyOnChain: %v", err)
	}
	if ok {
		t.Fatal("VerifyOnChain = true, want false -- a partial delegation must never be treated as the full amount")
	}
}

func TestVerifyOnChain_PropagatesReaderError(t *testing.T) {
	wantErr := errors.New("chain node unreachable")
	reader := NewFakeTronReader()
	reader.SetError("TStaging1", "deleg-4", wantErr)
	b := newTestBuffer(nil, reader, Config{})

	d := provider.Delegation{ID: "deleg-4", TargetAddress: "TStaging1", EnergyUnits: 5000}
	_, err := b.VerifyOnChain(context.Background(), d)
	if !errors.Is(err, wantErr) {
		t.Fatalf("VerifyOnChain error = %v, want wrapping %v", err, wantErr)
	}
}
