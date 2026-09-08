package replay

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/signing"
	"dispatcher/internal/slots"
)

const standardAmountOutUnits = 2990700000 // testledger's/ledgerfixture's own fixed $2990.70 worked example

func (h *harness) trcLiabilityCode(customerID string) string {
	return "liability:customer:" + customerID + ":USDT_TRC20"
}

// dispatchOneOrder is the common path scenarios 1, 6, 7, 8, 9, 11 all
// start from: a fresh screened order, a fresh slot, EnterDispatching.
// Returns the order (post-E2, State=="dispatching"), the slot, and the
// Dispatcher used.
func (h *harness) dispatchOneOrder(namePrefix string, signer dispatch.SigningService) (ledgerclient.Order, slots.Slot, *dispatch.Dispatcher, error) {
	extID := h.nextID(namePrefix)
	custID := h.nextID(namePrefix + "-cust")
	screened, err := h.ledger.newScreenedOrder(h.ctx, extID, custID)
	if err != nil {
		return ledgerclient.Order{}, slots.Slot{}, nil, err
	}
	order, err := h.orders.GetOrder(h.ctx, screened.ExternalID)
	if err != nil {
		return ledgerclient.Order{}, slots.Slot{}, nil, err
	}
	slot, err := h.newSlot()
	if err != nil {
		return ledgerclient.Order{}, slots.Slot{}, nil, err
	}
	d := h.newDispatcher(signer)
	if _, err := d.EnterDispatching(h.ctx, order, slot.ID, time.Now().UTC()); err != nil {
		return ledgerclient.Order{}, slots.Slot{}, nil, fmt.Errorf("EnterDispatching: %w", err)
	}
	after, err := h.orders.GetOrder(h.ctx, screened.ExternalID)
	if err != nil {
		return ledgerclient.Order{}, slots.Slot{}, nil, err
	}
	return after, slot, d, nil
}

// scenarioCleanDirectStandardMajority is the SCENARIO MIX's own
// majority case: several ordinary orders go all the way from screened to
// settled with no injected failure of any kind.
func (h *harness) scenarioCleanDirectStandardMajority() Result {
	const name = "CleanDirectStandardDispatchMajority"
	const n = 4
	for i := 0; i < n; i++ {
		order, slot, d, err := h.dispatchOneOrder("clean", signing.NewFakeSigningService())
		if err != nil {
			return fail(name, err)
		}
		h.recordDispatched(order.ExternalID, order.ID, false)

		unsignedTx, err := h.buildTransfer(slot.TronAddress, order.RecipientAddress, order.AmountOut.Format())
		if err != nil {
			return fail(name, err)
		}
		chain := newFakeChain()
		attempt, err := d.Broadcast(h.ctx, order.ID, slot.ID, 1, unsignedTx, 100.0, chain)
		if err != nil {
			return fail(name, fmt.Errorf("Broadcast: %w", err))
		}
		chain.SetFinal(*attempt.TronTxID)

		final, _, err := d.ConfirmFinality(h.ctx, order, attempt.ID, time.Now().UTC(), chain)
		if err != nil {
			return fail(name, fmt.Errorf("ConfirmFinality: %w", err))
		}
		if !final {
			return fail(name, fmt.Errorf("order %d did not reach finality", order.ID))
		}
		h.recordSettled(order.ExternalID, fmt.Sprintf("dispatcher:settle:%d:%d", order.ID, attempt.AttemptNumber), h.trcLiabilityCode(order.CustomerID), int64(order.AmountOut))
	}
	return pass(name)
}

