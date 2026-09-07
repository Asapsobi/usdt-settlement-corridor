package replay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"energybroker/internal/buffer"
	"energybroker/internal/money"
	"energybroker/internal/provider"
	"energybroker/internal/reservations"
	"energybroker/internal/routing"
)

func pass(name string) Result { return Result{Name: name, Passed: true} }
func fail(name string, err error) Result {
	return Result{Name: name, Passed: false, Detail: err.Error()}
}

func defaultWeights() routing.RoutingWeights {
	return routing.RoutingWeights{
		provider.Tronsell: 0.60,
		provider.Netts:    0.35,
		provider.Catfee:   0.05,
	}
}

// newRequest builds one POST /v1/reservations-equivalent Request against
// a freshly created real C1 order -- every scenario below needs exactly
// this, differing only in targetAddress/energyUnits/deadline/tier.
// targetAddress is always the caller's own choice, never invented here:
// a fast-path scenario must pass its rig's own slotAddress (the only
// address that rig's buffer ever pre-provisions against), while a
// slow-path-only scenario (never touches the buffer at all) can pass any
// address, unique or not.
func (h *harness) newRequest(prefix, targetAddress string, energyUnits int64, deadline time.Duration, tier string) (order fixtureOrder, req reservations.Request, err error) {
	order, err = h.createOrder(prefix)
	if err != nil {
		return fixtureOrder{}, reservations.Request{}, err
	}
	req = reservations.Request{
		IdempotencyKey: h.nextID(prefix + "-idem"),
		ExternalID:     order.ExternalID,
		TargetAddress:  targetAddress,
		EnergyUnits:    energyUnits,
		Tier:           tier,
		Deadline:       time.Now().Add(deadline),
	}
	return order, req, nil
}

// scenarioCleanFastPathMajority is the SCENARIO MIX's majority case: a
// warm buffer serves several reservations entirely via the fast path,
// staying near its own configured floor.
func (h *harness) scenarioCleanFastPathMajority() Result {
	const name = "CleanFastPathReservationsMajority"
	if err := h.drainBuffer(); err != nil {
		return fail(name, err)
	}
	const floor = 5000
	const perOrder = 200
	const orders = 5

	// A single provider and a partial-fill cap: Replenish's own inner
	// loop then needs several Delegate calls, each capped at perOrder, to
	// reach floor -- producing `orders` separate energy_buffer rows.
	// Without the cap, one Replenish call would satisfy the whole
	// shortfall with a SINGLE Delegate (MockProvider's own default
	// MaxUnitsAvailable is enormous), producing one monolithic row that
	// the first order's own Reserve call would then claim WHOLE (Reserve
	// never splits a row) -- starving every other order in this scenario
	// of fast-path capacity despite the buffer nominally holding enough.
	rig, err := h.newRig(routing.RoutingWeights{provider.Tronsell: 1.0}, 100, floor)
	if err != nil {
		return fail(name, err)
	}
	rig.tronsell.ForcePartialFill(perOrder)
	for i := 0; i < floor/perOrder; i++ {
		if err := rig.buffer.Replenish(h.ctx); err != nil {
			return fail(name, err)
		}
	}
	available, err := rig.buffer.AvailableTotal(h.ctx)
	if err != nil {
		return fail(name, err)
	}
	if available != floor {
		return fail(name, fmt.Errorf("buffer available after replenishing to floor = %d, want exactly %d", available, floor))
	}

	for i := 0; i < orders; i++ {
		_, req, err := h.newRequest("cleanfast", rig.slotAddress, perOrder, 5*time.Second, "STANDARD")
		if err != nil {
			return fail(name, err)
		}
		res, err := rig.svc.Create(h.ctx, req)
		if err != nil {
			return fail(name, err)
		}
		if res.Status != reservations.StatusConfirmed {
			return fail(name, fmt.Errorf("order %d: status = %s, want CONFIRMED", i, res.Status))
		}
		if res.FastPath == nil {
			return fail(name, fmt.Errorf("order %d: fast_path is nil on a CONFIRMED reservation", i))
		}
		if !*res.FastPath {
			return fail(name, fmt.Errorf("order %d: fast_path = false, want true", i))
		}
	}

	after, err := rig.buffer.AvailableTotal(h.ctx)
	if err != nil {
		return fail(name, err)
	}
	want := int64(floor - orders*perOrder)
	if after != want {
		return fail(name, fmt.Errorf("buffer available after %d fast-path orders = %d, want %d (buffer stayed near target, consumed exactly what was reserved)", orders, after, want))
	}
	return pass(name)
}

