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
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/signing"
	"dispatcher/internal/testledger"
)

// fakeFinalityReader is a controllable stand-in for a real TRON solidity
// node: finality for a given txid only reports true once explicitly set.
type fakeFinalityReader struct {
	mu    sync.Mutex
	final map[string]bool
}

func newFakeFinalityReader() *fakeFinalityReader {
	return &fakeFinalityReader{final: make(map[string]bool)}
}

func (f *fakeFinalityReader) SetFinal(txid string, final bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.final[txid] = final
}

func (f *fakeFinalityReader) IsFinal(ctx context.Context, tronTxID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.final[tronTxID], nil
}

// broadcastToAttempt drives a fresh order all the way through
// EnterDispatching and a real (fake-chain) Broadcast, returning the
// resulting BroadcastAttempt and the order as it stood right before
// settlement -- the fixture every ConfirmFinality test in this file
// starts from.
func broadcastToAttempt(t *testing.T, ledger *testledger.Ledger, d *dispatch.Dispatcher, externalID, customerID string) (ledgerclient.Order, dispatch.BroadcastAttempt) {
	t.Helper()
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	created := ledger.CreateOrder(externalID, customerID)
	screened := ledger.AdvanceToScreened(created)
	order, err := client.GetOrder(context.Background(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}

	if _, err := d.EnterDispatching(context.Background(), order, 7, time.Now().UTC()); err != nil {
		t.Fatalf("EnterDispatching: %v", err)
	}
	dispatching, err := client.GetOrder(context.Background(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder (dispatching): %v", err)
	}

	unsignedTx := mustUnsignedTx(t, "1.500000", int64(len(externalID)))
	chain := &fakeBroadcastClient{}
	attempt, err := d.Broadcast(context.Background(), dispatching.ID, 7, 1, unsignedTx, 100.0, chain)
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	return dispatching, attempt
}

func TestConfirmFinality_NotYetFinalReturnsFalseNoTransition(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	d := dispatch.NewDispatcher(ledgerclient.New(ledger.BaseURL(), ledger.Token()), dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)

	order, attempt := broadcastToAttempt(t, ledger, d, "order-finality-pending", "cust-finality-pending")

	chain := newFakeFinalityReader()
	final, got, err := d.ConfirmFinality(context.Background(), order, attempt.ID, time.Now().UTC(), chain)
	if err != nil {
		t.Fatalf("ConfirmFinality: %v", err)
	}
	if final {
		t.Fatal("ConfirmFinality reported final = true before finality was set")
	}
	if got.Status != dispatch.BroadcastBroadcast {
		t.Fatalf("Status = %q, want still BROADCAST", got.Status)
	}

	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	after, err := client.GetOrder(context.Background(), order.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if after.State != "dispatching" {
		t.Fatalf("order.State = %q, want still dispatching", after.State)
	}
}

func TestConfirmFinality_FinalSettlesWithCorrectE3(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)

	order, attempt := broadcastToAttempt(t, ledger, d, "order-finality-final", "cust-finality-final")

	chain := newFakeFinalityReader()
	chain.SetFinal(*attempt.TronTxID, true)

	final, got, err := d.ConfirmFinality(context.Background(), order, attempt.ID, time.Now().UTC(), chain)
	if err != nil {
		t.Fatalf("ConfirmFinality: %v", err)
	}
	if !final {
		t.Fatal("ConfirmFinality reported final = false after finality was set")
	}
	if got.Status != dispatch.BroadcastConfirmed {
		t.Fatalf("Status = %q, want CONFIRMED", got.Status)
	}

	after, err := client.GetOrder(context.Background(), order.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if after.State != "settled" {
		t.Fatalf("order.State = %q, want settled", after.State)
	}

	// §B's E3: the slot account lands at exactly -amount_out -- the
	// order's own real settlement amount ($2990.70, from
	// testledger.CreateOrder's fixed fixture), NOT whatever arbitrary
	// amount this test used to build the fake unsigned tx (E3 is built
	// from order.AmountOut alone, never from the broadcast transaction's
	// own content). (The customer's TRC20 liability also moves, but its
	// own net balance after E2 depends on that entry's amounts too --
	// checking the slot account alone is sufficient proof this is really
	// the E3 shape, and C1 already guarantees each entry's own lines
	// balance to zero.)
	slotAccount := "asset:tron:slot:7"
	if got, want := ledger.AccountBalance(slotAccount), int64(-2990700000); got != want {
		t.Fatalf("%s balance = %d, want %d", slotAccount, got, want)
	}
}

// TestConfirmFinality_ReplayIsIdempotent covers the acceptance criterion
// directly: replaying ConfirmFinality for an already-settled attempt
// hits C1's idempotency path -- no duplicate E3.
func TestConfirmFinality_ReplayIsIdempotent(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)

	order, attempt := broadcastToAttempt(t, ledger, d, "order-finality-replay", "cust-finality-replay")

	chain := newFakeFinalityReader()
	chain.SetFinal(*attempt.TronTxID, true)

	occurredAt := time.Now().UTC()
	if _, _, err := d.ConfirmFinality(context.Background(), order, attempt.ID, occurredAt, chain); err != nil {
		t.Fatalf("ConfirmFinality (1st): %v", err)
	}

	final, got, err := d.ConfirmFinality(context.Background(), order, attempt.ID, occurredAt, chain)
	if err != nil {
		t.Fatalf("ConfirmFinality (2nd, replay): %v", err)
	}
	if !final || got.Status != dispatch.BroadcastConfirmed {
		t.Fatalf("ConfirmFinality (2nd) = final=%v status=%s, want final=true status=CONFIRMED", final, got.Status)
	}

	settleKey := fmt.Sprintf("dispatcher:settle:%d:%d", order.ID, attempt.AttemptNumber)
	if got := ledger.JournalEntryCount(settleKey); got != 1 {
		t.Fatalf("JournalEntryCount(%q) = %d, want exactly 1", settleKey, got)
	}
}

func TestConfirmFinality_StaleCeilingReturnsErrFinalityStale(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
	d.FinalityStaleCeiling = 1 * time.Millisecond

	order, attempt := broadcastToAttempt(t, ledger, d, "order-finality-stale", "cust-finality-stale")
	time.Sleep(20 * time.Millisecond)

	chain := newFakeFinalityReader() // never final
	final, _, err := d.ConfirmFinality(context.Background(), order, attempt.ID, time.Now().UTC(), chain)
	if final {
		t.Fatal("ConfirmFinality reported final = true, want false")
	}
	if !errors.Is(err, dispatch.ErrFinalityStale) {
		t.Fatalf("ConfirmFinality error = %v, want ErrFinalityStale", err)
	}
}

func TestConfirmFinality_AlreadyConfirmedShortCircuitsLocally(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	signer := signing.NewFakeSigningService()
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)

	order, attempt := broadcastToAttempt(t, ledger, d, "order-finality-shortcircuit", "cust-finality-shortcircuit")

	chain := newFakeFinalityReader()
	chain.SetFinal(*attempt.TronTxID, true)
	if _, _, err := d.ConfirmFinality(context.Background(), order, attempt.ID, time.Now().UTC(), chain); err != nil {
		t.Fatalf("ConfirmFinality (1st): %v", err)
	}

	// A finality reader that would error if actually queried again --
	// proves the already-CONFIRMED short-circuit never calls IsFinal.
	final, got, err := d.ConfirmFinality(context.Background(), order, attempt.ID, time.Now().UTC(), erroringFinalityReader{})
	if err != nil {
		t.Fatalf("ConfirmFinality (2nd, should short-circuit): %v", err)
	}
	if !final || got.Status != dispatch.BroadcastConfirmed {
		t.Fatalf("ConfirmFinality (2nd) = final=%v status=%s, want final=true status=CONFIRMED", final, got.Status)
	}
}

type erroringFinalityReader struct{}

func (erroringFinalityReader) IsFinal(ctx context.Context, tronTxID string) (bool, error) {
	return false, fmt.Errorf("erroringFinalityReader: IsFinal must not be called for an already-CONFIRMED attempt")
}