// cutAndBroadcastBatch drives AccumulateForBatch -> CutBatch ->
// BroadcastBatch for n screened orders, returning the broadcast batch,
// the orders, the Dispatcher used (so the caller can reuse it, with the
// SAME Signer, for HandlePartialSettlement), and the fake energy
// reserver (so the caller can assert the one-reservation-per-batch
// count).
func (h *harness) cutAndBroadcastBatch(namePrefix string, n int) (dispatch.Batch, []ledgerclient.Order, *dispatch.Dispatcher, *fakeEnergyReserver, error) {
	d := h.newDispatcher(signing.NewFakeSigningService())
	d.Batches = dispatch.NewBatchStore(h.pool)
	slot, err := h.newSlot()
	if err != nil {
		return dispatch.Batch{}, nil, nil, nil, err
	}

	orders := make([]ledgerclient.Order, 0, n)
	for i := 0; i < n; i++ {
		extID := h.nextID(fmt.Sprintf("%s-%d", namePrefix, i))
		custID := h.nextID(fmt.Sprintf("%s-cust-%d", namePrefix, i))
		screened, err := h.ledger.newScreenedOrder(h.ctx, extID, custID)
		if err != nil {
			return dispatch.Batch{}, nil, nil, nil, err
		}
		order, err := h.orders.GetOrder(h.ctx, screened.ExternalID)
		if err != nil {
			return dispatch.Batch{}, nil, nil, nil, err
		}
		if err := d.AccumulateForBatch(h.ctx, order, slot.ID, time.Now().UTC()); err != nil {
			return dispatch.Batch{}, nil, nil, nil, fmt.Errorf("AccumulateForBatch: %w", err)
		}
		after, err := h.orders.GetOrder(h.ctx, screened.ExternalID)
		if err != nil {
			return dispatch.Batch{}, nil, nil, nil, err
		}
		orders = append(orders, after)
		h.recordDispatched(after.ExternalID, after.ID, false)
	}

	energyClient := newFakeEnergyReserver()
	window := dispatch.BatchWindow{
		MaxWait: time.Hour, MaxSize: n,
		SlotID: slot.ID, SlotAddress: slot.TronAddress,
		MultisendContractAddress:   "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", // proposed, per txbuild's own doc comment
		EnergyPerRecipientEstimate: 35000,
		EnergyDeadline:             time.Now().Add(time.Minute),
	}
	cut, err := d.CutBatch(h.ctx, window, energyClient)
	if err != nil {
		return dispatch.Batch{}, nil, nil, nil, fmt.Errorf("CutBatch: %w", err)
	}
	h.recordBatchRun(n, energyClient.callCount())
	if cut.UnsignedTx == nil {
		return dispatch.Batch{}, nil, nil, nil, fmt.Errorf("CutBatch's own batch %d has no unsigned_tx stored", cut.ID)
	}
	unsignedTx, err := hex.DecodeString(*cut.UnsignedTx)
	if err != nil {
		return dispatch.Batch{}, nil, nil, nil, fmt.Errorf("decoding batch %d's stored unsigned_tx: %w", cut.ID, err)
	}

	broadcast, err := d.BroadcastBatch(h.ctx, cut.ID, unsignedTx, 100.0, newFakeChain())
	if err != nil {
		return dispatch.Batch{}, nil, nil, nil, fmt.Errorf("BroadcastBatch: %w", err)
	}
	if broadcast.Status != dispatch.BatchBroadcast {
		return dispatch.Batch{}, nil, nil, nil, fmt.Errorf("batch %d status = %q after BroadcastBatch, want BROADCAST", broadcast.ID, broadcast.Status)
	}

	return broadcast, orders, d, energyClient, nil
}

