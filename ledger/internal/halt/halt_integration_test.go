//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. Covers this package's own share of C1.7's
// ship gate: Set and Clear both round-trip through system_state and both
// append to halt_log, clearing without an operator id is rejected (the
// pure unit test in halt_test.go covers the missing-note case too), and
// Cache actually holds its value for up to but not more than 1 second.
package halt_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"ledger/internal/halt"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

func applyMigrations(t *testing.T, url string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(sqlDB, migrationsDir(t)))
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testDatabaseURL(t)
	applyMigrations(t, url)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `
		UPDATE system_state
		SET halted = false, halt_reason = NULL, halt_detail = NULL, halted_at = NULL, halted_by = NULL
		WHERE id = 1
	`)
	require.NoError(t, err)
	return pool
}

func haltLogCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, action string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM halt_log WHERE action = $1", action).Scan(&n))
	return n
}

func TestSetAndClearRoundTripAndLogBoth(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	setLogBefore := haltLogCount(t, ctx, pool, "set")
	clearLogBefore := haltLogCount(t, ctx, pool, "clear")

	require.NoError(t, halt.Set(ctx, pool, halt.SetParams{
		Reason: "TEST_REASON",
		Detail: map[string]any{"note": "integration test"},
		Actor:  "test:actor",
	}))

	halted, err := halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.True(t, halted)

	reason, err := halt.Reason(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, "TEST_REASON", reason)

	require.Equal(t, setLogBefore+1, haltLogCount(t, ctx, pool, "set"), "Set must append exactly one halt_log row")

	require.NoError(t, halt.Clear(ctx, pool, halt.ClearParams{
		Actor: "ops:bob",
		Note:  "confirmed false positive, resuming",
	}))

	halted, err = halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.False(t, halted)

	require.Equal(t, clearLogBefore+1, haltLogCount(t, ctx, pool, "clear"), "Clear must append exactly one halt_log row")
}

func TestClearWithoutOperatorRejectedAgainstRealDB(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	require.NoError(t, halt.Set(ctx, pool, halt.SetParams{Reason: "TEST_REASON", Actor: "test:actor"}))

	err := halt.Clear(ctx, pool, halt.ClearParams{Actor: "", Note: "trying anyway"})
	require.ErrorIs(t, err, halt.ErrClearRequiresOperator)

	halted, err := halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.True(t, halted, "a rejected clear must not have changed anything")
}

func TestHaltLogAppendOnly(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	require.NoError(t, halt.Set(ctx, pool, halt.SetParams{Reason: "TEST_REASON", Actor: "test:actor"}))

	var id int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM halt_log WHERE action = 'set' ORDER BY id DESC LIMIT 1").Scan(&id))

	_, err := pool.Exec(ctx, "UPDATE halt_log SET actor = 'x' WHERE id = $1", id)
	require.Error(t, err, "halt_log must reject UPDATE even for the table owner")
	_, err = pool.Exec(ctx, "DELETE FROM halt_log WHERE id = $1", id)
	require.Error(t, err, "halt_log must reject DELETE even for the table owner")
}

// freshIsolatedPool creates a throwaway database on the same cluster as
// LEDGER_TEST_DATABASE_URL and returns a pool for it plus automatic
// cleanup. TestCacheHoldsForUpToOneSecond needs this, not just a longer
// sleep: `go test ./...` runs different packages concurrently by
// default, system_state is a single shared row (id=1), and other
// packages' tests (orders_test resets it to unhalted at the start of
// nearly every test; recon_test's IngestSnapshot tests set it too) race
// against this test's multi-second window on the shared ledger_test
// database. A longer sleep doesn't fix a correctness problem -- it just
// widens the window another concurrent test can land in.
func freshIsolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	baseURL := testDatabaseURL(t)
	u, err := url.Parse(baseURL)
	require.NoError(t, err)
	adminURL := *u
	adminURL.Path = "/postgres"
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, adminURL.String())
	require.NoError(t, err)
	dbName := fmt.Sprintf("halt_isolated_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	adminPool.Close()

	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), adminURL.String())
		if err != nil {
			return
		}
		defer cleanupPool.Close()
		_, _ = cleanupPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
	})

	freshURL := *u
	freshURL.Path = "/" + dbName
	applyMigrations(t, freshURL.String())

	pool, err := pgxpool.New(ctx, freshURL.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestCacheHoldsForUpToOneSecond(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()
	cache := halt.NewCache(pool)

	halted, err := cache.IsHalted(ctx)
	require.NoError(t, err)
	require.False(t, halted, "starts unhalted")

	require.NoError(t, halt.Set(ctx, pool, halt.SetParams{Reason: "TEST_REASON", Actor: "test:actor"}))

	halted, err = cache.IsHalted(ctx)
	require.NoError(t, err)
	require.False(t, halted, "cache must still report the pre-halt value within its staleness window")

	time.Sleep(1100 * time.Millisecond)

	halted, err = cache.IsHalted(ctx)
	require.NoError(t, err)
	require.True(t, halted, "cache must reflect the real state once its staleness window has passed")
}