// scenarioBufferExhaustionDemandBurstSlowPath: a small warm buffer
// covers only the first of several sequential reservations; the rest
// exhaust it and fall through to a real, successful slow-path Delegate.
func (h *harness) scenarioBufferExhaustionDemandBurstSlowPath() Result {
	const name = "BufferExhaustionDemandBurstSlowPathSuccess"
	if err := h.drainBuffer(); err != nil {
		return fail(name, err)
	}
	rig, err := h.newRig(defaultWeights(), 100, 0)
	if err != nil {
		return fail(name, err)
	}
	if err := h.seedAvailableRow(provider.Tronsell, h.nextID("exhaustion-seed"), rig.slotAddress, 150, 3750, time.Hour); err != nil {
		return fail(name, err)
	}

	var fastCount, slowCount int
	for i := 0; i < 3; i++ {
		_, req, err := h.newRequest("exhaustion", rig.slotAddress, 100, 5*time.Second, "STANDARD")
		if err != nil {
			return fail(name, err)
		}
		res, err := rig.svc.Create(h.ctx, req)
		if err != nil {
			return fail(name, err)
		}
		if res.Status != reservations.StatusConfirmed {
			return fail(name, fmt.Errorf("order %d: status = %s, want CONFIRMED", i, res.Status))
		}
		if res.FastPath == nil {
			return fail(name, fmt.Errorf("order %d: fast_path is nil on a CONFIRMED reservation", i))
		}
		if *res.FastPath {
			fastCount++
		} else {
			slowCount++
		}
	}
	if fastCount != 1 {
		return fail(name, fmt.Errorf("fast-path confirmations = %d, want exactly 1 (the 150-unit seeded row covers one 100-unit order, claimed whole)", fastCount))
	}
	if slowCount != 2 {
		return fail(name, fmt.Errorf("slow-path confirmations = %d, want exactly 2 (buffer exhausted after the first order)", slowCount))
	}
	return pass(name)
}

