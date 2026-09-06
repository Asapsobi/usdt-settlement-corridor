//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package rescreen_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"screening/internal/cache"
	"screening/internal/db"
	"screening/internal/ledgerclient"
	"screening/internal/provider"
	"screening/internal/rescreen"
	"screening/internal/verdict"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("SCREENING_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("SCREENING_TEST_DATABASE_URL not set; skipping integration test")
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

	raw, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	pool := &db.Pool{Pool: raw}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(context.Background(), `TRUNCATE rescreen_flags, holds, screening_queue, screening_results, screening_result_invalidations`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

// fakeOrderLister serves one scripted page per state, and records which
// states it was actually asked for.
type fakeOrderLister struct {
	pages   map[string][]ledgerclient.OrderRef
	queried []string
}

func newFakeOrderLister() *fakeOrderLister {
	return &fakeOrderLister{pages: make(map[string][]ledgerclient.OrderRef)}
}

func (f *fakeOrderLister) setPage(state string, refs []ledgerclient.OrderRef) {
	f.pages[state] = refs
}

func (f *fakeOrderLister) ListOrdersByState(ctx context.Context, state, cursor string) ([]ledgerclient.OrderRef, string, error) {
	f.queried = append(f.queried, state)
	if cursor != "" {
		// Every fixture in this file fits on one page.
		return nil, cursor, nil
	}
	return f.pages[state], "done:" + state, nil
}

func ref(orderID int64, externalID, address string) ledgerclient.OrderRef {
	return ledgerclient.OrderRef{OrderID: orderID, ExternalID: externalID, SenderAddress: &address}
}

func seedPreviousVerdict(t *testing.T, pool *db.Pool, address string, flagged bool, riskScore float64) {
	t.Helper()
	v := provider.Verdict{RiskScore: riskScore, Flagged: flagged, ProviderName: "mock", CheckedAt: time.Now().UTC()}
	if _, err := cache.Put(context.Background(), pool, "mock", address, v, time.Hour); err != nil {
		t.Fatalf("seeding previous verdict for %s: %v", address, err)
	}
}

func testConfig() rescreen.Config {
	return rescreen.Config{ProviderName: "mock", Timeout: time.Second, Thresholds: verdict.DefaultThresholds, TTL: cache.DefaultTTLConfig}
}

func TestRunTick_CleanReScreenProducesNoFlag(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const address = "0xRescreenCleanAddr00000000000001"
	seedPreviousVerdict(t, pool, address, false, 0.1)

	lister := newFakeOrderLister()
	lister.setPage("screened", []ledgerclient.OrderRef{ref(1, "ext-clean-1", address)})
	mock := provider.NewMockProvider(1) // this address's natural deterministic score is not forced flagged

	if err := rescreen.RunTick(ctx, pool, lister, mock, testConfig()); err != nil {
		t.Fatalf("RunTick: %v", err)
	}

	flags, err := rescreen.ListUnresolved(ctx, pool)
	if err != nil {
		t.Fatalf("ListUnresolved: %v", err)
	}
	for _, f := range flags {
		if f.ExternalID == "ext-clean-1" {
			t.Fatalf("a clean re-screen produced a flag: %+v", f)
		}
	}
}

func TestRunTick_FlaggedReScreenProducesExactlyOneFlagPerAffectedOrder(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const address = "0xRescreenFlagAddr000000000000002"
	seedPreviousVerdict(t, pool, address, false, 0.1) // previously clean -- Pass

	lister := newFakeOrderLister()
	lister.setPage("screened", []ledgerclient.OrderRef{
		ref(10, "ext-flag-10", address),
		ref(11, "ext-flag-11", address), // same sender, two orders -- both must be flagged
	})
	mock := provider.NewMockProvider(1)
	mock.ForceFlagged(address) // fresh check now comes back flagged

	if err := rescreen.RunTick(ctx, pool, lister, mock, testConfig()); err != nil {
		t.Fatalf("RunTick: %v", err)
	}

	flags, err := rescreen.ListUnresolved(ctx, pool)
	if err != nil {
		t.Fatalf("ListUnresolved: %v", err)
	}
	found := map[string]bool{}
	for _, f := range flags {
		found[f.ExternalID] = true
		if f.OrderStateAtDetection != "screened" {
			t.Errorf("flag for %s has order_state_at_detection %q, want screened", f.ExternalID, f.OrderStateAtDetection)
		}
		if f.PreviousVerdictID == 0 || f.NewVerdictID == 0 {
			t.Errorf("flag for %s has zero-value verdict ids: %+v", f.ExternalID, f)
		}
		if f.PreviousVerdictID == f.NewVerdictID {
			t.Errorf("flag for %s has the same previous and new verdict id, want two distinct rows", f.ExternalID)
		}
	}
	if !found["ext-flag-10"] || !found["ext-flag-11"] {
		t.Fatalf("expected flags for both orders sharing the address, got %+v", flags)
	}
	if len(flags) != 2 {
		t.Fatalf("got %d flags, want exactly 2 (one per affected order)", len(flags))
	}

	if got := mock.ScreenCallCount(address); got != 1 {
		t.Fatalf("provider.Screen called %d times for one shared address, want exactly 1", got)
	}
}

func TestRunTick_OnlyQueriesScreenedAndDispatchingNeverTerminalStates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	lister := newFakeOrderLister()
	mock := provider.NewMockProvider(1)

	if err := rescreen.RunTick(ctx, pool, lister, mock, testConfig()); err != nil {
		t.Fatalf("RunTick: %v", err)
	}

	queried := map[string]bool{}
	for _, s := range lister.queried {
		queried[s] = true
	}
	if !queried["screened"] || !queried["dispatching"] {
		t.Fatalf("queried states = %v, want both screened and dispatching", lister.queried)
	}
	for _, terminal := range []string{"settled", "refunded", "expired", "funded", "held", "quoted"} {
		if queried[terminal] {
			t.Fatalf("RunTick queried state %q, which must never be re-screened", terminal)
		}
	}
}

func TestRunTick_NoPreviousVerdictSkipsSilently(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const address = "0xRescreenNoPriorAddr0000000000003"
	// Deliberately no seedPreviousVerdict call.

	lister := newFakeOrderLister()
	lister.setPage("dispatching", []ledgerclient.OrderRef{ref(20, "ext-no-prior", address)})
	mock := provider.NewMockProvider(1)
	mock.ForceFlagged(address)

	if err := rescreen.RunTick(ctx, pool, lister, mock, testConfig()); err != nil {
		t.Fatalf("RunTick: %v", err)
	}

	if got := mock.ScreenCallCount(address); got != 0 {
		t.Fatalf("provider.Screen called %d times with no previous verdict to compare against, want 0", got)
	}
	flags, err := rescreen.ListUnresolved(ctx, pool)
	if err != nil {
		t.Fatalf("ListUnresolved: %v", err)
	}
	for _, f := range flags {
		if f.ExternalID == "ext-no-prior" {
			t.Fatal("a flag was recorded despite no previous verdict existing to compare against")
		}
	}
}

func TestResolve_RequiresNonEmptyResolutionAndIsNotIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const address = "0xRescreenResolveAddr0000000000004"
	seedPreviousVerdict(t, pool, address, false, 0.1)

	lister := newFakeOrderLister()
	lister.setPage("screened", []ledgerclient.OrderRef{ref(30, "ext-resolve", address)})
	mock := provider.NewMockProvider(1)
	mock.ForceFlagged(address)
	if err := rescreen.RunTick(ctx, pool, lister, mock, testConfig()); err != nil {
		t.Fatalf("RunTick: %v", err)
	}

	flags, err := rescreen.ListUnresolved(ctx, pool)
	if err != nil {
		t.Fatalf("ListUnresolved: %v", err)
	}
	var flagID int64
	for _, f := range flags {
		if f.ExternalID == "ext-resolve" {
			flagID = f.ID
		}
	}
	if flagID == 0 {
		t.Fatal("setup: flag for ext-resolve was not created")
	}

	if err := rescreen.Resolve(ctx, pool, flagID, ""); err == nil {
		t.Fatal("expected an error resolving with an empty resolution")
	}
	if err := rescreen.Resolve(ctx, pool, flagID, "operator:alice confirmed clean, releasing manually via C3.6"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := rescreen.Resolve(ctx, pool, flagID, "second attempt"); err == nil {
		t.Fatal("expected an error resolving an already-resolved flag a second time")
	}

	got, err := rescreen.Get(ctx, pool, flagID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Resolution == nil || *got.Resolution != "operator:alice confirmed clean, releasing manually via C3.6" {
		t.Fatalf("resolution = %v, want the first Resolve call's text", got.Resolution)
	}

	stillUnresolved, err := rescreen.ListUnresolved(ctx, pool)
	if err != nil {
		t.Fatalf("ListUnresolved: %v", err)
	}
	for _, f := range stillUnresolved {
		if f.ID == flagID {
			t.Fatal("ListUnresolved returned a flag that was just resolved")
		}
	}
}

func TestRunTick_MultiPagePagination(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// A lister whose first call for "screened" returns a page AND a
	// cursor, and whose second call (with that cursor) returns a second
	// page then stops -- proves collectOrders actually walks pages
	// rather than assuming everything fits on one.
	pagedLister := &multiPageLister{
		pages: [][]ledgerclient.OrderRef{
			{ref(40, "ext-page1", "0xRescreenPage1Addr00000000005")},
			{ref(41, "ext-page2", "0xRescreenPage2Addr00000000006")},
		},
	}
	seedPreviousVerdict(t, pool, "0xRescreenPage1Addr00000000005", false, 0.1)
	seedPreviousVerdict(t, pool, "0xRescreenPage2Addr00000000006", false, 0.1)
	mock := provider.NewMockProvider(1)
	mock.ForceFlagged("0xRescreenPage1Addr00000000005")
	mock.ForceFlagged("0xRescreenPage2Addr00000000006")

	if err := rescreen.RunTick(ctx, pool, pagedLister, mock, testConfig()); err != nil {
		t.Fatalf("RunTick: %v", err)
	}

	flags, err := rescreen.ListUnresolved(ctx, pool)
	if err != nil {
		t.Fatalf("ListUnresolved: %v", err)
	}
	found := map[string]bool{}
	for _, f := range flags {
		found[f.ExternalID] = true
	}
	if !found["ext-page1"] || !found["ext-page2"] {
		t.Fatalf("expected flags for orders on both pages, got %+v", flags)
	}
}

// multiPageLister returns one page per call for "screened", then an
// empty page; "dispatching" is always empty.
type multiPageLister struct {
	pages [][]ledgerclient.OrderRef
	calls int
}

func (m *multiPageLister) ListOrdersByState(ctx context.Context, state, cursor string) ([]ledgerclient.OrderRef, string, error) {
	if state != "screened" {
		return nil, "", nil
	}
	if m.calls >= len(m.pages) {
		return nil, fmt.Sprintf("cursor-%d", m.calls), nil
	}
	page := m.pages[m.calls]
	m.calls++
	return page, fmt.Sprintf("cursor-%d", m.calls), nil
}
