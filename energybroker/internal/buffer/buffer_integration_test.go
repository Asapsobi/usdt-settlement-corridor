//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package buffer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"energybroker/internal/db"
	"energybroker/internal/pricing"
	"energybroker/internal/provider"
	"energybroker/internal/routing"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("BROKER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("BROKER_TEST_DATABASE_URL not set; skipping integration test")
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
		t.Fatalf("running migrations: %v", err)
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `TRUNCATE energy_buffer, buffer_allocations, vendor_overcharge_events, manual_fallback_events RESTART IDENTITY`); err != nil {
		t.Fatalf("truncating buffer tables: %v", err)
	}
	return pool
}

// seedAvailableRow inserts an AVAILABLE energy_buffer row directly,
// bypassing Replenish -- Reserve's own tests care about claim
// correctness against a known set of rows, not about how they got there.
func seedAvailableRow(t *testing.T, pool *db.Pool, providerName string, delegationID string, units int64, expiresAt time.Time) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO energy_buffer (provider_name, delegation_id, units, acquired_at, cost_trx, expires_at, status)
		VALUES ($1, $2, $3, now(), 0, $4, $5)
		RETURNING id
	`, providerName, delegationID, units, expiresAt, string(StatusAvailable)).Scan(&id)
	if err != nil {
		t.Fatalf("seeding available row: %v", err)
	}
	return id
}

func rowStatus(t *testing.T, pool *db.Pool, id int64) Status {
	t.Helper()
	var s Status
	if err := pool.QueryRow(context.Background(), `SELECT status FROM energy_buffer WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("fetching status for row %d: %v", id, err)
	}
	return s
}

func TestReserve_ExactlyEnoughCapacitySucceeds(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	b, err := NewBuffer(pool, nil, nil, nil, nil, nil, Config{Ceiling: 1000})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	future := time.Now().Add(time.Hour)
	id1 := seedAvailableRow(t, pool, provider.Tronsell, "d1", 300, future)
	id2 := seedAvailableRow(t, pool, provider.Netts, "d2", 200, future)

	alloc, err := b.Reserve(ctx, 42, 500)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if alloc.Units != 500 {
		t.Fatalf("Allocation.Units = %d, want 500", alloc.Units)
	}
	if len(alloc.RowIDs) != 2 {
		t.Fatalf("Allocation.RowIDs = %v, want exactly the 2 seeded rows", alloc.RowIDs)
	}
	if rowStatus(t, pool, id1) != StatusReserved || rowStatus(t, pool, id2) != StatusReserved {
		t.Fatal("both seeded rows must be RESERVED")
	}

	total, err := availableTotal(ctx, pool)
	if err != nil {
		t.Fatalf("availableTotal: %v", err)
	}
	if total != 0 {
		t.Fatalf("availableTotal after reserving exactly all capacity = %d, want 0", total)
	}
}

