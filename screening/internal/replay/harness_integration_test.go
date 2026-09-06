//go:build integration

// Requires a real, reachable Postgres 16 instance (SCREENING_TEST_DATABASE_URL
// and LEDGER_TEST_DATABASE_URL) AND a sibling checkout of the ledger module
// at ../../../ledger -- the same usdt-settlement-corridor layout this whole
// project already uses. Builds and runs a REAL ledgerd binary as a
// subprocess via internal/testledger, exactly as internal/ledgerclient's
// and internal/pipeline's own live tests do. Run via `make test-integration`
// (build tag "integration"). This is C3's ship gate -- see C3.9's own
// build spec.
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

	"screening/internal/db"
	"screening/internal/replay"
	"screening/internal/testledger"
)

const (
	replayLedgerListenAddr = ":18437"
	replayLedgerToken      = "c3.9-replay-test-token"
	replayLedgerActor      = "screening-svc"
)

// freshIsolatedScreeningPool creates a throwaway database on the same
// cluster as SCREENING_TEST_DATABASE_URL, migrated fresh, and returns a
// pool for it. Same reasoning as C1.9's and C2.10's own isolated pools:
// this run's FINAL ASSERTIONS are only meaningful against a database
// this run had entirely to itself -- sharing screening_test with
// whatever else runs concurrently under `go test ./...` would let stale
// rows from other packages' own tests produce a false assertion failure.
func freshIsolatedScreeningPool(t *testing.T) *db.Pool {
	t.Helper()
	baseURL := os.Getenv("SCREENING_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("SCREENING_TEST_DATABASE_URL not set; skipping integration test")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parsing SCREENING_TEST_DATABASE_URL: %v", err)
	}
	adminURL := *u
	adminURL.Path = "/postgres"
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connecting to admin database: %v", err)
	}
	dbName := fmt.Sprintf("screening_replay_isolated_%d", time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("creating isolated database: %v", err)
	}
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

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", freshURL.String())
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("running screening migrations: %v", err)
	}

	pool, err := db.Open(ctx, db.Config{DatabaseURL: freshURL.String()})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRun_AllScenariosAndAssertionsPass(t *testing.T) {
	pool := freshIsolatedScreeningPool(t)

	ledger := testledger.Start(t, replayLedgerListenAddr, replayLedgerToken, replayLedgerActor)

	ledgerDBURL := os.Getenv("LEDGER_TEST_DATABASE_URL")
	ledgerPool, err := pgxpool.New(context.Background(), ledgerDBURL)
	if err != nil {
		t.Fatalf("connecting to ledger database: %v", err)
	}
	defer ledgerPool.Close()

	cfg := replay.Config{
		Seed:          1788253816246464000,
		LedgerBaseURL: ledger.BaseURL(),
		LedgerToken:   ledger.Token(),
	}

	report, err := replay.Run(context.Background(), pool, ledgerPool, cfg)
	if report == nil {
		t.Fatalf("Run returned a nil report: %v", err)
	}

	var buf bytes.Buffer
	report.Print(&buf)
	if !report.Passed() {
		t.Fatalf("replay harness did not pass:\n%s", buf.String())
	}
	if err != nil {
		t.Fatalf("Run returned an error despite Passed()=true (should not happen): %v\n%s", err, buf.String())
	}
	t.Logf("replay harness report:\n%s", buf.String())
}