// scenarioSingleProviderUnhealthyRenormalizedRouting: catfee goes
// unhealthy; SelectProvider keeps choosing between tronsell/netts at
// their own renormalized 60:35 ratio, never catfee, never the manual
// ladder.
func (h *harness) scenarioSingleProviderUnhealthyRenormalizedRouting() Result {
	const name = "SingleProviderUnhealthyRenormalizedRoutingContinues"
	rig, err := h.newRig(defaultWeights(), 100, 0)
	if err != nil {
		return fail(name, err)
	}
	rig.catfee.ForceMalformed()
	if err := rig.poller.PollAll(h.ctx); err != nil {
		return fail(name, err)
	}

	const draws = 3000
	counts := map[string]int{}
	for i := 0; i < draws; i++ {
		sel, err := rig.router.SelectProvider(h.ctx, defaultWeights(), rig.ceiling)
		if err != nil {
			return fail(name, err)
		}
		if sel.Provider == provider.JustLendManual {
			return fail(name, fmt.Errorf("draw %d fell back to the manual ladder (reason %s) even though 2 of 3 primaries are healthy", i, sel.Reason))
		}
		counts[sel.Provider]++
	}
	if counts[provider.Catfee] != 0 {
		return fail(name, fmt.Errorf("catfee was selected %d times while forced unhealthy, want 0", counts[provider.Catfee]))
	}

	const tolerance = 0.03
	wantTronsell := 0.60 / 0.95
	wantNetts := 0.35 / 0.95
	if diff := absFloat(float64(counts[provider.Tronsell])/draws - wantTronsell); diff > tolerance {
		return fail(name, fmt.Errorf("tronsell share = %.4f, want %.4f +/- %.2f (renormalized 60:35)", float64(counts[provider.Tronsell])/draws, wantTronsell, tolerance))
	}
	if diff := absFloat(float64(counts[provider.Netts])/draws - wantNetts); diff > tolerance {
		return fail(name, fmt.Errorf("netts share = %.4f, want %.4f +/- %.2f (renormalized 60:35)", float64(counts[provider.Netts])/draws, wantNetts, tolerance))
	}
	return pass(name)
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// scenarioAllProvidersUnhealthyManualFallback: all three primaries
// unhealthy; a reservation with an empty buffer and no way to reach the
// slow path correctly fails once its own deadline elapses, with zero
// automated Delegate attempts, and exactly one open manual_fallback_events
// row recorded for it.
func (h *harness) scenarioAllProvidersUnhealthyManualFallback() Result {
	const name = "AllProvidersUnhealthyManualFallbackReservationFailsAtDeadline"
	if err := h.drainBuffer(); err != nil {
		return fail(name, err)
	}
	rig, err := h.newRig(defaultWeights(), 100, 0)
	if err != nil {
		return fail(name, err)
	}
	rig.tronsell.ForceMalformed()
	rig.netts.ForceMalformed()
	rig.catfee.ForceMalformed()
	if err := rig.poller.PollAll(h.ctx); err != nil {
		return fail(name, err)
	}

	order, req, err := h.newRequest("allunhealthy", rig.slotAddress, 100, 1500*time.Millisecond, "STANDARD")
	if err != nil {
		return fail(name, err)
	}
	res, err := rig.svc.Create(h.ctx, req)
	if err != nil {
		return fail(name, err)
	}
	if res.Status != reservations.StatusFailed {
		return fail(name, fmt.Errorf("status = %s, want FAILED", res.Status))
	}

	baseline := rig.idBase
	if got := rig.tronsell.DelegateCallCount(); got != 0 {
		return fail(name, fmt.Errorf("tronsell.DelegateCallCount = %d, want 0 (zero automated attempts against an unhealthy primary)", got))
	}
	if got := rig.netts.DelegateCallCount(); got != 0 {
		return fail(name, fmt.Errorf("netts.DelegateCallCount = %d, want 0", got))
	}
	if got := rig.catfee.DelegateCallCount(); got != 0 {
		return fail(name, fmt.Errorf("catfee.DelegateCallCount = %d, want 0", got))
	}
	_ = baseline // warmup advances the delegation-id sequence directly (AdvanceDelegationSequence), never via a real Delegate call, so 0 is the true baseline too

	events, err := routing.ListFallbackEvents(h.ctx, h.pool, boolPtr(false))
	if err != nil {
		return fail(name, err)
	}
	var found bool
	for _, e := range events {
		if e.Reason == routing.FallbackReasonAllUnhealthy && e.OrderID != nil && *e.OrderID == order.ID {
			found = true
		}
	}
	if !found {
		return fail(name, fmt.Errorf("no open manual_fallback_events row with reason %s references order %d", routing.FallbackReasonAllUnhealthy, order.ID))
	}
	return pass(name)
}

func boolPtr(b bool) *bool { return &b }

// scenarioPriceSpikeAboveCeilingAllProviders: all three primaries are
// healthy and quotable, but every one is priced above ceiling -- the
// same fallback path as the outage scenario, distinguished by reason
// (all_over_ceiling, not all_unhealthy).
func (h *harness) scenarioPriceSpikeAboveCeilingAllProviders() Result {
	const name = "PriceSpikeAboveCeilingAllProvidersSameFallbackDistinctReason"
	if err := h.drainBuffer(); err != nil {
		return fail(name, err)
	}
	const ceiling = 20.0
	rig, err := h.newRig(defaultWeights(), ceiling, 0)
	if err != nil {
		return fail(name, err)
	}
	rig.tronsell.ForcePrice(ceiling + 5)
	rig.netts.ForcePrice(ceiling + 8)
	rig.catfee.ForcePrice(ceiling + 10)
	if err := rig.poller.PollAll(h.ctx); err != nil {
		return fail(name, err)
	}

	order, req, err := h.newRequest("pricespike", rig.slotAddress, 100, 1500*time.Millisecond, "STANDARD")
	if err != nil {
		return fail(name, err)
	}
	res, err := rig.svc.Create(h.ctx, req)
	if err != nil {
		return fail(name, err)
	}
	if res.Status != reservations.StatusFailed {
		return fail(name, fmt.Errorf("status = %s, want FAILED", res.Status))
	}
	if got := rig.tronsell.DelegateCallCount() + rig.netts.DelegateCallCount() + rig.catfee.DelegateCallCount(); got != 0 {
		return fail(name, fmt.Errorf("total Delegate calls = %d, want 0 (every primary was filtered out by ceiling before ever reaching Delegate)", got))
	}

	events, err := routing.ListFallbackEvents(h.ctx, h.pool, boolPtr(false))
	if err != nil {
		return fail(name, err)
	}
	var found bool
	for _, e := range events {
		if e.Reason == routing.FallbackReasonAllOverCeiling && e.OrderID != nil && *e.OrderID == order.ID {
			found = true
		}
	}
	if !found {
		return fail(name, fmt.Errorf("no open manual_fallback_events row with reason %s references order %d", routing.FallbackReasonAllOverCeiling, order.ID))
	}
	return pass(name)
}

// scenarioVendorChargedMoreThanQuoted: the slow path's chosen vendor
// charges a different (but still under-ceiling) price than it quoted --
// flagged in vendor_overcharge_events, and the actual charged cost (not
// the stale quote) is what gets posted as this delegation's E4 entry.
func (h *harness) scenarioVendorChargedMoreThanQuoted() Result {
	const name = "VendorChargedMoreThanQuotedFlaggedCorrectActualCostPosted"
	if err := h.drainBuffer(); err != nil {
		return fail(name, err)
	}
	const ceiling = 40.0
	rig, err := h.newRig(routing.RoutingWeights{provider.Tronsell: 1.0}, ceiling, 0)
	if err != nil {
		return fail(name, err)
	}
	rig.tronsell.ForceChargedPriceSun(35.0)
	predictedID := rig.nextDelegationID(provider.Tronsell)

	order, req, err := h.newRequest("overcharge", rig.slotAddress, 500, 5*time.Second, "STANDARD")
	if err != nil {
		return fail(name, err)
	}
	res, err := rig.svc.Create(h.ctx, req)
	if err != nil {
		return fail(name, err)
	}
	if res.Status != reservations.StatusConfirmed {
		return fail(name, fmt.Errorf("status = %s, want CONFIRMED (still under ceiling)", res.Status))
	}
	if res.FastPath == nil || *res.FastPath {
		return fail(name, fmt.Errorf("fast_path = %v, want false (slow path)", res.FastPath))
	}

	var chargedPriceSun float64
	var overCeiling bool
	err = h.pool.QueryRow(h.ctx, `
		SELECT charged_price_sun, over_ceiling FROM vendor_overcharge_events
		WHERE delegation_id = $1 AND order_id = $2
	`, predictedID, order.ID).Scan(&chargedPriceSun, &overCeiling)
	if err != nil {
		return fail(name, fmt.Errorf("looking up vendor_overcharge_events for delegation %s, order %d: %w", predictedID, order.ID, err))
	}
	if overCeiling {
		return fail(name, errors.New("vendor_overcharge_events.over_ceiling = true, want false (35 sun is under the 40 sun ceiling)"))
	}
	if absFloat(chargedPriceSun-35.0) > 0.01 {
		return fail(name, fmt.Errorf("vendor_overcharge_events.charged_price_sun = %v, want ~35.0", chargedPriceSun))
	}

	expectedCost, err := money.ParseDecimal(fmt.Sprintf("%.6f", 35.0*float64(req.EnergyUnits)/1_000000))
	if err != nil {
		return fail(name, err)
	}
	if res.CostTRX == nil || *res.CostTRX != expectedCost {
		return fail(name, fmt.Errorf("reservation CostTRX = %v, want %v (the ACTUAL charged cost, not the stale quote)", res.CostTRX, expectedCost))
	}

	var found bool
	for _, c := range h.costReportsSnapshot() {
		if c.delegation.ID == predictedID && c.orderID == order.ID {
			found = true
			if c.delegation.CostTRX != expectedCost {
				return fail(name, fmt.Errorf("E4 cost report for delegation %s = %v, want %v (actual charged cost)", predictedID, c.delegation.CostTRX, expectedCost))
			}
		}
	}
	if !found {
		return fail(name, fmt.Errorf("no ReportEnergyCost call recorded for delegation %s, order %d", predictedID, order.ID))
	}
	return pass(name)
}

// scenarioDelegationNeverLandedOnChain: the vendor's Delegate call
// returns success (a "200"), but the shared FakeTronReader shows the
// delegation never actually landed on-chain -- the reservation must
// never confirm, and never get marked AVAILABLE, on the strength of the
// vendor's own claim alone (invariant 1).
func (h *harness) scenarioDelegationNeverLandedOnChain() Result {
	const name = "DelegationNeverLandedOnChainDespiteVendor200NeverConfirmed"
	if err := h.drainBuffer(); err != nil {
		return fail(name, err)
	}
	rig, err := h.newRig(routing.RoutingWeights{provider.Tronsell: 1.0}, 100, 0)
	if err != nil {
		return fail(name, err)
	}

	_, req, err := h.newRequest("neverlanded", rig.slotAddress, 300, 5*time.Second, "STANDARD")
	if err != nil {
		return fail(name, err)
	}
	// Pre-register the shared reader for the EXACT delegation this call
	// is about to produce (deterministic: this rig's own first Delegate
	// call, on a provider warmed up to a known, disjoint id base) as
	// "0 units on-chain" -- a vendor claiming success with nothing
	// actually delegated.
	predictedID := rig.nextDelegationID(provider.Tronsell)
	h.reader.inner.SetUnits(req.TargetAddress, predictedID, 0)

	res, err := rig.svc.Create(h.ctx, req)
	if err != nil {
		return fail(name, err)
	}
	if res.Status != reservations.StatusFailed {
		return fail(name, fmt.Errorf("status = %s, want FAILED (VerifyOnChain must reject a 0-unit on-chain answer)", res.Status))
	}
	if got := rig.tronsell.DelegateCallCount(); got != 1 {
		return fail(name, fmt.Errorf("tronsell.DelegateCallCount = %d, want exactly 1 (the vendor really was called once, and really did claim success)", got))
	}

	available, err := rig.buffer.AvailableTotal(h.ctx)
	if err != nil {
		return fail(name, err)
	}
	if available != 0 {
		return fail(name, fmt.Errorf("buffer AvailableTotal = %d, want 0 (never marked AVAILABLE on an unverified delegation)", available))
	}
	for _, c := range h.costReportsSnapshot() {
		if c.delegation.ID == predictedID {
			return fail(name, fmt.Errorf("an E4 entry was reported for delegation %s despite it never landing on-chain", predictedID))
		}
	}
	return pass(name)
}

// scenarioReconciliationCatchesEarlyRevokedDelegation: an AVAILABLE row
// nearing its own expiry is revoked on-chain (the vendor's own dashboard
// pulled it early, or it expired sooner than bookkeeping expected) --
// Reconcile must catch it, mark it EXPIRED, and alert.
func (h *harness) scenarioReconciliationCatchesEarlyRevokedDelegation() Result {
	const name = "ReconciliationCatchesEarlyRevokedDelegationMidWindow"
	if err := h.drainBuffer(); err != nil {
		return fail(name, err)
	}
	rig, err := h.newRig(defaultWeights(), 100, 0)
	if err != nil {
		return fail(name, err)
	}

	spy := &spyAlerter{}
	// Reconcile is a *buffer.Buffer method that reads b.alerter directly;
	// there is no setter, so this scenario builds its own Buffer sharing
	// everything else the rig already built, purely to inject an
	// observable Alerter -- mirroring the same "construct a second,
	// narrowly-different instance around shared real dependencies"
	// pattern this harness already uses for rig isolation itself.
	reconcileBuf, err := buffer.NewBuffer(h.pool, rig.providers, rig.router, stubDemandObserver{}, h.reader, spy, buffer.Config{
		SlotAddresses: []string{rig.slotAddress},
		Ceiling:       rig.ceiling,
	})
	if err != nil {
		return fail(name, err)
	}

	delegationID := h.nextID("revoked")
	if err := h.seedAvailableRow(provider.Tronsell, delegationID, rig.slotAddress, 400, 25*400, 5*time.Minute); err != nil {
		return fail(name, err)
	}
	// Revoked on-chain: the reader now reports 0 units for this exact
	// (address, delegationID) pair, even though bookkeeping still shows
	// it AVAILABLE.
	h.reader.inner.SetUnits(rig.slotAddress, delegationID, 0)

	beforeAvailable, err := reconcileBuf.AvailableTotal(h.ctx)
	if err != nil {
		return fail(name, err)
	}
	if err := reconcileBuf.Reconcile(h.ctx); err != nil {
		return fail(name, err)
	}
	afterAvailable, err := reconcileBuf.AvailableTotal(h.ctx)
	if err != nil {
		return fail(name, err)
	}
	if afterAvailable != beforeAvailable-400 {
		return fail(name, fmt.Errorf("available after Reconcile = %d, want %d (the revoked 400-unit row corrected out)", afterAvailable, beforeAvailable-400))
	}
	if spy.calls != 1 {
		return fail(name, fmt.Errorf("AlertBufferShortfall was called %d times, want exactly 1", spy.calls))
	}
	return pass(name)
}

type spyAlerter struct {
	mu    sync.Mutex
	calls int
}

func (s *spyAlerter) AlertBufferShortfall(ctx context.Context, row buffer.Row, expectedUnits, actualUnits int64) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
}

