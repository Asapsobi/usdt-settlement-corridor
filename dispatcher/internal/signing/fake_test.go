package signing

import (
	"context"
	"errors"
	"testing"
)

func TestFakeSigningService_IdempotentReplayReturnsTheSameRequest(t *testing.T) {
	f := NewFakeSigningService()
	first, err := f.RequestSignature(context.Background(), 1, [32]byte{1}, 5000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature (1st): %v", err)
	}
	second, err := f.RequestSignature(context.Background(), 1, [32]byte{9}, 999, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature (2nd, replayed): %v", err)
	}
	if second != first {
		t.Fatalf("replayed request = %+v, want identical to first %+v", second, first)
	}
}

func TestFakeSigningService_ForceError(t *testing.T) {
	f := NewFakeSigningService()
	wantErr := errors.New("simulated S1 outage")
	f.ForceError("idem-1", wantErr)
	if _, err := f.RequestSignature(context.Background(), 1, [32]byte{1}, 5000, "idem-1"); !errors.Is(err, wantErr) {
		t.Fatalf("RequestSignature error = %v, want %v", err, wantErr)
	}
}

func TestFakeSigningService_ForceDuplicateSignature(t *testing.T) {
	f := NewFakeSigningService()
	original, err := f.RequestSignature(context.Background(), 1, [32]byte{1}, 5000, "idem-original")
	if err != nil {
		t.Fatalf("RequestSignature (original): %v", err)
	}
	f.ForceDuplicateSignature("idem-duplicate", "idem-original")
	dup, err := f.RequestSignature(context.Background(), 1, [32]byte{2}, 5000, "idem-duplicate")
	if err != nil {
		t.Fatalf("RequestSignature (duplicate): %v", err)
	}
	if dup.SignedTx != original.SignedTx {
		t.Fatalf("forced-duplicate signature = %x, want it to match the original's %x", dup.SignedTx, original.SignedTx)
	}
	if dup.ID == original.ID {
		t.Fatal("forced-duplicate request got the SAME id as the original, want two distinct requests sharing a signature")
	}
}

func TestFakeSigningService_SlotAddress(t *testing.T) {
	f := NewFakeSigningService()
	f.SetSlotAddress(3, "TFakeSlot30000000000000000003")
	addr, err := f.SlotAddress(context.Background(), 3)
	if err != nil {
		t.Fatalf("SlotAddress: %v", err)
	}
	if addr != "TFakeSlot30000000000000000003" {
		t.Fatalf("SlotAddress = %q, want the configured address", addr)
	}
}
