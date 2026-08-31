//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is the C1.2 ship-gate proof: the balance
// invariant holds both in Go (journal.Post) and independently in Postgres
// (the deferred constraint trigger, exercised here via raw SQL that never
// goes through Post at all), and journal_entries/journal_lines are
// genuinely append-only -- for ledger_writer AND for the table owner, who
// would otherwise bypass role grants by virtue of owning the tables.
package journal_test

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/journal"
	"ledger/internal/money"
)

// runID makes idempotency keys unique per `go test` invocation.
// journal.Post has no replay semantics yet (that's C1.3), so a fixed key
// reused across back-to-back runs against the same persistent test
// database would collide on journal_entries' UNIQUE idempotency_key --
// not a bug in Post, just a reason test keys can't be fixed strings.
var runID = randomHex()

func randomHex() string {
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

func idemKey(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + ":" + t.Name() + ":" + runID
}

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testDatabaseURL(t)
	applyMigrations(t, url)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, accounts.Seed(ctx, pool))
	return pool
}

func applyMigrations(t *testing.T, url string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(sqlDB, migrationsDir(t)))
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

// withTx runs fn inside a transaction and commits, returning fn's error
// (uncommitted) or the commit error.
func withTx(t *testing.T, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx) // no-op once committed

	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// customerAccount creates a fresh liability:customer account for asset,
// unique to the running test, since customer accounts have a variable code
// segment and are never part of the fixed seed chart.
func customerAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, asset money.Asset) string {
	t.Helper()
	code := "liability:customer:" + t.Name() + ":" + string(asset)
	_, err := accounts.Create(ctx, pool, code, accounts.Liability, asset)
	require.NoError(t, err)
	return code
}

func accountID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, code string) int64 {
	t.Helper()
	acc, err := accounts.GetByCode(ctx, pool, code)
	require.NoError(t, err)
	return acc.ID
}

// postSimpleEntry posts a minimal, valid, balanced two-line entry between
// two fresh TRX accounts, for tests that just need *an* entry to exist.
func postSimpleEntry(t *testing.T, ctx context.Context, pool *pgxpool.Pool) journal.Entry {
	t.Helper()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	var entry journal.Entry
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entry, err = journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: idemKey(t, "simple"),
			EntryType:      "test_entry",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5_000000}},
				{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -5_000000}},
			},
		})
		return err
	})
	require.NoError(t, err)
	return entry
}

// TestConversionShapeAccepted replicates §B's E2 conversion entry: one
// entry, two assets, each independently summing to zero. This is the
// design decision the whole ledger rests on, and it must work.
func TestConversionShapeAccepted(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	customerBEP := customerAccount(t, ctx, pool, money.USDT_BEP20)
	customerTRC := customerAccount(t, ctx, pool, money.USDT_TRC20)

	var entry journal.Entry
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entry, err = journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: idemKey(t, "conversion"),
			EntryType:      "conversion",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				{AccountCode: customerBEP, Amount: money.Amount{Asset: money.USDT_BEP20, Units: 3000_000000}},
				{AccountCode: "position:corridor:USDT_BEP20", Amount: money.Amount{Asset: money.USDT_BEP20, Units: -3000_000000}},
				{AccountCode: "position:corridor:USDT_TRC20", Amount: money.Amount{Asset: money.USDT_TRC20, Units: 3000_000000}},
				{AccountCode: customerTRC, Amount: money.Amount{Asset: money.USDT_TRC20, Units: -2990_700000}},
				{AccountCode: "revenue:fee", Amount: money.Amount{Asset: money.USDT_TRC20, Units: -7_500000}},
				{AccountCode: "revenue:network_fee", Amount: money.Amount{Asset: money.USDT_TRC20, Units: -1_800000}},
			},
		})
		return err
	})
	require.NoError(t, err)
	require.NotZero(t, entry.ID)
	require.Len(t, entry.Lines, 6)
}

// TestSingleAssetImbalanceRejectedByGo covers a same-asset imbalance
// (+5 / -3 in TRX, sums to 2): rejected in Go before any INSERT runs.
func TestSingleAssetImbalanceRejectedByGo(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	key := idemKey(t, "imbalance")
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: key,
			EntryType:      "bogus",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5}},
				{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -3}},
			},
		})
		return err
	})
	require.ErrorIs(t, err, journal.ErrUnbalanced)
	requireNoEntryWritten(t, ctx, pool, key)
}

// TestCrossAssetNettingRejectedByGo is the specific acceptance case: a
// TRX +5 / BNB -5 entry sums to zero across the whole entry but is NOT
// balanced per-asset, and per-asset balance is the only rule that counts.
// Cross-asset netting must never be accepted.
func TestCrossAssetNettingRejectedByGo(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.BNB)

	key := idemKey(t, "netting")
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: key,
			EntryType:      "bogus",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5}},
				{AccountCode: acc2, Amount: money.Amount{Asset: money.BNB, Units: -5}},
			},
		})
		return err
	})
	require.ErrorIs(t, err, journal.ErrUnbalanced)
	requireNoEntryWritten(t, ctx, pool, key)
}

