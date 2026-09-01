//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is the rest of C1.7's ship gate:
// injected drift on a USDT account halts with the account named in
// halt_detail; TRX drift within a configured tolerance does not halt,
// while exceeding it does; the reconciler's actual ticker (not just the
// underlying check function) detects a corrupted cache and an unbalanced
// journal within one real interval; and an exceeded corridor ceiling
// alerts without halting.
package recon_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/money"
	"ledger/internal/recon"
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

func applyMigrations(t *testing.T, dbURL string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(sqlDB, migrationsDir(t)))
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := testDatabaseURL(t)
	applyMigrations(t, dbURL)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, accounts.Seed(ctx, pool))
	_, err = pool.Exec(ctx, `
		UPDATE system_state
		SET halted = false, halt_reason = NULL, halt_detail = NULL, halted_at = NULL, halted_by = NULL
		WHERE id = 1
	`)
	require.NoError(t, err)
	return pool
}

func withTx(t *testing.T, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var seq int64

func uniqueSuffix(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&seq, 1)
	return fmt.Sprintf("%s:%d:%d", t.Name(), time.Now().UnixNano(), n)
}

func twoAccounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, asset money.Asset) (string, string) {
	t.Helper()
	suffix := uniqueSuffix(t)
	a := "liability:customer:" + suffix + ":a"
	b := "liability:customer:" + suffix + ":b"
	_, err := accounts.Create(ctx, pool, a, accounts.Liability, asset)
	require.NoError(t, err)
	_, err = accounts.Create(ctx, pool, b, accounts.Liability, asset)
	require.NoError(t, err)
	return a, b
}

// postEntry posts a simple two-line balanced entry and returns its id --
// used as the watermark IngestSnapshot needs.
func postEntry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acc1, acc2 string, asset money.Asset, units int64) journal.Entry {
	t.Helper()
	var entry journal.Entry
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entry, err = journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: "recon_test:" + uniqueSuffix(t),
			EntryType:      "recon_test_base",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				{AccountCode: acc1, Amount: money.Amount{Asset: asset, Units: units}},
				{AccountCode: acc2, Amount: money.Amount{Asset: asset, Units: -units}},
			},
		})
		return err
	})
	require.NoError(t, err)
	return entry
}

func haltDetail(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	var detail map[string]any
	require.NoError(t, pool.QueryRow(ctx, "SELECT halt_detail FROM system_state WHERE id = 1").Scan(&detail))
	return detail
}

func TestIngestSnapshotHaltsOnUSDTDriftAndNamesAccount(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()
	acc1, acc2 := twoAccounts(t, ctx, pool, money.USDT_TRC20)
	entry := postEntry(t, ctx, pool, acc1, acc2, money.USDT_TRC20, 5_000000)

	ledgerBal, err := journal.BalanceAsOf(ctx, pool, acc1, entry.ID)
	require.NoError(t, err)

	cfg := recon.Config{}
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := recon.IngestSnapshot(ctx, tx, cfg, recon.SnapshotParams{
			AccountCode:   acc1,
			ObservedUnits: ledgerBal.Units + 1, // 1 minor unit of drift
			ChainRef:      "test-block-1",
			Watermark:     entry.ID,
			ObservedAt:    time.Now(),
		})
		return err
	})
	require.NoError(t, err)

	halted, err := halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.True(t, halted)

	reason, err := halt.Reason(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, "BALANCE_DRIFT", reason)

	detail := haltDetail(t, ctx, pool)
	require.Equal(t, acc1, detail["account_code"], "halt_detail must name the drifted account")
}

func TestIngestSnapshotRecordsSnapshotEvenWhenNoDrift(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()
	acc1, acc2 := twoAccounts(t, ctx, pool, money.USDT_TRC20)
	entry := postEntry(t, ctx, pool, acc1, acc2, money.USDT_TRC20, 5_000000)

	ledgerBal, err := journal.BalanceAsOf(ctx, pool, acc1, entry.ID)
	require.NoError(t, err)

	var countBefore int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM recon_snapshots WHERE account_id = (SELECT id FROM accounts WHERE code = $1)", acc1).Scan(&countBefore))

	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := recon.IngestSnapshot(ctx, tx, recon.Config{}, recon.SnapshotParams{
			AccountCode:   acc1,
			ObservedUnits: ledgerBal.Units,
			ChainRef:      "test-block-2",
			Watermark:     entry.ID,
			ObservedAt:    time.Now(),
		})
		return err
	})
	require.NoError(t, err)

	halted, err := halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.False(t, halted, "matching observed/ledger units must not halt")

	var countAfter int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM recon_snapshots WHERE account_id = (SELECT id FROM accounts WHERE code = $1)", acc1).Scan(&countAfter))
	require.Equal(t, countBefore+1, countAfter, "the snapshot must be recorded regardless of drift")
}

