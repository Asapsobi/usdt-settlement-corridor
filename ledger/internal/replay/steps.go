package replay

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/journal"
	"ledger/internal/money"
	"ledger/internal/orders"
)

// scaleFactors are pre-vetted (numerator, denominator) pairs for
// AmountVariance: each divides every one of baseAmounts' four values
// evenly, so k*(amountIn) == k*(amountOut)+k*(fee)+k*(networkFee) holds
// exactly under integer division with no rounding remainder to account
// for separately.
var scaleFactors = [][2]int64{
	{1, 1000}, // dust
	{1, 2},    // 50% underpay
	{9, 10},   // 90% (mild underpay)
	{11, 10},  // 110% (mild overpay)
	{3, 2},    // 150% overpay
}

// baseAmounts is §B's worked example: amountIn == amountOut + fee +
// networkFee, exactly. Every scenario that needs a balanced conversion
// entry starts here, optionally scaled by scaleFactors for AmountVariance.
func baseAmounts() (amountIn, amountOut, fee, networkFee money.Amount) {
	return money.Amount{Asset: money.USDT_BEP20, Units: 3000_000000},
		money.Amount{Asset: money.USDT_TRC20, Units: 2990_700000},
		money.Amount{Asset: money.USDT_TRC20, Units: 7_500000},
		money.Amount{Asset: money.USDT_TRC20, Units: 1_800000}
}

func scaledAmounts(rng *rand.Rand) (amountIn, amountOut, fee, networkFee money.Amount) {
	amountIn, amountOut, fee, networkFee = baseAmounts()
	f := scaleFactors[rng.Intn(len(scaleFactors))]
	scale := func(a money.Amount) money.Amount {
		return money.Amount{Asset: a.Asset, Units: a.Units * f[0] / f[1]}
	}
	return scale(amountIn), scale(amountOut), scale(fee), scale(networkFee)
}

func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(ctx context.Context, tx pgx.Tx) error) error {
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

const (
	haltRetryAttempts = 100
	haltRetryDelay    = 20 * time.Millisecond
)

// doTransition wraps orders.Transition in its own tx and retries on
// ErrSystemHalted. Only the four HaltBlocked pairs (see
// internal/orders/transitions.go) can ever actually return that error, so
// this is safe to use uniformly for every transition in this package
// rather than maintaining a halt-aware path and a plain path separately.
//
// The retry exists because this harness runs 32 workers against one
// shared system_state row: a scenario-B order elsewhere can halt the
// ledger for the brief window between its own halt.Set and its own
// immediate halt.Clear (see reorgAfterSettlement), and an unrelated
// order's halt-blocked transition landing in that window would otherwise
// fail for a reason that has nothing to do with what that order is
// actually testing. Retrying is safe because a halt-rejected attempt
// never touches the database at all -- the order's version is unchanged,
// so re-issuing the identical call is exactly correct, not a special case.
func doTransition(ctx context.Context, pool *pgxpool.Pool, orderID int64, toState orders.State, expectedVersion int32, params orders.TransitionParams) (orders.Order, error) {
	var lastErr error
	for i := 0; i < haltRetryAttempts; i++ {
		var updated orders.Order
		err := withTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = orders.Transition(ctx, tx, orderID, toState, expectedVersion, params)
			return err
		})
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, orders.ErrSystemHalted) {
			return orders.Order{}, err
		}
		lastErr = err
		time.Sleep(haltRetryDelay)
	}
	return orders.Order{}, fmt.Errorf("replay: order %d: gave up after %d retries waiting for halt to clear: %w",
		orderID, haltRetryAttempts, lastErr)
}

// entryIDByKey looks up an already-posted entry's id by the idempotency
// key the caller used to post it. Transition returns the updated Order,
// not the Entry it posted -- this is how a caller that needs the entry id
// afterward (to reverse it later, e.g. refundViaReversal, failDispatch)
// gets it, using the exact key it already knows.
func entryIDByKey(ctx context.Context, pool *pgxpool.Pool, idemKey string) (int64, error) {
	var id int64
	err := withTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		entry, err := journal.GetEntryByIdempotencyKey(ctx, tx, idemKey)
		if err != nil {
			return err
		}
		id = entry.ID
		return nil
	})
	return id, err
}

// fundOrder takes occurredAt explicitly, rather than calling time.Now()
// itself, so a caller that submits the same idemKey more than once (see
// duplicateIdempotency) can reuse one timestamp across every attempt --
// exactly as a real retrying caller would, since OccurredAt represents
// when the deposit was confirmed, not when this particular call happened
// to run. Two attempts under the same key with two different
// self-generated timestamps would hash to different payloads and hit a
// hard idempotency conflict instead of a clean replay, which is a bug in
// the caller, not a legitimate conflict.
func fundOrder(ctx context.Context, pool *pgxpool.Pool, order orders.Order, depositAcc, custBEP, idemKey string, amountIn money.Amount, occurredAt time.Time) (orders.Order, error) {
	orderID := order.ID
	return doTransition(ctx, pool, order.ID, orders.Funded, order.Version, orders.TransitionParams{
		Actor: "replay:watcher", Reason: "deposit reached 15 confirmations", OccurredAt: occurredAt,
		Entry: &journal.EntryRequest{
			IdempotencyKey: idemKey, EntryType: "deposit_final", Actor: "replay:watcher",
			OccurredAt: occurredAt, OrderID: &orderID,
			Lines: []journal.Line{
				{AccountCode: depositAcc, Amount: amountIn},
				{AccountCode: custBEP, Amount: money.Amount{Asset: amountIn.Asset, Units: -amountIn.Units}},
			},
		},
	})
}

