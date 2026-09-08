//go:build integration

package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/energy"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/signing"
)

// fakeEnergyReserver counts calls and records the units requested on
// each -- CutBatch's own "one reservation per batch, sized for the
// total" acceptance criterion is proven against these two things
// directly, not inferred.
type fakeEnergyReserver struct {
	mu    sync.Mutex
	calls []int64 // units requested, one entry per call
}

func (f *fakeEnergyReserver) Reserve(ctx context.Context, externalID, targetAddress string, units int64, tier string, deadline time.Time, idempotencyKey string) (energy.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, units)
	return energy.Reservation{ID: int64(len(f.calls)), Status: "CONFIRMED"}, nil
}

func (f *fakeEnergyReserver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const multisendContractAddress = "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj" // provisional, per txbuild's own doc comment

func TestCutBatch_OneReservationSizedForTheWholeBatch(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	d.Batches = dispatch.NewBatchStore(pool)

	const n = 5
	for i := 0; i < n; i++ {
		created := ledger.CreateOrder(fmt.Sprintf("order-batch-energy-%d", i), fmt.Sprintf("cust-batch-energy-%d", i))
		screened := ledger.AdvanceToScreened(created)
		order, err := client.GetOrder(context.Background(), screened.ExternalID)
		if err != nil {
			t.Fatalf("GetOrder: %v", err)
		}
		if err := d.AccumulateForBatch(context.Background(), order, 3, time.Now().UTC()); err != nil {
			t.Fatalf("AccumulateForBatch (%d): %v", i, err)
		}
	}

	energyClient := &fakeEnergyReserver{}
	window := dispatch.BatchWindow{
		MaxWait: time.Hour, MaxSize: n,
		SlotID: 3, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		MultisendContractAddress:   multisendContractAddress,
		EnergyPerRecipientEstimate: 35000,
		EnergyDeadline:             time.Now().Add(time.Minute),
	}

	batch, err := d.CutBatch(context.Background(), window, energyClient)
	if err != nil {
		t.Fatalf("CutBatch: %v", err)
	}
	if batch.RecipientCount != n {
		t.Fatalf("RecipientCount = %d, want %d", batch.RecipientCount, n)
	}
	if got := energyClient.callCount(); got != 1 {
		t.Fatalf("energy reservation call count = %d, want exactly 1", got)
	}
	if got, want := energyClient.calls[0], int64(35000*n); got != want {
		t.Fatalf("energy units requested = %d, want %d (sized for the whole batch, not per recipient)", got, want)
	}
}

func TestCutBatch_BelowThresholdReturnsErrBatchWindowNotReady(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	d.Batches = dispatch.NewBatchStore(pool)

	created := ledger.CreateOrder("order-batch-notready", "cust-batch-notready")
	screened := ledger.AdvanceToScreened(created)
	order, err := client.GetOrder(context.Background(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if err := d.AccumulateForBatch(context.Background(), order, 3, time.Now().UTC()); err != nil {
		t.Fatalf("AccumulateForBatch: %v", err)
	}

	window := dispatch.BatchWindow{MaxWait: time.Hour, MaxSize: 10, SlotID: 3, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", MultisendContractAddress: multisendContractAddress}
	_, err = d.CutBatch(context.Background(), window, &fakeEnergyReserver{})
	if !errors.Is(err, dispatch.ErrBatchWindowNotReady) {
		t.Fatalf("CutBatch error = %v, want ErrBatchWindowNotReady", err)
	}
}

// TestHandlePartialSettlement_PartialSuccessSettlesAndRequeues mirrors
// C1.9's own 40/50 partial-settlement scenario at a smaller, test-practical
// scale (8 recipients, 5 succeed / 3 fail is the same ~60-80% ratio):
// the successes reach `settled` with their own correct individual E3
// entries, the failures are requeued for the next window, and the batch
// itself is never represented as a single order-like entity anywhere in
// C1 (only individual orders ever transition).
func TestHandlePartialSettlement_PartialSuccessSettlesAndRequeues(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	d.Batches = dispatch.NewBatchStore(pool)

	const n = 8
	const numFail = 3
	orderIDs := make([]int64, n)
	for i := 0; i < n; i++ {
		created := ledger.CreateOrder(fmt.Sprintf("order-batch-partial-%d", i), fmt.Sprintf("cust-batch-partial-%d", i))
		screened := ledger.AdvanceToScreened(created)
		order, err := client.GetOrder(context.Background(), screened.ExternalID)
		if err != nil {
			t.Fatalf("GetOrder: %v", err)
		}
		if err := d.AccumulateForBatch(context.Background(), order, 5, time.Now().UTC()); err != nil {
			t.Fatalf("AccumulateForBatch (%d): %v", i, err)
		}
		orderIDs[i] = order.ID
	}

	window := dispatch.BatchWindow{
		MaxWait: time.Hour, MaxSize: n,
		SlotID: 5, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		MultisendContractAddress:   multisendContractAddress,
		EnergyPerRecipientEstimate: 35000,
		EnergyDeadline:             time.Now().Add(time.Minute),
	}
	batch, err := d.CutBatch(context.Background(), window, &fakeEnergyReserver{})
	if err != nil {
		t.Fatalf("CutBatch: %v", err)
	}

	succeeded := make(map[int64]bool, n)
	for i, id := range orderIDs {
		succeeded[id] = i >= numFail // first numFail fail, the rest succeed
	}

	settledCount, requeuedCount, err := d.HandlePartialSettlement(context.Background(), batch.ID, time.Now().UTC(), succeeded)
	if err != nil {
		t.Fatalf("HandlePartialSettlement: %v", err)
	}
	if settledCount != n-numFail {
		t.Fatalf("settled = %d, want %d", settledCount, n-numFail)
	}
	if requeuedCount != numFail {
		t.Fatalf("requeued = %d, want %d", requeuedCount, numFail)
	}

	for i, id := range orderIDs {
		externalID := fmt.Sprintf("order-batch-partial-%d", i)
		order, err := client.GetOrder(context.Background(), externalID)
		if err != nil {
			t.Fatalf("GetOrder (%s): %v", externalID, err)
		}
		if succeeded[id] {
			if order.State != "settled" {
				t.Fatalf("order %s State = %q, want settled", externalID, order.State)
			}
		} else {
			if order.State != "dispatching" {
				t.Fatalf("order %s State = %q, want still dispatching (requeued, not settled or held)", externalID, order.State)
			}
		}
	}

	// §B: the slot account absorbed exactly the sum of the settled
	// recipients' own amounts -- each E3 used its own amount_out
	// ($2990.70, testledger's fixed fixture), never a shared batch total.
	if got, want := ledger.AccountBalance("asset:tron:slot:5"), int64(-2990700000*(n-numFail)); got != want {
		t.Fatalf("asset:tron:slot:5 balance = %d, want %d", got, want)
	}
}
