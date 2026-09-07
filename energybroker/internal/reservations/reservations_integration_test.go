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
	"sync"
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

	if _, err := pool.Exec(ctx, `TRUNCATE energy_buffer, buffer_allocations, reservations, vendor_overcharge_events, manual_fallback_events RESTART IDENTITY`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

// seedAvailableRow inserts an AVAILABLE energy_buffer row directly --
// this package's own tests care about Create's own fast/slow-path
// branching, not about how a warm buffer got that way (internal/buffer's
// own tests already cover Replenish exhaustively). costTRX is a real,
// non-zero minor-units amount, not a placeholder: confirmFastPath (C4.5)
// sums exactly this value to attribute the fast path's own real cost, so
// a test seeding 0 here would silently hide a cost-attribution
// regression rather than catch one.
func seedAvailableRow(t *testing.T, pool *db.Pool, providerName, delegationID string, units, costTRX int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO energy_buffer (provider_name, delegation_id, units, acquired_at, cost_trx, expires_at, status)
		VALUES ($1, $2, $3, now(), $4, now() + interval '1 hour', 'AVAILABLE')
	`, providerName, delegationID, units, costTRX)
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
	router := routing.NewRouter(poller, pool, 1)

	buf, err := buffer.NewBuffer(pool, providers, router, stubDemandObserver{}, reader, nil, buffer.Config{
		StagingAddress: "TStagingReservations0000000001",
		Ceiling:        ceiling,
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

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

	seedAvailableRow(t, pool, provider.Tronsell, "seed-1", 500, 1_200000)

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 7, ExternalID: "order-fast-1"}}
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

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
	if res.CostTRX == nil || *res.CostTRX != 1_200000 {
		t.Fatalf("CostTRX = %v, want 1200000 -- the seeded buffer row's own original acquisition cost, attributed to this order (redelegation itself is free, but the energy never was)", res.CostTRX)
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

	seedAvailableRow(t, pool, provider.Tronsell, "seed-1", 500, 1_200000)

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 8, ExternalID: "order-idem-1"}}
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

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
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

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
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
		// Short poll interval and deadline: the price stays above ceiling
		// for the entire test, so the slow path's own retry loop (C4.6)
		// polls a couple of times and then correctly fails once the
		// deadline elapses -- no need for a multi-second test to prove
		// that.
		FallbackPollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-ceiling-1:1",
		ExternalID:     "order-ceiling-1",
		TargetAddress:  "TPayoutSlot00000000000000000004",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(150 * time.Millisecond),
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

	// C4.6: a live reservation hitting the fallback ladder must record a
	// manual_fallback_events row (via routing.OnFallbackTriggered), not
	// just fail silently.
	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM manual_fallback_events WHERE reason = 'all_over_ceiling' AND order_id = $1`, orders.order.ID).Scan(&eventCount); err != nil {
		t.Fatalf("counting manual_fallback_events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("manual_fallback_events rows for order %d = %d, want exactly 1", orders.order.ID, eventCount)
	}
}

// TestCreate_SlowPath_RecoversIfAVendorBecomesSelectableWithinDeadline is
// this chunk's own central behavioral correction over C4.4: a single bad
// SelectProvider result (every vendor over ceiling right now) must NOT
// fail the reservation outright -- only running out the deadline does.
// This proves the other half: if a vendor becomes selectable again
// WHILE the reservation is still waiting, it must actually recover and
// confirm, not just eventually time out anyway.
func TestCreate_SlowPath_RecoversIfAVendorBecomesSelectableWithinDeadline(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	h := newTestHarness(t, pool, reader)

	for _, p := range h.providers {
		p.(*provider.MockProvider).ForcePrice(9999.0)
	}
	if err := h.poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll (forcing everyone over ceiling): %v", err)
	}

	// After a short delay, tronsell comes back under ceiling -- a real
	// vendor recovering mid-outage. The slow path's own retry loop
	// should notice on its very next poll and proceed to Delegate,
	// rather than having already given up.
	go func() {
		time.Sleep(60 * time.Millisecond)
		h.providers[provider.Tronsell].(*provider.MockProvider).ForcePrice(24.0)
		_ = h.poller.PollAll(context.Background())
	}()

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 12, ExternalID: "order-recovers-1"}}
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights:              defaultWeights(),
		Ceiling:              ceiling,
		FallbackPollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-recovers-1:1",
		ExternalID:     "order-recovers-1",
		TargetAddress:  "TPayoutSlot00000000000000000008",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(2 * time.Second),
	}

	res, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Status != reservations.StatusConfirmed {
		t.Fatalf("Status = %s, want CONFIRMED -- the slow path should have retried until tronsell recovered, well before the 2s deadline", res.Status)
	}
	if res.Vendor == nil || *res.Vendor != provider.Tronsell {
		t.Fatalf("Vendor = %v, want tronsell (the one that recovered)", res.Vendor)
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
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

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
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	req := reservations.Request{
		IdempotencyKey: "dispatch:unknown:1",
		ExternalID:     "unknown-order",
		TargetAddress:  "TPayoutSlot00000000000000000006",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(5 * time.Second),
	}

	_, err = svc.Create(ctx, req)
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

type recordedCostReport struct {
	delegation provider.Delegation
	orderID    int64
}

type recordingCostReporter struct {
	mu      sync.Mutex
	reports []recordedCostReport
}

func (r *recordingCostReporter) ReportEnergyCost(ctx context.Context, delegation provider.Delegation, orderID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, recordedCostReport{delegation, orderID})
	return nil
}