func TestReserve_ExhaustedBufferTouchesZeroRows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	b, err := NewBuffer(pool, nil, nil, nil, nil, nil, Config{Ceiling: 1000})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	future := time.Now().Add(time.Hour)
	id1 := seedAvailableRow(t, pool, provider.Tronsell, "d1", 100, future)

	_, err = b.Reserve(ctx, 42, 500)
	if !errors.Is(err, ErrBufferExhausted) {
		t.Fatalf("Reserve error = %v, want ErrBufferExhausted", err)
	}

	if rowStatus(t, pool, id1) != StatusAvailable {
		t.Fatal("the one seeded row must remain AVAILABLE -- an exhausted Reserve touches zero rows")
	}
	var allocCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM buffer_allocations`).Scan(&allocCount); err != nil {
		t.Fatalf("counting buffer_allocations: %v", err)
	}
	if allocCount != 0 {
		t.Fatalf("buffer_allocations rows = %d, want 0 -- a failed Reserve must not record an allocation", allocCount)
	}
}

// TestReserve_50ConcurrentCallsNeverOverOrUnderClaim is this chunk's own
// deadlock/race acceptance criterion, the same posture as C1.4's own
// concurrent-balance test: 50 orders concurrently reserve against a
// buffer stocked with EXACTLY enough total capacity to satisfy all of
// them combined -- every call must succeed, no two allocations may ever
// claim the same row, and the union of every claimed row must be every
// seeded row, exactly once each.
func TestReserve_50ConcurrentCallsNeverOverOrUnderClaim(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	b, err := NewBuffer(pool, nil, nil, nil, nil, nil, Config{Ceiling: 1000})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	const n = 50
	const perOrder = 100
	future := time.Now().Add(time.Hour)

	seeded := make(map[int64]bool, n)
	for i := 0; i < n; i++ {
		id := seedAvailableRow(t, pool, provider.Tronsell, fmt.Sprintf("d%d", i), perOrder, future)
		seeded[id] = true
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	claimedBy := make(map[int64]int64) // row id -> order id, to detect double-claims
	var errs []error

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(orderID int64) {
			defer wg.Done()
			alloc, err := b.Reserve(ctx, orderID, perOrder)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("order %d: %w", orderID, err))
				return
			}
			for _, rowID := range alloc.RowIDs {
				if existing, dup := claimedBy[rowID]; dup {
					errs = append(errs, fmt.Errorf("row %d claimed by both order %d and order %d", rowID, existing, orderID))
				}
				claimedBy[rowID] = orderID
			}
		}(int64(i + 1))
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d error(s) during concurrent Reserve, first: %v", len(errs), errs[0])
	}
	if len(claimedBy) != n {
		t.Fatalf("total rows claimed across all 50 orders = %d, want %d (every seeded row claimed exactly once)", len(claimedBy), n)
	}
	for id := range seeded {
		if _, ok := claimedBy[id]; !ok {
			t.Errorf("seeded row %d was never claimed by any order", id)
		}
	}

	total, err := availableTotal(ctx, pool)
	if err != nil {
		t.Fatalf("availableTotal: %v", err)
	}
	if total != 0 {
		t.Fatalf("availableTotal after all 50 orders reserved = %d, want 0", total)
	}
}

func TestReplenish_ReachesTargetLevelRespectingMaxUnitsChunking(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	providers := map[string]provider.EnergyProvider{
		provider.Tronsell: provider.NewMockProvider(provider.Tronsell, 1, 24.0),
		provider.Netts:    provider.NewMockProvider(provider.Netts, 2, 28.0),
		provider.Catfee:   provider.NewMockProvider(provider.Catfee, 3, 30.0),
	}
	// A small, shared cap on every provider forces Replenish to make
	// several Delegate calls to reach its target, regardless of which
	// provider the weighted draw happens to pick each time -- proving
	// MaxUnitsAvailable chunking is respected without depending on any
	// one specific provider being chosen.
	for _, p := range providers {
		p.(*provider.MockProvider).ForcePartialFill(500)
	}

	poller := pricing.NewPoller(pool, providers, time.Hour)
	if err := poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll: %v", err)
	}
	router := routing.NewRouter(poller, pool, 1)

	reader := NewFakeTronReader()
	reader.AutoConfirm(1_000_000) // every delegation this test makes verifies on-chain

	const stagingAddress = "TStagingBuffer0000000000000001"
	const target = 2200 // not a multiple of 500 -- the last chunk must cover the remainder exactly

	buf, err := NewBuffer(pool, providers, router, fakeDemandObserver{recent: target}, reader, nil, Config{
		MinimumFloor:    0,
		LookbackWindow:  time.Hour,
		LookaheadWindow: time.Hour, // equal windows: TargetLevel == recent demand exactly
		Weights: routing.RoutingWeights{
			provider.Tronsell: 0.60,
			provider.Netts:    0.35,
			provider.Catfee:   0.05,
		},
		Ceiling:            1000, // generous -- this test is about chunking, not ceiling rejection
		DelegationDuration: 24 * time.Hour,
		StagingAddress:     stagingAddress,
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	if err := buf.Replenish(ctx); err != nil {
		t.Fatalf("Replenish: %v", err)
	}

	total, err := availableTotal(ctx, pool)
	if err != nil {
		t.Fatalf("availableTotal: %v", err)
	}
	if total != target {
		t.Fatalf("availableTotal after Replenish = %d, want exactly the target %d", total, target)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM energy_buffer WHERE status = $1`, string(StatusAvailable)).Scan(&rowCount); err != nil {
		t.Fatalf("counting available rows: %v", err)
	}
	if rowCount < 5 {
		t.Fatalf("available row count = %d, want at least 5 -- a 2200-unit target against a 500-unit-per-call cap must chunk across multiple Delegate calls", rowCount)
	}

	// A second Replenish call, already at target, must be a no-op.
	if err := buf.Replenish(ctx); err != nil {
		t.Fatalf("Replenish (2nd, already at target): %v", err)
	}
	total2, err := availableTotal(ctx, pool)
	if err != nil {
		t.Fatalf("availableTotal (2nd): %v", err)
	}
	if total2 != target {
		t.Fatalf("availableTotal after a 2nd Replenish already at target = %d, want unchanged %d", total2, target)
	}
}

