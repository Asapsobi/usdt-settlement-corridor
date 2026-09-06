//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package cache_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"screening/internal/cache"
	"screening/internal/provider"
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
	return pool
}

func uniqueAddress(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("0xtest-%s-%d", t.Name(), time.Now().UnixNano())
}

func sampleVerdict() provider.Verdict {
	return provider.Verdict{
		RiskScore:    0.1,
		Flagged:      false,
		ReasonCodes:  nil,
		RawResponse:  []byte(`{"ok":true}`),
		ProviderName: "mock",
		CheckedAt:    time.Now().UTC().Truncate(time.Microsecond),
	}
}

func TestGet_UnexpiredRowIsReturned(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	address := uniqueAddress(t)
	v := sampleVerdict()

	putID, err := cache.Put(ctx, pool, "mock", address, v, time.Hour)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if putID == 0 {
		t.Fatal("Put returned id 0, want a real row id")
	}

	got, err := cache.Get(ctx, pool, "mock", address)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil for an unexpired row")
	}
	if got.RiskScore != v.RiskScore || got.Flagged != v.Flagged {
		t.Fatalf("Get returned %+v, want a match for %+v", got, v)
	}
	if got.ID != putID {
		t.Fatalf("Get returned id %d, want the id Put reported (%d)", got.ID, putID)
	}
}

func TestGet_ExpiredRowReturnsNilButIsNotDeleted(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	address := uniqueAddress(t)
	v := sampleVerdict()
	v.CheckedAt = time.Now().UTC().Add(-2 * time.Hour)

	if _, err := cache.Put(ctx, pool, "mock", address, v, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := cache.Get(ctx, pool, "mock", address)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Fatalf("Get returned an expired row: %+v", got)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM screening_results WHERE sender_address = $1`, address).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expired row was deleted -- history must be kept, got %d rows", count)
	}
}

func TestPut_NeverOverwritesAlwaysInserts(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	address := uniqueAddress(t)

	first := sampleVerdict()
	first.RiskScore = 0.1
	second := sampleVerdict()
	second.RiskScore = 0.9
	second.CheckedAt = first.CheckedAt.Add(time.Second)

	firstID, err := cache.Put(ctx, pool, "mock", address, first, time.Hour)
	if err != nil {
		t.Fatalf("Put (first): %v", err)
	}
	secondID, err := cache.Put(ctx, pool, "mock", address, second, time.Hour)
	if err != nil {
		t.Fatalf("Put (second): %v", err)
	}
	if firstID == secondID {
		t.Fatalf("two Puts against the same key returned the same id (%d) -- each must be its own row", firstID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM screening_results WHERE sender_address = $1`, address).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("got %d rows for two Puts against the same key, want 2 (a full history, never an overwrite)", count)
	}

	got, err := cache.Get(ctx, pool, "mock", address)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.RiskScore != second.RiskScore {
		t.Fatalf("Get should return the freshest row (risk_score=%v), got %+v", second.RiskScore, got)
	}
}

func TestInvalidate_HidesResultWithoutDeletingHistory(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	address := uniqueAddress(t)
	v := sampleVerdict()

	if _, err := cache.Put(ctx, pool, "mock", address, v, time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got, err := cache.Get(ctx, pool, "mock", address); err != nil || got == nil {
		t.Fatalf("setup: expected a cache hit before invalidation, got %+v, err=%v", got, err)
	}

	if err := cache.Invalidate(ctx, pool, "mock", address, "operator distrusts this verdict", "operator:alice"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	got, err := cache.Get(ctx, pool, "mock", address)
	if err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if got != nil {
		t.Fatalf("Get returned a result for an invalidated key (expires_at hadn't naturally passed): %+v", got)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM screening_results WHERE sender_address = $1`, address).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("invalidation deleted or mutated the underlying row; got %d rows, want 1", count)
	}

	// A fresh check after invalidation is trusted again.
	fresh := sampleVerdict()
	fresh.CheckedAt = time.Now().UTC()
	if _, err := cache.Put(ctx, pool, "mock", address, fresh, time.Hour); err != nil {
		t.Fatalf("Put (fresh, post-invalidation): %v", err)
	}
	got, err = cache.Get(ctx, pool, "mock", address)
	if err != nil {
		t.Fatalf("Get after fresh Put: %v", err)
	}
	if got == nil {
		t.Fatal("a fresh Put after invalidation should be trusted, got nil")
	}
}

func TestInvalidate_RequiresReasonAndActor(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	address := uniqueAddress(t)

	if err := cache.Invalidate(ctx, pool, "mock", address, "", "operator:alice"); err == nil {
		t.Fatal("expected an error for an empty reason")
	}
	if err := cache.Invalidate(ctx, pool, "mock", address, "some reason", ""); err == nil {
		t.Fatal("expected an error for an empty actor")
	}
}

func TestGet_ConcurrentReadsDuringInFlightPut(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	address := uniqueAddress(t)

	// Seed one row so concurrent Gets have something to legitimately
	// return, then hammer Get concurrently with a second Put in flight.
	if _, err := cache.Put(ctx, pool, "mock", address, sampleVerdict(), time.Hour); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 101)

	wg.Add(1)
	go func() {
		defer wg.Done()
		v := sampleVerdict()
		v.CheckedAt = time.Now().UTC().Add(time.Millisecond)
		if _, err := cache.Put(ctx, pool, "mock", address, v, time.Hour); err != nil {
			errs <- fmt.Errorf("concurrent Put: %w", err)
		}
	}()

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cache.Get(ctx, pool, "mock", address); err != nil {
				errs <- fmt.Errorf("concurrent Get: %w", err)
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
