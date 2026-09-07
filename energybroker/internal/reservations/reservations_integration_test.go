//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package reservations_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"energybroker/internal/buffer"
	"energybroker/internal/db"
	"energybroker/internal/ledgerclient"
	"energybroker/internal/pricing"
	"energybroker/internal/provider"
	"energybroker/internal/reservations"
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

	if _, err := pool.Exec(ctx, `TRUNCATE energy_buffer, buffer_allocations, reservations RESTART IDENTITY`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

// seedAvailableRow inserts an AVAILABLE energy_buffer row directly --
// this package's own tests care about Create's own fast/slow-path
// branching, not about how a warm buffer got that way (internal/buffer's
// own tests already cover Replenish exhaustively).
func seedAvailableRow(t *testing.T, pool *db.Pool, providerName, delegationID string, units int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO energy_buffer (provider_name, delegation_id, units, acquired_at, cost_trx, expires_at, status)
		VALUES ($1, $2, $3, now(), 0, now() + interval '1 hour', 'AVAILABLE')
	`, providerName, delegationID, units)
	if err != nil {
		t.Fatalf("seeding available row: %v", err)
	}
}

type fakeOrderResolver struct {
	order ledgerclient.Order
	err   error
}

func (f fakeOrderResolver) GetOrder(ctx context.Context, externalID string) (ledgerclient.Order, error) {
	return f.order, f.err
}

// testHarness bundles a real, Postgres-backed Buffer and Router (so
// Reserve/RowsByIDs/VerifyOnChain and SelectProvider all behave exactly
// as they do in production) around a set of MockProviders this test
// controls directly.
type testHarness struct {
	pool      *db.Pool
	providers map[string]provider.EnergyProvider
	buf       *buffer.Buffer
	router    *routing.Router
	poller    *pricing.Poller
}

func newTestHarness(t *testing.T, pool *db.Pool, reader *buffer.FakeTronReader) *testHarness {
	t.Helper()
	providers := map[string]provider.EnergyProvider{
		provider.Tronsell: provider.NewMockProvider(provider.Tronsell, 1, 24.0),
		provider.Netts:    provider.NewMockProvider(provider.Netts, 2, 28.0),
		provider.Catfee:   provider.NewMockProvider(provider.Catfee, 3, 30.0),
	}

	poller := pricing.NewPoller(pool, providers, time.Hour)
	if err := poller.PollAll(context.Background()); err != nil {
		t.Fatalf("PollAll: %v", err)
	}
	router := routing.NewRouter(poller, 1)

	buf := buffer.NewBuffer(pool, providers, router, stubDemandObserver{}, reader, nil, buffer.Config{
		StagingAddress: "TStagingReservations0000000001",
	})

	return &testHarness{pool: pool, providers: providers, buf: buf, router: router, poller: poller}
}

type stubDemandObserver struct{}

func (stubDemandObserver) RecentReservedUnits(ctx context.Context, window time.Duration) (int64, error) {
	return 0, nil
}

func defaultWeights() routing.RoutingWeights {
	return routing.RoutingWeights{
		provider.Tronsell: 0.60,
		provider.Netts:    0.35,
		provider.Catfee:   0.05,
	}
}

const ceiling = 25.7

func TestCreate_FastPath_ConfirmsWellWithinDeadlineWithZeroDelegateCalls(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000) // every Redelegate this test makes verifies on-chain
	h := newTestHarness(t, pool, reader)

	seedAvailableRow(t, pool, provider.Tronsell, "seed-1", 500)

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 7, ExternalID: "order-fast-1"}}
	svc := reservations.NewService(pool, orders, h.buf, h.router, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-fast-1:1",
		ExternalID:     "order-fast-1",
		TargetAddress:  "TPayoutSlot00000000000000000001",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(5 * time.Second),
	}

	start := time.Now()
	res, err := svc.Create(ctx, req)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Status != reservations.StatusConfirmed {
		t.Fatalf("Status = %s, want CONFIRMED", res.Status)
	}
	if res.Vendor == nil || *res.Vendor != provider.Tronsell {
		t.Fatalf("Vendor = %v, want tronsell", res.Vendor)
	}
	if res.CostTRX == nil || *res.CostTRX != 0 {
		t.Fatalf("CostTRX = %v, want 0 (redelegation is free; the acquisition cost was already attributed by Replenish)", res.CostTRX)
	}
	// A tight, explicit wall-clock bound -- the fast path against fakes
	// with no artificial delay should complete in well under a second,
	// nowhere near the 5-second request deadline.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("fast path took %s, want well under 500ms", elapsed)
	}

	mock := h.providers[provider.Tronsell].(*provider.MockProvider)
	if got := mock.DelegateCallCount(); got != 0 {
		t.Fatalf("Delegate was called %d times on the fast path, want 0", got)
	}
	if got := mock.RedelegateCallCount(); got != 1 {
		t.Fatalf("Redelegate was called %d times, want exactly 1", got)
	}
}

func TestCreate_IdempotentReplayReturnsOriginalReservationNeverASecondDelegation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	h := newTestHarness(t, pool, reader)

	seedAvailableRow(t, pool, provider.Tronsell, "seed-1", 500)

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 8, ExternalID: "order-idem-1"}}
	svc := reservations.NewService(pool, orders, h.buf, h.router, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-idem-1:1",
		ExternalID:     "order-idem-1",
		TargetAddress:  "TPayoutSlot00000000000000000002",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(5 * time.Second),
	}

	first, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create (1st): %v", err)
	}
	second, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create (2nd, replayed): %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("replayed Create returned reservation %d, want the original %d", second.ID, first.ID)
	}
	if second.Status != reservations.StatusConfirmed {
		t.Fatalf("replayed reservation status = %s, want CONFIRMED", second.Status)
	}

	mock := h.providers[provider.Tronsell].(*provider.MockProvider)
	if got := mock.RedelegateCallCount(); got != 1 {
		t.Fatalf("Redelegate was called %d times across both Create calls, want exactly 1 -- a replay must never delegate twice", got)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reservations WHERE idempotency_key = $1`, req.IdempotencyKey).Scan(&count); err != nil {
		t.Fatalf("counting reservations: %v", err)
	}
	if count != 1 {
		t.Fatalf("reservations rows for this idempotency key = %d, want exactly 1", count)
	}
}

