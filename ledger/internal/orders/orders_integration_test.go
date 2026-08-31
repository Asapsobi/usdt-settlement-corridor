//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is the C1.5 ship-gate proof.
//
// expectedTransitions below is written independently of the production
// transitionTable in transitions.go, copied straight from the build
// spec's table rather than imported from the package under test. The
// point of the exhaustive cross-product test is to check that
// orders.Transition's actual runtime behavior matches the SPEC, not that
// the unexported map agrees with itself.
package orders_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cryptorand "crypto/rand"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/money"
	"ledger/internal/orders"
)

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

// prepareTestPool does the setup every orders test needs regardless of
// how its pool was constructed: seed the fixed chart of accounts, wire up
// the halt cache Transition now requires (SetHaltCache is a package-level
// call, so every test process needs it, not just ones that touch halt
// directly), and reset system_state to unhalted. That last part matters
// even for tests that never mention halting: system_state is a single
// shared row in the persistent test database, so without resetting it, a
// halt left behind by an earlier reorg-scenario-B test (in this run or an
// earlier one) would make every halt-blocked transition in an unrelated
// test fail with ErrSystemHalted for reasons that test never caused.
func prepareTestPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	require.NoError(t, accounts.Seed(ctx, pool))
	orders.SetHaltCache(halt.NewCache(pool))
	_, err := pool.Exec(ctx, `
		UPDATE system_state
		SET halted = false, halt_reason = NULL, halt_detail = NULL, halted_at = NULL, halted_by = NULL
		WHERE id = 1
	`)
	require.NoError(t, err)
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testDatabaseURL(t)
	applyMigrations(t, url)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	prepareTestPool(t, ctx, pool)
	return pool
}

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

	prepareTestPool(t, ctx, pool)
	return pool
}

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

// runInTx is withTx without any *testing.T dependency, for use from
// worker goroutines, where require/t.Fatal are not safe to call.
func runInTx(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var extIDSeq, accountSeq, entrySeq int64

func newQuotedOrder(t *testing.T, ctx context.Context, pool *pgxpool.Pool) orders.Order {
	t.Helper()
	n := atomic.AddInt64(&extIDSeq, 1)
	o, err := orders.Create(ctx, pool, orders.CreateParams{
		ExternalID:       fmt.Sprintf("ext:%s:%s:%d", t.Name(), runID, n),
		CustomerID:       "cust-1",
		Tier:             orders.Standard,
		AmountIn:         money.Amount{Asset: money.USDT_BEP20, Units: 3000_000000},
		AmountOut:        money.Amount{Asset: money.USDT_TRC20, Units: 2990_700000},
		FeeUnits:         money.Amount{Asset: money.USDT_TRC20, Units: 7_500000},
		NetworkFeeUnits:  money.Amount{Asset: money.USDT_TRC20, Units: 1_800000},
		RecipientAddress: "T-recipient-address",
		QuotedAt:         time.Now(),
		QuoteExpiresAt:   time.Now().Add(90 * time.Second),
	})
	require.NoError(t, err)
	return o
}

func twoTRXAccounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	n := atomic.AddInt64(&accountSeq, 1)
	a := fmt.Sprintf("liability:customer:%s:%s:orderentry:%d:a", t.Name(), runID, n)
	b := fmt.Sprintf("liability:customer:%s:%s:orderentry:%d:b", t.Name(), runID, n)
	_, err := accounts.Create(ctx, pool, a, accounts.Liability, money.TRX)
	require.NoError(t, err)
	_, err = accounts.Create(ctx, pool, b, accounts.Liability, money.TRX)
	require.NoError(t, err)
	return a, b
}

func simpleEntry(t *testing.T, acc1, acc2 string) journal.EntryRequest {
	t.Helper()
	n := atomic.AddInt64(&entrySeq, 1)
	return journal.EntryRequest{
		IdempotencyKey: fmt.Sprintf("orders_test:%s:%s:%d", runID, t.Name(), n),
		EntryType:      "order_transition_test",
		Actor:          "test",
		OccurredAt:     time.Now(),
		Lines: []journal.Line{
			{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 1_000000}},
			{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -1_000000}},
		},
	}
}

func transitionRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orderID int64) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM order_transitions WHERE order_id = $1", orderID).Scan(&n))
	return n
}

// expectedRule and expectedTransitions are the independent, hand-copied
// version of the build spec's transition table (11 pairs). allStates and
// pathToState derive the walk needed to place a fresh order at any given
// state, purely from this same independent table.
type expectedRule struct {
	RequiresEntry bool
}