// TestCreate_ReportsCostToC1AfterConfirming proves Create actually
// wires C4.5's own EnergyCostReporter into both paths -- exactly one
// report per underlying delegation, each carrying the real order id and
// the real, non-zero cost, dispatched only AFTER the reservation is
// already CONFIRMED (never before, and never at all for a FAILED one).
func TestCreate_ReportsCostToC1AfterConfirming(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	h := newTestHarness(t, pool, reader)

	seedAvailableRow(t, pool, provider.Tronsell, "seed-cost-1", 500, 1_200000)

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 99, ExternalID: "order-cost-1"}}
	reporter := &recordingCostReporter{}
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, reporter, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-cost-1:1",
		ExternalID:     "order-cost-1",
		TargetAddress:  "TPayoutSlot00000000000000000007",
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

	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	if len(reporter.reports) != 1 {
		t.Fatalf("cost reports = %d, want exactly 1", len(reporter.reports))
	}
	report := reporter.reports[0]
	if report.orderID != 99 {
		t.Fatalf("reported order_id = %d, want 99", report.orderID)
	}
	if report.delegation.ID != "seed-cost-1" {
		t.Fatalf("reported delegation id = %q, want %q (the underlying buffer row's own original acquisition)", report.delegation.ID, "seed-cost-1")
	}
	if report.delegation.CostTRX != 1_200000 {
		t.Fatalf("reported CostTRX = %v, want 1200000", report.delegation.CostTRX)
	}
}

// TestNewService_RejectsNonPositiveCeiling is C4.7's own adversarial
// scenario applied to reservations.Service, mirroring buffer.NewBuffer's
// own identical check.
func TestNewService_RejectsNonPositiveCeiling(t *testing.T) {
	for _, badCeiling := range []float64{0, -1} {
		_, err := reservations.NewService(nil, nil, nil, nil, nil, nil, reservations.Config{Ceiling: badCeiling})
		if !errors.Is(err, routing.ErrInvalidCeiling) {
			t.Fatalf("NewService(Ceiling=%v) error = %v, want routing.ErrInvalidCeiling", badCeiling, err)
		}
	}
}