func TestCreate_SlowPath_FallsThroughAndDelegatesDirectly(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	h := newTestHarness(t, pool, reader)
	// No seeded rows -- Reserve is exhausted immediately.

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 9, ExternalID: "order-slow-1"}}
	svc := reservations.NewService(pool, orders, h.buf, h.router, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-slow-1:1",
		ExternalID:     "order-slow-1",
		TargetAddress:  "TPayoutSlot00000000000000000003",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(5 * time.Second),
	}

	res, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Status != reservations.StatusConfirmed {
		t.Fatalf("Status = %s, want CONFIRMED", res.Status)
	}
	if res.CostTRX == nil || *res.CostTRX <= 0 {
		t.Fatalf("CostTRX = %v, want a real, positive cost -- the slow path made a fresh, real Delegate call", res.CostTRX)
	}

	// At least one of the three primaries must have actually been
	// delegated to -- which one depends on the weighted draw.
	var totalDelegateCalls int
	for _, p := range h.providers {
		totalDelegateCalls += p.(*provider.MockProvider).DelegateCallCount()
	}
	if totalDelegateCalls != 1 {
		t.Fatalf("total Delegate calls across all providers = %d, want exactly 1", totalDelegateCalls)
	}
}

func TestCreate_SlowPath_RespectsTheCeilingAndNeverPaysThrough(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	h := newTestHarness(t, pool, reader)

	// Every primary is priced above the ceiling -- re-polled after
	// forcing so the price feed the Router reads actually reflects it
	// (newTestHarness's own initial PollAll already ran before this).
	for _, p := range h.providers {
		p.(*provider.MockProvider).ForcePrice(9999.0)
	}
	if err := h.poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll (re-poll after forcing price): %v", err)
	}

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 10, ExternalID: "order-ceiling-1"}}
	svc := reservations.NewService(pool, orders, h.buf, h.router, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-ceiling-1:1",
		ExternalID:     "order-ceiling-1",
		TargetAddress:  "TPayoutSlot00000000000000000004",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(5 * time.Second),
	}

	res, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Status != reservations.StatusFailed {
		t.Fatalf("Status = %s, want FAILED -- routing must fall back to the manual ladder, never pay above ceiling", res.Status)
	}

	for name, p := range h.providers {
		if got := p.(*provider.MockProvider).DelegateCallCount(); got != 0 {
			t.Fatalf("provider %s: Delegate was called %d times, want 0 -- a price above ceiling must never be paid (invariant 3)", name, got)
		}
	}
}

func TestCreate_SlowPath_DeadlineElapsedReturnsFailed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	h := newTestHarness(t, pool, reader)

	// Every primary hangs -- forcing the slow path to run out its own
	// deadline rather than ever completing.
	for _, p := range h.providers {
		p.(*provider.MockProvider).ForceTimeout()
	}

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 11, ExternalID: "order-deadline-1"}}
	svc := reservations.NewService(pool, orders, h.buf, h.router, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-deadline-1:1",
		ExternalID:     "order-deadline-1",
		TargetAddress:  "TPayoutSlot00000000000000000005",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(80 * time.Millisecond),
	}

	start := time.Now()
	res, err := svc.Create(ctx, req)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Status != reservations.StatusFailed {
		t.Fatalf("Status = %s, want FAILED once the deadline elapses", res.Status)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Create took %s to fail after an 80ms deadline, want it bounded close to that deadline, not hanging", elapsed)
	}
}

func TestCreate_UnknownExternalIDPropagatesOrderResolverError(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	h := newTestHarness(t, pool, reader)

	wantErr := errors.New("ledgerclient: C1 returned 404 not_found: no such order")
	orders := fakeOrderResolver{err: wantErr}
	svc := reservations.NewService(pool, orders, h.buf, h.router, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})

	req := reservations.Request{
		IdempotencyKey: "dispatch:unknown:1",
		ExternalID:     "unknown-order",
		TargetAddress:  "TPayoutSlot00000000000000000006",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(5 * time.Second),
	}

	_, err := svc.Create(ctx, req)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Create error = %v, want wrapping %v", err, wantErr)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reservations`).Scan(&count); err != nil {
		t.Fatalf("counting reservations: %v", err)
	}
	if count != 0 {
		t.Fatalf("reservations rows = %d, want 0 -- a failed order lookup must never create a reservation row", count)
	}
}
