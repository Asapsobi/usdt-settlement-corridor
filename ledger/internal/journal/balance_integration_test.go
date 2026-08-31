//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is the C1.4 ship-gate proof: the two
// balance paths (cache and computed) agree after ordinary posting, stay
// agreeing at scale (10,000 entries), the ascending-account_id ordering
// rule genuinely prevents deadlocks under concurrent overlapping writes,
// BalanceAsOf reproduces history correctly, and VerifyBalances actually
// catches a corrupted cache rather than trusting it.
package journal_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/journal"
	"ledger/internal/money"
)

func TestBalanceOfUntouchedAccountIsZero(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc := customerAccount(t, ctx, pool, money.TRX)

	bal, err := journal.Balance(ctx, pool, acc)
	require.NoError(t, err)
	require.Equal(t, int64(0), bal.Units)
	require.Equal(t, money.TRX, bal.Asset)
}

func TestBalanceMatchesComputedAfterPost(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: idemKey(t, "balance"),
			EntryType:      "balance_test",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 7_000000}},
				{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -7_000000}},
			},
		})
		return err
	})
	require.NoError(t, err)

	cached, err := journal.Balance(ctx, pool, acc1)
	require.NoError(t, err)
	computed, err := journal.BalanceComputed(ctx, pool, acc1)
	require.NoError(t, err)
	require.Equal(t, int64(7_000000), cached.Units)
	require.Equal(t, cached.Units, computed.Units)
}

func TestBalanceAccumulatesAcrossMultipleEntries(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	post := func(units int64, suffix string) {
		err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			_, err := journal.Post(ctx, tx, journal.EntryRequest{
				IdempotencyKey: idemKey(t, "accum:"+suffix),
				EntryType:      "accum_test",
				Actor:          "test",
				OccurredAt:     time.Now(),
				Lines: []journal.Line{
					{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: units}},
					{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -units}},
				},
			})
			return err
		})
		require.NoError(t, err)
	}

	post(1_000000, "a")
	post(2_000000, "b")
	post(-500000, "c")

	bal, err := journal.Balance(ctx, pool, acc1)
	require.NoError(t, err)
	require.Equal(t, int64(2_500000), bal.Units)
}

// TestBalanceAsOfMatchesCacheAtEachStep posts a sequence of entries and
// records the cache balance after each one, then confirms BalanceAsOf
// reproduces exactly those historical values -- not just the current one.
func TestBalanceAsOfMatchesCacheAtEachStep(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	const steps = 20
	entryIDs := make([]int64, steps)
	cacheAfter := make([]int64, steps)
	rng := rand.New(rand.NewSource(99))

	for i := 0; i < steps; i++ {
		amount := int64(rng.Intn(1000) + 1)
		var entry journal.Entry
		err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			entry, err = journal.Post(ctx, tx, journal.EntryRequest{
				IdempotencyKey: idemKey(t, fmt.Sprintf("asof:%d", i)),
				EntryType:      "asof_test",
				Actor:          "test",
				OccurredAt:     time.Now(),
				Lines: []journal.Line{
					{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: amount}},
					{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -amount}},
				},
			})
			return err
		})
		require.NoError(t, err)
		entryIDs[i] = entry.ID

		bal, err := journal.Balance(ctx, pool, acc1)
		require.NoError(t, err)
		cacheAfter[i] = bal.Units
	}

	for _, i := range []int{0, 5, 12, 19} {
		asOf, err := journal.BalanceAsOf(ctx, pool, acc1, entryIDs[i])
		require.NoError(t, err)
		require.Equalf(t, cacheAfter[i], asOf.Units, "BalanceAsOf(entry %d) mismatch at step %d", entryIDs[i], i)
	}
}

func TestVerifyBalancesDetectsCorruption(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	acc1 := customerAccount(t, ctx, pool, money.TRX)
	acc2 := customerAccount(t, ctx, pool, money.TRX)

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, journal.EntryRequest{
			IdempotencyKey: idemKey(t, "corrupt"),
			EntryType:      "corrupt_test",
			Actor:          "test",
			OccurredAt:     time.Now(),
			Lines: []journal.Line{
				{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 3_000000}},
				{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -3_000000}},
			},
		})
		return err
	})
	require.NoError(t, err)

	// The shared test database can carry genuine, unrelated discrepancies
	// from journal activity posted before account_balances existed (C1.2
	// and C1.3 test runs never touched that table, since it didn't exist
	// yet) -- that is real drift VerifyBalances is right to report, not
	// test pollution to suppress. So this test only asserts what it can
	// actually control: its own account's presence or absence in the list.
	before, err := journal.VerifyBalances(ctx, pool)
	require.NoError(t, err)
	require.False(t, hasDiscrepancyFor(before, acc1), "acc1 must not be reported before corruption")

	id1 := accountID(t, ctx, pool, acc1)
	_, err = pool.Exec(ctx, "UPDATE account_balances SET balance_units = balance_units + 999 WHERE account_id = $1", id1)
	require.NoError(t, err)

	after, err := journal.VerifyBalances(ctx, pool)
	require.NoError(t, err)
	d, ok := findDiscrepancyFor(after, acc1)
	require.True(t, ok, "VerifyBalances must name the corrupted account (acc1) after corruption")
	require.NotEqual(t, d.CachedUnits, d.ComputedUnits)
}

func hasDiscrepancyFor(discs []journal.Discrepancy, code string) bool {
	_, ok := findDiscrepancyFor(discs, code)
	return ok
}

func findDiscrepancyFor(discs []journal.Discrepancy, code string) (journal.Discrepancy, bool) {
	for _, d := range discs {
		if d.AccountCode == code {
			return d, true
		}
	}
	return journal.Discrepancy{}, false
}