func TestReplenish_NeverMarksAvailableWithoutOnChainConfirmation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	providers := map[string]provider.EnergyProvider{
		provider.Tronsell: provider.NewMockProvider(provider.Tronsell, 1, 24.0),
	}
	poller := pricing.NewPoller(pool, providers, time.Hour)
	if err := poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll: %v", err)
	}
	router := routing.NewRouter(poller, pool, 1)

	// AutoConfirm defaults to 0 -- every delegation this test makes
	// fails on-chain verification.
	reader := NewFakeTronReader()

	buf, err := NewBuffer(pool, providers, router, fakeDemandObserver{recent: 1000}, reader, nil, Config{
		MinimumFloor:       0,
		LookbackWindow:     time.Hour,
		LookaheadWindow:    time.Hour,
		Weights:            routing.RoutingWeights{provider.Tronsell: 1.0},
		Ceiling:            1000,
		DelegationDuration: 24 * time.Hour,
		StagingAddress:     "TStagingBuffer0000000000000002",
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	if err := buf.Replenish(ctx); err != nil {
		t.Fatalf("Replenish: %v", err)
	}

	total, err := availableTotal(ctx, pool)
	if err != nil {
		t.Fatalf("availableTotal: %v", err)
	}
	if total != 0 {
		t.Fatalf("availableTotal = %d, want 0 -- a delegation that fails on-chain verification must never be recorded AVAILABLE (invariant 1)", total)
	}
}

type recordingAlerter struct {
	mu    sync.Mutex
	calls []alertCall
}
type alertCall struct {
	row              Row
	expected, actual int64
}

func (r *recordingAlerter) AlertBufferShortfall(ctx context.Context, row Row, expected, actual int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, alertCall{row, expected, actual})
}

