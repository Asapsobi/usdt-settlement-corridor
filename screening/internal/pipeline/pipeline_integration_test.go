//go:build integration

// Requires a real, reachable Postgres 16 instance (SCREENING_TEST_DATABASE_URL);
// run via `make test-integration`. Uses a fake Reporter, so these
// exercise the pipeline's own logic (cache hit skips the provider,
// illegal_transition marks DONE without retrying, a provider error
// fails closed) against real cache/discovery tables, without needing a
// live C1. See pipeline_live_test.go for the full-stack proof against a
// real C1.
package pipeline_test

import (
	"context"
	"database/sql"
	"errors"
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
	"screening/internal/discovery"
	"screening/internal/ledgerclient"
	"screening/internal/pipeline"
	"screening/internal/provider"
	"screening/internal/verdict"
)

func testPool(t *testing.T) *pgxpool.Pool {
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

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(context.Background(), `TRUNCATE rescreen_flags, holds, screening_queue, screening_results, screening_result_invalidations`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

func testConfig() pipeline.Config {
	return pipeline.Config{
		ProviderName: "mock",
		Timeout:      time.Second,
		Thresholds:   verdict.DefaultThresholds,
		TTL:          cache.DefaultTTLConfig,
	}
}

// fakeReporter is a pipeline.Reporter whose per-order outcome is
// scripted by the test, and which records every Decision it was called
// with -- so a test can assert exactly what the pipeline decided,
// independent of any real C1's own transition table.
type fakeReporter struct {
	fail      map[string]error
	decisions map[string]verdict.Decision
	callCount map[string]int
}

func newFakeReporter() *fakeReporter {
	return &fakeReporter{
		fail:      make(map[string]error),
		decisions: make(map[string]verdict.Decision),
		callCount: make(map[string]int),
	}
}

func (f *fakeReporter) failWith(externalID string, err error) { f.fail[externalID] = err }

func (f *fakeReporter) ReportVerdict(ctx context.Context, externalID string, decision verdict.Decision) error {
	f.callCount[externalID]++
	f.decisions[externalID] = decision
	if err, ok := f.fail[externalID]; ok {
		return err
	}
	return nil
}

func enqueue(t *testing.T, pool *pgxpool.Pool, orderID int64, externalID, senderAddress string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO screening_queue (order_id, external_id, sender_address) VALUES ($1, $2, $3)
	`, orderID, externalID, senderAddress)
	if err != nil {
		t.Fatalf("seeding screening_queue for order %d: %v", orderID, err)
	}
}

func queueStatus(t *testing.T, pool *pgxpool.Pool, orderID int64) discovery.Status {
	t.Helper()
	entry, err := discovery.Get(context.Background(), pool, orderID)
	if err != nil {
		t.Fatalf("Get(%d): %v", orderID, err)
	}
	return entry.Status
}

func TestScreenAndReport_CacheHitSkipsProviderCall(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	reporter := newFakeReporter()
	mock := provider.NewMockProvider(1)
	cfg := testConfig()

	const address = "0xCacheHitAddress0000000000000000001"
	enqueue(t, pool, 1, "ext-cachehit-1", address)
	enqueue(t, pool, 2, "ext-cachehit-2", address)

	e1, err := discovery.Get(ctx, pool, 1)
	if err != nil {
		t.Fatalf("Get(1): %v", err)
	}
	if err := pipeline.ScreenAndReport(ctx, pool, mock, reporter, cfg, e1); err != nil {
		t.Fatalf("ScreenAndReport (1st order): %v", err)
	}

	e2, err := discovery.Get(ctx, pool, 2)
	if err != nil {
		t.Fatalf("Get(2): %v", err)
	}
	if err := pipeline.ScreenAndReport(ctx, pool, mock, reporter, cfg, e2); err != nil {
		t.Fatalf("ScreenAndReport (2nd order, same sender): %v", err)
	}

	if got := mock.ScreenCallCount(address); got != 1 {
		t.Fatalf("provider.Screen called %d times for %s, want exactly 1 (second order should hit the cache)", got, address)
	}
	if reporter.callCount["ext-cachehit-1"] != 1 || reporter.callCount["ext-cachehit-2"] != 1 {
		t.Fatalf("expected exactly one ReportVerdict call per order, got %+v", reporter.callCount)
	}
	if queueStatus(t, pool, 1) != discovery.Done || queueStatus(t, pool, 2) != discovery.Done {
		t.Fatal("both orders should be DONE after a successful report")
	}
}

func TestScreenAndReport_IllegalTransitionMarksDoneWithoutRetrying(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	reporter := newFakeReporter()
	reporter.failWith("ext-illegal", fmt.Errorf("wrapped: %w", ledgerclient.ErrIllegalTransition))
	mock := provider.NewMockProvider(1)
	cfg := testConfig()

	enqueue(t, pool, 10, "ext-illegal", "0xIllegalTransitionAddr00000000001")
	entry, err := discovery.Get(ctx, pool, 10)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if err := pipeline.ScreenAndReport(ctx, pool, mock, reporter, cfg, entry); err != nil {
		t.Fatalf("ScreenAndReport should absorb illegal_transition, got error: %v", err)
	}
	if queueStatus(t, pool, 10) != discovery.Done {
		t.Fatal("order should be marked DONE after illegal_transition, not left PENDING for a retry")
	}
	if reporter.callCount["ext-illegal"] != 1 {
		t.Fatalf("ReportVerdict called %d times, want exactly 1 (no retry-storm)", reporter.callCount["ext-illegal"])
	}
}

func TestScreenAndReport_ProviderErrorFailsClosed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	reporter := newFakeReporter()
	mock := provider.NewMockProvider(1)
	const address = "0xProviderErrorAddress00000000000002"
	mock.ForceTimeout(address)
	cfg := testConfig()
	cfg.Timeout = 20 * time.Millisecond
	cfg.Retries = 1 // this test is about the exhausted->fail-closed outcome, not retry count

	enqueue(t, pool, 20, "ext-provider-error", address)
	entry, err := discovery.Get(ctx, pool, 20)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if err := pipeline.ScreenAndReport(ctx, pool, mock, reporter, cfg, entry); err != nil {
		t.Fatalf("ScreenAndReport: %v", err)
	}

	decision, ok := reporter.decisions["ext-provider-error"]
	if !ok {
		t.Fatal("ReportVerdict was never called")
	}
	if decision.Classification != verdict.Hold || decision.ReasonCode != verdict.ReasonHoldUnavailable {
		t.Fatalf("decision = %+v, want Hold/screening_hold_unavailable (fail-closed)", decision)
	}
	if decision.ScreeningResultID != 0 {
		t.Fatalf("decision.ScreeningResultID = %d, want 0 -- a provider failure has no backing screening_results row", decision.ScreeningResultID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM screening_results WHERE sender_address = $1`, address).Scan(&count); err != nil {
		t.Fatalf("counting screening_results: %v", err)
	}
	if count != 0 {
		t.Fatalf("a failed provider call should never write a screening_results row, found %d", count)
	}

	// C3.6: a Hold decision must open a holds row before it's ever
	// reported to C1 -- see ScreenAndReport's own doc comment for why
	// that ordering matters.
	var holdCount int
	var holdStatus string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(status) FROM holds WHERE order_id = $1`, 20).Scan(&holdCount, &holdStatus); err != nil {
		t.Fatalf("counting holds: %v", err)
	}
	if holdCount != 1 {
		t.Fatalf("got %d holds rows for order 20, want exactly 1", holdCount)
	}
	if holdStatus != "OPEN" {
		t.Fatalf("hold status = %q, want OPEN", holdStatus)
	}
}

// fakeMetrics counts MetricsRecorder calls -- used to verify
// screening_vendor_unavailable_total fires on every exhausted-retries
// event regardless of OutagePolicy, per C3.5's own CONFIG section, and
// (C3.8) that verdicts and hold-opens are counted by classification.
type fakeMetrics struct {
	vendorUnavailableCalls int
	verdictsReported       map[verdict.Classification]int
	holdOpenedCalls        int
}

func (m *fakeMetrics) VendorUnavailable() { m.vendorUnavailableCalls++ }

func (m *fakeMetrics) VerdictReported(classification verdict.Classification) {
	if m.verdictsReported == nil {
		m.verdictsReported = make(map[verdict.Classification]int)
	}
	m.verdictsReported[classification]++
}

func (m *fakeMetrics) HoldOpened() { m.holdOpenedCalls++ }

func TestScreenAndReport_FailOpenReportsAuditablePassAndReachesScreened(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	reporter := newFakeReporter()
	mock := provider.NewMockProvider(1)
	const address = "0xFailOpenAddress000000000000000003"
	mock.ForceTimeout(address)
	metrics := &fakeMetrics{}
	cfg := testConfig()
	cfg.Timeout = 20 * time.Millisecond
	cfg.Retries = 1
	cfg.OutagePolicy = provider.FailOpen
	cfg.Metrics = metrics

	enqueue(t, pool, 40, "ext-fail-open", address)
	entry, err := discovery.Get(ctx, pool, 40)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if err := pipeline.ScreenAndReport(ctx, pool, mock, reporter, cfg, entry); err != nil {
		t.Fatalf("ScreenAndReport: %v", err)
	}

	decision, ok := reporter.decisions["ext-fail-open"]
	if !ok {
		t.Fatal("ReportVerdict was never called")
	}
	if decision.Classification != verdict.Pass {
		t.Fatalf("decision.Classification = %v, want Pass (fail-open)", decision.Classification)
	}
	if decision.ReasonCode != verdict.ReasonPassVendorUnavailable {
		t.Fatalf("decision.ReasonCode = %q, want %q -- must be auditable, never a plain screening_pass",
			decision.ReasonCode, verdict.ReasonPassVendorUnavailable)
	}
	if decision.ScreeningResultID != 0 {
		t.Fatalf("decision.ScreeningResultID = %d, want 0 -- no real vendor response to cache", decision.ScreeningResultID)
	}
	if metrics.vendorUnavailableCalls != 1 {
		t.Fatalf("VendorUnavailable() called %d times, want exactly 1", metrics.vendorUnavailableCalls)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM screening_results WHERE sender_address = $1`, address).Scan(&count); err != nil {
		t.Fatalf("counting screening_results: %v", err)
	}
	if count != 0 {
		t.Fatalf("a fail-open outage placeholder should never write a screening_results row, found %d", count)
	}
}