// TestCreate_SlowPath_VendorChargedMoreThanQuotedButUnderCeiling_FlaggedAndConfirmed
// is C4.7's own central scenario applied to the slow path: a vendor that
// doesn't honor its own quote, but whose actual charge still clears
// ceiling, must still confirm the reservation -- crediting the ACTUAL
// charge, never the stale quote -- while flagging the discrepancy.
func TestCreate_SlowPath_VendorChargedMoreThanQuotedButUnderCeiling_FlaggedAndConfirmed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	h := newTestHarness(t, pool, reader)
	// No seeded buffer rows -- Reserve is exhausted, forcing the slow path.

	mock := h.providers[provider.Tronsell].(*provider.MockProvider)
	mock.ForcePrice(24.0)           // quoted
	mock.ForceChargedPriceSun(25.5) // actually charged -- mismatched, still under the 25.7 ceiling
	if err := h.poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll (re-poll after forcing price): %v", err)
	}

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 20, ExternalID: "order-overcharge-1"}}
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-overcharge-1:1",
		ExternalID:     "order-overcharge-1",
		TargetAddress:  "TPayoutSlot00000000000000000009",
		EnergyUnits:    1000,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(5 * time.Second),
	}

	res, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Status != reservations.StatusConfirmed {
		t.Fatalf("Status = %s, want CONFIRMED -- a mismatched-but-under-ceiling charge must still confirm", res.Status)
	}
	wantCost := int64(25.5 * 1000)
	if res.CostTRX == nil || int64(*res.CostTRX) != wantCost {
		t.Fatalf("CostTRX = %v, want %d -- the ACTUAL charged amount, never the stale 24.0 quote", res.CostTRX, wantCost)
	}

	var overchargeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vendor_overcharge_events WHERE over_ceiling = false AND order_id = $1`, orders.order.ID).Scan(&overchargeCount); err != nil {
		t.Fatalf("counting vendor_overcharge_events: %v", err)
	}
	if overchargeCount != 1 {
		t.Fatalf("vendor_overcharge_events rows (over_ceiling=false) for order %d = %d, want exactly 1", orders.order.ID, overchargeCount)
	}
}

// TestCreate_SlowPath_VendorChargedAboveCeiling_FlaggedAndFailed is the
// other half: the actual charge exceeds ceiling outright. The
// reservation must FAIL, never confirm on the strength of a charge
// invariant 3 never allowed -- even though the vendor's own Delegate
// call already "succeeded" and real TRX already moved on-chain.
func TestCreate_SlowPath_VendorChargedAboveCeiling_FlaggedAndFailed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	h := newTestHarness(t, pool, reader)

	mock := h.providers[provider.Tronsell].(*provider.MockProvider)
	mock.ForcePrice(24.0)           // quoted, well under ceiling
	mock.ForceChargedPriceSun(30.0) // actually charged -- above the 25.7 ceiling
	if err := h.poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll (re-poll after forcing price): %v", err)
	}

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 21, ExternalID: "order-overcharge-2"}}
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	req := reservations.Request{
		IdempotencyKey: "dispatch:order-overcharge-2:1",
		ExternalID:     "order-overcharge-2",
		TargetAddress:  "TPayoutSlot00000000000000000010",
		EnergyUnits:    1000,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(5 * time.Second),
	}

	res, err := svc.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Status != reservations.StatusFailed {
		t.Fatalf("Status = %s, want FAILED -- an over-ceiling charge must never be confirmed (invariant 3)", res.Status)
	}

	var overchargeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vendor_overcharge_events WHERE over_ceiling = true AND order_id = $1`, orders.order.ID).Scan(&overchargeCount); err != nil {
		t.Fatalf("counting vendor_overcharge_events: %v", err)
	}
	if overchargeCount != 1 {
		t.Fatalf("vendor_overcharge_events rows (over_ceiling=true) for order %d = %d, want exactly 1", orders.order.ID, overchargeCount)
	}
}

