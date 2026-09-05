//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is the HandleDepositReorg half of C1.6's
// ship gate: scenario A returns an order to quoted with the books back to
// zero; scenario B (both starting states the spec names -- dispatching
// and settled) books an exact loss, halts the ledger, and leaves the
// order's state untouched; an order in a state neither scenario is
// defined for gets a named error, not a guess.
package orders_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/money"
	"ledger/internal/orders"
)

// resetHaltState directly clears system_state via raw SQL so each halt
// test starts from a known, unhalted baseline regardless of what earlier
// tests (in this run or a prior one, against the same persistent test
// database) left behind. There is no ClearHalt yet -- that is C1.7's job
// -- so this is a test-fixture reset, not a claim that production code
// can clear a halt this way.
func resetHaltState(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		UPDATE system_state
		SET halted = false, halt_reason = NULL, halt_detail = NULL, halted_at = NULL, halted_by = NULL
		WHERE id = 1
	`)
	require.NoError(t, err)
}

// depositedOrder creates a fresh order and walks it to Funded, posting a
// real E1 (deposit_final) entry with a known idempotency key, exactly as
// C2 (the deposit watcher) would -- so the returned key is what a caller
// would pass to HandleDepositReorg.
func depositedOrder(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (order orders.Order, custBEP, depositKey string) {
	t.Helper()
	order = newQuotedOrder(t, ctx, pool)

	custBEP = fmt.Sprintf("liability:customer:%s:%s:reorg:%d", t.Name(), runID, order.ID)
	_, err := accounts.Create(ctx, pool, custBEP, accounts.Liability, money.USDT_BEP20)
	require.NoError(t, err)

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	_, err = accounts.Create(ctx, pool, depositAcc, accounts.Asset, money.USDT_BEP20)
	require.NoError(t, err)

	depositKey = fmt.Sprintf("watcher:deposit_final:%s:%s:%d", t.Name(), runID, order.ID)
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		order, err = orders.Transition(ctx, tx, order.ID, orders.Funded, order.Version, orders.TransitionParams{
			Actor: "test:watcher", Reason: "deposit reached 15 confirmations", OccurredAt: time.Now(),
			Entry: &journal.EntryRequest{
				IdempotencyKey: depositKey,
				EntryType:      "deposit_final",
				Actor:          "test:watcher",
				OccurredAt:     time.Now(),
				OrderID:        &order.ID,
				Lines: []journal.Line{
					{AccountCode: depositAcc, Amount: money.Amount{Asset: money.USDT_BEP20, Units: order.AmountIn.Units}},
					{AccountCode: custBEP, Amount: money.Amount{Asset: money.USDT_BEP20, Units: -order.AmountIn.Units}},
				},
			},
		})
		return err
	})
	require.NoError(t, err)
	return order, custBEP, depositKey
}

func TestReorgScenarioA(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order, custBEP, depositKey := depositedOrder(t, ctx, pool)

	beforeBalance, err := journal.Balance(ctx, pool, custBEP)
	require.NoError(t, err)
	require.NotEqual(t, int64(0), beforeBalance.Units, "sanity check: the deposit must have actually moved the balance")

	var updated orders.Order
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = orders.HandleDepositReorg(ctx, tx, order.ID, depositKey, "test:watcher")
		return err
	})
	require.NoError(t, err)

	require.Equal(t, orders.Quoted, updated.State, "scenario A must return the order to quoted")
	require.Equal(t, order.Version+1, updated.Version)

	afterBalance, err := journal.Balance(ctx, pool, custBEP)
	require.NoError(t, err)
	require.Equal(t, int64(0), afterBalance.Units, "the deposit's effect on the customer's BEP20 balance must be fully undone")

	trial, err := journal.TrialBalance(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, int64(0), trial[money.USDT_BEP20])
}

// TestReorgRetryFromQuoted_AfterScenarioA_IsIdempotent covers the exact
// gap found while building C2 (the deposit watcher) against a real
// running ledgerd: a caller that redelivers a scenario-A reorg report
// after the order is already back in Quoted must see success, not
// ErrUnexpectedState -- invariant 2's idempotency guarantee applies to
// this endpoint too, not just to journal.Post/Reverse directly.
func TestReorgRetryFromQuoted_AfterScenarioA_IsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order, _, depositKey := depositedOrder(t, ctx, pool)

	var afterFirst orders.Order
	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		afterFirst, err = orders.HandleDepositReorg(ctx, tx, order.ID, depositKey, "test:watcher")
		return err
	})
	require.NoError(t, err)
	require.Equal(t, orders.Quoted, afterFirst.State)

	var afterRetry orders.Order
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		afterRetry, err = orders.HandleDepositReorg(ctx, tx, order.ID, depositKey, "test:watcher")
		return err
	})
	require.NoError(t, err, "a redelivered scenario-A report must not error")
	require.Equal(t, orders.Quoted, afterRetry.State)
	require.Equal(t, afterFirst.Version, afterRetry.Version, "a retry must post nothing and transition nothing")

	trial, err := journal.TrialBalance(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, int64(0), trial[money.USDT_BEP20], "a retry must have zero additional effect on the books")
}

// TestReorgOnFreshQuotedOrder_StillErrors guards the narrowness of that
// fix: an order that has been Quoted since creation -- never funded at
// all -- must still get ErrUnexpectedState for an unrelated or bogus
// key, not be silently treated as an idempotent reorg retry.
func TestReorgOnFreshQuotedOrder_StillErrors(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := newQuotedOrder(t, ctx, pool)

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.HandleDepositReorg(ctx, tx, order.ID, "no-such-entry-key", "test:watcher")
		return err
	})
	require.ErrorIs(t, err, orders.ErrUnexpectedState)
	require.Contains(t, err.Error(), string(orders.Quoted))
}

// TestTransition_ReplayAfterAlreadyFunded_IsIdempotent covers the C2.7
// gap found while building C2's ReportDepositFinal against a real
// running ledgerd: a redelivered "fund this order" request -- the exact
// shape a C2 restart re-observing an already-final on-chain deposit
// produces, since C2's own finality.Tracker keeps no persistent record
// of what it already reported -- must succeed as a no-op, not
// ErrIllegalTransition (Funded -> Funded is not a legal transitionTable
// pair, so before this fix it fell straight through to that error).
func TestTransition_ReplayAfterAlreadyFunded_IsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order, _, depositKey := depositedOrder(t, ctx, pool)
	require.Equal(t, orders.Funded, order.State)

	original, err := journal.GetEntryByKey(ctx, pool, depositKey)
	require.NoError(t, err)
	lines := make([]journal.Line, len(original.Lines))
	for i, l := range original.Lines {
		lines[i] = journal.Line{AccountCode: l.AccountCode, Amount: l.Amount}
	}

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	balanceBefore, err := journal.Balance(ctx, pool, depositAcc)
	require.NoError(t, err)

	// The redelivered request: same idempotency key, same entry_type,
	// order_id, lines, and occurred_at (canonicalHash's exact inputs) --
	// actor and the transition's own reason may differ, since neither is
	// part of the entry's payload hash.
	var replay orders.Order
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		replay, err = orders.Transition(ctx, tx, order.ID, orders.Funded, order.Version, orders.TransitionParams{
			Actor: "test:watcher-restart", Reason: "redelivered deposit_final report", OccurredAt: time.Now(),
			Entry: &journal.EntryRequest{
				IdempotencyKey: depositKey,
				EntryType:      original.EntryType,
				Actor:          "test:watcher-restart",
				OccurredAt:     original.OccurredAt,
				OrderID:        &order.ID,
				Lines:          lines,
			},
		})
		return err
	})
	require.NoError(t, err, "a redelivered fund request must succeed as a no-op replay")
	require.Equal(t, orders.Funded, replay.State)
	require.Equal(t, order.Version, replay.Version, "a replay must not bump the version")

	balanceAfter, err := journal.Balance(ctx, pool, depositAcc)
	require.NoError(t, err)
	require.Equal(t, balanceBefore.Units, balanceAfter.Units, "a replay must not double-credit")
}

// TestTransition_SameStateNewEntry_StillIllegal guards the narrowness of
// that fix: a BRAND NEW idempotency key naming the order's current state
// must still be rejected outright, never silently posted through this
// path -- replayIfAlreadyPosted only ever acts on a key that already
// exists.
func TestTransition_SameStateNewEntry_StillIllegal(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order, custBEP, _ := depositedOrder(t, ctx, pool)
	require.Equal(t, orders.Funded, order.State)

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	newKey := fmt.Sprintf("test:brand-new-key:%s:%d", runID, order.ID)

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.Transition(ctx, tx, order.ID, orders.Funded, order.Version, orders.TransitionParams{
			Actor: "test", Reason: "should be rejected", OccurredAt: time.Now(),
			Entry: &journal.EntryRequest{
				IdempotencyKey: newKey,
				EntryType:      "deposit_final",
				Actor:          "test",
				OccurredAt:     time.Now(),
				OrderID:        &order.ID,
				Lines: []journal.Line{
					{AccountCode: depositAcc, Amount: order.AmountIn},
					{AccountCode: custBEP, Amount: money.Amount{Asset: order.AmountIn.Asset, Units: -order.AmountIn.Units}},
				},
			},
		})
		return err
	})
	require.ErrorIs(t, err, orders.ErrIllegalTransition)

	_, err = journal.GetEntryByKey(ctx, pool, newKey)
	require.ErrorIs(t, err, journal.ErrEntryNotFound, "a brand-new key must never get posted through the same-state replay path")
}

// advanceDepositedOrderTo walks an already-Funded, already-deposited
// order the rest of the way to target (Screened, Dispatching, or
// Settled), posting the real E2/E3 entries the §B worked example uses,
// against fresh accounts unrelated to the deposit side.
func advanceDepositedOrderTo(t *testing.T, ctx context.Context, pool *pgxpool.Pool, order orders.Order, custBEP string, target orders.State) orders.Order {
	t.Helper()

	var err error
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		order, err = orders.Transition(ctx, tx, order.ID, orders.Screened, order.Version, orders.TransitionParams{
			Actor: "test:screening", Reason: "screening verdict pass", OccurredAt: time.Now(),
		})
		return err
	})
	require.NoError(t, err)
	if target == orders.Screened {
		return order
	}

	custTRC := fmt.Sprintf("liability:customer:%s:%s:reorg_trc:%d", t.Name(), runID, order.ID)
	_, err = accounts.Create(ctx, pool, custTRC, accounts.Liability, money.USDT_TRC20)
	require.NoError(t, err)

	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		order, err = orders.Transition(ctx, tx, order.ID, orders.Dispatching, order.Version, orders.TransitionParams{
			Actor: "test:dispatcher", Reason: "posting the conversion entry", OccurredAt: time.Now(),
			Entry: &journal.EntryRequest{
				IdempotencyKey: fmt.Sprintf("dispatcher:conversion:%s:%s:%d", t.Name(), runID, order.ID),
				EntryType:      "conversion",
				Actor:          "test:dispatcher",
				OccurredAt:     time.Now(),
				OrderID:        &order.ID,
				Lines: []journal.Line{
					{AccountCode: "position:corridor:USDT_BEP20", Amount: money.Amount{Asset: money.USDT_BEP20, Units: -order.AmountIn.Units}},
					{AccountCode: custBEP, Amount: money.Amount{Asset: money.USDT_BEP20, Units: order.AmountIn.Units}},
					{AccountCode: "position:corridor:USDT_TRC20", Amount: money.Amount{Asset: money.USDT_TRC20, Units: order.AmountIn.Units}},
					{AccountCode: custTRC, Amount: money.Amount{Asset: money.USDT_TRC20, Units: -order.AmountOut.Units}},
					{AccountCode: "revenue:fee", Amount: money.Amount{Asset: money.USDT_TRC20, Units: -order.FeeUnits.Units}},
					{AccountCode: "revenue:network_fee", Amount: money.Amount{Asset: money.USDT_TRC20, Units: -order.NetworkFeeUnits.Units}},
				},
			},
		})
		return err
	})
	require.NoError(t, err)
	if target == orders.Dispatching {
		return order
	}

	slotAcc := fmt.Sprintf("asset:tron:slot:%s:%s:%d", t.Name(), runID, order.ID)
	_, err = accounts.Create(ctx, pool, slotAcc, accounts.Asset, money.USDT_TRC20)
	require.NoError(t, err)

	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		order, err = orders.Transition(ctx, tx, order.ID, orders.Settled, order.Version, orders.TransitionParams{
			Actor: "test:dispatcher", Reason: "payout SR-final", OccurredAt: time.Now(),
			Entry: &journal.EntryRequest{
				IdempotencyKey: fmt.Sprintf("dispatcher:payout_settled:%s:%s:%d", t.Name(), runID, order.ID),
				EntryType:      "payout_settled",
				Actor:          "test:dispatcher",
				OccurredAt:     time.Now(),
				OrderID:        &order.ID,
				Lines: []journal.Line{
					{AccountCode: custTRC, Amount: money.Amount{Asset: money.USDT_TRC20, Units: order.AmountOut.Units}},
					{AccountCode: slotAcc, Amount: money.Amount{Asset: money.USDT_TRC20, Units: -order.AmountOut.Units}},
				},
			},
		})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, orders.Settled, order.State)
	return order
}

func testReorgScenarioBFrom(t *testing.T, target orders.State) {
	pool := testPool(t)
	ctx := context.Background()
	resetHaltState(t, ctx, pool)

	order, custBEP, depositKey := depositedOrder(t, ctx, pool)
	order = advanceDepositedOrderTo(t, ctx, pool, order, custBEP, target)
	require.Equal(t, target, order.State)

	lossBefore, err := journal.Balance(ctx, pool, "expense:loss:reorg")
	require.NoError(t, err)

	halted, err := halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.False(t, halted, "must start unhalted")

	var updated orders.Order
	err = withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = orders.HandleDepositReorg(ctx, tx, order.ID, depositKey, "test:watcher")
		return err
	})
	require.NoError(t, err)

	require.Equal(t, target, updated.State, "scenario B must leave the order's state untouched")
	require.Equal(t, order.Version, updated.Version, "scenario B calls no Transition, so version must be untouched")

	lossAfter, err := journal.Balance(ctx, pool, "expense:loss:reorg")
	require.NoError(t, err)
	require.Equal(t, order.AmountOut.Units, lossAfter.Units-lossBefore.Units,
		"expense:loss:reorg must increase by exactly amount_out")

	halted, err = halt.IsHalted(ctx, pool)
	require.NoError(t, err)
	require.True(t, halted, "the ledger must be halted after a post-settlement reorg")

	reason, err := halt.Reason(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, "POST_SETTLEMENT_REORG", reason)

	trial, err := journal.TrialBalance(ctx, pool)
	require.NoError(t, err)
	require.Equal(t, int64(0), trial[money.USDT_BEP20])
	require.Equal(t, int64(0), trial[money.USDT_TRC20])
}

func TestReorgScenarioBFromSettled(t *testing.T) {
	testReorgScenarioBFrom(t, orders.Settled)
}

func TestReorgScenarioBFromDispatching(t *testing.T) {
	testReorgScenarioBFrom(t, orders.Dispatching)
}

func TestReorgOnUnexpectedStateErrors(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := advanceToState(t, ctx, pool, orders.Expired)

	err := withTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orders.HandleDepositReorg(ctx, tx, order.ID, "irrelevant-key", "test")
		return err
	})
	require.ErrorIs(t, err, orders.ErrUnexpectedState)
	require.Contains(t, err.Error(), string(orders.Expired), "the error must name the actual state")
}