func TestScreenAndReport_VendorUnavailableMetricFiresForFailClosedToo(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	reporter := newFakeReporter()
	mock := provider.NewMockProvider(1)
	const address = "0xFailClosedMetricAddr00000000004"
	mock.ForceTimeout(address)
	metrics := &fakeMetrics{}
	cfg := testConfig()
	cfg.Timeout = 20 * time.Millisecond
	cfg.Retries = 1
	cfg.Metrics = metrics // OutagePolicy left at zero value: FailClosed

	enqueue(t, pool, 41, "ext-fail-closed-metric", address)
	entry, err := discovery.Get(ctx, pool, 41)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := pipeline.ScreenAndReport(ctx, pool, mock, reporter, cfg, entry); err != nil {
		t.Fatalf("ScreenAndReport: %v", err)
	}
	if metrics.vendorUnavailableCalls != 1 {
		t.Fatalf("VendorUnavailable() called %d times under FailClosed, want exactly 1 -- the metric must fire regardless of policy", metrics.vendorUnavailableCalls)
	}
}

// TestScreenAndReport_SucceedsOnRetryUsesRealVerdictNotOutagePath is
// C3.5's own named acceptance criterion, exercised through the full
// pipeline (not just provider.ScreenWithPolicy in isolation): a
// provider that fails N-1 times then succeeds must be classified from
// that real result, cached normally, never routed through the outage
// placeholder.
func TestScreenAndReport_SucceedsOnRetryUsesRealVerdictNotOutagePath(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	reporter := newFakeReporter()
	mock := &flakyThenRecoverMock{recoverAfter: 2, inner: provider.NewMockProvider(1)}
	const address = "0xFlakyRecoverAddress0000000000005"
	cfg := testConfig()
	cfg.Timeout = time.Second
	cfg.Retries = 3

	enqueue(t, pool, 42, "ext-flaky-recover", address)
	entry, err := discovery.Get(ctx, pool, 42)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := pipeline.ScreenAndReport(ctx, pool, mock, reporter, cfg, entry); err != nil {
		t.Fatalf("ScreenAndReport: %v", err)
	}

	decision, ok := reporter.decisions["ext-flaky-recover"]
	if !ok {
		t.Fatal("ReportVerdict was never called")
	}
	if decision.ReasonCode == verdict.ReasonHoldUnavailable || decision.ReasonCode == verdict.ReasonPassVendorUnavailable {
		t.Fatalf("decision = %+v, took the outage path despite the 3rd attempt succeeding", decision)
	}
	if decision.ScreeningResultID == 0 {
		t.Fatal("decision.ScreeningResultID = 0, want a real screening_results row from the successful attempt")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM screening_results WHERE sender_address = $1`, address).Scan(&count); err != nil {
		t.Fatalf("counting screening_results: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 cached screening_results row from the successful attempt, found %d", count)
	}
}

// flakyThenRecoverMock fails its first N Screen calls with a timeout-like
// error, then delegates to a real MockProvider from then on.
type flakyThenRecoverMock struct {
	recoverAfter int
	calls        int
	inner        *provider.MockProvider
}

func (m *flakyThenRecoverMock) Screen(ctx context.Context, address string) (provider.Verdict, error) {
	m.calls++
	if m.calls <= m.recoverAfter {
		return provider.Verdict{}, errors.New("flakyThenRecoverMock: still failing")
	}
	return m.inner.Screen(ctx, address)
}

func TestScreenAndReport_ReporterFailureLeavesRowPendingForRetry(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	reporter := newFakeReporter()
	reporter.failWith("ext-transient", errors.New("network blip"))
	mock := provider.NewMockProvider(1)
	cfg := testConfig()

	enqueue(t, pool, 30, "ext-transient", "0xTransientFailureAddr000000000003")
	entry, err := discovery.Get(ctx, pool, 30)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if err := pipeline.ScreenAndReport(ctx, pool, mock, reporter, cfg, entry); err == nil {
		t.Fatal("expected an error for a transient reporter failure")
	}
	if queueStatus(t, pool, 30) != discovery.Pending {
		t.Fatal("a transient reporting failure must leave the row PENDING for the next tick, not DONE")
	}
}