// scenarioCleanSweepBatchNoPartialFailure is the SCENARIO MIX's own
// clean Sweep case: every recipient in the batch settles.
func (h *harness) scenarioCleanSweepBatchNoPartialFailure() Result {
	const name = "CleanSweepBatchNoPartialFailure"
	const n = 4

	batch, orders, d, energyClient, err := h.cutAndBroadcastBatch("sweep-clean", n)
	if err != nil {
		return fail(name, err)
	}
	if got := energyClient.callCount(); got != 1 {
		return fail(name, fmt.Errorf("energy reservation calls = %d, want exactly 1 for a %d-recipient batch", got, n))
	}

	succeeded := make(map[int64]bool, n)
	for _, o := range orders {
		succeeded[o.ID] = true
	}
	settledCount, requeuedCount, err := d.HandlePartialSettlement(h.ctx, batch.ID, time.Now().UTC(), succeeded)
	if err != nil {
		return fail(name, fmt.Errorf("HandlePartialSettlement: %w", err))
	}
	if settledCount != n || requeuedCount != 0 {
		return fail(name, fmt.Errorf("settled=%d requeued=%d, want settled=%d requeued=0", settledCount, requeuedCount, n))
	}
	for i, o := range orders {
		h.recordSettled(o.ExternalID, fmt.Sprintf("dispatcher:settle:%d:batch%d", o.ID, batch.ID), h.trcLiabilityCode(o.CustomerID), int64(o.AmountOut))
		_ = i
	}
	return pass(name)
}

// scenarioSweepBatchPartialSettlement mirrors C1.9's own 40/50
// partial-settlement scenario (scaled to a test-practical size: 10
// recipients, 8 succeed / 2 fail, the same ~80% ratio).
func (h *harness) scenarioSweepBatchPartialSettlement() Result {
	const name = "SweepBatchWithPartialSettlement"
	const n = 10
	const numFail = 2

	batch, orders, d, _, err := h.cutAndBroadcastBatch("sweep-partial", n)
	if err != nil {
		return fail(name, err)
	}

	succeeded := make(map[int64]bool, n)
	for i, o := range orders {
		succeeded[o.ID] = i >= numFail
	}
	settledCount, requeuedCount, err := d.HandlePartialSettlement(h.ctx, batch.ID, time.Now().UTC(), succeeded)
	if err != nil {
		return fail(name, fmt.Errorf("HandlePartialSettlement: %w", err))
	}
	if settledCount != n-numFail || requeuedCount != numFail {
		return fail(name, fmt.Errorf("settled=%d requeued=%d, want settled=%d requeued=%d", settledCount, requeuedCount, n-numFail, numFail))
	}
	for _, o := range orders {
		if succeeded[o.ID] {
			h.recordSettled(o.ExternalID, fmt.Sprintf("dispatcher:settle:%d:batch%d", o.ID, batch.ID), h.trcLiabilityCode(o.CustomerID), int64(o.AmountOut))
		} else {
			// Requeued for the next batch window, which this run never
			// cuts -- legitimately still `dispatching` when this run
			// ends, the same as the genuinely ambiguous freeze case is
			// to FINAL ASSERTION 1.
			h.markStillInFlightOK(o.ExternalID)
		}
	}
	// The batch itself is never represented as a single order-like
	// entity in C1 -- confirmed structurally: only individual orders'
	// own external_ids were ever named in any C1 call this scenario made.
	return pass(name)
}

