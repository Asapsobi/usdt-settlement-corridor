//go:build integration

// Requires two real, reachable Postgres 16 instances: one for
// dispatcher's own database (DISPATCHER_TEST_DATABASE_URL) and one for
// a real ledgerd subprocess (LEDGER_TEST_DATABASE_URL) -- same
// convention as internal/dispatch's own integration suite.
package orchestrate_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"google.golang.org/protobuf/proto"

	"dispatcher/internal/db"
	"dispatcher/internal/dispatch"
	"dispatcher/internal/energy"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/orchestrate"
	"dispatcher/internal/signing"
	"dispatcher/internal/slots"
	"dispatcher/internal/testledger"
	"dispatcher/internal/txbuild"
)

const testSlotAddress = "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH"

var portSeq int64 = 19538

func startLedger(t *testing.T) *testledger.Ledger {
	t.Helper()
	dbURL := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	port := atomic.AddInt64(&portSeq, 1)
	return testledger.Start(t, dbURL, fmt.Sprintf(":%d", port), "orchestrate-test-token", "dispatcher")
}

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("DISPATCHER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("DISPATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	pool, err := db.Open(context.Background(), db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeEnergy stands in for C4 -- always confirms immediately, mirroring
// internal/replay's own fakeEnergyReserver for the happy path. calls is
// keyed by external_id, not a bare count: dispatcher_test/ledger_test2
// are shared across this whole module's integration suite (every
// package's own test binary points at the same two databases), and
// orchestrate's own RunTick deliberately scans EVERY currently-screened
// order -- correct, required production behavior, but it means a raw
// global call count would also capture whatever sibling packages' own
// tests happen to leave in `screened` around the same time. Keying by
// this test's own external_id keeps the "reserved exactly once, never
// re-reserved on a retry tick" assertion meaningful regardless.
type fakeEnergy struct{ calls map[string]int }

func newFakeEnergy() *fakeEnergy { return &fakeEnergy{calls: make(map[string]int)} }

func (f *fakeEnergy) Reserve(ctx context.Context, externalID, targetAddress string, units int64, tier string, deadline time.Time, idempotencyKey string) (energy.Reservation, error) {
	f.calls[externalID]++
	return energy.Reservation{Status: "CONFIRMED", Vendor: strPtr("fake-vendor"), EnergyUnits: units}, nil
}

func strPtr(s string) *string { return &s }

// fakeChain stands in for a real TRON node: Broadcast computes a
// deterministic txid the same way GrpcBroadcastClient's own doc comment
// says a real node's response implies (SHA256 of raw_data), and IsFinal
// answers from an explicitly armed set -- nothing is final until the
// test calls SetFinal, matching every other chain fake in this project
// (internal/replay's own fakeChain).
type fakeChain struct {
	final map[string]bool
}

func newFakeChain() *fakeChain { return &fakeChain{final: make(map[string]bool)} }

func (f *fakeChain) Broadcast(ctx context.Context, tx *core.Transaction) (string, error) {
	raw, err := proto.Marshal(tx.RawData)
	if err != nil {
		return "", err
	}
	digest := txbuild.Digest(raw)
	return hex.EncodeToString(digest[:]), nil
}

func (f *fakeChain) IsFinal(ctx context.Context, tronTxID string) (bool, error) {
	return f.final[tronTxID], nil
}

func (f *fakeChain) SetFinal(tronTxID string) { f.final[tronTxID] = true }

// fixedBlocks stands in for a real TRON node's GetNowBlock -- a single
// fixed, valid-shaped BlockReference is enough for txbuild.BuildTransfer,
// which never validates the block reference's own freshness.
type fixedBlocks struct{}

func (fixedBlocks) CurrentBlockReference(ctx context.Context) (txbuild.BlockReference, error) {
	now := time.Now().UTC()
	return txbuild.BlockReference{
		BlockNumber: 60_000_000,
		BlockHash:   [32]byte{0xaa, 0xbb, 0xcc},
		Timestamp:   now,
		Expiration:  now.Add(time.Minute),
	}, nil
}

func newOrchestrator(pool *db.Pool, client *ledgerclient.Client, chain *fakeChain) (*orchestrate.Orchestrator, *fakeEnergy) {
	d := dispatch.NewDispatcher(client, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signing.NewFakeSigningService())
	energyClient := newFakeEnergy()
	o := orchestrate.New(client, slots.NewStore(pool), slots.Caps{BalanceCeiling: 1_000_000_000_000, TxCountCeiling: 1_000_000},
		d, energyClient, chain, fixedBlocks{}, chain, orchestrate.Config{EnergyPerTransferUnits: 65000, EnergyReservationWait: time.Minute})
	return o, energyClient
}

// TestRunTick_DirectOrderDispatchesBroadcastsAndSettles drives one
// screened Direct-tier order through two RunTick calls -- the first
// dispatches, reserves energy, and broadcasts (nothing final yet); the
// second observes finality and settles -- proving the whole background
// loop cmd/dispatchd's own main.go doc comment names as never having
// been wired actually carries an order end to end with no manual step,
// and that ConfirmFinality's own dispatch_state.MarkSettled fix (found
// while building this loop) takes effect through it.
func TestRunTick_DirectOrderDispatchesBroadcastsAndSettles(t *testing.T) {
	ledger := startLedger(t)
	pool := testPool(t)
	client := ledgerclient.New(ledger.BaseURL(), ledger.Token())

	slotStore := slots.NewStore(pool)
	if _, err := slotStore.Create(context.Background(), 21, testSlotAddress, time.Now().UTC()); err != nil {
		t.Fatalf("slots.Create: %v", err)
	}
	ledger.CreateAccount("asset:tron:slot:21", "ASSET", "USDT_TRC20")

	chain := newFakeChain()
	o, energyClient := newOrchestrator(pool, client, chain)

	created := ledger.CreateOrder("orchestrate-direct-1", "orchestrate-cust-1")
	screened := ledger.AdvanceToScreened(created)

	if err := o.RunTick(context.Background()); err != nil {
		t.Fatalf("RunTick (1st, dispatch+broadcast): %v", err)
	}

	order, err := client.GetOrder(context.Background(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if order.State != "dispatching" {
		t.Fatalf("order.State after 1st RunTick = %q, want dispatching", order.State)
	}
	if energyClient.calls[screened.ExternalID] != 1 {
		t.Fatalf("energy.Reserve call count for %s = %d, want 1", screened.ExternalID, energyClient.calls[screened.ExternalID])
	}

	attempt, err := dispatch.NewAttemptStore(pool).LatestForOrder(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("LatestForOrder: %v", err)
	}
	if attempt.Status != dispatch.BroadcastBroadcast {
		t.Fatalf("attempt.Status after 1st RunTick = %q, want BROADCAST", attempt.Status)
	}
	if attempt.TronTxID == nil {
		t.Fatal("attempt.TronTxID is nil after broadcast")
	}

	// Not final yet -- a 2nd tick before SetFinal must be a pure no-op on
	// the chain/ledger side (idempotent: Broadcast sees a terminal
	// dispatch_attempts row and returns immediately; ConfirmFinality sees
	// IsFinal still false and returns without settling).
	if err := o.RunTick(context.Background()); err != nil {
		t.Fatalf("RunTick (2nd, still pending finality): %v", err)
	}
	if energyClient.calls[screened.ExternalID] != 1 {
		t.Fatalf("energy.Reserve call count for %s after 2nd RunTick = %d, want still 1 (never re-reserved)", screened.ExternalID, energyClient.calls[screened.ExternalID])
	}
	stillDispatching, err := client.GetOrder(context.Background(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder (still pending): %v", err)
	}
	if stillDispatching.State != "dispatching" {
		t.Fatalf("order.State after 2nd RunTick (pre-finality) = %q, want still dispatching", stillDispatching.State)
	}

	chain.SetFinal(*attempt.TronTxID)
	if err := o.RunTick(context.Background()); err != nil {
		t.Fatalf("RunTick (3rd, now final): %v", err)
	}

	settled, err := client.GetOrder(context.Background(), screened.ExternalID)
	if err != nil {
		t.Fatalf("GetOrder (after finality): %v", err)
	}
	if settled.State != "settled" {
		t.Fatalf("order.State after finality = %q, want settled", settled.State)
	}

	local, err := dispatch.NewStore(pool).Get(context.Background(), order.ID)
	if err != nil {
		t.Fatalf("Store.Get after finality: %v", err)
	}
	if local.Status != dispatch.StatusSettled {
		t.Fatalf("dispatch_state.Status after finality = %q, want SETTLED -- this is the MarkSettled fix this test exists to prove", local.Status)
	}

	inFlight, err := dispatch.NewStore(pool).ListDispatching(context.Background())
	if err != nil {
		t.Fatalf("ListDispatching: %v", err)
	}
	for _, a := range inFlight {
		if a.OrderID == order.ID {
			t.Fatalf("order %d still listed as DISPATCHING after settling", order.ID)
		}
	}

	// A 4th tick must touch nothing further: RunTick's own
	// dispatchScreened pass no longer sees this order (state=screened
	// filter), and confirmInFlight's own pass no longer sees it either
	// (ListDispatching, just proven empty for it above).
	if err := o.RunTick(context.Background()); err != nil {
		t.Fatalf("RunTick (4th, fully settled): %v", err)
	}
}
