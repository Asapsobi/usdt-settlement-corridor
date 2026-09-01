package replay

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/money"
	"ledger/internal/orders"
)

// execResult is what one order's scenario function reports back.
// Provoked counts errors the scenario deliberately caused and correctly
// handled (a version conflict from a duplicate submission, an illegal
// transition from a deposit landing on an already-expired order) -- the
// FINAL ASSERTIONS section's "any error the harness did not deliberately
// provoke fails the run" means these must be counted and reported, not
// silently absorbed. Err is set only when something genuinely
// unexpected happened.
type execResult struct {
	plan       orderPlan
	orderID    int64
	externalID string
	finalState string
	provoked   int
	err        error
}

func newOrder(ctx context.Context, pool *pgxpool.Pool, runTag string, idx int, tier orders.Tier, amountIn, amountOut, fee, networkFee money.Amount) (orders.Order, error) {
	return orders.Create(ctx, pool, orders.CreateParams{
		ExternalID:       fmt.Sprintf("replay-%s-%d", runTag, idx),
		CustomerID:       fmt.Sprintf("replay-cust-%d", idx),
		Tier:             tier,
		AmountIn:         amountIn,
		AmountOut:        amountOut,
		FeeUnits:         fee,
		NetworkFeeUnits:  networkFee,
		RecipientAddress: fmt.Sprintf("T-recipient-%s-%d", runTag, idx),
		QuotedAt:         time.Now(),
		QuoteExpiresAt:   time.Now().Add(90 * time.Second),
	})
}

func createDepositAccount(ctx context.Context, pool *pgxpool.Pool, orderID int64) (string, error) {
	acc := fmt.Sprintf("asset:bsc:deposit:%d", orderID)
	if _, err := accounts.Create(ctx, pool, acc, accounts.Asset, money.USDT_BEP20); err != nil {
		return "", err
	}
	return acc, nil
}

// runScenario dispatches to the right function for plan.scenario. Every
// scenario function returns a fully-populated execResult (never a bare
// error) so a mid-lifecycle failure still reports whatever state/id the
// order reached before the failure -- useful for debugging a failed gate
// run, and required for the report's per-scenario breakdown to add up.
func runScenario(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, rng *rand.Rand) execResult {
	switch plan.scenario {
	case scenarioHappyPath:
		return happyPath(ctx, pool, shared, runTag, plan, orders.Standard, rng)
	case scenarioDuplicateIdempotency:
		return duplicateIdempotency(ctx, pool, shared, runTag, plan, rng)
	case scenarioReorgBeforeDispatch:
		return reorgBeforeDispatch(ctx, pool, shared, runTag, plan, rng)
	case scenarioReorgAfterSettlement:
		return reorgAfterSettlement(ctx, pool, shared, runTag, plan, rng)
	case scenarioScreeningHoldRelease:
		return screeningHoldRelease(ctx, pool, shared, runTag, plan, rng)
	case scenarioScreeningHoldReject:
		return screeningHoldReject(ctx, pool, shared, runTag, plan, rng)
	case scenarioQuoteExpiry:
		return quoteExpiry(ctx, pool, shared, runTag, plan, rng)
	case scenarioAmountVariance:
		return amountVariance(ctx, pool, shared, runTag, plan, rng)
	case scenarioSweepBatch:
		if plan.sweepFails {
			return nonRetryableFailure(ctx, pool, shared, runTag, plan, orders.Sweep, rng)
		}
		return happyPath(ctx, pool, shared, runTag, plan, orders.Sweep, rng)
	case scenarioNonRetryableFailure:
		return nonRetryableFailure(ctx, pool, shared, runTag, plan, orders.Standard, rng)
	default:
		return execResult{plan: plan, err: fmt.Errorf("replay: unknown scenario %v", plan.scenario)}
	}
}

func happyPath(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, tier orders.Tier, rng *rand.Rand) execResult {
	amountIn, amountOut, fee, networkFee := baseAmounts()
	return runToSettled(ctx, pool, shared, runTag, plan, tier, amountIn, amountOut, fee, networkFee, rng)
}

