//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is the Reverse half of C1.6's ship gate:
// negation is exact, a second reversal of the same entry is a hard error
// (not a silent replay) both through the Go API and via raw SQL bypassing
// it entirely, a reversal cannot itself be reversed, and the trial
// balance holds after all of it.
package journal_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"ledger/internal/journal"
	"ledger/internal/money"
)

func postSimpleTRXEntry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, acc1, acc2 string, key string) journal.Entry {
	t.Helper()
	var entry journal.Entry
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	entry, err = journal.Post(ctx, tx, journal.EntryRequest{
		IdempotencyKey: key,
		EntryType:      "reversal_test_base",
		Actor:          "test",
		OccurredAt:     time.Now(),
		Lines: []journal.Line{
			{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 4_000000}},
			{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -4_000000}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	return entry
}

func TestReverseNegatesLinesAndSetsReversalOf(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)
	original := postSimpleTRXEntry(t, ctx, pool, acc1, acc2, idemKey(t, "orig"))

	var reversal journal.Entry
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reversal, err = journal.Reverse(ctx, tx, original.ID, "test", "test reversal", time.Now())
		return err
	})
	require.NoError(t, err)

	require.NotNil(t, reversal.ReversalOf)
	require.Equal(t, original.ID, *reversal.ReversalOf)
	require.Equal(t, "ledger:reverse:"+original.IdempotencyKey, reversal.IdempotencyKey)
	require.Equal(t, journal.Created, reversal.Outcome)
	require.Len(t, reversal.Lines, len(original.Lines))

	for i, ol := range original.Lines {
		rl := reversal.Lines[i]
		require.Equal(t, ol.AccountCode, rl.AccountCode)
		require.Equal(t, ol.Amount.Asset, rl.Amount.Asset)
		require.Equal(t, -ol.Amount.Units, rl.Amount.Units, "line %d must be exactly negated", i)
	}
}

func TestReverseAppliesBalancesBackToZero(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)
	original := postSimpleTRXEntry(t, ctx, pool, acc1, acc2, idemKey(t, "orig"))

	afterOriginal, err := journal.Balance(ctx, pool, acc1)
	require.NoError(t, err)
	require.Equal(t, int64(4_000000), afterOriginal.Units)

	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Reverse(ctx, tx, original.ID, "test", "undo", time.Now())
		return err
	})
	require.NoError(t, err)

	afterReversal, err := journal.Balance(ctx, pool, acc1)
	require.NoError(t, err)
	require.Equal(t, int64(0), afterReversal.Units, "balance must return to exactly its pre-original value")
}

func TestReverseTwiceErrorsAndWritesOnlyOneRow(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)
	original := postSimpleTRXEntry(t, ctx, pool, acc1, acc2, idemKey(t, "orig"))

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Reverse(ctx, tx, original.ID, "test", "first reversal", time.Now())
		return err
	})
	require.NoError(t, err)

	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Reverse(ctx, tx, original.ID, "test", "second reversal attempt", time.Now())
		return err
	})
	require.ErrorIs(t, err, journal.ErrAlreadyReversed)

	var count int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM journal_entries WHERE reversal_of = $1", original.ID).Scan(&count))
	require.Equal(t, 1, count, "exactly one reversal row must exist")
}

// TestReverseOfReversalUniqueConstraintViaRawSQL bypasses Reverse()
// entirely: it proves the UNIQUE constraint on reversal_of holds even
// against raw SQL, independent of Go's own preliminary check.
func TestReverseUniqueConstraintHoldsViaRawSQL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)
	original := postSimpleTRXEntry(t, ctx, pool, acc1, acc2, idemKey(t, "orig"))

	_, err := pool.Exec(ctx, `
		INSERT INTO journal_entries (idempotency_key, payload_hash, entry_type, actor, occurred_at, reversal_of)
		VALUES ($1, $2, 'reversal', 'test', now(), $3)
	`, "raw:reversal:1:"+idemKey(t, "raw1"), []byte("deadbeef"), original.ID)
	require.NoError(t, err, "first raw reversal row must succeed")

	_, err = pool.Exec(ctx, `
		INSERT INTO journal_entries (idempotency_key, payload_hash, entry_type, actor, occurred_at, reversal_of)
		VALUES ($1, $2, 'reversal', 'test', now(), $3)
	`, "raw:reversal:2:"+idemKey(t, "raw2"), []byte("deadbeef"), original.ID)
	require.Error(t, err, "a second row naming the same reversal_of must be rejected by the UNIQUE constraint")
}

func TestCannotReverseAReversal(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)
	original := postSimpleTRXEntry(t, ctx, pool, acc1, acc2, idemKey(t, "orig"))

	var reversal journal.Entry
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reversal, err = journal.Reverse(ctx, tx, original.ID, "test", "first reversal", time.Now())
		return err
	})
	require.NoError(t, err)

	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Reverse(ctx, tx, reversal.ID, "test", "reverse the reversal", time.Now())
		return err
	})
	require.ErrorIs(t, err, journal.ErrCannotReverseAReversal)
}

func TestReverseUnknownEntryErrors(t *testing.T) {
	pool := testPool(t)

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Reverse(ctx, tx, -1, "test", "no such entry", time.Now())
		return err
	})
	require.ErrorIs(t, err, journal.ErrEntryNotFound)
}

func TestReverseKeepsTrialBalanceZero(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)
	original := postSimpleTRXEntry(t, ctx, pool, acc1, acc2, idemKey(t, "orig"))

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Reverse(ctx, tx, original.ID, "test", "undo", time.Now())
		return err
	})
	require.NoError(t, err)

	trial, err := journal.TrialBalance(ctx, pool)
	require.NoError(t, err)
	require.Equalf(t, int64(0), trial[money.TRX], "trial balance for TRX after a reversal must be 0")
}