func TestReconcile_CorrectsAnEarlyRevocationAndAlerts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	const stagingAddress = "TStagingBuffer0000000000000003"
	reader := NewFakeTronReader()
	alerter := &recordingAlerter{}
	buf, err := NewBuffer(pool, nil, nil, nil, reader, alerter, Config{
		StagingAddress:     stagingAddress,
		ReconcileLookahead: time.Hour,
		Ceiling:            1000,
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	// Nearing expiry (within the 1h lookahead) -- this is the row the
	// fake vendor is about to revoke early.
	revokedID := seedAvailableRow(t, pool, provider.Tronsell, "revoked-1", 5000, time.Now().Add(10*time.Minute))
	// A second row, also nearing expiry, that the chain still confirms
	// in full -- must be left untouched.
	stillGoodID := seedAvailableRow(t, pool, provider.Tronsell, "still-good-1", 3000, time.Now().Add(10*time.Minute))
	// A row NOT nearing expiry -- must never even be queried.
	notNearID := seedAvailableRow(t, pool, provider.Tronsell, "not-near-1", 1000, time.Now().Add(48*time.Hour))

	reader.SetUnits(stagingAddress, "revoked-1", 0) // the vendor's own dashboard revoked it early
	reader.SetUnits(stagingAddress, "still-good-1", 3000)
	reader.SetUnits(stagingAddress, "not-near-1", 0) // if this were ever queried, it would (wrongly) look revoked too

	if err := buf.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if got := rowStatus(t, pool, revokedID); got != StatusExpired {
		t.Fatalf("revoked row status = %s, want EXPIRED", got)
	}
	if got := rowStatus(t, pool, stillGoodID); got != StatusAvailable {
		t.Fatalf("still-good row status = %s, want unchanged AVAILABLE", got)
	}
	if got := rowStatus(t, pool, notNearID); got != StatusAvailable {
		t.Fatalf("not-nearing-expiry row status = %s, want unchanged AVAILABLE (must not even be reconciled yet)", got)
	}

	alerter.mu.Lock()
	defer alerter.mu.Unlock()
	if len(alerter.calls) != 1 {
		t.Fatalf("alert calls = %d, want exactly 1", len(alerter.calls))
	}
	if alerter.calls[0].row.ID != revokedID {
		t.Fatalf("alert was for row %d, want %d", alerter.calls[0].row.ID, revokedID)
	}
	if alerter.calls[0].expected != 5000 || alerter.calls[0].actual != 0 {
		t.Fatalf("alert expected/actual = %d/%d, want 5000/0", alerter.calls[0].expected, alerter.calls[0].actual)
	}
}

// TestNewBuffer_RejectsNonPositiveCeiling is C4.7's own adversarial
// scenario: a misconfigured ceiling must refuse to start.
func TestNewBuffer_RejectsNonPositiveCeiling(t *testing.T) {
	for _, badCeiling := range []float64{0, -1} {
		_, err := NewBuffer(nil, nil, nil, nil, nil, nil, Config{Ceiling: badCeiling})
		if !errors.Is(err, routing.ErrInvalidCeiling) {
			t.Fatalf("NewBuffer(Ceiling=%v) error = %v, want routing.ErrInvalidCeiling", badCeiling, err)
		}
	}
}

func newReconcileTestHarness(t *testing.T, pool *db.Pool) (*provider.MockProvider, *routing.Router, *FakeTronReader) {
	t.Helper()
	mock := provider.NewMockProvider(provider.Tronsell, 1, 24.0)
	providers := map[string]provider.EnergyProvider{provider.Tronsell: mock}
	poller := pricing.NewPoller(pool, providers, time.Hour)
	if err := poller.PollAll(context.Background()); err != nil {
		t.Fatalf("PollAll: %v", err)
	}
	router := routing.NewRouter(poller, pool, 1)
	reader := NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	return mock, router, reader
}

// TestReplenish_VendorChargedMoreThanQuotedButUnderCeiling_FlaggedAndAccepted
// is C4.7's own central adversarial scenario, the half where the
// mismatch stays under ceiling: the delegation must still be flagged
// (a real vendor-integrity signal worth auditing) but the buffer must
// still accept it, crediting the ACTUAL charged amount, never the stale
// quote.
func TestReplenish_VendorChargedMoreThanQuotedButUnderCeiling_FlaggedAndAccepted(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	mock, router, reader := newReconcileTestHarness(t, pool)
	mock.ForcePrice(24.0)           // quoted
	mock.ForceChargedPriceSun(25.5) // actually charged -- a mismatch, but still under the 25.7 ceiling below
	if err := pricing.NewPoller(pool, map[string]provider.EnergyProvider{provider.Tronsell: mock}, time.Hour).PollAll(ctx); err != nil {
		t.Fatalf("re-poll after forcing price: %v", err)
	}

	buf, err := NewBuffer(pool, map[string]provider.EnergyProvider{provider.Tronsell: mock}, router, fakeDemandObserver{recent: 1000}, reader, nil, Config{
		MinimumFloor: 0, LookbackWindow: time.Hour, LookaheadWindow: time.Hour,
		Weights: routing.RoutingWeights{provider.Tronsell: 1.0}, Ceiling: 25.7,
		DelegationDuration: 24 * time.Hour, StagingAddress: "TStagingOvercharge0000000001",
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	if err := buf.Replenish(ctx); err != nil {
		t.Fatalf("Replenish: %v", err)
	}

	total, err := availableTotal(ctx, pool)
	if err != nil {
		t.Fatalf("availableTotal: %v", err)
	}
	if total != 1000 {
		t.Fatalf("availableTotal = %d, want 1000 -- a mismatched-but-under-ceiling charge must still be accepted", total)
	}

	var costTRX int64
	if err := pool.QueryRow(ctx, `SELECT cost_trx FROM energy_buffer WHERE provider_name = $1`, provider.Tronsell).Scan(&costTRX); err != nil {
		t.Fatalf("fetching recorded cost: %v", err)
	}
	wantCost := int64(25.5 * 1000) // 25.5 sun/unit * 1000 units
	if costTRX != wantCost {
		t.Fatalf("recorded cost_trx = %d, want %d -- the ACTUAL charged amount, never the stale 24.0 quote", costTRX, wantCost)
	}

	var overchargeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vendor_overcharge_events WHERE over_ceiling = false`).Scan(&overchargeCount); err != nil {
		t.Fatalf("counting vendor_overcharge_events: %v", err)
	}
	if overchargeCount != 1 {
		t.Fatalf("vendor_overcharge_events rows (over_ceiling=false) = %d, want exactly 1", overchargeCount)
	}
}

// TestReplenish_VendorChargedAboveCeiling_FlaggedAndRefused is the other
// half: the actual charge exceeds ceiling outright. Invariant 3 must
// hold even though the vendor's own Delegate call already "succeeded"
// -- the capacity must never enter the buffer as ordinary AVAILABLE
// inventory.
func TestReplenish_VendorChargedAboveCeiling_FlaggedAndRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	mock, router, reader := newReconcileTestHarness(t, pool)
	mock.ForcePrice(24.0)           // quoted, well under ceiling
	mock.ForceChargedPriceSun(30.0) // actually charged -- above the 25.7 ceiling
	if err := pricing.NewPoller(pool, map[string]provider.EnergyProvider{provider.Tronsell: mock}, time.Hour).PollAll(ctx); err != nil {
		t.Fatalf("re-poll after forcing price: %v", err)
	}

	buf, err := NewBuffer(pool, map[string]provider.EnergyProvider{provider.Tronsell: mock}, router, fakeDemandObserver{recent: 1000}, reader, nil, Config{
		MinimumFloor: 0, LookbackWindow: time.Hour, LookaheadWindow: time.Hour,
		Weights: routing.RoutingWeights{provider.Tronsell: 1.0}, Ceiling: 25.7,
		DelegationDuration: 24 * time.Hour, StagingAddress: "TStagingOvercharge0000000002",
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	if err := buf.Replenish(ctx); err != nil {
		t.Fatalf("Replenish: %v", err)
	}

	total, err := availableTotal(ctx, pool)
	if err != nil {
		t.Fatalf("availableTotal: %v", err)
	}
	if total != 0 {
		t.Fatalf("availableTotal = %d, want 0 -- an over-ceiling charge must never enter the buffer as ordinary inventory (invariant 3)", total)
	}

	var overchargeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vendor_overcharge_events WHERE over_ceiling = true`).Scan(&overchargeCount); err != nil {
		t.Fatalf("counting vendor_overcharge_events: %v", err)
	}
	if overchargeCount != 1 {
		t.Fatalf("vendor_overcharge_events rows (over_ceiling=true) = %d, want exactly 1", overchargeCount)
	}
}