func amountVariance(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, rng *rand.Rand) execResult {
	amountIn, amountOut, fee, networkFee := scaledAmounts(rng)
	return runToSettled(ctx, pool, shared, runTag, plan, orders.Standard, amountIn, amountOut, fee, networkFee, rng)
}

// runToSettled is the shared quoted->funded->screened->dispatching->settled
// path every "everything goes fine" scenario (happy path, sweep-settle,
// amount variance) funnels through, parameterized on tier and amounts.
func runToSettled(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, tier orders.Tier, amountIn, amountOut, fee, networkFee money.Amount, rng *rand.Rand) execResult {
	res := execResult{plan: plan}

	custBEP, custTRC := shared.pickCustomer(rng.Intn)
	order, err := newOrder(ctx, pool, runTag, plan.index, tier, amountIn, amountOut, fee, networkFee)
	if err != nil {
		res.err = fmt.Errorf("create: %w", err)
		return res
	}
	res.orderID, res.externalID = order.ID, order.ExternalID

	depositAcc, err := createDepositAccount(ctx, pool, order.ID)
	if err != nil {
		res.err = fmt.Errorf("deposit account: %w", err)
		return res
	}

	order, err = fundOrder(ctx, pool, order, depositAcc, custBEP, idemKey(runTag, plan.index, "e1"), amountIn, time.Now())
	if err != nil {
		res.err = fmt.Errorf("fund: %w", err)
		return res
	}

	order, err = screenOrder(ctx, pool, order)
	if err != nil {
		res.err = fmt.Errorf("screen: %w", err)
		return res
	}

	order, err = dispatchOrder(ctx, pool, order, custBEP, custTRC, idemKey(runTag, plan.index, "e2"), amountIn, amountOut, fee, networkFee, time.Now())
	if err != nil {
		res.err = fmt.Errorf("dispatch: %w", err)
		return res
	}

	slot := shared.pickSlot(rng.Intn)
	order, err = settleOrder(ctx, pool, order, custTRC, slot, idemKey(runTag, plan.index, "e3"), amountOut, time.Now())
	if err != nil {
		res.err = fmt.Errorf("settle: %w", err)
		return res
	}

	res.finalState = string(order.State)
	return res
}

func idemKey(runTag string, idx int, step string) string {
	return fmt.Sprintf("replay:%s:%d:%s", runTag, idx, step)
}

// duplicateIdempotency exercises C1.3's guarantee under the harness's own
// concurrent load: the funding transition is submitted twice with the
// identical idempotency key -- concurrently for half these orders,
// sequentially for the other half. Exactly one succeeds; the other loses
// the order's version CAS (the entry itself replayed cleanly regardless
// of which call "won" the order-level race -- see doTransition's doc
// comment for why that's always safe to retry-shaped code, and note this
// isn't a retry, it's two independent callers, which is the point).
// ErrVersionConflict here is provoked on purpose, not a failure.
func duplicateIdempotency(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, rng *rand.Rand) execResult {
	res := execResult{plan: plan}
	amountIn, amountOut, fee, networkFee := baseAmounts()

	custBEP, custTRC := shared.pickCustomer(rng.Intn)
	order, err := newOrder(ctx, pool, runTag, plan.index, orders.Standard, amountIn, amountOut, fee, networkFee)
	if err != nil {
		res.err = fmt.Errorf("create: %w", err)
		return res
	}
	res.orderID, res.externalID = order.ID, order.ExternalID

	depositAcc, err := createDepositAccount(ctx, pool, order.ID)
	if err != nil {
		res.err = fmt.Errorf("deposit account: %w", err)
		return res
	}

	key := idemKey(runTag, plan.index, "e1")
	orderID, version := order.ID, order.Version
	// Captured once and reused by every attempt below: two calls under the
	// identical idempotency key must carry the identical payload for this
	// to actually exercise replay semantics, not a genuine hard conflict.
	occurredAt := time.Now()

	attempt := func() (orders.Order, error) {
		return fundOrder(ctx, pool, orders.Order{ID: orderID, Version: version}, depositAcc, custBEP, key, amountIn, occurredAt)
	}

	var winner orders.Order
	var errs []error

	if plan.index%2 == 0 {
		// Concurrent: two goroutines racing the identical call.
		var mu sync.Mutex
		var wg sync.WaitGroup
		wg.Add(2)
		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				o, err := attempt()
				mu.Lock()
				defer mu.Unlock()
				if err == nil {
					winner = o
				} else {
					errs = append(errs, err)
				}
			}()
		}
		wg.Wait()
	} else {
		// Sequential: the same call again, as if a caller retried after
		// not seeing the first response.
		o1, err1 := attempt()
		o2, err2 := attempt()
		if err1 == nil {
			winner = o1
		} else {
			errs = append(errs, err1)
		}
		if err2 == nil {
			winner = o2
		} else {
			errs = append(errs, err2)
		}
	}

	if winner.ID == 0 {
		res.err = fmt.Errorf("duplicate funding: both attempts failed: %v", errs)
		return res
	}
	for _, e := range errs {
		if !errors.Is(e, orders.ErrVersionConflict) {
			res.err = fmt.Errorf("duplicate funding: unexpected error from a losing attempt: %w", e)
			return res
		}
		res.provoked++
	}

	order = winner
	order, err = screenOrder(ctx, pool, order)
	if err != nil {
		res.err = fmt.Errorf("screen: %w", err)
		return res
	}
	order, err = dispatchOrder(ctx, pool, order, custBEP, custTRC, idemKey(runTag, plan.index, "e2"), amountIn, amountOut, fee, networkFee, time.Now())
	if err != nil {
		res.err = fmt.Errorf("dispatch: %w", err)
		return res
	}
	slot := shared.pickSlot(rng.Intn)
	order, err = settleOrder(ctx, pool, order, custTRC, slot, idemKey(runTag, plan.index, "e3"), amountOut, time.Now())
	if err != nil {
		res.err = fmt.Errorf("settle: %w", err)
		return res
	}

	res.finalState = string(order.State)
	return res
}