func screenOrder(ctx context.Context, pool *pgxpool.Pool, order orders.Order) (orders.Order, error) {
	return doTransition(ctx, pool, order.ID, orders.Screened, order.Version, orders.TransitionParams{
		Actor: "replay:screening", Reason: "screening verdict pass", OccurredAt: time.Now(),
	})
}

func holdOrder(ctx context.Context, pool *pgxpool.Pool, order orders.Order) (orders.Order, error) {
	return doTransition(ctx, pool, order.ID, orders.Held, order.Version, orders.TransitionParams{
		Actor: "replay:screening", Reason: "screening verdict hold", OccurredAt: time.Now(),
	})
}

func releaseHold(ctx context.Context, pool *pgxpool.Pool, order orders.Order) (orders.Order, error) {
	return doTransition(ctx, pool, order.ID, orders.Screened, order.Version, orders.TransitionParams{
		Actor: "replay:ops", Reason: "manual release", OccurredAt: time.Now(),
	})
}

func dispatchOrder(ctx context.Context, pool *pgxpool.Pool, order orders.Order, custBEP, custTRC, idemKey string, amountIn, amountOut, fee, networkFee money.Amount, occurredAt time.Time) (orders.Order, error) {
	orderID := order.ID
	return doTransition(ctx, pool, order.ID, orders.Dispatching, order.Version, orders.TransitionParams{
		Actor: "replay:dispatcher", Reason: "posting the conversion entry", OccurredAt: occurredAt,
		Entry: &journal.EntryRequest{
			IdempotencyKey: idemKey, EntryType: "conversion", Actor: "replay:dispatcher",
			OccurredAt: occurredAt, OrderID: &orderID,
			Lines: []journal.Line{
				{AccountCode: custBEP, Amount: amountIn},
				{AccountCode: "position:corridor:USDT_BEP20", Amount: money.Amount{Asset: money.USDT_BEP20, Units: -amountIn.Units}},
				{AccountCode: "position:corridor:USDT_TRC20", Amount: money.Amount{Asset: money.USDT_TRC20, Units: amountIn.Units}},
				{AccountCode: custTRC, Amount: money.Amount{Asset: money.USDT_TRC20, Units: -amountOut.Units}},
				{AccountCode: "revenue:fee", Amount: money.Amount{Asset: money.USDT_TRC20, Units: -fee.Units}},
				{AccountCode: "revenue:network_fee", Amount: money.Amount{Asset: money.USDT_TRC20, Units: -networkFee.Units}},
			},
		},
	})
}

func settleOrder(ctx context.Context, pool *pgxpool.Pool, order orders.Order, custTRC, slot, idemKey string, amountOut money.Amount, occurredAt time.Time) (orders.Order, error) {
	return doTransition(ctx, pool, order.ID, orders.Settled, order.Version, orders.TransitionParams{
		Actor: "replay:dispatcher", Reason: "payout SR-final", OccurredAt: occurredAt,
		Entry: &journal.EntryRequest{
			IdempotencyKey: idemKey, EntryType: "payout_settled", Actor: "replay:dispatcher",
			OccurredAt: occurredAt, OrderID: &order.ID,
			Lines: []journal.Line{
				{AccountCode: custTRC, Amount: amountOut},
				{AccountCode: slot, Amount: money.Amount{Asset: amountOut.Asset, Units: -amountOut.Units}},
			},
		},
	})
}

// refundViaReversal reverses originalEntryID (the deposit) and transitions
// order to Refunded with that reversal as the transition's entry -- the
// same "reverse first, then transition with EntryID" pattern
// HandleDepositReorg's scenario A uses, since refunding a customer is,
// mechanically, reversing their deposit.
func refundViaReversal(ctx context.Context, pool *pgxpool.Pool, order orders.Order, originalEntryID int64) (orders.Order, error) {
	var reversalID int64
	err := withTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		reversal, err := journal.Reverse(ctx, tx, originalEntryID, "replay:ops", "manual reject", time.Now())
		if err != nil {
			return err
		}
		reversalID = reversal.ID
		return nil
	})
	if err != nil {
		return orders.Order{}, fmt.Errorf("reversing entry %d for refund: %w", originalEntryID, err)
	}

	return doTransition(ctx, pool, order.ID, orders.Refunded, order.Version, orders.TransitionParams{
		Actor: "replay:ops", Reason: "manual reject", OccurredAt: time.Now(),
		EntryID: &reversalID,
	})
}

// failDispatch reverses the conversion entry (dispatching -> held is the
// non-retryable-failure path, marked "(reversal)" in the transition
// table) and transitions to Held with that reversal as the entry.
func failDispatch(ctx context.Context, pool *pgxpool.Pool, order orders.Order, conversionEntryID int64) (orders.Order, error) {
	var reversalID int64
	err := withTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		reversal, err := journal.Reverse(ctx, tx, conversionEntryID, "replay:dispatcher", "non-retryable dispatch failure", time.Now())
		if err != nil {
			return err
		}
		reversalID = reversal.ID
		return nil
	})
	if err != nil {
		return orders.Order{}, fmt.Errorf("reversing conversion entry %d: %w", conversionEntryID, err)
	}

	return doTransition(ctx, pool, order.ID, orders.Held, order.Version, orders.TransitionParams{
		Actor: "replay:dispatcher", Reason: "non-retryable dispatch failure", OccurredAt: time.Now(),
		EntryID: &reversalID,
	})
}