// scenarioConcurrentReservationsRacingBuffer is this chunk's own
// PERFORMANCE TARGET: real concurrency in Create's own fast-path claim,
// under both plenty (every concurrent order gets fast-path capacity) and
// scarcity (the buffer covers only some of them; the rest correctly and
// successfully fall through to the healthy slow path -- never lost,
// never double-spent, never deadlocked).
func (h *harness) scenarioConcurrentReservationsRacingBuffer() Result {
	const name = "ConcurrentReservationsRacingBufferUnderPlentyAndScarcity"
	const perOrder = 100

	// Plenty: exactly enough for every one of n concurrent orders to win
	// the fast path.
	const nPlenty = 5
	if err := h.drainBuffer(); err != nil {
		return fail(name, err)
	}
	plentyRig, err := h.newRig(defaultWeights(), 100, 0)
	if err != nil {
		return fail(name, err)
	}
	// One row PER order, each exactly perOrder-sized: Reserve claims whole
	// rows, so a single combined nPlenty*perOrder row would let just the
	// first winner claim the entire thing, starving the other n-1 --
	// exactly the "plenty" case must not do.
	for i := 0; i < nPlenty; i++ {
		if err := h.seedAvailableRow(provider.Tronsell, h.nextID("plenty-seed"), plentyRig.slotAddress, perOrder, 25*perOrder, time.Hour); err != nil {
			return fail(name, err)
		}
	}
	if res, err := h.runConcurrentCreates(plentyRig, "plenty", nPlenty, perOrder); err != nil {
		return fail(name, err)
	} else {
		var fastCount int
		for _, r := range res {
			if r.Status != reservations.StatusConfirmed {
				return fail(name, fmt.Errorf("plenty phase: order status = %s, want CONFIRMED", r.Status))
			}
			if r.FastPath != nil && *r.FastPath {
				fastCount++
			}
		}
		if fastCount != nPlenty {
			return fail(name, fmt.Errorf("plenty phase: %d/%d orders confirmed via fast path, want all %d (buffer had exactly enough)", fastCount, nPlenty, nPlenty))
		}
	}

	// Scarcity: enough for only k of n concurrent orders' fast path; the
	// rest must still all succeed, via the (healthy) slow path.
	const nScarce = 5
	const kFast = 2
	if err := h.drainBuffer(); err != nil {
		return fail(name, err)
	}
	scarceRig, err := h.newRig(defaultWeights(), 100, 0)
	if err != nil {
		return fail(name, err)
	}
	for i := 0; i < kFast; i++ {
		if err := h.seedAvailableRow(provider.Tronsell, h.nextID("scarce-seed"), scarceRig.slotAddress, perOrder, 25*perOrder, time.Hour); err != nil {
			return fail(name, err)
		}
	}
	res, err := h.runConcurrentCreates(scarceRig, "scarce", nScarce, perOrder)
	if err != nil {
		return fail(name, err)
	}
	var fastCount, slowCount int
	for _, r := range res {
		if r.Status != reservations.StatusConfirmed {
			return fail(name, fmt.Errorf("scarcity phase: order status = %s, want CONFIRMED (the slow path is healthy and must catch every fast-path loser)", r.Status))
		}
		if r.FastPath == nil {
			return fail(name, errors.New("scarcity phase: fast_path is nil on a CONFIRMED reservation"))
		}
		if *r.FastPath {
			fastCount++
		} else {
			slowCount++
		}
	}
	if fastCount != kFast {
		return fail(name, fmt.Errorf("scarcity phase: %d orders confirmed via fast path, want exactly %d", fastCount, kFast))
	}
	if slowCount != nScarce-kFast {
		return fail(name, fmt.Errorf("scarcity phase: %d orders confirmed via slow path, want exactly %d", slowCount, nScarce-kFast))
	}
	return pass(name)
}

// runConcurrentCreates fires n concurrent Service.Create calls (each its
// own real C1 order, created sequentially beforehand so the HTTP fixture
// setup itself is never the source of any race this scenario cares
// about) and returns every result in submission order, or the first
// error any goroutine hit.
func (h *harness) runConcurrentCreates(rig *rig, prefix string, n int, energyUnits int64) ([]reservations.Reservation, error) {
	reqs := make([]reservations.Request, n)
	for i := 0; i < n; i++ {
		_, req, err := h.newRequest(fmt.Sprintf("%s%d", prefix, i), rig.slotAddress, energyUnits, 5*time.Second, "STANDARD")
		if err != nil {
			return nil, err
		}
		reqs[i] = req
	}

	results := make([]reservations.Reservation, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = rig.svc.Create(h.ctx, reqs[i])
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}
