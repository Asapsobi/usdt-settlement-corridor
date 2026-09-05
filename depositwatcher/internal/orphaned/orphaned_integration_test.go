//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package orphaned_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"depositwatcher/internal/orphaned"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("WATCHER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("WATCHER_TEST_DATABASE_URL not set; skipping integration test")
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

func sampleDeposit(txHash string) orphaned.Deposit {
	return orphaned.Deposit{
		OrderID:               4242,
		ExternalID:            "order-4242",
		TxHash:                txHash,
		LogIndex:              0,
		Amount:                3000_000000,
		DetectedAt:            time.Now().UTC().Truncate(time.Microsecond),
		OrderStateAtDetection: "expired",
	}
}

func TestRecord_AndList(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	txHash := "0xtest-record-and-list"

	if err := orphaned.Record(ctx, pool, sampleDeposit(txHash)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	deposits, err := orphaned.List(ctx, pool)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found *orphaned.Deposit
	for i := range deposits {
		if deposits[i].TxHash == txHash {
			found = &deposits[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("List did not return the recorded deposit %s", txHash)
	}
	if found.OrderID != 4242 || found.ExternalID != "order-4242" || found.Amount != 3000_000000 ||
		found.OrderStateAtDetection != "expired" {
		t.Fatalf("List returned a mismatched row: %+v", found)
	}
	if found.Resolution != nil || found.ResolvedAt != nil {
		t.Fatalf("a freshly recorded deposit must be unresolved: %+v", found)
	}
}

func TestRecord_IdempotentOnTxHashAndLogIndex(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	txHash := "0xtest-idempotent-record"
	deposit := sampleDeposit(txHash)

	if err := orphaned.Record(ctx, pool, deposit); err != nil {
		t.Fatalf("Record (first): %v", err)
	}
	if err := orphaned.Record(ctx, pool, deposit); err != nil {
		t.Fatalf("Record (duplicate): %v", err)
	}

	deposits, err := orphaned.List(ctx, pool)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	count := 0
	for _, d := range deposits {
		if d.TxHash == txHash {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d rows for a duplicate-recorded (tx_hash, log_index), want exactly 1", count)
	}
}
