//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package dispatch_test

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

	"dispatcher/internal/db"
	"dispatcher/internal/dispatch"
)

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
		t.Fatalf("running migrations: %v", err)
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `TRUNCATE dispatch_state, dispatch_attempts, batch_queue, batches, slots`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

func TestCreate_IdempotentOnOrderID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := dispatch.NewStore(pool)

	first, err := store.Create(ctx, dispatch.Attempt{
		OrderID: 1, SlotID: 3, ConversionEntryKey: "dispatcher:enter_dispatching:1",
		Status: dispatch.StatusDispatching, EnteredDispatchingAt: time.Now().UTC().Truncate(time.Microsecond),
	})
	if err != nil {
		t.Fatalf("Create (1st): %v", err)
	}

	// A second Create for the same order, even with different-looking
	// fields, must return the ORIGINAL row unchanged -- this is the
	// local-storage half of the "crash between the C1 call succeeding
	// and this INSERT" recovery case, not a caller free to overwrite.
	second, err := store.Create(ctx, dispatch.Attempt{
		OrderID: 1, SlotID: 5, ConversionEntryKey: "dispatcher:enter_dispatching:1",
		Status: dispatch.StatusDispatching, EnteredDispatchingAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Create (2nd): %v", err)
	}
	if second.SlotID != first.SlotID || !second.EnteredDispatchingAt.Equal(first.EnteredDispatchingAt) {
		t.Fatalf("Create (2nd) = %+v, want the original row %+v unchanged", second, first)
	}
}

func TestGet_NotFound(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := dispatch.NewStore(pool)

	_, err := store.Get(ctx, 999)
	if !errors.Is(err, dispatch.ErrAttemptNotFound) {
		t.Fatalf("Get error = %v, want ErrAttemptNotFound", err)
	}
}

func TestMarkSettled_FromDispatching(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := dispatch.NewStore(pool)

	if _, err := store.Create(ctx, dispatch.Attempt{
		OrderID: 2, SlotID: 1, ConversionEntryKey: "dispatcher:enter_dispatching:2",
		Status: dispatch.StatusDispatching, EnteredDispatchingAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.MarkSettled(ctx, 2); err != nil {
		t.Fatalf("MarkSettled: %v", err)
	}
	got, err := store.Get(ctx, 2)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != dispatch.StatusSettled {
		t.Fatalf("Status = %q, want SETTLED", got.Status)
	}
}

func TestMarkHeld_FromDispatching(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := dispatch.NewStore(pool)

	if _, err := store.Create(ctx, dispatch.Attempt{
		OrderID: 3, SlotID: 1, ConversionEntryKey: "dispatcher:enter_dispatching:3",
		Status: dispatch.StatusDispatching, EnteredDispatchingAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.MarkHeld(ctx, 3); err != nil {
		t.Fatalf("MarkHeld: %v", err)
	}
	got, err := store.Get(ctx, 3)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != dispatch.StatusHeld {
		t.Fatalf("Status = %q, want HELD", got.Status)
	}
}

// TestMarkSettled_NotDispatchingRejected covers the one-way lifecycle:
// a row already SETTLED or HELD must never move again.
func TestMarkSettled_NotDispatchingRejected(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := dispatch.NewStore(pool)

	if _, err := store.Create(ctx, dispatch.Attempt{
		OrderID: 4, SlotID: 1, ConversionEntryKey: "dispatcher:enter_dispatching:4",
		Status: dispatch.StatusDispatching, EnteredDispatchingAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.MarkHeld(ctx, 4); err != nil {
		t.Fatalf("MarkHeld: %v", err)
	}
	if err := store.MarkSettled(ctx, 4); err == nil {
		t.Fatal("MarkSettled after MarkHeld: want an error, got nil")
	}
}
