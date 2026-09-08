//go:build integration

package dispatch_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/signing"
	"dispatcher/internal/testledger"
)

// cutRealBatch drives n orders through AccumulateForBatch and CutBatch,
// returning the resulting Batch and its own persisted unsignedTx bytes
// (decoded from batches.unsigned_tx, the same way a real caller resuming
// after a crash would read them back rather than reconstructing them
// independently) -- the fixture every BroadcastBatch test in this file
// starts from.
func cutRealBatch(t *testing.T, ledger *testledger.Ledger, d *dispatch.Dispatcher, client *ledgerclient.Client, namePrefix string, n int, slotID int) (dispatch.Batch, []byte) {
	t.Helper()
	for i := 0; i < n; i++ {
		created := ledger.CreateOrder(fmt.Sprintf("%s-%d", namePrefix, i), fmt.Sprintf("%s-cust-%d", namePrefix, i))
		screened := ledger.AdvanceToScreened(created)
		order, err := client.GetOrder(context.Background(), screened.ExternalID)
		if err != nil {
			t.Fatalf("GetOrder: %v", err)
		}
		if err := d.AccumulateForBatch(context.Background(), order, slotID, time.Now().UTC()); err != nil {
			t.Fatalf("AccumulateForBatch (%d): %v", i, err)
		}
	}

	window := dispatch.BatchWindow{
		MaxWait: time.Hour, MaxSize: n,
		SlotID: slotID, SlotAddress: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH",
		MultisendContractAddress:   multisendContractAddress,
		EnergyPerRecipientEstimate: 35000,
		EnergyDeadline:             time.Now().Add(time.Minute),
	}
	batch, err := d.CutBatch(context.Background(), window, &fakeEnergyReserver{})
	if err != nil {
		t.Fatalf("CutBatch: %v", err)
	}
	if batch.UnsignedTx == nil {
		t.Fatal("CutBatch's own batch row has no unsigned_tx stored")
	}
	unsignedTx, err := hex.DecodeString(*batch.UnsignedTx)
	if err != nil {
		t.Fatalf("decoding stored unsigned_tx: %v", err)
	}
	return batch, unsignedTx
}

func TestBroadcastBatch_HappyPath(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	d.Batches = dispatch.NewBatchStore(pool)

	batch, unsignedTx := cutRealBatch(t, ledger, d, client, "batch-broadcast-happy", 3, 11)

	chain := &fakeBroadcastClient{}
	broadcast, err := d.BroadcastBatch(context.Background(), batch.ID, unsignedTx, 100.0, chain)
	if err != nil {
		t.Fatalf("BroadcastBatch: %v", err)
	}
	if broadcast.Status != dispatch.BatchBroadcast {
		t.Fatalf("Status = %q, want BROADCAST", broadcast.Status)
	}
	if broadcast.TronTxID == nil || *broadcast.TronTxID == "" {
		t.Fatal("TronTxID is empty")
	}
	if broadcast.SignedTx == nil || *broadcast.SignedTx == "" {
		t.Fatal("SignedTx is empty")
	}
	if chain.callCount() != 1 {
		t.Fatalf("chain.callCount() = %d, want 1", chain.callCount())
	}
}

// TestBroadcastBatch_ResumeAfterCrashBetweenSignedAndBroadcastNeverResigns
// mirrors broadcast_integration_test.go's own single-order crash-recovery
// test: a first BroadcastBatch call reaches SIGNED (committed) but never
// reaches BROADCAST (a failing chain client stands in for the process
// dying before observing any broadcast response). A resumed call, now
// given a working chain client, must broadcast using the SAME stored
// signature.
func TestBroadcastBatch_ResumeAfterCrashBetweenSignedAndBroadcastNeverResigns(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	d.Batches = dispatch.NewBatchStore(pool)

	batch, unsignedTx := cutRealBatch(t, ledger, d, client, "batch-broadcast-crash", 3, 12)

	failingChain := &alwaysFailChain{}
	_, err := d.BroadcastBatch(context.Background(), batch.ID, unsignedTx, 100.0, failingChain)
	if err == nil {
		t.Fatal("BroadcastBatch (1st, with a failing chain client): want an error, got nil")
	}
	if failingChain.callCount() != 1 {
		t.Fatalf("failingChain.callCount() = %d, want 1", failingChain.callCount())
	}
	if got := signer.RequestSignatureCallCount(); got != 1 {
		t.Fatalf("RequestSignature call count after 1st BroadcastBatch = %d, want 1", got)
	}

	workingChain := &fakeBroadcastClient{}
	broadcast, err := d.BroadcastBatch(context.Background(), batch.ID, unsignedTx, 100.0, workingChain)
	if err != nil {
		t.Fatalf("BroadcastBatch (2nd, resumed): %v", err)
	}
	if broadcast.Status != dispatch.BatchBroadcast {
		t.Fatalf("Status = %q, want BROADCAST", broadcast.Status)
	}
	if workingChain.callCount() != 1 {
		t.Fatalf("workingChain.callCount() = %d, want 1", workingChain.callCount())
	}
	if got := signer.RequestSignatureCallCount(); got != 1 {
		t.Fatalf("RequestSignature call count after resumed BroadcastBatch = %d, want still 1 (never re-signed)", got)
	}
}

func TestBroadcastBatch_AlreadyBroadcastIsANoOp(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	d.Batches = dispatch.NewBatchStore(pool)

	batch, unsignedTx := cutRealBatch(t, ledger, d, client, "batch-broadcast-noop", 2, 13)

	chain := &fakeBroadcastClient{}
	first, err := d.BroadcastBatch(context.Background(), batch.ID, unsignedTx, 100.0, chain)
	if err != nil {
		t.Fatalf("BroadcastBatch (1st): %v", err)
	}

	second, err := d.BroadcastBatch(context.Background(), batch.ID, unsignedTx, 100.0, chain)
	if err != nil {
		t.Fatalf("BroadcastBatch (2nd, already BROADCAST): %v", err)
	}
	if second.TronTxID == nil || first.TronTxID == nil || *second.TronTxID != *first.TronTxID {
		t.Fatalf("TronTxID changed between calls: 1st=%v 2nd=%v", first.TronTxID, second.TronTxID)
	}
	if chain.callCount() != 1 {
		t.Fatalf("chain.callCount() = %d, want still 1 (never re-broadcast)", chain.callCount())
	}
}