func TestIngestSnapshotTRXWithinToleranceDoesNotHalt(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()
	acc1, acc2 := twoAccounts(t, ctx, pool, money.TRX)
	entry := postEntry(t, ctx, pool, acc1, acc2, money.TRX, 10_000000)

	ledgerBal, err := journal.BalanceAsOf(ctx, pool, acc1, entry.ID)
	require.NoError(t, err)

	cfg := recon.Config{TRXToleranceUnits: 5}
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := recon.IngestSnapshot(ctx, tx, cfg, recon.SnapshotParams{
			AccountCode:   acc1,
			ObservedUnits: ledgerBal.Units + 5, // exactly at tolerance
			ChainRef:      "test-block-3",
			Watermark:     entry.ID,
			ObservedAt:    time.Now(),
		})
		return err
	})
	require.NoError(t, err)

	halted, err := halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.False(t, halted, "drift within configured TRX tolerance must not halt")
}

func TestIngestSnapshotTRXExceedingToleranceHalts(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()
	acc1, acc2 := twoAccounts(t, ctx, pool, money.TRX)
	entry := postEntry(t, ctx, pool, acc1, acc2, money.TRX, 10_000000)

	ledgerBal, err := journal.BalanceAsOf(ctx, pool, acc1, entry.ID)
	require.NoError(t, err)

	cfg := recon.Config{TRXToleranceUnits: 5}
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := recon.IngestSnapshot(ctx, tx, cfg, recon.SnapshotParams{
			AccountCode:   acc1,
			ObservedUnits: ledgerBal.Units + 6, // one past tolerance
			ChainRef:      "test-block-4",
			Watermark:     entry.ID,
			ObservedAt:    time.Now(),
		})
		return err
	})
	require.NoError(t, err)

	halted, err := halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.True(t, halted, "drift beyond configured TRX tolerance must halt")
}

func waitForHalt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		halted, err := halt.IsHalted(ctx, pool)
		require.NoError(t, err)
		if halted {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// freshIsolatedPool creates a throwaway database (migrated, accounts
// seeded), on the same cluster as LEDGER_TEST_DATABASE_URL, and returns a
// pool for it plus a cleanup that drops it. Every test that exercises the
// reconciler's actual ticker needs this, not just the trigger-bypass one:
// VerifyBalances (behind CACHE_DIVERGENCE) is a GLOBAL check across every
// account in the database, and the shared ledger_test database carries
// permanent, genuine drift from journal activity posted in earlier
// chunks' test runs, before account_balances existed (see C1.4's own
// notes on this). Running the real ticker against ledger_test means
// checkCacheDivergence finds that pre-existing drift and halts on it
// almost immediately, before whatever the test actually wants to trigger
// ever gets a chance to run -- isolation here isn't about the risky
// trigger-disabling case alone, it's required by VerifyBalances' scope.
//
// The IngestSnapshot tests below need it for a related but distinct
// reason: system_state (what halt.IsHalted reads) is a single singleton
// row too, and go test parallelizes across packages by default, so
// another package's test halting or clearing mid-run can flip what this
// test observes regardless of anything this test itself does. Confirmed
// by TestIngestSnapshotRecordsSnapshotEvenWhenNoDrift actually failing
// this way once it ran as part of the full `go test ./...` suite rather
// than in isolation.
func freshIsolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	baseURL := testDatabaseURL(t)
	adminURL := withDBName(t, baseURL, "postgres")
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, adminURL)
	require.NoError(t, err)
	dbName := fmt.Sprintf("recon_isolated_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	adminPool.Close()

	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer cleanupPool.Close()
		_, _ = cleanupPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
	})

	freshURL := withDBName(t, baseURL, dbName)
	applyMigrations(t, freshURL)

	pool, err := pgxpool.New(ctx, freshURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, accounts.Seed(ctx, pool))
	return pool
}

func withDBName(t *testing.T, rawURL, dbName string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	u.Path = "/" + dbName
	return u.String()
}

func accountID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, code string) int64 {
	t.Helper()
	acc, err := accounts.GetByCode(ctx, pool, code)
	require.NoError(t, err)
	return acc.ID
}