// TestUnbalancedEntryRejectedByTriggerViaRawSQL bypasses internal/journal
// entirely: it is the proof that the balance invariant holds even for SQL
// Go never wrote. The deferred constraint trigger must fail the COMMIT.
func TestUnbalancedEntryRejectedByTriggerViaRawSQL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)
	id1 := accountID(t, ctx, pool, acc1)
	id2 := accountID(t, ctx, pool, acc2)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	var entryID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO journal_entries (idempotency_key, payload_hash, entry_type, actor, occurred_at)
		VALUES ($1, $2, 'raw_sql_test', 'test', now())
		RETURNING id
	`, idemKey(t, "raw"), []byte("deadbeef")).Scan(&entryID)
	require.NoError(t, err)

	_, err = tx.Exec(ctx, `INSERT INTO journal_lines (entry_id, seq, account_id, asset, amount_units) VALUES ($1, 0, $2, 'TRX', 5)`, entryID, id1)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO journal_lines (entry_id, seq, account_id, asset, amount_units) VALUES ($1, 1, $2, 'TRX', -3)`, entryID, id2)
	require.NoError(t, err)

	err = tx.Commit(ctx)
	require.Error(t, err, "COMMIT must fail: the deferred balance-check trigger should raise for an unbalanced entry written via raw SQL")
}

// TestBalancedEntryViaRawSQLCommits is the control for the test above:
// proves the trigger only rejects genuinely unbalanced entries, not every
// raw-SQL write.
func TestBalancedEntryViaRawSQLCommits(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)
	id1 := accountID(t, ctx, pool, acc1)
	id2 := accountID(t, ctx, pool, acc2)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	var entryID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO journal_entries (idempotency_key, payload_hash, entry_type, actor, occurred_at)
		VALUES ($1, $2, 'raw_sql_test', 'test', now())
		RETURNING id
	`, idemKey(t, "raw_ok"), []byte("deadbeef")).Scan(&entryID)
	require.NoError(t, err)

	_, err = tx.Exec(ctx, `INSERT INTO journal_lines (entry_id, seq, account_id, asset, amount_units) VALUES ($1, 0, $2, 'TRX', 5)`, entryID, id1)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO journal_lines (entry_id, seq, account_id, asset, amount_units) VALUES ($1, 1, $2, 'TRX', -5)`, entryID, id2)
	require.NoError(t, err)

	require.NoError(t, tx.Commit(ctx), "a genuinely balanced entry must commit")
}

func TestLineAssetMismatchRejected(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX) // account holds TRX
	acc2 := customerAccount(t, ctx, pool, money.BNB)

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: idemKey(t, "mismatch"),
			EntryType:      "bogus",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				// Line claims BNB against an account whose fixed asset is TRX.
				{AccountCode: acc1, Amount: money.Amount{Asset: money.BNB, Units: 5}},
				{AccountCode: acc2, Amount: money.Amount{Asset: money.BNB, Units: -5}},
			},
		})
		return err
	})
	require.ErrorIs(t, err, journal.ErrAssetMismatch)
}

func TestZeroAmountLineRejected(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: idemKey(t, "zero"),
			EntryType:      "bogus",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5}},
				{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: 0}},
			},
		})
		return err
	})
	require.ErrorIs(t, err, journal.ErrZeroAmountLine)
}

func TestJournalEntriesAppendOnly(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	entry := postSimpleEntry(t, ctx, pool)

	_, err := pool.Exec(ctx, "UPDATE journal_entries SET actor = 'x' WHERE id = $1", entry.ID)
	require.Error(t, err, "table owner must not be able to UPDATE journal_entries")
	_, err = pool.Exec(ctx, "DELETE FROM journal_entries WHERE id = $1", entry.ID)
	require.Error(t, err, "table owner must not be able to DELETE journal_entries")

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	_, err = conn.Exec(ctx, "SET ROLE ledger_writer")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.Exec(ctx, "RESET ROLE") })

	_, err = conn.Exec(ctx, "UPDATE journal_entries SET actor = 'x' WHERE id = $1", entry.ID)
	require.Error(t, err, "ledger_writer must not be able to UPDATE journal_entries")
	_, err = conn.Exec(ctx, "DELETE FROM journal_entries WHERE id = $1", entry.ID)
	require.Error(t, err, "ledger_writer must not be able to DELETE journal_entries")
}

func TestJournalLinesAppendOnly(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	entry := postSimpleEntry(t, ctx, pool)

	_, err := pool.Exec(ctx, "UPDATE journal_lines SET amount_units = 999 WHERE entry_id = $1 AND seq = 0", entry.ID)
	require.Error(t, err, "table owner must not be able to UPDATE journal_lines")
	_, err = pool.Exec(ctx, "DELETE FROM journal_lines WHERE entry_id = $1 AND seq = 0", entry.ID)
	require.Error(t, err, "table owner must not be able to DELETE journal_lines")

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	_, err = conn.Exec(ctx, "SET ROLE ledger_writer")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.Exec(ctx, "RESET ROLE") })

	_, err = conn.Exec(ctx, "UPDATE journal_lines SET amount_units = 999 WHERE entry_id = $1 AND seq = 0", entry.ID)
	require.Error(t, err, "ledger_writer must not be able to UPDATE journal_lines")
	_, err = conn.Exec(ctx, "DELETE FROM journal_lines WHERE entry_id = $1 AND seq = 0", entry.ID)
	require.Error(t, err, "ledger_writer must not be able to DELETE journal_lines")
}

func requireNoEntryWritten(t *testing.T, ctx context.Context, pool *pgxpool.Pool, idempotencyKey string) {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM journal_entries WHERE idempotency_key = $1", idempotencyKey).Scan(&count))
	require.Equal(t, 0, count, "Post must not have written an entry when validation rejected the request")
}