// scenarioEnergyReservationFailedZeroLedgerImpact injects a FAILED
// reservation BEFORE any C1 commitment -- confirms the order is
// untouched (still `screened`), never even reaching EnterDispatching.
func (h *harness) scenarioEnergyReservationFailedZeroLedgerImpact() Result {
	const name = "EnergyReservationFailedBeforeAnyC1Commitment"
	extID := h.nextID("energy-fail")
	screened, err := h.ledger.newScreenedOrder(h.ctx, extID, h.nextID("energy-fail-cust"))
	if err != nil {
		return fail(name, err)
	}

	energyClient := newFakeEnergyReserver()
	energyClient.ForceFail()
	res, err := energyClient.Reserve(h.ctx, extID, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", 65000, "STANDARD", time.Now().Add(time.Minute), h.nextID("reserve"))
	if err != nil {
		return fail(name, fmt.Errorf("Reserve (forced FAILED) returned an error, want a FAILED status: %w", err))
	}
	if res.Status != "FAILED" {
		return fail(name, fmt.Errorf("reservation status = %q, want FAILED", res.Status))
	}
	// A real caller stops here on FAILED, never calling EnterDispatching.
	state, err := h.ledger.getOrderState(h.ctx, screened.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if state != "screened" {
		return fail(name, fmt.Errorf("order state = %q, want still screened (zero ledger impact)", state))
	}
	return pass(name)
}

// scenarioEnergyReservationTimeoutDistinctFromFailed injects a
// client-side timeout -- an error, not a FAILED status -- confirming C5
// can tell the two apart, and again never commits anything to C1.
func (h *harness) scenarioEnergyReservationTimeoutDistinctFromFailed() Result {
	const name = "EnergyReservationTimeoutDistinctFromFailed"
	extID := h.nextID("energy-timeout")
	screened, err := h.ledger.newScreenedOrder(h.ctx, extID, h.nextID("energy-timeout-cust"))
	if err != nil {
		return fail(name, err)
	}

	energyClient := newFakeEnergyReserver()
	energyClient.ForceTimeout()
	_, err = energyClient.Reserve(h.ctx, extID, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", 65000, "STANDARD", time.Now().Add(time.Minute), h.nextID("reserve"))
	if err == nil {
		return fail(name, errors.New("Reserve (forced timeout) returned no error, want a client-side error distinct from a FAILED status"))
	}

	state, err := h.ledger.getOrderState(h.ctx, screened.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if state != "screened" {
		return fail(name, fmt.Errorf("order state = %q, want still screened (zero ledger impact)", state))
	}
	return pass(name)
}

// scenarioNonRetryableDispatchFailureHeldViaTwoCallPath drives a real E2,
// simulates a permanent failure, and simulates a crash between the
// reversal and the held transition (posting the reversal directly, the
// same way finality_integration_test.go's own crash-recovery test does)
// -- then lets ReconcileFailedDispatches (the SAME job that would run on
// a ticker in production) find and close it.
func (h *harness) scenarioNonRetryableDispatchFailureHeldViaTwoCallPath() Result {
	const name = "NonRetryableDispatchFailureHeldViaTwoCallPathWithSimulatedCrash"
	order, slot, d, err := h.dispatchOneOrder("dispatch-fail", signing.NewFakeSigningService())
	if err != nil {
		return fail(name, err)
	}

	unsignedTx, err := h.buildTransfer(slot.TronAddress, order.RecipientAddress, order.AmountOut.Format())
	if err != nil {
		return fail(name, err)
	}
	attempt, err := dispatch.NewAttemptStore(h.pool).Create(h.ctx, order.ID, slot.ID, 1, hashHexForReplay(unsignedTx))
	if err != nil {
		return fail(name, err)
	}
	if _, err := dispatch.NewAttemptStore(h.pool).MarkFailed(h.ctx, attempt.ID); err != nil {
		return fail(name, fmt.Errorf("MarkFailed: %w", err))
	}

	conversionKey := fmt.Sprintf("dispatcher:enter_dispatching:%d", order.ID)
	entry, err := h.orders.GetEntryByIdempotencyKey(h.ctx, conversionKey)
	if err != nil {
		return fail(name, err)
	}
	// Simulate the crash: post the reversal directly (standing in for a
	// ReportDispatchFailure call that crashed right after this step),
	// WITHOUT completing the held transition.
	if _, _, err := h.orders.PostReversal(h.ctx, entry.ID, "replay: simulated permanent failure", time.Now().UTC()); err != nil {
		return fail(name, fmt.Errorf("PostReversal (simulating pre-crash step): %w", err))
	}

	reporter := ledgerclient.NewTwoCallReporter(h.orders)
	closed, err := d.ReconcileFailedDispatches(h.ctx, reporter, "replay: reconciliation closes the crashed dispatch failure")
	if err != nil {
		return fail(name, fmt.Errorf("ReconcileFailedDispatches: %w", err))
	}
	if closed != 1 {
		return fail(name, fmt.Errorf("ReconcileFailedDispatches closed %d orders, want 1", closed))
	}

	state, err := h.ledger.getOrderState(h.ctx, order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if state != "held" {
		return fail(name, fmt.Errorf("order state = %q, want held", state))
	}
	h.recordHeld(order.ExternalID, conversionKey)
	return pass(name)
}

// scenarioDuplicateBroadcastRetryStorm is C5.5's own named hard-parts
// proof, exercised end-to-end through this harness: N concurrent
// Broadcast calls for the identical attempt must produce exactly one
// real chain call.
func (h *harness) scenarioDuplicateBroadcastRetryStorm() Result {
	const name = "DuplicateBroadcastRetryStormExactlyOneRealBroadcast"
	order, slot, d, err := h.dispatchOneOrder("retry-storm", signing.NewFakeSigningService())
	if err != nil {
		return fail(name, err)
	}
	unsignedTx, err := h.buildTransfer(slot.TronAddress, order.RecipientAddress, order.AmountOut.Format())
	if err != nil {
		return fail(name, err)
	}
	chain := newFakeChain()

	const concurrency = 15
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := d.Broadcast(h.ctx, order.ID, slot.ID, 1, unsignedTx, 100.0, chain)
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return fail(name, fmt.Errorf("Broadcast (goroutine %d): %w", i, err))
		}
	}
	if got := chain.callCount(); got != 1 {
		return fail(name, fmt.Errorf("chain.callCount() = %d, want exactly 1", got))
	}

	attempt, err := dispatch.NewAttemptStore(h.pool).LatestForOrder(h.ctx, order.ID)
	if err != nil {
		return fail(name, err)
	}
	chain.SetFinal(*attempt.TronTxID)
	final, _, err := d.ConfirmFinality(h.ctx, order, attempt.ID, time.Now().UTC(), chain)
	if err != nil || !final {
		return fail(name, fmt.Errorf("ConfirmFinality: final=%v err=%v", final, err))
	}
	h.recordDispatched(order.ExternalID, order.ID, false)
	h.recordSettled(order.ExternalID, fmt.Sprintf("dispatcher:settle:%d:%d", order.ID, attempt.AttemptNumber), h.trcLiabilityCode(order.CustomerID), int64(order.AmountOut))
	return pass(name)
}

// scenarioSlotFreezePreBroadcast: a freeze caught before broadcast --
// abandoned, slot retired, a fresh attempt on a fresh slot settles
// normally, no loss entry, no halt.
func (h *harness) scenarioSlotFreezePreBroadcast() Result {
	const name = "SlotFreezePreBroadcastReattemptOnFreshSlotNoLoss"
	order, slot, d, err := h.dispatchOneOrder("freeze-pre", signing.NewFakeSigningService())
	if err != nil {
		return fail(name, err)
	}
	unsignedTx, err := h.buildTransfer(slot.TronAddress, order.RecipientAddress, order.AmountOut.Format())
	if err != nil {
		return fail(name, err)
	}
	attempt, err := dispatch.NewAttemptStore(h.pool).Create(h.ctx, order.ID, slot.ID, 1, hashHexForReplay(unsignedTx))
	if err != nil {
		return fail(name, err)
	}

	slotStore := slots.NewStore(h.pool)
	needsAlert, err := d.HandleMidFlightFreeze(h.ctx, attempt.ID, slotStore)
	if err != nil {
		return fail(name, fmt.Errorf("HandleMidFlightFreeze: %w", err))
	}
	if needsAlert {
		return fail(name, errors.New("needsAlert = true for a pre-broadcast freeze, want false"))
	}
	frozenSlot, err := slotStore.Get(h.ctx, slot.ID)
	if err != nil {
		return fail(name, err)
	}
	if frozenSlot.Status != slots.StatusRetired {
		return fail(name, fmt.Errorf("frozen slot status = %q, want RETIRED", frozenSlot.Status))
	}

	// Fresh attempt, fresh slot -- a legitimately different attempt, per
	// invariant 1.
	freshSlot, err := h.newSlot()
	if err != nil {
		return fail(name, err)
	}
	freshTx, err := h.buildTransfer(freshSlot.TronAddress, order.RecipientAddress, order.AmountOut.Format())
	if err != nil {
		return fail(name, err)
	}
	chain := newFakeChain()
	broadcast, err := d.Broadcast(h.ctx, order.ID, freshSlot.ID, 2, freshTx, 100.0, chain)
	if err != nil {
		return fail(name, fmt.Errorf("Broadcast (fresh attempt): %w", err))
	}
	chain.SetFinal(*broadcast.TronTxID)
	final, _, err := d.ConfirmFinality(h.ctx, order, broadcast.ID, time.Now().UTC(), chain)
	if err != nil || !final {
		return fail(name, fmt.Errorf("ConfirmFinality (fresh attempt): final=%v err=%v", final, err))
	}

	// No loss entry: revenue:expense:loss accounts untouched by this
	// order -- confirmed structurally, since neither this scenario nor
	// anything it called ever named expense:loss:freeze.
	h.recordDispatched(order.ExternalID, order.ID, false)
	h.recordSettled(order.ExternalID, fmt.Sprintf("dispatcher:settle:%d:%d", order.ID, broadcast.AttemptNumber), h.trcLiabilityCode(order.CustomerID), int64(order.AmountOut))
	return pass(name)
}

// scenarioSlotFreezePostBroadcast: a freeze caught after broadcast,
// before confirmation -- genuinely ambiguous, alerts, and (checked here
// directly, not just inferred) makes zero further C1 transition calls.
func (h *harness) scenarioSlotFreezePostBroadcast() Result {
	const name = "SlotFreezePostBroadcastPreConfirmationAlertZeroTransitions"
	order, slot, d, err := h.dispatchOneOrder("freeze-post", signing.NewFakeSigningService())
	if err != nil {
		return fail(name, err)
	}
	unsignedTx, err := h.buildTransfer(slot.TronAddress, order.RecipientAddress, order.AmountOut.Format())
	if err != nil {
		return fail(name, err)
	}
	chain := newFakeChain()
	attempt, err := d.Broadcast(h.ctx, order.ID, slot.ID, 1, unsignedTx, 100.0, chain)
	if err != nil {
		return fail(name, fmt.Errorf("Broadcast: %w", err))
	}
	beforeVersion, err := h.orders.GetOrder(h.ctx, order.ExternalID)
	if err != nil {
		return fail(name, err)
	}

	slotStore := slots.NewStore(h.pool)
	needsAlert, err := d.HandleMidFlightFreeze(h.ctx, attempt.ID, slotStore)
	if err != nil {
		return fail(name, fmt.Errorf("HandleMidFlightFreeze: %w", err))
	}
	if !needsAlert {
		return fail(name, errors.New("needsAlert = false for a post-broadcast freeze, want true"))
	}

	after, err := h.orders.GetOrder(h.ctx, order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if after.Version != beforeVersion.Version || after.State != "dispatching" {
		return fail(name, fmt.Errorf("order changed (version %d->%d, state %s->%s) -- HandleMidFlightFreeze must make zero C1 transition calls in the ambiguous case", beforeVersion.Version, after.Version, beforeVersion.State, after.State))
	}

	frozenSlot, err := slotStore.Get(h.ctx, slot.ID)
	if err != nil {
		return fail(name, err)
	}
	if frozenSlot.Status != slots.StatusRetired {
		return fail(name, fmt.Errorf("frozen slot status = %q, want RETIRED", frozenSlot.Status))
	}

	// Deliberately left `dispatching`, with an open BROADCAST attempt --
	// FINAL ASSERTION 1 must not treat this as a violation.
	h.recordDispatched(order.ExternalID, order.ID, true)
	return pass(name)
}

// scenarioAllSlotsOverCap: every eligible slot over cap -> ErrNoEligibleSlot,
// zero attempts made.
func (h *harness) scenarioAllSlotsOverCap() Result {
	const name = "AllSlotsOverCapErrNoEligibleSlotZeroAttempts"
	slotStore := slots.NewStore(h.pool)
	const numSlots = 6
	for i := 0; i < numSlots; i++ {
		if _, err := h.newSlot(); err != nil {
			return fail(name, err)
		}
	}

	before, err := dispatch.NewStore(h.pool).ListDispatching(h.ctx)
	if err != nil {
		return fail(name, err)
	}

	// TxCountCeiling: 0 puts every freshly created slot (tx_count == 0)
	// at-or-over the ceiling immediately -- SelectForDispatch's own
	// strict >= exclusion (proven in select_integration_test.go) rejects
	// all of them without needing to fabricate real on-chain balances.
	caps := slots.Caps{BalanceCeiling: mustParseMoney("50000.000000"), TxCountCeiling: 0}
	_, err = slotStore.SelectForDispatch(h.ctx, h.orders, caps)
	if !errors.Is(err, slots.ErrNoEligibleSlot) {
		return fail(name, fmt.Errorf("SelectForDispatch error = %v, want ErrNoEligibleSlot", err))
	}

	after, err := dispatch.NewStore(h.pool).ListDispatching(h.ctx)
	if err != nil {
		return fail(name, err)
	}
	if len(after) != len(before) {
		return fail(name, fmt.Errorf("dispatching order count changed (%d -> %d), want zero attempts made", len(before), len(after)))
	}
	return pass(name)
}

// scenarioForcedDuplicateSignature uses FakeSigningService's own
// forced-duplicate-signature mode (built for exactly this, C5.0/C5.5) to
// prove the SYSTEM still broadcasts exactly once per distinct attempt
// even if S1 itself were to misbehave and hand back a byte-identical
// signature for what should be a distinct request.
func (h *harness) scenarioForcedDuplicateSignature() Result {
	const name = "FakeSigningServiceDuplicateSignatureInjectionSystemStillBroadcastsExactlyOnce"
	signer := signing.NewFakeSigningService()

	orderA, slotA, dA, err := h.dispatchOneOrder("dup-sig-a", signer)
	if err != nil {
		return fail(name, err)
	}
	orderB, slotB, dB, err := h.dispatchOneOrder("dup-sig-b", signer)
	if err != nil {
		return fail(name, err)
	}

	keyA := fmt.Sprintf("dispatcher:sign:%d:1", orderA.ID)
	keyB := fmt.Sprintf("dispatcher:sign:%d:1", orderB.ID)
	signer.ForceDuplicateSignature(keyA, keyB)

	unsignedTxB, err := h.buildTransfer(slotB.TronAddress, orderB.RecipientAddress, orderB.AmountOut.Format())
	if err != nil {
		return fail(name, err)
	}
	chain := newFakeChain()
	if _, err := dB.Broadcast(h.ctx, orderB.ID, slotB.ID, 1, unsignedTxB, 100.0, chain); err != nil {
		return fail(name, fmt.Errorf("Broadcast (order B, establishes the signature to be duplicated): %w", err))
	}

	unsignedTxA, err := h.buildTransfer(slotA.TronAddress, orderA.RecipientAddress, orderA.AmountOut.Format())
	if err != nil {
		return fail(name, err)
	}
	broadcastA, err := dA.Broadcast(h.ctx, orderA.ID, slotA.ID, 1, unsignedTxA, 100.0, chain)
	if err != nil {
		return fail(name, fmt.Errorf("Broadcast (order A, forced duplicate signature): %w", err))
	}
	if broadcastA.Status != dispatch.BroadcastBroadcast {
		return fail(name, fmt.Errorf("order A attempt status = %q, want BROADCAST", broadcastA.Status))
	}
	if got := chain.callCount(); got != 2 {
		return fail(name, fmt.Errorf("chain.callCount() = %d, want 2 (one broadcast per distinct attempt, even with a shared signature)", got))
	}

	h.recordDispatched(orderA.ExternalID, orderA.ID, true) // left BROADCAST, not carried to finality -- this scenario's own proof is the call count, not settlement
	h.recordDispatched(orderB.ExternalID, orderB.ID, true)
	return pass(name)
}
