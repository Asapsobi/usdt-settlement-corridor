package slots_test

import (
	"context"
	"testing"
	"time"

	"dispatcher/internal/slots"
)

type fakeFreezeChecker struct {
	blacklisted map[string]bool
}

func (f fakeFreezeChecker) IsBlackListed(ctx context.Context, tronAddress string) (bool, error) {
	return f.blacklisted[tronAddress], nil
}

func TestDetectFreeze_ReportsBlacklistedSlot(t *testing.T) {
	slot := slots.Slot{ID: 1, TronAddress: "TFrozenAddress0000000000000000001", Status: slots.StatusActive, ActivatedAt: time.Now()}
	chain := fakeFreezeChecker{blacklisted: map[string]bool{"TFrozenAddress0000000000000000001": true}}

	frozen, err := slots.DetectFreeze(context.Background(), slot, chain)
	if err != nil {
		t.Fatalf("DetectFreeze: %v", err)
	}
	if !frozen {
		t.Fatal("DetectFreeze = false, want true")
	}
}

func TestDetectFreeze_ReportsHealthySlot(t *testing.T) {
	slot := slots.Slot{ID: 1, TronAddress: "THealthyAddress000000000000000001", Status: slots.StatusActive, ActivatedAt: time.Now()}
	chain := fakeFreezeChecker{blacklisted: map[string]bool{}}

	frozen, err := slots.DetectFreeze(context.Background(), slot, chain)
	if err != nil {
		t.Fatalf("DetectFreeze: %v", err)
	}
	if frozen {
		t.Fatal("DetectFreeze = true, want false")
	}
}
