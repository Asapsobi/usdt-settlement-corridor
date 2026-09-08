//go:build integration

package dispatch_test

import (
	"context"
	"testing"
	"time"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/signing"
	"dispatcher/internal/slots"
)

// panicIfCalledLedgerClient satisfies dispatch.LedgerClient but fails the
// test the moment any method is called -- C5.9's own acceptance
// criterion for the post-broadcast freeze case is "NO automatic C1
// transition is attempted in either direction," proven structurally
// here rather than merely inferred from HandleMidFlightFreeze's return
// value.
type panicIfCalledLedgerClient struct {
	t *testing.T
}

func (p panicIfCalledLedgerClient) GetOrder(ctx context.Context, externalID string) (ledgerclient.Order, error) {
	p.t.Fatal("GetOrder was called; HandleMidFlightFreeze must never touch C1")
	return ledgerclient.Order{}, nil
}

func (p panicIfCalledLedgerClient) EnsureAccount(ctx context.Context, code string, accountType ledgerclient.AccountType, asset string, idempotencyKey string) error {
	p.t.Fatal("EnsureAccount was called; HandleMidFlightFreeze must never touch C1")
	return nil
}

func (p panicIfCalledLedgerClient) TransitionWithEntry(ctx context.Context, externalID, toState string, expectedVersion int32, reason, entryType string, occurredAt time.Time, lines []ledgerclient.EntryLine, idempotencyKey string) (ledgerclient.Order, error) {
	p.t.Fatal("TransitionWithEntry was called; HandleMidFlightFreeze must never touch C1")
	return ledgerclient.Order{}, nil
}

func TestHandleMidFlightFreeze_PreBroadcastAbandonsAndRetiresSlot(t *testing.T) {
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(panicIfCalledLedgerClient{t: t}, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	slotStore := slots.NewStore(pool)

	if _, err := slotStore.Create(context.Background(), 1, "TFreezeTestSlot0000000000000001", time.Now().UTC()); err != nil {
		t.Fatalf("slots.Create: %v", err)
	}
	attempt, err := dispatch.NewAttemptStore(pool).Create(context.Background(), 100, 1, 1, hashOf([]byte("pre-broadcast-freeze-fixture")))
	if err != nil {
		t.Fatalf("AttemptStore.Create: %v", err)
	}
	if attempt.Status != dispatch.BroadcastBuilt {
		t.Fatalf("fixture attempt.Status = %q, want BUILT", attempt.Status)
	}

	needsAlert, err := d.HandleMidFlightFreeze(context.Background(), attempt.ID, slotStore)
	if err != nil {
		t.Fatalf("HandleMidFlightFreeze: %v", err)
	}
	if needsAlert {
		t.Fatal("needsAlert = true, want false (caught before broadcast, no loss, no human alert)")
	}

	after, err := dispatch.NewAttemptStore(pool).Get(context.Background(), attempt.ID)
	if err != nil {
		t.Fatalf("AttemptStore.Get: %v", err)
	}
	if after.Status != dispatch.BroadcastFailed {
		t.Fatalf("attempt.Status = %q, want FAILED", after.Status)
	}

	slot, err := slotStore.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("slots.Get: %v", err)
	}
	if slot.Status != slots.StatusRetired {
		t.Fatalf("slot.Status = %q, want RETIRED (not just RETIRING -- a frozen slot is not coming back)", slot.Status)
	}
}

// TestHandleMidFlightFreeze_PostBroadcastIsAmbiguousAndAlertsWithoutTransition
// drives a real attempt through Broadcast (a fake chain client, real
// unsigned tx, real signing via the fake signer) to reach a genuine
// BROADCAST status, then simulates the freeze being detected right
// after -- before any ConfirmFinality call ever ran.
func TestHandleMidFlightFreeze_PostBroadcastIsAmbiguousAndAlertsWithoutTransition(t *testing.T) {
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(panicIfCalledLedgerClient{t: t}, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	slotStore := slots.NewStore(pool)

	if _, err := slotStore.Create(context.Background(), 2, "TFreezeTestSlot0000000000000002", time.Now().UTC()); err != nil {
		t.Fatalf("slots.Create: %v", err)
	}

	unsignedTx := mustUnsignedTx(t, "6.000000", 600)
	chain := &fakeBroadcastClient{}
	broadcast, err := d.Broadcast(context.Background(), 101, 2, 1, unsignedTx, 100.0, chain)
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if broadcast.Status != dispatch.BroadcastBroadcast {
		t.Fatalf("fixture broadcast.Status = %q, want BROADCAST", broadcast.Status)
	}

	needsAlert, err := d.HandleMidFlightFreeze(context.Background(), broadcast.ID, slotStore)
	if err != nil {
		t.Fatalf("HandleMidFlightFreeze: %v", err)
	}
	if !needsAlert {
		t.Fatal("needsAlert = false, want true (ambiguous: did the payout land before the freeze or not)")
	}

	// The attempt itself is left exactly as it was -- still BROADCAST,
	// never auto-resolved to CONFIRMED or FAILED by guessing.
	after, err := dispatch.NewAttemptStore(pool).Get(context.Background(), broadcast.ID)
	if err != nil {
		t.Fatalf("AttemptStore.Get: %v", err)
	}
	if after.Status != dispatch.BroadcastBroadcast {
		t.Fatalf("attempt.Status = %q, want still BROADCAST (never auto-resolved)", after.Status)
	}

	slot, err := slotStore.Get(context.Background(), 2)
	if err != nil {
		t.Fatalf("slots.Get: %v", err)
	}
	if slot.Status != slots.StatusRetired {
		t.Fatalf("slot.Status = %q, want RETIRED (the freeze itself is still a fact, independent of this attempt's own ambiguity)", slot.Status)
	}
}

// TestHandleMidFlightFreeze_FailedAttemptIsANoOp covers the "already
// resolved" branch: an attempt that failed for an unrelated reason
// before any freeze was ever detected must not have its (already-retired
// or still-active) slot touched again, and must not alert -- there is
// nothing new to react to.
func TestHandleMidFlightFreeze_FailedAttemptIsANoOp(t *testing.T) {
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(panicIfCalledLedgerClient{t: t}, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	slotStore := slots.NewStore(pool)

	if _, err := slotStore.Create(context.Background(), 3, "TFreezeTestSlot0000000000000003", time.Now().UTC()); err != nil {
		t.Fatalf("slots.Create: %v", err)
	}
	attemptStore := dispatch.NewAttemptStore(pool)
	attempt, err := attemptStore.Create(context.Background(), 102, 3, 1, hashOf([]byte("already-failed-fixture")))
	if err != nil {
		t.Fatalf("AttemptStore.Create: %v", err)
	}
	if _, err := attemptStore.MarkFailed(context.Background(), attempt.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	needsAlert, err := d.HandleMidFlightFreeze(context.Background(), attempt.ID, slotStore)
	if err != nil {
		t.Fatalf("HandleMidFlightFreeze: %v", err)
	}
	if needsAlert {
		t.Fatal("needsAlert = true for an already-FAILED attempt, want false")
	}

	slot, err := slotStore.Get(context.Background(), 3)
	if err != nil {
		t.Fatalf("slots.Get: %v", err)
	}
	if slot.Status != slots.StatusActive {
		t.Fatalf("slot.Status = %q, want still ACTIVE (an already-resolved attempt's freeze report is a no-op)", slot.Status)
	}
}