// reorgBeforeDispatch is C1.6 scenario A: fund, then reorg before the
// order ever reaches dispatching. HandleDepositReorg returns it to
// quoted; per C1.6's own description of the expected follow-up ("if the
// quote has since expired, the very next call moves it quoted ->
// expired"), the harness makes that next call itself, landing on a
// terminal state rather than leaving the order sitting in quoted.
func reorgBeforeDispatch(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, rng *rand.Rand) execResult {
	res := execResult{plan: plan}
	amountIn, amountOut, fee, networkFee := baseAmounts()

	custBEP, _ := shared.pickCustomer(rng.Intn)
	order, err := newOrder(ctx, pool, runTag, plan.index, orders.Standard, amountIn, amountOut, fee, networkFee)
	if err != nil {
		res.err = fmt.Errorf("create: %w", err)
		return res
	}
	res.orderID, res.externalID = order.ID, order.ExternalID

	depositAcc, err := createDepositAccount(ctx, pool, order.ID)
	if err != nil {
		res.err = fmt.Errorf("deposit account: %w", err)
		return res
	}

	depositKey := idemKey(runTag, plan.index, "e1")
	order, err = fundOrder(ctx, pool, order, depositAcc, custBEP, depositKey, amountIn, time.Now())
	if err != nil {
		res.err = fmt.Errorf("fund: %w", err)
		return res
	}

	var afterReorg orders.Order
	err = withTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		afterReorg, err = orders.HandleDepositReorg(ctx, tx, order.ID, depositKey, "replay:watcher")
		return err
	})
	if err != nil {
		res.err = fmt.Errorf("reorg scenario A: %w", err)
		return res
	}

	final, err := doTransition(ctx, pool, order.ID, orders.Expired, afterReorg.Version, orders.TransitionParams{
		Actor: "replay:ops", Reason: "quote expired after reorg", OccurredAt: time.Now(),
	})
	if err != nil {
		res.err = fmt.Errorf("expiring after reorg: %w", err)
		return res
	}

	res.finalState = string(final.State)
	return res
}

