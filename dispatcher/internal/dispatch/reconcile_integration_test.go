//go:build integration

package dispatch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/signing"
)

// hashOf mirrors dispatch's own (unexported) hashHex -- this test needs
// to simulate a FAILED broadcast attempt directly, without going through
// Broadcast itself (already covered by broadcast_integration_test.go),
// so it needs to compute the same content-address AttemptStore.Create
// expects.
func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestReconcileFailedDispatches_ReportsAndClosesHeldOrder(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)

	created := ledger.CreateOrder("order-reconcile-1", "cust-reconcile-1")
	screened := ledger.AdvanceToScreened(created)
	order, err := client.GetOrder(context.Background(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	local, err := d.EnterDispatching(context.Background(), order, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("EnterDispatching: %v", err)
	}

	// Simulate a permanently failed attempt directly.
	unsignedTx := mustUnsignedTx(t, "9.000000", 900)
	attempt, err := dispatch.NewAttemptStore(pool).Create(context.Background(), local.OrderID, 1, 1, hashOf(unsignedTx))
	if err != nil {
		t.Fatalf("AttemptStore.Create: %v", err)
	}
	if _, err := dispatch.NewAttemptStore(pool).MarkFailed(context.Background(), attempt.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	reporter := ledgerclient.NewTwoCallReporter(client)
	closed, err := d.ReconcileFailedDispatches(context.Background(), reporter, "reconciliation test: attempt permanently failed")
	if err != nil {
		t.Fatalf("ReconcileFailedDispatches: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	after, err := client.GetOrder(context.Background(), order.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder (after): %v", err)
	}
	if after.State != "held" {
		t.Fatalf("order.State = %q, want held", after.State)
	}

	localAfter, err := dispatch.NewStore(pool).Get(context.Background(), local.OrderID)
	if err != nil {
		t.Fatalf("Store.Get: %v", err)
	}
	if localAfter.Status != dispatch.StatusHeld {
		t.Fatalf("local Status = %q, want HELD", localAfter.Status)
	}
}

// TestReconcileFailedDispatches_ResumesAfterReversalWithoutDuplicate
// simulates the exact crash invariant 4 names: the reversal already
// landed (posted directly here, standing in for a first
// ReportDispatchFailure call that crashed right after it), but the order
// still shows `dispatching`. A reconciliation pass must complete the
// transition to `held` without posting a second reversal.
func TestReconcileFailedDispatches_ResumesAfterReversalWithoutDuplicate(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)

	created := ledger.CreateOrder("order-reconcile-2", "cust-reconcile-2")
	screened := ledger.AdvanceToScreened(created)
	order, err := client.GetOrder(context.Background(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	local, err := d.EnterDispatching(context.Background(), order, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("EnterDispatching: %v", err)
	}

	unsignedTx := mustUnsignedTx(t, "9.500000", 950)
	attemptStore := dispatch.NewAttemptStore(pool)
	attempt, err := attemptStore.Create(context.Background(), local.OrderID, 1, 1, hashOf(unsignedTx))
	if err != nil {
		t.Fatalf("AttemptStore.Create: %v", err)
	}
	if _, err := attemptStore.MarkFailed(context.Background(), attempt.ID); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	// Simulate the crash: post the reversal directly, as a prior,
	// crashed ReportDispatchFailure call would have, WITHOUT completing
	// the transition to held.
	entry, err := client.GetEntryByIdempotencyKey(context.Background(), local.ConversionEntryKey)
	if err != nil {
		t.Fatalf("GetEntryByIdempotencyKey: %v", err)
	}
	if _, _, err := client.PostReversal(context.Background(), entry.ID, "pre-crash reversal", time.Now().UTC()); err != nil {
		t.Fatalf("PostReversal (simulating pre-crash step): %v", err)
	}

	reporter := ledgerclient.NewTwoCallReporter(client)
	closed, err := d.ReconcileFailedDispatches(context.Background(), reporter, "reconciliation test: resumed after crash")
	if err != nil {
		t.Fatalf("ReconcileFailedDispatches: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	after, err := client.GetOrder(context.Background(), order.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder (after): %v", err)
	}
	if after.State != "held" {
		t.Fatalf("order.State = %q, want held", after.State)
	}

	if got := ledger.JournalEntryCount("ledger:reverse:" + local.ConversionEntryKey); got != 1 {
		t.Fatalf("JournalEntryCount(reversal) = %d, want exactly 1 (no duplicate reversal posted)", got)
	}
}

func TestReconcileFailedDispatches_InFlightAttemptLeftAlone(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)

	created := ledger.CreateOrder("order-reconcile-3", "cust-reconcile-3")
	screened := ledger.AdvanceToScreened(created)
	order, err := client.GetOrder(context.Background(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if _, err := d.EnterDispatching(context.Background(), order, 1, time.Now().UTC()); err != nil {
		t.Fatalf("EnterDispatching: %v", err)
	}
	// No attempt row at all yet -- still legitimately in flight.

	reporter := ledgerclient.NewTwoCallReporter(client)
	closed, err := d.ReconcileFailedDispatches(context.Background(), reporter, "should not fire")
	if err != nil {
		t.Fatalf("ReconcileFailedDispatches: %v", err)
	}
	if closed != 0 {
		t.Fatalf("closed = %d, want 0 (order has no failed attempt)", closed)
	}

	after, err := client.GetOrder(context.Background(), order.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder (after): %v", err)
	}
	if after.State != "dispatching" {
		t.Fatalf("order.State = %q, want still dispatching", after.State)
	}
}
