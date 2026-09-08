//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package slots_test

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
	"dispatcher/internal/slots"
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

	if _, err := pool.Exec(ctx, `TRUNCATE slots`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

func TestCreate_RejectsDuplicateIDAndAddress(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := slots.NewStore(pool)

	if _, err := store.Create(ctx, 1, "TSlot0000000000000000000000001", time.Now()); err != nil {
		t.Fatalf("Create (1st): %v", err)
	}
	if _, err := store.Create(ctx, 1, "TSlotDifferentAddress0000000002", time.Now()); !errors.Is(err, slots.ErrDuplicateSlot) {
		t.Fatalf("Create (duplicate id) error = %v, want ErrDuplicateSlot", err)
	}
	if _, err := store.Create(ctx, 2, "TSlot0000000000000000000000001", time.Now()); !errors.Is(err, slots.ErrDuplicateSlot) {
		t.Fatalf("Create (duplicate address) error = %v, want ErrDuplicateSlot", err)
	}
}

func TestLifecycle_ActiveToRetiringToRetired(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := slots.NewStore(pool)

	if _, err := store.Create(ctx, 1, "TSlot0000000000000000000000001", time.Now()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.MarkRetiring(ctx, 1); err != nil {
		t.Fatalf("MarkRetiring: %v", err)
	}
	got, err := store.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != slots.StatusRetiring {
		t.Fatalf("Status = %s, want RETIRING", got.Status)
	}

	if err := store.MarkRetired(ctx, 1); err != nil {
		t.Fatalf("MarkRetired: %v", err)
	}
	got, err = store.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != slots.StatusRetired || got.RetiredAt == nil {
		t.Fatalf("got = %+v, want RETIRED with a RetiredAt set", got)
	}

	// The DB trigger itself must reject un-retiring, independent of this
	// package's own Go-level checks.
	if _, err := pool.Exec(ctx, `UPDATE slots SET status = 'ACTIVE' WHERE id = 1`); err == nil {
		t.Fatal("a raw UPDATE un-retiring a slot succeeded; want the DB trigger to reject it")
	}
}

func TestLifecycle_ActiveDirectlyToRetiredForAFreeze(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := slots.NewStore(pool)

	if _, err := store.Create(ctx, 1, "TSlot0000000000000000000000001", time.Now()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// C5.9's own mid-flight-freeze path: ACTIVE straight to RETIRED,
	// skipping RETIRING entirely.
	if err := store.MarkRetired(ctx, 1); err != nil {
		t.Fatalf("MarkRetired (direct from ACTIVE): %v", err)
	}
	got, err := store.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != slots.StatusRetired {
		t.Fatalf("Status = %s, want RETIRED", got.Status)
	}
}

func TestIncrementTxCount(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := slots.NewStore(pool)

	if _, err := store.Create(ctx, 1, "TSlot0000000000000000000000001", time.Now()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.IncrementTxCount(ctx, 1, 5); err != nil {
		t.Fatalf("IncrementTxCount: %v", err)
	}
	if err := store.IncrementTxCount(ctx, 1, 3); err != nil {
		t.Fatalf("IncrementTxCount (2nd): %v", err)
	}
	got, err := store.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TxCount != 8 {
		t.Fatalf("TxCount = %d, want 8", got.TxCount)
	}

	if err := store.IncrementTxCount(ctx, 99, 1); !errors.Is(err, slots.ErrSlotNotFound) {
		t.Fatalf("IncrementTxCount (unknown slot) error = %v, want ErrSlotNotFound", err)
	}
}
