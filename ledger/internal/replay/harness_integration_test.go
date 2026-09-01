//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is a fast regression guard for the
// replay package itself, run at a scale small enough to stay quick in
// the normal test suite -- not a substitute for the full gate ("make
// gate"), which is what actually certifies C1 at the spec's 10,000-order
// scale. 2,500 orders is chosen specifically because it is the smallest
// round number that still produces at least one 50-order sweep batch
// (2,500 * 2% = 50), so every scenario type in the mix gets exercised at
// least once, not just the fast, always-present ones.
package replay_test

import (
	"bytes"
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

	"ledger/internal/replay"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	return u
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

func applyMigrations(t *testing.T, dbURL string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(sqlDB, migrationsDir(t)))
}

// freshIsolatedPool creates a throwaway database on the same cluster as
// LEDGER_TEST_DATABASE_URL and returns a pool for it, with cleanup
// registered. The replay harness's own FINAL ASSERTIONS (exact
// journal_entries count, whole-database VerifyBalances/TrialBalance) are
// only meaningful against a database this run had entirely to itself --
// see harness.go's doc comment -- so, unlike most of this codebase's
// other integration tests, this can't share ledger_test with whatever
// else is running concurrently under `go test ./...`.
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
	dbName := fmt.Sprintf("replay_isolated_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	adminPool.Close()

	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), adminURL.String())
		if err != nil {
			return
		}
		defer cleanupPool.Close()
		_, _ = cleanupPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	})

	freshURL := *u
	freshURL.Path = "/" + dbName
	applyMigrations(t, freshURL.String())

	pool, err := pgxpool.New(ctx, freshURL.String())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestRun_PassesAtModestScale(t *testing.T) {
	pool := freshIsolatedPool(t)

	cfg := replay.Config{
		Seed:      1788253816246464000,
		NumOrders: 2500,
		Workers:   8,
		Scenarios: replay.DefaultScenarioCounts(2500),
	}

	report, err := replay.Run(context.Background(), pool, cfg)
	require.NotNil(t, report, "Run should return a report even on assertion failure")

	var buf bytes.Buffer
	report.Print(&buf)
	require.NoErrorf(t, err, "replay harness did not pass at 2,500 orders:\n%s", buf.String())
	require.True(t, report.Passed())
}