// reorgAfterSettlement is C1.6 scenario B, the loss case: must halt every
// time. The same worker that causes the halt clears it immediately
// afterward (as a simulated operator), keeping the window where a
// concurrent, unrelated order could hit ErrSystemHalted as short as
// possible; doTransition's retry is the safety net for whatever window
// remains.
func reorgAfterSettlement(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, rng *rand.Rand) execResult {
	happy := happyPath(ctx, pool, shared, runTag, plan, orders.Standard, rng)
	if happy.err != nil || happy.finalState != string(orders.Settled) {
		happy.err = fmt.Errorf("reorg scenario B: order did not reach settled first: %w", happy.err)
		return happy
	}

	depositKey := idemKey(runTag, plan.index, "e1")
	var afterReorg orders.Order
	err := withTx(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		afterReorg, err = orders.HandleDepositReorg(ctx, tx, happy.orderID, depositKey, "replay:watcher")
		return err
	})
	if err != nil {
		happy.err = fmt.Errorf("reorg scenario B: %w", err)
		return happy
	}

	if err := halt.Clear(ctx, pool, halt.ClearParams{
		Actor: "replay:ops", Note: "simulated operator clearing scenario-B halt",
	}); err != nil {
		happy.err = fmt.Errorf("clearing scenario-B halt: %w", err)
		return happy
	}

	happy.finalState = string(afterReorg.State) // still Settled
	return happy
}

func screeningHoldRelease(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, rng *rand.Rand) execResult {
	res := execResult{plan: plan}
	amountIn, amountOut, fee, networkFee := baseAmounts()

	custBEP, custTRC := shared.pickCustomer(rng.Intn)
	order, err := newOrder(ctx, pool, runTag, plan.index, orders.Standard, amountIn, amountOut, fee, networkFee)
	if err != nil {
		res.err = fmt.Errorf("create: %w", err)
		return res
	}
	res.orderID, res.externalID = order.ID, order.ExternalID

	depositAcc, err := createDepositAccount(ctx, pool, order.ID)
	if err != nil {
		res.err = fmt.Errorf("deposit account: %w", err)
		return res
	}
	order, err = fundOrder(ctx, pool, order, depositAcc, custBEP, idemKey(runTag, plan.index, "e1"), amountIn, time.Now())
	if err != nil {
		res.err = fmt.Errorf("fund: %w", err)
		return res
	}
	order, err = holdOrder(ctx, pool, order)
	if err != nil {
		res.err = fmt.Errorf("hold: %w", err)
		return res
	}
	order, err = releaseHold(ctx, pool, order)
	if err != nil {
		res.err = fmt.Errorf("release: %w", err)
		return res
	}
	order, err = dispatchOrder(ctx, pool, order, custBEP, custTRC, idemKey(runTag, plan.index, "e2"), amountIn, amountOut, fee, networkFee, time.Now())
	if err != nil {
		res.err = fmt.Errorf("dispatch: %w", err)
		return res
	}
	slot := shared.pickSlot(rng.Intn)
	order, err = settleOrder(ctx, pool, order, custTRC, slot, idemKey(runTag, plan.index, "e3"), amountOut, time.Now())
	if err != nil {
		res.err = fmt.Errorf("settle: %w", err)
		return res
	}

	res.finalState = string(order.State)
	return res
}

func screeningHoldReject(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, rng *rand.Rand) execResult {
	res := execResult{plan: plan}
	amountIn, amountOut, fee, networkFee := baseAmounts()

	custBEP, _ := shared.pickCustomer(rng.Intn)
	order, err := newOrder(ctx, pool, runTag, plan.index, orders.Standard, amountIn, amountOut, fee, networkFee)
	if err != nil {
		res.err = fmt.Errorf("create: %w", err)
		return res
	}
	res.orderID, res.externalID = order.ID, order.ExternalID

	depositAcc, err := createDepositAccount(ctx, pool, order.ID)
	if err != nil {
		res.err = fmt.Errorf("deposit account: %w", err)
		return res
	}
	depositKey := idemKey(runTag, plan.index, "e1")
	order, err = fundOrder(ctx, pool, order, depositAcc, custBEP, depositKey, amountIn, time.Now())
	if err != nil {
		res.err = fmt.Errorf("fund: %w", err)
		return res
	}
	order, err = holdOrder(ctx, pool, order)
	if err != nil {
		res.err = fmt.Errorf("hold: %w", err)
		return res
	}

	entryID, err := entryIDByKey(ctx, pool, depositKey)
	if err != nil {
		res.err = fmt.Errorf("looking up deposit entry: %w", err)
		return res
	}
	order, err = refundViaReversal(ctx, pool, order, entryID)
	if err != nil {
		res.err = fmt.Errorf("refund: %w", err)
		return res
	}

	res.finalState = string(order.State)
	return res
}

