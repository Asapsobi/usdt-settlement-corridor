//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is the C1.3 ship-gate proof: the SEMANTICS
// table in post.go's Post doc comment holds under genuine concurrency, a
// changed payload is a loud conflict rather than a silent second write,
// line reordering and sub-microsecond occurred_at drift still replay, and
// the idempotency-key length cap holds even against raw SQL.
package journal_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/journal"
	"ledger/internal/money"
)

// testPoolWithMaxConns is testPool but with a larger connection pool, so
// the concurrency test below gets genuine parallelism against Postgres
// rather than being serialized down to testPool's small default.
func testPoolWithMaxConns(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	url := testDatabaseURL(t)
	applyMigrations(t, url)

	cfg, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	cfg.MaxConns = maxConns

	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, accounts.Seed(ctx, pool))
	return pool
}

// runInTx is withTx without any *testing.T dependency: it must never call
// require/t.Fatal, because those are only safe to call from the goroutine
// running the test, and this is used from worker goroutines in the
// concurrency test below.
func runInTx(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op once committed

	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TestPostIsIdempotentUnderConcurrency is C1.3's headline acceptance
// criterion: 1,000 goroutines Post the same key and payload concurrently.
// Exactly one must land as Created, all 1,000 must agree on the entry id,
// and exactly one row must exist afterward.
func TestPostIsIdempotentUnderConcurrency(t *testing.T) {
	pool := testPoolWithMaxConns(t, 50)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	req := journal.EntryRequest{
		IdempotencyKey: idemKey(t, "concurrent"),
		EntryType:      "concurrency_test",
		Actor:          "test",
		OccurredAt:     time.Now(),
		Lines: []journal.Line{
			{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5_000000}},
			{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -5_000000}},
		},
	}

	const n = 1000
	ids := make([]int64, n)
	outcomes := make([]journal.Outcome, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = runInTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				entry, err := journal.Post(ctx, tx, req)
				if err != nil {
					return err
				}
				ids[i] = entry.ID
				outcomes[i] = entry.Outcome
				return nil
			})
		}(i)
	}
	wg.Wait()

	var createdCount int
	firstID := ids[0]
	for i := 0; i < n; i++ {
		require.NoErrorf(t, errs[i], "goroutine %d", i)
		require.Equalf(t, firstID, ids[i], "goroutine %d returned a different entry id", i)
		if outcomes[i] == journal.Created {
			createdCount++
		}
	}
	require.Equal(t, 1, createdCount, "exactly one of 1000 concurrent callers must report Created")

	var rowCount int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM journal_entries WHERE idempotency_key = $1", req.IdempotencyKey).Scan(&rowCount))
	require.Equal(t, 1, rowCount, "exactly one row must exist after 1000 concurrent Posts of the same key")
}

func TestConflictingPayloadRejected(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	key := idemKey(t, "conflict")
	baseLines := []journal.Line{
		{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5_000000}},
		{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -5_000000}},
	}

	var first journal.Entry
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: key, EntryType: "conflict_test", Actor: "test",
			OccurredAt: time.Now(), Lines: baseLines,
		})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, journal.Created, first.Outcome)

	// Same key, one unit different in a line amount -- "one byte different
	// in the payload."
	changedLines := []journal.Line{
		{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5_000001}},
		{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -5_000001}},
	}
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: key, EntryType: "conflict_test", Actor: "test",
			OccurredAt: time.Now(), Lines: changedLines,
		})
		return err
	})
	require.ErrorIs(t, err, journal.ErrIdempotencyConflict)

	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM journal_entries WHERE idempotency_key = $1", key).Scan(&count))
	require.Equal(t, 1, count, "a conflicting payload must not write a second row")
}

func TestReplayWithReorderedLinesSucceeds(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	key := idemKey(t, "reorder")
	occurredAt := time.Now()
	line1 := journal.Line{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5_000000}}
	line2 := journal.Line{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -5_000000}}

	var first journal.Entry
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: key, EntryType: "reorder_test", Actor: "test",
			OccurredAt: occurredAt, Lines: []journal.Line{line1, line2},
		})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, journal.Created, first.Outcome)

	var second journal.Entry
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: key, EntryType: "reorder_test", Actor: "test",
			OccurredAt: occurredAt, Lines: []journal.Line{line2, line1}, // reordered
		})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, journal.Replayed, second.Outcome, "reordered lines with the same key must still replay, not conflict")
	require.Equal(t, first.ID, second.ID)
}

func TestReplaySubMicrosecondOccurredAtDifference(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	key := idemKey(t, "submicro")
	occurredAt := time.Now()
	lines := []journal.Line{
		{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5_000000}},
		{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -5_000000}},
	}

	var first journal.Entry
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: key, EntryType: "submicro_test", Actor: "test",
			OccurredAt: occurredAt, Lines: lines,
		})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, journal.Created, first.Outcome)

	var second journal.Entry
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: key, EntryType: "submicro_test", Actor: "test",
			OccurredAt: occurredAt.Add(400 * time.Nanosecond), Lines: lines,
		})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, journal.Replayed, second.Outcome, "a sub-microsecond occurred_at difference must still replay")

	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: key, EntryType: "submicro_test", Actor: "test",
			OccurredAt: occurredAt.Add(2 * time.Microsecond), Lines: lines,
		})
		return err
	})
	require.ErrorIs(t, err, journal.ErrIdempotencyConflict, "a 2-microsecond occurred_at difference is a real conflict, not noise")
}

func TestIdempotencyKeyTooLongRejectedByGo(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: strings.Repeat("x", 256),
			EntryType:      "too_long_test",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 1}},
				{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -1}},
			},
		})
		return err
	})
	require.ErrorIs(t, err, journal.ErrInvalidIdempotencyKey)
}

func TestIdempotencyKeyLengthEnforcedByDB(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO journal_entries (idempotency_key, payload_hash, entry_type, actor, occurred_at)
		VALUES ($1, $2, 'raw_sql_test', 'test', now())
	`, strings.Repeat("y", 256), []byte("deadbeef"))
	require.Error(t, err, "the CHECK constraint must reject an idempotency_key over 255 bytes even via raw SQL, bypassing Go entirely")
}