func TestTickerDetectsCacheDivergenceWithinOneCycle(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()
	acc1, acc2 := twoAccounts(t, ctx, pool, money.TRX)
	postEntry(t, ctx, pool, acc1, acc2, money.TRX, 3_000000)

	id1 := accountID(t, ctx, pool, acc1)
	_, err := pool.Exec(ctx, "UPDATE account_balances SET balance_units = balance_units + 999 WHERE account_id = $1", id1)
	require.NoError(t, err)

	cfg := recon.Config{Interval: 50 * time.Millisecond}
	r := recon.NewReconciler(pool, cfg)
	tickerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.Run(tickerCtx)

	require.True(t, waitForHalt(t, ctx, pool, 2*time.Second), "the ticker must halt within a couple of cycles")

	reason, err := halt.Reason(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, "CACHE_DIVERGENCE", reason)
}

// TestTickerDetectsTrialBalanceBrokenWithinOneCycle is the "second line
// of defence" test: an unbalanced entry should be impossible to commit at
// all, because of C1.2's deferred constraint trigger. To prove the
// self-check ticker would still catch it if that trigger somehow didn't
// fire, this test disables the trigger, lands an unbalanced entry, and
// re-enables it -- against freshIsolatedPool's throwaway database, never
// the shared ledger_test database every other test in this suite relies
// on. Disabling that trigger persistently (ALTER TABLE ... DISABLE
// TRIGGER is a catalog change, not session-scoped) on shared
// infrastructure would be a serious, silent safety regression for every
// test that runs afterward; isolating it here is not optional caution,
// it's the difference between testing this safely and not.
func TestTickerDetectsTrialBalanceBrokenWithinOneCycle(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()

	acc1, acc2 := twoAccounts(t, ctx, pool, money.TRX)
	id1 := accountID(t, ctx, pool, acc1)
	id2 := accountID(t, ctx, pool, acc2)

	_, err := pool.Exec(ctx, "ALTER TABLE journal_lines DISABLE TRIGGER journal_lines_balance_check")
	require.NoError(t, err)

	func() {
		defer func() {
			_, err := pool.Exec(ctx, "ALTER TABLE journal_lines ENABLE TRIGGER journal_lines_balance_check")
			require.NoError(t, err)
		}()

		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer tx.Rollback(ctx)

		var entryID int64
		require.NoError(t, tx.QueryRow(ctx, `
			INSERT INTO journal_entries (idempotency_key, payload_hash, entry_type, actor, occurred_at)
			VALUES ('bypass:trigger:test', 'deadbeef', 'raw_sql_test', 'test', now())
			RETURNING id
		`).Scan(&entryID))
		_, err = tx.Exec(ctx, `INSERT INTO journal_lines (entry_id, seq, account_id, asset, amount_units) VALUES ($1, 0, $2, 'TRX', 5)`, entryID, id1)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `INSERT INTO journal_lines (entry_id, seq, account_id, asset, amount_units) VALUES ($1, 1, $2, 'TRX', -3)`, entryID, id2)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx), "with the trigger disabled, this unbalanced entry must commit -- that's the point")
	}()

	cfg := recon.Config{Interval: 50 * time.Millisecond}
	r := recon.NewReconciler(pool, cfg)
	tickerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go r.Run(tickerCtx)

	require.True(t, waitForHalt(t, ctx, pool, 2*time.Second), "the ticker must catch the unbalanced entry within a couple of cycles")

	reason, err := halt.Reason(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, "TRIAL_BALANCE_BROKEN", reason)
}

func TestCorridorCeilingAlertsWithoutHalting(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()

	acc, _ := twoAccounts(t, ctx, pool, money.USDT_BEP20)
	// Move position:corridor:USDT_BEP20 well past a deliberately tiny ceiling.
	postEntry(t, ctx, pool, "position:corridor:USDT_BEP20", acc, money.USDT_BEP20, 1_000000)

	var buf bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(original) })

	cfg := recon.Config{
		Interval:         time.Hour, // won't tick on its own during this test
		CorridorCeilings: map[money.Asset]int64{money.USDT_BEP20: 1},
	}
	r := recon.NewReconciler(pool, cfg)
	r.RunOnce(ctx)

	halted, err := halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.False(t, halted, "exceeding the corridor ceiling must never halt")

	require.Contains(t, buf.String(), "position:corridor exceeds configured ceiling", "exceeding the ceiling must log an alert")
}
