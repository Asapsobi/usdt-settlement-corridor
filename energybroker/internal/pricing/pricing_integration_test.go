//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package pricing

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

	"energybroker/internal/db"
	"energybroker/internal/provider"
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

	// Every test in this file assumes a clean slate -- this package's own
	// tests are the only thing that ever writes to price_observations in
	// the test database, so truncating here keeps each test's assertions
	// exact rather than "at least" checks against a shared, growing table.
	if _, err := pool.Exec(ctx, `TRUNCATE price_observations`); err != nil {
		t.Fatalf("truncating price_observations: %v", err)
	}

	return pool
}

func TestPollAll_CapturesAllProvidersEveryTick(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	providers := map[string]provider.EnergyProvider{
		provider.Tronsell: provider.NewMockProvider(provider.Tronsell, 1, 24.0),
		provider.Netts:    provider.NewMockProvider(provider.Netts, 2, 28.0),
		provider.Catfee:   provider.NewMockProvider(provider.Catfee, 3, 30.0),
	}
	poller := NewPoller(pool, providers, time.Hour)

	if err := poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll: %v", err)
	}

	for name := range providers {
		q, err := poller.CurrentPrice(ctx, name)
		if err != nil {
			t.Fatalf("CurrentPrice(%s): %v", name, err)
		}
		if q.ProviderName != name {
			t.Errorf("CurrentPrice(%s).ProviderName = %q, want %q", name, q.ProviderName, name)
		}
		if q.PricePerUnitSun <= 0 {
			t.Errorf("CurrentPrice(%s).PricePerUnitSun = %v, want > 0", name, q.PricePerUnitSun)
		}
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM price_observations`).Scan(&count); err != nil {
		t.Fatalf("counting price_observations: %v", err)
	}
	if count != len(providers) {
		t.Fatalf("price_observations has %d rows after one tick across %d providers, want exactly %d", count, len(providers), len(providers))
	}

	// A second tick must capture a fresh row per provider too -- this
	// table is append-only, never an upsert.
	if err := poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll (2nd tick): %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM price_observations`).Scan(&count); err != nil {
		t.Fatalf("counting price_observations after 2nd tick: %v", err)
	}
	if count != 2*len(providers) {
		t.Fatalf("price_observations has %d rows after two ticks, want exactly %d", count, 2*len(providers))
	}
}

func TestCurrentPrice_ProviderErroringOnQuoteIsExcludedStartingThatTick(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	mock := provider.NewMockProvider(provider.Tronsell, 1, 24.0)
	providers := map[string]provider.EnergyProvider{provider.Tronsell: mock}
	// A staleness window generous enough that only the immediate
	// unhealthy flag -- never staleness -- could explain a failure here.
	poller := NewPoller(pool, providers, time.Hour)

	if err := poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll (1st, healthy): %v", err)
	}
	if _, err := poller.CurrentPrice(ctx, provider.Tronsell); err != nil {
		t.Fatalf("CurrentPrice after a healthy poll: %v", err)
	}

	mock.ForceMalformed()
	if err := poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll (2nd, now erroring): %v", err)
	}

	_, err := poller.CurrentPrice(ctx, provider.Tronsell)
	if !errors.Is(err, ErrProviderUnhealthy) {
		t.Fatalf("CurrentPrice error = %v, want ErrProviderUnhealthy -- an erroring provider must be excluded starting from that exact tick, not eventually via staleness", err)
	}
	if errors.Is(err, ErrPriceStale) {
		t.Fatalf("CurrentPrice error = %v, wrongly also matches ErrPriceStale -- an active vendor error and a stale price are different facts", err)
	}

	// The old, still-fresh row must remain in the table for audit --
	// PollAll never deletes or overwrites a past observation.
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM price_observations WHERE provider_name = $1`, provider.Tronsell).Scan(&count); err != nil {
		t.Fatalf("counting price_observations: %v", err)
	}
	if count != 1 {
		t.Fatalf("price_observations rows for %s = %d, want 1 (the one successful poll; the failed poll writes nothing)", provider.Tronsell, count)
	}
}

func TestCurrentPrice_GoesStaleOnceWindowElapsesEvenAcrossAFreshPoller(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	mock := provider.NewMockProvider(provider.Netts, 1, 28.0)
	providers := map[string]provider.EnergyProvider{provider.Netts: mock}
	staleness := 50 * time.Millisecond
	poller := NewPoller(pool, providers, staleness)

	if err := poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll: %v", err)
	}
	if _, err := poller.CurrentPrice(ctx, provider.Netts); err != nil {
		t.Fatalf("CurrentPrice immediately after a fresh poll: %v", err)
	}

	time.Sleep(staleness + 30*time.Millisecond)

	// A brand new Poller, sharing nothing in-memory with the one that
	// did the polling above (simulating a process restart) -- staleness
	// must be caught from the database alone, not from any in-memory
	// health flag this new instance never had the chance to set.
	restarted := NewPoller(pool, providers, staleness)
	_, err := restarted.CurrentPrice(ctx, provider.Netts)
	if !errors.Is(err, ErrPriceStale) {
		t.Fatalf("CurrentPrice error = %v, want ErrPriceStale", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM price_observations WHERE provider_name = $1`, provider.Netts).Scan(&count); err != nil {
		t.Fatalf("counting price_observations: %v", err)
	}
	if count != 1 {
		t.Fatalf("price_observations rows for %s = %d, want the old row still present for audit", provider.Netts, count)
	}
}

func TestCurrentPrice_UnknownProviderIsStaleNotAPanic(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	poller := NewPoller(pool, map[string]provider.EnergyProvider{}, time.Hour)
	_, err := poller.CurrentPrice(ctx, "never-polled")
	if !errors.Is(err, ErrPriceStale) {
		t.Fatalf("CurrentPrice error = %v, want ErrPriceStale for a provider with no observation at all", err)
	}
}
