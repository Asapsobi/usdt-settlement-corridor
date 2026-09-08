//go:build integration

// Requires two real, reachable Postgres 16 instances: one for dispatcher's
// own database (DISPATCHER_TEST_DATABASE_URL) and one for a real ledgerd
// subprocess built from the sibling ledger module (LEDGER_TEST_DATABASE_URL);
// run via `make test-integration`.
package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/money"
	"dispatcher/internal/signing"
	"dispatcher/internal/testledger"
)

var portSeq int64 = 18538

func startLedger(t *testing.T) *testledger.Ledger {
	t.Helper()
	dbURL := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	port := atomic.AddInt64(&portSeq, 1)
	return testledger.Start(t, dbURL, fmt.Sprintf(":%d", port), "dispatch-test-token", "dispatcher")
}

func mustAmount(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return a
}

func TestEnterDispatching_ScreenedOrderEntersDispatchingWithBalancedE2(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signing.NewFakeSigningService())

	// position:corridor:* and revenue:* are global accounts shared by
	// every order this whole test binary dispatches, not just this
	// test's own -- read their balances before, and assert the DELTA
	// this one E2 entry contributed, not an absolute value (which would
	// be correct only when this test happens to run against an
	// otherwise-empty ledger, and silently wrong the moment any other
	// test's own order lands first in the same shared ledger_test2
	// database).
	beforeCorridorBEP := ledger.AccountBalance("position:corridor:USDT_BEP20")
	beforeCorridorTRC := ledger.AccountBalance("position:corridor:USDT_TRC20")
	beforeFee := ledger.AccountBalance("revenue:fee")
	beforeNetworkFee := ledger.AccountBalance("revenue:network_fee")

	created := ledger.CreateOrder("order-enter-1", "cust-enter-1")
	screened := ledger.AdvanceToScreened(created)

	order, err := client.GetOrder(t.Context(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if order.State != "screened" {
		t.Fatalf("order.State = %q, want screened", order.State)
	}

	occurredAt := time.Now().UTC()
	attempt, err := d.EnterDispatching(context.Background(), order, 3, occurredAt)
	if err != nil {
		t.Fatalf("EnterDispatching: %v", err)
	}
	if attempt.SlotID != 3 || attempt.Status != dispatch.StatusDispatching {
		t.Fatalf("attempt = %+v, want SlotID=3 Status=DISPATCHING", attempt)
	}

	after, err := client.GetOrder(t.Context(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder (after): %v", err)
	}
	if after.State != "dispatching" {
		t.Fatalf("order.State (after) = %q, want dispatching", after.State)
	}
	if after.Version != order.Version+1 {
		t.Fatalf("order.Version (after) = %d, want %d", after.Version, order.Version+1)
	}

	// §B's worked example, verified for real: both assets balance
	// independently across the E2 entry's own lines.
	bepLiability := "liability:customer:cust-enter-1:USDT_BEP20"
	trcLiability := "liability:customer:cust-enter-1:USDT_TRC20"
	if got, want := ledger.AccountBalance(bepLiability), int64(0); got != want {
		t.Fatalf("%s balance = %d, want %d (BEP20 side of E1+E2 nets to zero)", bepLiability, got, want)
	}
	if got, want := ledger.AccountBalance(trcLiability), mustAmount(t, "-2990.700000"); int64(got) != int64(want) {
		t.Fatalf("%s balance = %d, want %d", trcLiability, got, want)
	}
	if got, want := ledger.AccountBalance("position:corridor:USDT_BEP20")-beforeCorridorBEP, int64(mustAmount(t, "-3000.000000")); got != want {
		t.Fatalf("position:corridor:USDT_BEP20 delta = %d, want %d", got, want)
	}
	if got, want := ledger.AccountBalance("position:corridor:USDT_TRC20")-beforeCorridorTRC, int64(mustAmount(t, "3000.000000")); got != want {
		t.Fatalf("position:corridor:USDT_TRC20 delta = %d, want %d", got, want)
	}
	if got, want := ledger.AccountBalance("revenue:fee")-beforeFee, int64(mustAmount(t, "-7.500000")); got != want {
		t.Fatalf("revenue:fee delta = %d, want %d", got, want)
	}
	if got, want := ledger.AccountBalance("revenue:network_fee")-beforeNetworkFee, int64(mustAmount(t, "-1.800000")); got != want {
		t.Fatalf("revenue:network_fee delta = %d, want %d", got, want)
	}

	// The attempt was recorded locally too.
	local, err := dispatch.NewStore(pool).Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Store.Get: %v", err)
	}
	if local.ConversionEntryKey != fmt.Sprintf("dispatcher:enter_dispatching:%d", order.ID) {
		t.Fatalf("ConversionEntryKey = %q", local.ConversionEntryKey)
	}
}

// TestEnterDispatching_ReplayAfterCrashIsIdempotent simulates a C5
// restart between the C1 call succeeding and the local INSERT: calling
// EnterDispatching again for the same order (fresh Dispatcher, fresh
// order value re-fetched from C1 as a real crash-recovery caller would)
// must not post a second conversion entry.
func TestEnterDispatching_ReplayAfterCrashIsIdempotent(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signing.NewFakeSigningService())

	created := ledger.CreateOrder("order-replay-1", "cust-replay-1")
	screened := ledger.AdvanceToScreened(created)
	order, err := client.GetOrder(t.Context(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}

	// A real crash-recovery caller must supply the SAME occurredAt on
	// every retry of the same logical attempt (see EnterDispatching's own
	// doc comment) -- simulated here by a caller-owned fixed value rather
	// than each call computing its own time.Now().
	occurredAt := time.Now().UTC()

	if _, err := d.EnterDispatching(context.Background(), order, 1, occurredAt); err != nil {
		t.Fatalf("EnterDispatching (1st): %v", err)
	}

	// Re-fetch the order fresh, as a real crash-recovery caller would --
	// its version has already advanced to reflect the successful E2.
	refetched, err := client.GetOrder(t.Context(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder (refetch): %v", err)
	}
	if refetched.State != "dispatching" {
		t.Fatalf("refetched.State = %q, want dispatching", refetched.State)
	}

	if _, err := d.EnterDispatching(context.Background(), refetched, 1, occurredAt); err != nil {
		t.Fatalf("EnterDispatching (2nd, replay): %v", err)
	}

	conversionKey := fmt.Sprintf("dispatcher:enter_dispatching:%d", order.ID)
	if got := ledger.JournalEntryCount(conversionKey); got != 1 {
		t.Fatalf("JournalEntryCount(%q) = %d, want exactly 1", conversionKey, got)
	}
}

// TestEnterDispatching_HaltedRetriesAndSucceedsOnceCleared proves the
// halt path is a backoff-and-retry, never a failure requiring C5.7's
// dispatch-failure handling.
func TestEnterDispatching_HaltedRetriesAndSucceedsOnceCleared(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signing.NewFakeSigningService())
	d.HaltBackoff = 100 * time.Millisecond

	created := ledger.CreateOrder("order-halt-1", "cust-halt-1")
	screened := ledger.AdvanceToScreened(created)
	order, err := client.GetOrder(t.Context(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}

	ledger.SetHalted(true, "test halt")
	defer ledger.SetHalted(false, "")

	resultCh := make(chan error, 1)
	go func() {
		_, err := d.EnterDispatching(context.Background(), order, 1, time.Now().UTC())
		resultCh <- err
	}()

	// Give it a couple of retry cycles against the real halt before
	// clearing it.
	time.Sleep(350 * time.Millisecond)
	ledger.SetHalted(false, "")

	select {
	case err := <-resultCh:
		if err != nil {
			t.Fatalf("EnterDispatching (after halt cleared): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnterDispatching did not return after the halt cleared")
	}

	after, err := client.GetOrder(t.Context(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder (after): %v", err)
	}
	if after.State != "dispatching" {
		t.Fatalf("order.State (after) = %q, want dispatching", after.State)
	}
}

// TestEnterDispatching_CtxCancelledDuringHaltBackoffReturnsPromptly
// covers the only way out of the halt-retry loop besides the halt
// itself clearing.
func TestEnterDispatching_CtxCancelledDuringHaltBackoffReturnsPromptly(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signing.NewFakeSigningService())
	d.HaltBackoff = 5 * time.Second

	created := ledger.CreateOrder("order-halt-cancel-1", "cust-halt-cancel-1")
	screened := ledger.AdvanceToScreened(created)
	order, err := client.GetOrder(t.Context(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}

	ledger.SetHalted(true, "test halt")
	defer ledger.SetHalted(false, "")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = d.EnterDispatching(ctx, order, 1, time.Now().UTC())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EnterDispatching error = %v, want context.DeadlineExceeded", err)
	}
}