var expectedTransitions = map[orders.State]map[orders.State]expectedRule{
	orders.Quoted: {
		orders.Funded:  {RequiresEntry: true},
		orders.Expired: {RequiresEntry: false},
	},
	orders.Funded: {
		orders.Screened: {RequiresEntry: false},
		orders.Held:     {RequiresEntry: false},
		orders.Refunded: {RequiresEntry: true},
		orders.Quoted:   {RequiresEntry: true},
	},
	orders.Held: {
		orders.Screened: {RequiresEntry: false},
		orders.Refunded: {RequiresEntry: true},
	},
	orders.Screened: {
		orders.Dispatching: {RequiresEntry: true},
	},
	orders.Dispatching: {
		orders.Settled: {RequiresEntry: true},
		orders.Held:    {RequiresEntry: true},
	},
}

var allStates = []orders.State{
	orders.Quoted, orders.Funded, orders.Screened, orders.Dispatching,
	orders.Settled, orders.Held, orders.Refunded, orders.Expired,
}

var pathToState = map[orders.State][]orders.State{
	orders.Quoted:      nil,
	orders.Funded:      {orders.Funded},
	orders.Screened:    {orders.Funded, orders.Screened},
	orders.Held:        {orders.Funded, orders.Held},
	orders.Dispatching: {orders.Funded, orders.Screened, orders.Dispatching},
	orders.Settled:     {orders.Funded, orders.Screened, orders.Dispatching, orders.Settled},
	orders.Refunded:    {orders.Funded, orders.Refunded},
	orders.Expired:     {orders.Expired},
}

// advanceToState creates a fresh order and walks it, via real,
// legitimate Transition calls, from Quoted to target.
func advanceToState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, target orders.State) orders.Order {
	t.Helper()
	order := newQuotedOrder(t, ctx, pool)
	current := order.State
	version := order.Version

	for _, next := range pathToState[target] {
		rule, ok := expectedTransitions[current][next]
		require.Truef(t, ok, "no expected rule for %s -> %s while walking to %s", current, next, target)

		var entry *journal.EntryRequest
		if rule.RequiresEntry {
			acc1, acc2 := twoTRXAccounts(t, ctx, pool)
			e := simpleEntry(t, acc1, acc2)
			entry = &e
		}

		var updated orders.Order
		err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = orders.Transition(ctx, tx, order.ID, next, version, orders.TransitionParams{
				Actor:      "test",
				Reason:     "advance to " + string(target),
				OccurredAt: time.Now(),
				Entry:      entry,
			})
			return err
		})
		require.NoErrorf(t, err, "advancing %s -> %s while walking to %s", current, next, target)

		order = updated
		current = updated.State
		version = updated.Version
	}
	return order
}

// TestFullStateCrossProduct is C1.5's headline acceptance criterion: over
// the full 8x8 state cross product, exactly the 11 pairs in
// expectedTransitions succeed and the other 53 return ErrIllegalTransition
// while writing nothing -- no order_transitions row, no change to the
// order's own state or version. A settled order rejecting every further
// transition is a special case of this: Settled has zero legal outgoing
// pairs, so all 8 of its targets are exercised here as illegal.
func TestFullStateCrossProduct(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var legalCount, illegalCount int

	for _, from := range allStates {
		probe := advanceToState(t, ctx, pool, from)
		beforeState, beforeVersion := probe.State, probe.Version

		for _, to := range allStates {
			if _, ok := expectedTransitions[from][to]; ok {
				legalCount++
				continue // exercised for real, separately, below
			}
			illegalCount++

			before := transitionRowCount(t, ctx, pool, probe.ID)
			err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orders.Transition(ctx, tx, probe.ID, to, beforeVersion, orders.TransitionParams{
					Actor: "test", Reason: "illegal probe", OccurredAt: time.Now(),
				})
				return err
			})
			require.ErrorIsf(t, err, orders.ErrIllegalTransition, "%s -> %s should be illegal", from, to)

			after := transitionRowCount(t, ctx, pool, probe.ID)
			require.Equalf(t, before, after, "%s -> %s: illegal attempt wrote an order_transitions row", from, to)

			reloaded, err := orders.Get(ctx, pool, probe.ID)
			require.NoError(t, err)
			require.Equalf(t, beforeState, reloaded.State, "%s -> %s: illegal attempt changed order state", from, to)
			require.Equalf(t, beforeVersion, reloaded.Version, "%s -> %s: illegal attempt changed order version", from, to)
		}
	}

	require.Equal(t, 11, legalCount, "expected exactly 11 legal pairs in the 8x8 cross product")
	require.Equal(t, 53, illegalCount, "expected exactly 53 illegal pairs in the 8x8 cross product")

	for from, targets := range expectedTransitions {
		for to, rule := range targets {
			order := advanceToState(t, ctx, pool, from)

			var entry *journal.EntryRequest
			if rule.RequiresEntry {
				acc1, acc2 := twoTRXAccounts(t, ctx, pool)
				e := simpleEntry(t, acc1, acc2)
				entry = &e
			}

			var updated orders.Order
			err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				updated, err = orders.Transition(ctx, tx, order.ID, to, order.Version, orders.TransitionParams{
					Actor: "test", Reason: "legal transition test", OccurredAt: time.Now(), Entry: entry,
				})
				return err
			})
			require.NoErrorf(t, err, "%s -> %s should be legal", from, to)
			require.Equal(t, to, updated.State)
			require.Equal(t, order.Version+1, updated.Version)
		}
	}
}

