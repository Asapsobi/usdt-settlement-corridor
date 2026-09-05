//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package orphaned_test

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
	txHash := fmt.Sprintf("0xtest-record-and-list-%d", time.Now().UnixNano())

	if err := orphaned.Record(ctx, pool, sampleDeposit(txHash)); err != nil {
		t.Fatalf("Record: %v", err)
	}

	deposits, err := orphaned.List(ctx, pool, nil)
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
	txHash := fmt.Sprintf("0xtest-idempotent-record-%d", time.Now().UnixNano())
	deposit := sampleDeposit(txHash)

	if err := orphaned.Record(ctx, pool, deposit); err != nil {
		t.Fatalf("Record (first): %v", err)
	}
	if err := orphaned.Record(ctx, pool, deposit); err != nil {
		t.Fatalf("Record (duplicate): %v", err)
	}

	deposits, err := orphaned.List(ctx, pool, nil)
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

func TestGet_NotFound(t *testing.T) {
	pool := testPool(t)
	_, err := orphaned.Get(context.Background(), pool, -1)
	if !errors.Is(err, orphaned.ErrNotFound) {
		t.Fatalf("Get(-1): got %v, want ErrNotFound", err)
	}
}

func TestResolve_SetsResolutionAndResolvedBy(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	txHash := fmt.Sprintf("0xtest-resolve-%d", time.Now().UnixNano())
	if err := orphaned.Record(ctx, pool, sampleDeposit(txHash)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	deposits, err := orphaned.List(ctx, pool, nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var id int64
	for _, d := range deposits {
		if d.TxHash == txHash {
			id = d.ID
		}
	}
	if id == 0 {
		t.Fatal("setup: could not find the recorded deposit's id")
	}

	resolved, err := orphaned.Resolve(ctx, pool, id, "manually refunded off-chain", "operator:alice")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Resolution == nil || *resolved.Resolution != "manually refunded off-chain" {
		t.Fatalf("Resolve: Resolution = %v, want set", resolved.Resolution)
	}
	if resolved.ResolvedBy == nil || *resolved.ResolvedBy != "operator:alice" {
		t.Fatalf("Resolve: ResolvedBy = %v, want operator:alice", resolved.ResolvedBy)
	}
	if resolved.ResolvedAt == nil {
		t.Fatal("Resolve: ResolvedAt is nil, want set")
	}

	// A second resolve must not silently overwrite the first.
	_, err = orphaned.Resolve(ctx, pool, id, "a different resolution", "operator:bob")
	if !errors.Is(err, orphaned.ErrAlreadyResolved) {
		t.Fatalf("Resolve (second attempt): got %v, want ErrAlreadyResolved", err)
	}
}

func TestResolve_NotFound(t *testing.T) {
	pool := testPool(t)
	_, err := orphaned.Resolve(context.Background(), pool, -1, "resolution", "operator:alice")
	if !errors.Is(err, orphaned.ErrNotFound) {
		t.Fatalf("Resolve(-1): got %v, want ErrNotFound", err)
	}
}

func TestList_FiltersByResolved(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	resolvedTxHash := fmt.Sprintf("0xtest-list-filter-resolved-%d", time.Now().UnixNano())
	unresolvedTxHash := fmt.Sprintf("0xtest-list-filter-unresolved-%d", time.Now().UnixNano())

	if err := orphaned.Record(ctx, pool, sampleDeposit(resolvedTxHash)); err != nil {
		t.Fatalf("Record (resolved fixture): %v", err)
	}
	if err := orphaned.Record(ctx, pool, sampleDeposit(unresolvedTxHash)); err != nil {
		t.Fatalf("Record (unresolved fixture): %v", err)
	}
	all, err := orphaned.List(ctx, pool, nil)
	if err != nil {
		t.Fatalf("List(nil): %v", err)
	}
	var resolvedID int64
	for _, d := range all {
		if d.TxHash == resolvedTxHash {
			resolvedID = d.ID
		}
	}
	if _, err := orphaned.Resolve(ctx, pool, resolvedID, "resolved for this test", "operator:alice"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	resolvedOnly := true
	resolvedList, err := orphaned.List(ctx, pool, &resolvedOnly)
	if err != nil {
		t.Fatalf("List(resolved=true): %v", err)
	}
	assertContainsTxHash(t, resolvedList, resolvedTxHash, true)
	assertContainsTxHash(t, resolvedList, unresolvedTxHash, false)

	unresolvedOnly := false
	unresolvedList, err := orphaned.List(ctx, pool, &unresolvedOnly)
	if err != nil {
		t.Fatalf("List(resolved=false): %v", err)
	}
	assertContainsTxHash(t, unresolvedList, resolvedTxHash, false)
	assertContainsTxHash(t, unresolvedList, unresolvedTxHash, true)
}

func assertContainsTxHash(t *testing.T, deposits []orphaned.Deposit, txHash string, want bool) {
	t.Helper()
	got := false
	for _, d := range deposits {
		if d.TxHash == txHash {
			got = true
		}
	}
	if got != want {
		t.Errorf("txHash %s present in list = %v, want %v", txHash, got, want)
	}
}