// quoteExpiry is quote_expires_at passing with nothing received. A
// portion of these orders also simulate a deposit landing after the
// order has already expired -- since expired is terminal (zero legal
// outgoing transitions), that attempt is expected, and required, to fail
// with ErrIllegalTransition. That failure is provoked deliberately, not
// a bug in the harness or the ledger.
func quoteExpiry(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, rng *rand.Rand) execResult {
	res := execResult{plan: plan}
	amountIn, amountOut, fee, networkFee := baseAmounts()

	custBEP, _ := shared.pickCustomer(rng.Intn)
	order, err := newOrder(ctx, pool, runTag, plan.index, orders.Standard, amountIn, amountOut, fee, networkFee)
	if err != nil {
		res.err = fmt.Errorf("create: %w", err)
		return res
	}
	res.orderID, res.externalID = order.ID, order.ExternalID

	order, err = doTransition(ctx, pool, order.ID, orders.Expired, order.Version, orders.TransitionParams{
		Actor: "replay:ops", Reason: "quote_expires_at passed, nothing received", OccurredAt: time.Now(),
	})
	if err != nil {
		res.err = fmt.Errorf("expire: %w", err)
		return res
	}

	if rng.Intn(4) == 0 { // ~25% of this scenario: a late deposit tries to land
		depositAcc, err := createDepositAccount(ctx, pool, order.ID)
		if err != nil {
			res.err = fmt.Errorf("deposit account: %w", err)
			return res
		}
		_, err = fundOrder(ctx, pool, order, depositAcc, custBEP, idemKey(runTag, plan.index, "e1_late"), amountIn, time.Now())
		if !errors.Is(err, orders.ErrIllegalTransition) {
			res.err = fmt.Errorf("late deposit on expired order: expected ErrIllegalTransition, got %v", err)
			return res
		}
		res.provoked++
	}

	res.finalState = string(order.State)
	return res
}

func nonRetryableFailure(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, tier orders.Tier, rng *rand.Rand) execResult {
	res := execResult{plan: plan}
	amountIn, amountOut, fee, networkFee := baseAmounts()

	custBEP, custTRC := shared.pickCustomer(rng.Intn)
	order, err := newOrder(ctx, pool, runTag, plan.index, tier, amountIn, amountOut, fee, networkFee)
	if err != nil {
		res.err = fmt.Errorf("create: %w", err)
		return res
	}
	res.orderID, res.externalID = order.ID, order.ExternalID

	depositAcc, err := createDepositAccount(ctx, pool, order.ID)
	if err != nil {
		res.err = fmt.Errorf("deposit account: %w", err)
		return res
	}
	order, err = fundOrder(ctx, pool, order, depositAcc, custBEP, idemKey(runTag, plan.index, "e1"), amountIn, time.Now())
	if err != nil {
		res.err = fmt.Errorf("fund: %w", err)
		return res
	}
	order, err = screenOrder(ctx, pool, order)
	if err != nil {
		res.err = fmt.Errorf("screen: %w", err)
		return res
	}

	convKey := idemKey(runTag, plan.index, "e2")
	order, err = dispatchOrder(ctx, pool, order, custBEP, custTRC, convKey, amountIn, amountOut, fee, networkFee, time.Now())
	if err != nil {
		res.err = fmt.Errorf("dispatch: %w", err)
		return res
	}

	entryID, err := entryIDByKey(ctx, pool, convKey)
	if err != nil {
		res.err = fmt.Errorf("looking up conversion entry: %w", err)
		return res
	}
	order, err = failDispatch(ctx, pool, order, entryID)
	if err != nil {
		res.err = fmt.Errorf("fail dispatch: %w", err)
		return res
	}

	res.finalState = string(order.State)
	return res
}