func TestTransitionRequiringEntryWithoutOneFails(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := advanceToState(t, ctx, pool, orders.Funded) // Funded -> Refunded requires an entry

	before := transitionRowCount(t, ctx, pool, order.ID)
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.Transition(ctx, tx, order.ID, orders.Refunded, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "no entry provided", OccurredAt: time.Now(),
		})
		return err
	})
	require.ErrorIs(t, err, orders.ErrEntryRequired)

	after := transitionRowCount(t, ctx, pool, order.ID)
	require.Equal(t, before, after, "must write nothing when a required entry is missing")

	reloaded, err := orders.Get(ctx, pool, order.ID)
	require.NoError(t, err)
	require.Equal(t, order.State, reloaded.State)
	require.Equal(t, order.Version, reloaded.Version)
}

func TestTransitionWithUnbalancedEntryFailsAndLeavesOrderUnchanged(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := advanceToState(t, ctx, pool, orders.Screened) // Screened -> Dispatching requires an entry

	acc1, acc2 := twoTRXAccounts(t, ctx, pool)
	badEntry := journal.EntryRequest{
		IdempotencyKey: fmt.Sprintf("orders_test:%s:%s:unbalanced", runID, t.Name()),
		EntryType:      "bad",
		Actor:          "test",
		OccurredAt:     time.Now(),
		Lines: []journal.Line{
			{AccountCode: acc1, Amount: money.Amount{Asset: money.TRX, Units: 5}},
			{AccountCode: acc2, Amount: money.Amount{Asset: money.TRX, Units: -3}}, // imbalance
		},
	}

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.Transition(ctx, tx, order.ID, orders.Dispatching, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "unbalanced entry", OccurredAt: time.Now(), Entry: &badEntry,
		})
		return err
	})
	require.Error(t, err)
	require.ErrorIs(t, err, journal.ErrUnbalanced)

	reloaded, err := orders.Get(ctx, pool, order.ID)
	require.NoError(t, err)
	require.Equal(t, order.State, reloaded.State, "order state must be unchanged after a failed transition")
	require.Equal(t, order.Version, reloaded.Version, "order version must be unchanged after a failed transition")
}

// TestConcurrentTransitionSameVersionExactlyOneSucceeds is C1.5's
// concurrency acceptance criterion: 50 goroutines racing Transition with
// the same expectedVersion must produce exactly one success and 49
// ErrVersionConflict -- never a retry-in-place, never more than one
// winner.
func TestConcurrentTransitionSameVersionExactlyOneSucceeds(t *testing.T) {
	pool := testPoolWithMaxConns(t, 50)
	ctx := context.Background()
	order := advanceToState(t, ctx, pool, orders.Quoted) // Quoted -> Expired needs no entry

	const n = 50
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for g := 0; g < n; g++ {
		go func(g int) {
			defer wg.Done()
			errs[g] = runInTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orders.Transition(ctx, tx, order.ID, orders.Expired, order.Version, orders.TransitionParams{
					Actor: "test", Reason: "concurrency test", OccurredAt: time.Now(),
				})
				return err
			})
		}(g)
	}
	wg.Wait()

	var successCount, conflictCount int
	for i, err := range errs {
		switch {
		case err == nil:
			successCount++
		case errors.Is(err, orders.ErrVersionConflict):
			conflictCount++
		default:
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	require.Equal(t, 1, successCount, "exactly one of 50 concurrent same-version transitions must succeed")
	require.Equal(t, n-1, conflictCount, "the other 49 must all get ErrVersionConflict")

	reloaded, err := orders.Get(ctx, pool, order.ID)
	require.NoError(t, err)
	require.Equal(t, orders.Expired, reloaded.State)
	require.Equal(t, order.Version+1, reloaded.Version)
}