// TestConcurrent_ReplenishAndSlowPathReservation_NeitherAcceptsAnOverCeilingCharge
// is C4.7's own fourth adversarial scenario: the buffer's own background
// replenishment and a live reservation's own slow path racing on the
// same ceiling check while the price is actively being updated. Each
// path reconciles against the price IT independently quoted right
// before ITS OWN Delegate call, so there is no shared, mutable "was this
// under ceiling" state for a stale read to leak through -- this proves
// that holds under genuine concurrency, not just sequentially.
func TestConcurrent_ReplenishAndSlowPathReservation_NeitherAcceptsAnOverCeilingCharge(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	h := newTestHarness(t, pool, reader)

	mock := h.providers[provider.Tronsell].(*provider.MockProvider)
	mock.ForcePrice(24.0)
	if err := h.poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll: %v", err)
	}

	bufCfg := buffer.Config{
		MinimumFloor: 500, LookbackWindow: time.Hour, LookaheadWindow: time.Hour,
		Weights: routing.RoutingWeights{provider.Tronsell: 1.0}, Ceiling: ceiling,
		DelegationDuration: 24 * time.Hour, StagingAddress: "TStagingConcurrentOvercharge01",
	}
	buf, err := buffer.NewBuffer(pool, h.providers, h.router, stubDemandObserver{}, reader, nil, bufCfg)
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	orders := fakeOrderResolver{order: ledgerclient.Order{ID: 22, ExternalID: "order-concurrent-1"}}
	svc, err := reservations.NewService(pool, orders, h.buf, h.router, nil, h.providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	req := reservations.Request{
		IdempotencyKey: "dispatch:order-concurrent-1:1",
		ExternalID:     "order-concurrent-1",
		TargetAddress:  "TPayoutSlot00000000000000000011",
		EnergyUnits:    500,
		Tier:           "STANDARD",
		Deadline:       time.Now().Add(2 * time.Second),
	}

	// The price flips above ceiling partway through, concurrently with
	// both Replenish and Create actually running -- whichever of them
	// samples the spiked price for its own Delegate call must refuse it;
	// whichever already committed to the pre-spike price must not be
	// affected by a later flip it never observed.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		mock.ForcePrice(9999.0)
		_ = h.poller.PollAll(context.Background())
	}()
	var replenishErr, createErr error
	var res reservations.Reservation
	go func() {
		defer wg.Done()
		replenishErr = buf.Replenish(ctx)
	}()
	go func() {
		defer wg.Done()
		res, createErr = svc.Create(ctx, req)
	}()
	wg.Wait()

	if replenishErr != nil {
		t.Fatalf("Replenish: %v", replenishErr)
	}
	if createErr != nil {
		t.Fatalf("Create: %v", createErr)
	}

	// Whatever actually landed, NOTHING may show an over-ceiling charge
	// treated as accepted: no AVAILABLE buffer row and no CONFIRMED
	// reservation may exist whose own recorded cost implies a per-unit
	// price above ceiling.
	rows, err := pool.Query(ctx, `SELECT units, cost_trx FROM energy_buffer WHERE status = 'AVAILABLE'`)
	if err != nil {
		t.Fatalf("querying energy_buffer: %v", err)
	}
	for rows.Next() {
		var units, cost int64
		if err := rows.Scan(&units, &cost); err != nil {
			t.Fatalf("scanning energy_buffer row: %v", err)
		}
		if perUnit := float64(cost) / float64(units); perUnit > ceiling+1e-6 {
			rows.Close()
			t.Fatalf("an AVAILABLE buffer row implies a per-unit price of %v, above ceiling %v", perUnit, ceiling)
		}
	}
	rows.Close()

	if res.Status == reservations.StatusConfirmed && res.CostTRX != nil {
		if perUnit := float64(*res.CostTRX) / float64(req.EnergyUnits); perUnit > ceiling+1e-6 {
			t.Fatalf("a CONFIRMED reservation implies a per-unit price of %v, above ceiling %v", perUnit, ceiling)
		}
	}
}
