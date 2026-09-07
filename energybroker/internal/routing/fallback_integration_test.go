//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package routing

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"energybroker/internal/db"
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

	if _, err := pool.Exec(ctx, `TRUNCATE manual_fallback_events RESTART IDENTITY`); err != nil {
		t.Fatalf("truncating manual_fallback_events: %v", err)
	}
	return pool
}

func TestOnFallbackTriggered_DeduplicatesAgainstAnOpenEvent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRouter(nil, pool, 1)

	orderA := int64(101)
	orderB := int64(102)

	first, err := r.OnFallbackTriggered(ctx, ReasonFallbackLadder, &orderA)
	if err != nil {
		t.Fatalf("OnFallbackTriggered (1st): %v", err)
	}
	second, err := r.OnFallbackTriggered(ctx, ReasonFallbackLadder, &orderB)
	if err != nil {
		t.Fatalf("OnFallbackTriggered (2nd, same outage window, different order): %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("2nd trigger created a new event %d, want the same open event %d", second.ID, first.ID)
	}
	if second.OrderID == nil || *second.OrderID != orderA {
		t.Fatalf("returned event's order_id = %v, want the FIRST trigger's own %d, unchanged by the 2nd call", second.OrderID, orderA)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM manual_fallback_events WHERE reason = $1`, FallbackReasonAllUnhealthy).Scan(&count); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if count != 1 {
		t.Fatalf("manual_fallback_events rows for %s = %d, want exactly 1", FallbackReasonAllUnhealthy, count)
	}
}

// TestOnFallbackTriggered_ConcurrentTriggersStillProduceExactlyOneEvent
// is this chunk's own acceptance criterion stated precisely: several
// reservations hitting the same outage window concurrently (not one
// after another) must still produce exactly one row -- proving the
// database-level partial unique index, not just the read-then-write
// check, is what actually prevents duplicates under a real race.
func TestOnFallbackTriggered_ConcurrentTriggersStillProduceExactlyOneEvent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRouter(nil, pool, 1)

	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	ids := make(map[int64]bool)
	var errs []error

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(orderID int64) {
			defer wg.Done()
			event, err := r.OnFallbackTriggered(ctx, ReasonFallbackLadder, &orderID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			ids[event.ID] = true
		}(int64(i))
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d error(s) during concurrent OnFallbackTriggered, first: %v", len(errs), errs[0])
	}
	if len(ids) != 1 {
		t.Fatalf("distinct event ids returned across %d concurrent triggers = %d, want exactly 1", n, len(ids))
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM manual_fallback_events`).Scan(&count); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if count != 1 {
		t.Fatalf("manual_fallback_events rows = %d, want exactly 1", count)
	}
}

func TestOnFallbackTriggered_DifferentReasonsGetSeparateEvents(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRouter(nil, pool, 1)

	unhealthy, err := r.OnFallbackTriggered(ctx, ReasonFallbackLadder, nil)
	if err != nil {
		t.Fatalf("OnFallbackTriggered (all_unhealthy): %v", err)
	}
	overCeiling, err := r.OnFallbackTriggered(ctx, ReasonManualRequired, nil)
	if err != nil {
		t.Fatalf("OnFallbackTriggered (all_over_ceiling): %v", err)
	}
	if unhealthy.ID == overCeiling.ID {
		t.Fatal("two different reasons must never share one event")
	}
	if unhealthy.OrderID != nil || overCeiling.OrderID != nil {
		t.Fatal("a nil orderID (buffer's own background loop) must be recorded as null, not defaulted to some sentinel")
	}
}

func TestOnFallbackTriggered_RejectsNonFallbackReason(t *testing.T) {
	pool := testPool(t)
	r := NewRouter(nil, pool, 1)
	_, err := r.OnFallbackTriggered(context.Background(), ReasonWeighted, nil)
	if err == nil {
		t.Fatal("expected an error for ReasonWeighted, which is not a fallback reason")
	}
}

func TestResolve_MarksResolvedAndAllowsAFreshEventLater(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRouter(nil, pool, 1)

	event, err := r.OnFallbackTriggered(ctx, ReasonFallbackLadder, nil)
	if err != nil {
		t.Fatalf("OnFallbackTriggered: %v", err)
	}
	if err := r.Resolve(ctx, event.ID, "vendors recovered on their own"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	var resolvedAt *string
	var resolution *string
	if err := pool.QueryRow(ctx, `SELECT resolved_at::text, resolution FROM manual_fallback_events WHERE id = $1`, event.ID).Scan(&resolvedAt, &resolution); err != nil {
		t.Fatalf("fetching resolved event: %v", err)
	}
	if resolvedAt == nil {
		t.Fatal("resolved_at is still null after Resolve")
	}
	if resolution == nil || *resolution != "vendors recovered on their own" {
		t.Fatalf("resolution = %v, want the exact string passed to Resolve", resolution)
	}

	// The outage recurring after being marked resolved must open a NEW
	// event, not silently reuse the now-closed one -- a real recurrence
	// deserves its own alert, the same way a real paging system re-pages
	// on recurrence after an incident is closed.
	fresh, err := r.OnFallbackTriggered(ctx, ReasonFallbackLadder, nil)
	if err != nil {
		t.Fatalf("OnFallbackTriggered (recurrence after resolution): %v", err)
	}
	if fresh.ID == event.ID {
		t.Fatal("a trigger after the prior event was resolved must open a new event, not return the closed one")
	}
}

func TestResolve_TwiceIsAnError(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	r := NewRouter(nil, pool, 1)

	event, err := r.OnFallbackTriggered(ctx, ReasonManualRequired, nil)
	if err != nil {
		t.Fatalf("OnFallbackTriggered: %v", err)
	}
	if err := r.Resolve(ctx, event.ID, "resolved once"); err != nil {
		t.Fatalf("Resolve (1st): %v", err)
	}
	if err := r.Resolve(ctx, event.ID, "resolved again"); err == nil {
		t.Fatal("expected an error resolving an already-resolved event twice, got nil")
	}
}

func TestResolve_UnknownEventIsAnError(t *testing.T) {
	pool := testPool(t)
	r := NewRouter(nil, pool, 1)

	err := r.Resolve(context.Background(), 999999, "resolved")
	if !errors.Is(err, ErrFallbackEventNotFound) {
		t.Fatalf("Resolve error = %v, want ErrFallbackEventNotFound", err)
	}
}
