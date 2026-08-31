//go:build integration

// This file requires a real, reachable Postgres 16 instance and is excluded
// from `go test ./...` by the integration build tag; run it via
// `make test-integration`. It is the C1.0 acceptance proof for the grant
// model: ledger_writer can INSERT and SELECT but never UPDATE or DELETE,
// enforced by Postgres itself, not by application code.
package db_test

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
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

// applyMigrations runs goose against url via database/sql (goose's
// interface), independent of the pgxpool the rest of the ledger uses.
func applyMigrations(t *testing.T, url string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer sqlDB.Close()

	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(sqlDB, migrationsDir(t)))
}

func TestLedgerWriterCannotUpdateOrDelete(t *testing.T) {
	url := testDatabaseURL(t)
	ctx := context.Background()

	applyMigrations(t, url)

	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	scratchTable := "scratch_c1_0_" + randomSuffix()
	_, err = pool.Exec(ctx, "CREATE TABLE "+scratchTable+" (id bigserial primary key, value text not null)")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS "+scratchTable)
	})

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	// Registered before "SET ROLE" below, so cleanup LIFO order resets the
	// role and releases the connection before the table drop and pool
	// close run -- t.Cleanup, unlike defer, would otherwise fire this
	// after the pool is already closed if mixed with defer here.
	t.Cleanup(conn.Release)

	_, err = conn.Exec(ctx, "SET ROLE ledger_writer")
	require.NoError(t, err, "granting login role must have been given ledger_writer membership by the migration")
	t.Cleanup(func() { _, _ = conn.Exec(ctx, "RESET ROLE") })

	_, err = conn.Exec(ctx, "INSERT INTO "+scratchTable+" (value) VALUES ('a')")
	require.NoError(t, err, "ledger_writer must be able to INSERT")

	var count int
	require.NoError(t, conn.QueryRow(ctx, "SELECT count(*) FROM "+scratchTable).Scan(&count))
	require.Equal(t, 1, count, "ledger_writer must be able to SELECT")

	_, err = conn.Exec(ctx, "UPDATE "+scratchTable+" SET value = 'b'")
	require.Error(t, err, "ledger_writer must NOT be able to UPDATE")

	_, err = conn.Exec(ctx, "DELETE FROM "+scratchTable)
	require.Error(t, err, "ledger_writer must NOT be able to DELETE")
}

func TestLedgerReaderIsSelectOnly(t *testing.T) {
	url := testDatabaseURL(t)
	ctx := context.Background()

	applyMigrations(t, url)

	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	scratchTable := "scratch_c1_0_" + randomSuffix()
	_, err = pool.Exec(ctx, "CREATE TABLE "+scratchTable+" (id bigserial primary key, value text not null)")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DROP TABLE IF EXISTS "+scratchTable)
	})
	_, err = pool.Exec(ctx, "INSERT INTO "+scratchTable+" (value) VALUES ('seed')")
	require.NoError(t, err)

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	t.Cleanup(conn.Release)

	_, err = conn.Exec(ctx, "SET ROLE ledger_reader")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.Exec(ctx, "RESET ROLE") })

	var count int
	require.NoError(t, conn.QueryRow(ctx, "SELECT count(*) FROM "+scratchTable).Scan(&count))
	require.Equal(t, 1, count, "ledger_reader must be able to SELECT")

	_, err = conn.Exec(ctx, "INSERT INTO "+scratchTable+" (value) VALUES ('b')")
	require.Error(t, err, "ledger_reader must NOT be able to INSERT")
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = cryptorand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0x0f]
	}
	return string(out)
}