// TestConcurrentOverlappingEntriesNeverDeadlock is C1.4's deadlock
// acceptance criterion: 200 goroutines post entries touching a shared
// pool of accounts, each in a random subset and random order, so without
// the ascending-account_id ordering rule in applyBalances this would
// deadlock routinely. With it, it must not deadlock even once.
func TestConcurrentOverlappingEntriesNeverDeadlock(t *testing.T) {
	pool := testPoolWithMaxConns(t, 50)
	ctx := context.Background()

	const numAccounts = 6
	codes := make([]string, numAccounts)
	for i := 0; i < numAccounts; i++ {
		code := fmt.Sprintf("liability:customer:%s:deadlock:%s:%d", t.Name(), runID, i)
		_, err := accounts.Create(ctx, pool, code, accounts.Liability, money.TRX)
		require.NoError(t, err)
		codes[i] = code
	}

	const n = 200
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for g := 0; g < n; g++ {
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			numLines := 2 + rng.Intn(3) // 2..4 accounts, random subset, random order
			perm := rng.Perm(numAccounts)[:numLines]

			lines := make([]journal.Line, numLines)
			var sum int64
			for i := 0; i < numLines-1; i++ {
				amt := int64(rng.Intn(1000) + 1)
				lines[i] = journal.Line{AccountCode: codes[perm[i]], Amount: money.Amount{Asset: money.TRX, Units: amt}}
				sum += amt
			}
			lines[numLines-1] = journal.Line{
				AccountCode: codes[perm[numLines-1]],
				Amount:      money.Amount{Asset: money.TRX, Units: -sum},
			}

			errs[g] = runInTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				_, err := journal.Post(ctx, tx, journal.EntryRequest{
					IdempotencyKey: idemKey(t, fmt.Sprintf("deadlock:%d", g)),
					EntryType:      "deadlock_test",
					Actor:          "test",
					OccurredAt:     time.Now(),
					Lines:          lines,
				})
				return err
			})
		}(g)
	}
	wg.Wait()

	var deadlocks, otherErrors int
	for i, err := range errs {
		if err == nil {
			continue
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			deadlocks++
			t.Logf("goroutine %d: deadlock: %v", i, err)
			continue
		}
		otherErrors++
		t.Logf("goroutine %d: unexpected error: %v", i, err)
	}
	require.Equal(t, 0, deadlocks, "zero deadlocks expected: the ascending account_id ordering rule must prevent them entirely")
	require.Equal(t, 0, otherErrors, "no other errors expected either")
}

// TestBulkEntriesVerifyAndTrialBalance is C1.4's other headline
// acceptance criterion: after 10,000 randomly generated valid entries
// spread across a shared account pool, VerifyBalances must find nothing
// and TrialBalance must be exactly zero for every asset touched.
func TestBulkEntriesVerifyAndTrialBalance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 10,000-entry test in -short mode")
	}
	pool := testPool(t)
	ctx := context.Background()

	assetList := []money.Asset{money.TRX, money.BNB, money.USDT_BEP20, money.USDT_TRC20}
	const accountsPerAsset = 10
	accountsByAsset := make(map[money.Asset][]string)
	for _, asset := range assetList {
		for i := 0; i < accountsPerAsset; i++ {
			code := fmt.Sprintf("liability:customer:%s:bulk:%s:%s:%d", t.Name(), runID, asset, i)
			_, err := accounts.Create(ctx, pool, code, accounts.Liability, asset)
			require.NoError(t, err)
			accountsByAsset[asset] = append(accountsByAsset[asset], code)
		}
	}

	rng := rand.New(rand.NewSource(1))
	const n = 10000
	for i := 0; i < n; i++ {
		asset := assetList[rng.Intn(len(assetList))]
		candidates := accountsByAsset[asset]
		a := candidates[rng.Intn(len(candidates))]
		b := candidates[rng.Intn(len(candidates))]
		for b == a {
			b = candidates[rng.Intn(len(candidates))]
		}
		amount := int64(rng.Intn(1_000_000) + 1)

		err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			_, err := journal.Post(ctx, tx, journal.EntryRequest{
				IdempotencyKey: idemKey(t, fmt.Sprintf("bulk:%d", i)),
				EntryType:      "bulk_test",
				Actor:          "test",
				OccurredAt:     time.Now(),
				Lines: []journal.Line{
					{AccountCode: a, Amount: money.Amount{Asset: asset, Units: amount}},
					{AccountCode: b, Amount: money.Amount{Asset: asset, Units: -amount}},
				},
			})
			return err
		})
		require.NoErrorf(t, err, "entry %d", i)
	}

	// Scoped to this test's own accounts, not asserted globally: the
	// shared test database can carry genuine, unrelated discrepancies
	// from journal activity posted before account_balances existed (see
	// TestVerifyBalancesDetectsCorruption for the same reasoning).
	discrepancies, err := journal.VerifyBalances(ctx, pool)
	require.NoError(t, err)
	for _, codes := range accountsByAsset {
		for _, code := range codes {
			require.Falsef(t, hasDiscrepancyFor(discrepancies, code), "account %s has a discrepancy after 10,000 entries", code)
		}
	}

	// TrialBalance sums journal_lines directly with no dependency on
	// account_balances, so unlike VerifyBalances above it is safe to
	// assert globally: every entry ever posted, including ones from
	// earlier chunks' test runs, was individually balance-checked at
	// post time (Go and the DB trigger both), so the cumulative sum per
	// asset must be exactly zero regardless of how much history exists.
	trial, err := journal.TrialBalance(ctx, pool)
	require.NoError(t, err)
	for asset, total := range trial {
		require.Equalf(t, int64(0), total, "trial balance for %s is %d, want 0", asset, total)
	}
}
