//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is C1.7's HALT SEMANTICS acceptance
// criterion, tested against real Transition/HandleDepositReorg calls
// while the ledger is genuinely halted: a halt-blocked transition is
// rejected, but deposit recording, reads, and a reversal-driven
// transition all keep working -- "you must be able to keep observing and
// correcting while halted" is not a slogan here, it's the thing this test
// actually exercises.
package orders_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/orders"
)

func TestHaltBlocksOnlyHaltBlockedTransitions(t *testing.T) {
	pool := freshIsolatedPool(t)
	ctx := context.Background()

	// screened -> dispatching is HaltBlocked=true.
	order := advanceToState(t, ctx, pool, orders.Screened)

	require.NoError(t, halt.Set(ctx, pool, halt.SetParams{Reason: "TEST_HALT", Actor: "test"}))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `UPDATE system_state SET halted = false WHERE id = 1`)
	})

	acc1, acc2 := twoTRXAccounts(t, ctx, pool)
	entry := simpleEntry(t, acc1, acc2)
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.Transition(ctx, tx, order.ID, orders.Dispatching, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "should be blocked while halted", OccurredAt: time.Now(), Entry: &entry,
		})
		return err
	})
	require.ErrorIs(t, err, orders.ErrSystemHalted, "screened -> dispatching must be rejected while halted")

	// quoted -> funded (deposit recording) is NOT halt-blocked and must
	// keep working: we must keep recording inbound reality while halted.
	_, custBEP, _ := depositedOrder(t, ctx, pool)
	bal, err := journal.Balance(ctx, pool, custBEP)
	require.NoError(t, err)
	require.Equal(t, int64(-3000_000000), bal.Units, "deposit recording must succeed while halted")

	// Reads must succeed while halted.
	_, err = orders.Get(ctx, pool, order.ID)
	require.NoError(t, err)

	// A reversal must succeed while halted. Exercised via
	// HandleDepositReorg scenario A, which calls journal.Reverse and then
	// Transition(Funded -> Quoted) -- HaltBlocked=false for that pair,
	// deliberately, for exactly this reason.
	orderR, custBEPR, depositKeyR := depositedOrder(t, ctx, pool)
	var updated orders.Order
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = orders.HandleDepositReorg(ctx, tx, orderR.ID, depositKeyR, "test")
		return err
	})
	require.NoError(t, err, "a reversal-driven transition must succeed while halted")
	require.Equal(t, orders.Quoted, updated.State)

	balR, err := journal.Balance(ctx, pool, custBEPR)
	require.NoError(t, err)
	require.Equal(t, int64(0), balR.Units)

	halted, err := halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.True(t, halted, "sanity check: the halt must still be in effect throughout this test")
}

func TestHaltCacheNotConfiguredFailsClosed(t *testing.T) {
	// This test intentionally does NOT call testPool/prepareTestPool's
	// SetHaltCache wiring path through the normal route -- it directly
	// resets the package-level cache to nil to prove Transition fails
	// closed (rejects, not silently allows) when no cache was ever
	// configured, rather than relying on every other test's setup order
	// to keep this untested.
	pool := testPool(t)
	ctx := context.Background()
	order := advanceToState(t, ctx, pool, orders.Screened)

	orders.SetHaltCache(nil)
	t.Cleanup(func() {
		cache, err := halt.NewCache(ctx, pool)
		require.NoError(t, err)
		orders.SetHaltCache(cache)
	})

	acc1, acc2 := twoTRXAccounts(t, ctx, pool)
	entry := simpleEntry(t, acc1, acc2)
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.Transition(ctx, tx, order.ID, orders.Dispatching, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "no cache configured", OccurredAt: time.Now(), Entry: &entry,
		})
		return err
	})
	require.ErrorIs(t, err, orders.ErrHaltCacheNotConfigured)
}
