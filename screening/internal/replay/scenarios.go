package replay

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"screening/internal/cache"
	"screening/internal/discovery"
	"screening/internal/holds"
	"screening/internal/pipeline"
	"screening/internal/provider"
	"screening/internal/rescreen"
	"screening/internal/verdict"
)

func pass(name string) Result { return Result{Name: name, Passed: true} }
func fail(name string, err error) Result {
	return Result{Name: name, Passed: false, Detail: err.Error()}
}

// findOpenHoldForOrder is the harness's own lookup for the one thing
// ScreenAndReport's own return value doesn't hand back directly: the
// hold row it opened. Scenarios need the hold's own id to Release/Reject
// it.
func (h *harness) findOpenHoldForOrder(orderID int64) (holds.Hold, error) {
	open, err := holds.ListOpen(h.ctx, h.pool)
	if err != nil {
		return holds.Hold{}, err
	}
	for _, hd := range open {
		if hd.OrderID == orderID {
			return hd, nil
		}
	}
	return holds.Hold{}, fmt.Errorf("replay: no open hold found for order %d", orderID)
}

// screeningResultsCountForAddress is fixture scaffolding only (no public
// API returns a bare count): scenarioCacheHit uses it to confirm a
// second, same-sender order really did reuse the one existing
// screening_results row rather than creating a second one.
func (h *harness) screeningResultsCountForAddress(address string) (int, error) {
	var count int
	err := h.pool.QueryRow(h.ctx, `SELECT count(*) FROM screening_results WHERE sender_address = $1`, address).Scan(&count)
	return count, err
}

// runRescreenTick is the only path any scenario uses to drive
// rescreen.RunTick -- routed through here, not called directly, so
// FINAL ASSERTION 4 ("zero automatic C1 transition calls originate from
// the re-screen job") can be checked from every call this run ever
// made, not just the one dedicated re-screen scenario happens to make.
func (h *harness) runRescreenTick() error {
	before := h.client.transitionCallCount()
	err := rescreen.RunTick(h.ctx, h.pool, h.client, h.mock, h.rescreenCfg)
	after := h.client.transitionCallCount()
	h.mu.Lock()
	h.rescreenCallDeltas = append(h.rescreenCallDeltas, after-before)
	h.mu.Unlock()
	return err
}

// scenarioCleanPassFirstScreen is the SCENARIO MIX's majority case: a
// sender address whose natural mock score lands below PassBelow, first
// time screened.
func (h *harness) scenarioCleanPassFirstScreen() Result {
	const name = "CleanPassFirstScreen"
	addr, err := h.findAddressInBand("clean", 0, verdict.DefaultThresholds.PassBelow)
	if err != nil {
		return fail(name, err)
	}
	fe, err := h.createFundedOrder("clean", "cust-clean", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe.entry); err != nil {
		return fail(name, err)
	}

	order, err := h.ledger.getOrder(h.ctx, fe.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order.State != "screened" {
		return fail(name, fmt.Errorf("order state = %s, want screened", order.State))
	}
	reason, err := h.ledger.latestTransitionReason(h.ctx, h.ledgerPool, fe.order.ID)
	if err != nil {
		return fail(name, err)
	}
	if reason != verdict.ReasonPass {
		return fail(name, fmt.Errorf("transition reason = %s, want %s", reason, verdict.ReasonPass))
	}
	return pass(name)
}

// scenarioFlaggedHeldThenReleased: forced-flagged sender -> held ->
// manually released -> screened.
func (h *harness) scenarioFlaggedHeldThenReleased() Result {
	const name = "FlaggedHeldThenManuallyReleased"
	addr := h.nextExternalID("flag-release")
	h.mock.ForceFlagged(addr)

	fe, err := h.createFundedOrder("flag-release", "cust-flag-release", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe.entry); err != nil {
		return fail(name, err)
	}

	order, err := h.ledger.getOrder(h.ctx, fe.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order.State != "held" {
		return fail(name, fmt.Errorf("order state = %s, want held", order.State))
	}

	hold, err := h.findOpenHoldForOrder(fe.order.ID)
	if err != nil {
		return fail(name, err)
	}

	if err := holds.Release(h.ctx, h.pool, h.client, hold.ID, "replay-reviewer-1", "clean on manual review"); err != nil {
		return fail(name, err)
	}

	resolved, err := holds.Get(h.ctx, h.pool, hold.ID)
	if err != nil {
		return fail(name, err)
	}
	if resolved.Status != holds.StatusReleased {
		return fail(name, fmt.Errorf("hold status = %s, want RELEASED", resolved.Status))
	}
	if resolved.ResolvedBy == nil || *resolved.ResolvedBy == "" {
		return fail(name, errors.New("released hold has no reviewer recorded"))
	}

	order, err = h.ledger.getOrder(h.ctx, fe.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order.State != "screened" {
		return fail(name, fmt.Errorf("order state after release = %s, want screened", order.State))
	}
	return pass(name)
}

// scenarioFlaggedHeldThenRejected: forced-flagged sender -> held ->
// manually rejected. Expected to fail at the refund-entry-construction
// step -- StubRefundEntryBuilder is the only RefundEntryBuilder this
// system ships (see holds.ErrRefundEntryNotImplemented's own doc
// comment: no owner exists yet for the physical BEP20 refund). This
// scenario's own pass condition IS that expected failure, not a
// successful reject.
func (h *harness) scenarioFlaggedHeldThenRejected() Result {
	const name = "FlaggedHeldThenManuallyRejected"
	addr := h.nextExternalID("flag-reject")
	h.mock.ForceFlagged(addr)

	fe, err := h.createFundedOrder("flag-reject", "cust-flag-reject", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe.entry); err != nil {
		return fail(name, err)
	}

	hold, err := h.findOpenHoldForOrder(fe.order.ID)
	if err != nil {
		return fail(name, err)
	}

	err = holds.Reject(h.ctx, h.pool, h.client, holds.StubRefundEntryBuilder{}, hold.ID, "replay-reviewer-2", "flagged, reject and refund")
	if !errors.Is(err, holds.ErrRefundEntryNotImplemented) {
		return fail(name, fmt.Errorf("Reject error = %v, want ErrRefundEntryNotImplemented", err))
	}

	// The hold must still be OPEN and C1's order must still be held --
	// Reject fails before ever calling C1 (see holds.Reject's own doc
	// comment: it fails at entry-building, one step before
	// rejecter.RejectHold), so nothing about either side should have
	// moved.
	stillOpen, err := holds.Get(h.ctx, h.pool, hold.ID)
	if err != nil {
		return fail(name, err)
	}
	if stillOpen.Status != holds.StatusOpen {
		return fail(name, fmt.Errorf("hold status = %s, want still OPEN after a failed reject", stillOpen.Status))
	}
	order, err := h.ledger.getOrder(h.ctx, fe.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order.State != "held" {
		return fail(name, fmt.Errorf("order state = %s, want still held after a failed reject", order.State))
	}
	return pass(name)
}

// scenarioCacheHit: a second order from an already-screened sender
// skips the vendor call entirely.
func (h *harness) scenarioCacheHit() Result {
	const name = "CacheHitSkipsVendorCall"
	addr, err := h.findAddressInBand("cachehit", 0, verdict.DefaultThresholds.PassBelow)
	if err != nil {
		return fail(name, err)
	}

	fe1, err := h.createFundedOrder("cachehit1", "cust-cachehit-1", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe1.entry); err != nil {
		return fail(name, err)
	}
	if got := h.mock.ScreenCallCount(addr); got != 1 {
		return fail(name, fmt.Errorf("ScreenCallCount after 1st order = %d, want 1", got))
	}

	fe2, err := h.createFundedOrder("cachehit2", "cust-cachehit-2", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe2.entry); err != nil {
		return fail(name, err)
	}
	if got := h.mock.ScreenCallCount(addr); got != 1 {
		return fail(name, fmt.Errorf("ScreenCallCount after 2nd, same-sender order = %d, want still 1 (cache hit)", got))
	}

	count, err := h.screeningResultsCountForAddress(addr)
	if err != nil {
		return fail(name, err)
	}
	if count != 1 {
		return fail(name, fmt.Errorf("screening_results rows for %s = %d, want exactly 1 (reused, not duplicated)", addr, count))
	}

	order2, err := h.ledger.getOrder(h.ctx, fe2.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order2.State != "screened" {
		return fail(name, fmt.Errorf("2nd order state = %s, want screened", order2.State))
	}
	return pass(name)
}

// fastOutageConfig returns pipelineCfg tuned for a forced-timeout
// scenario: a real 10s-timeout/3-retry run would take up to 30s per
// scenario, so timeout and retries are both cut down -- this is testing
// the outage-policy BRANCH, not the production timeout values
// themselves (those are cfg the operator sets, not this package's
// concern).
func (h *harness) fastOutageConfig(policy provider.OutagePolicy) pipeline.Config {
	cfg := h.pipelineCfg
	cfg.Timeout = 50 * time.Millisecond
	cfg.Retries = 2
	cfg.OutagePolicy = policy
	return cfg
}

// scenarioVendorTimeoutFailClosed: every retry against the vendor times
// out; FailClosed reports Hold/screening_hold_unavailable.
func (h *harness) scenarioVendorTimeoutFailClosed() Result {
	const name = "VendorTimeoutFailClosed"
	addr := h.nextExternalID("timeout-failclosed")
	h.mock.ForceTimeout(addr)

	fe, err := h.createFundedOrder("timeout-failclosed", "cust-timeout-fc", addr)
	if err != nil {
		return fail(name, err)
	}
	cfg := h.fastOutageConfig(provider.FailClosed)
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, cfg, fe.entry); err != nil {
		return fail(name, err)
	}

	order, err := h.ledger.getOrder(h.ctx, fe.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order.State != "held" {
		return fail(name, fmt.Errorf("order state = %s, want held", order.State))
	}
	reason, err := h.ledger.latestTransitionReason(h.ctx, h.ledgerPool, fe.order.ID)
	if err != nil {
		return fail(name, err)
	}
	if reason != verdict.ReasonHoldUnavailable {
		return fail(name, fmt.Errorf("transition reason = %s, want %s", reason, verdict.ReasonHoldUnavailable))
	}
	return pass(name)
}

// scenarioVendorTimeoutFailOpen: every retry against the vendor times
// out; FailOpen reports Pass/screening_pass_vendor_unavailable -- an
// auditable reason string, never a plain screening_pass.
func (h *harness) scenarioVendorTimeoutFailOpen() Result {
	const name = "VendorTimeoutFailOpen"
	addr := h.nextExternalID("timeout-failopen")
	h.mock.ForceTimeout(addr)

	fe, err := h.createFundedOrder("timeout-failopen", "cust-timeout-fo", addr)
	if err != nil {
		return fail(name, err)
	}
	cfg := h.fastOutageConfig(provider.FailOpen)
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, cfg, fe.entry); err != nil {
		return fail(name, err)
	}

	order, err := h.ledger.getOrder(h.ctx, fe.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order.State != "screened" {
		return fail(name, fmt.Errorf("order state = %s, want screened", order.State))
	}
	reason, err := h.ledger.latestTransitionReason(h.ctx, h.ledgerPool, fe.order.ID)
	if err != nil {
		return fail(name, err)
	}
	if reason != verdict.ReasonPassVendorUnavailable {
		return fail(name, fmt.Errorf("transition reason = %s, want %s (never a plain %s)", reason, verdict.ReasonPassVendorUnavailable, verdict.ReasonPass))
	}
	return pass(name)
}

// scenarioAmbiguousRiskBand: a natural score between PassBelow and
// FlaggedAtOrAbove, with the vendor's own Flagged=false -- held, with
// screening_hold_ambiguous, distinct from screening_hold_flagged.
func (h *harness) scenarioAmbiguousRiskBand() Result {
	const name = "AmbiguousRiskBand"
	addr, err := h.findAddressInBand("ambiguous", verdict.DefaultThresholds.PassBelow, verdict.DefaultThresholds.FlaggedAtOrAbove)
	if err != nil {
		return fail(name, err)
	}

	fe, err := h.createFundedOrder("ambiguous", "cust-ambiguous", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe.entry); err != nil {
		return fail(name, err)
	}

	order, err := h.ledger.getOrder(h.ctx, fe.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order.State != "held" {
		return fail(name, fmt.Errorf("order state = %s, want held", order.State))
	}
	reason, err := h.ledger.latestTransitionReason(h.ctx, h.ledgerPool, fe.order.ID)
	if err != nil {
		return fail(name, err)
	}
	if reason != verdict.ReasonHoldAmbiguous {
		return fail(name, fmt.Errorf("transition reason = %s, want %s", reason, verdict.ReasonHoldAmbiguous))
	}
	return pass(name)
}

// scenarioRescreenAfterAlreadyPassed: a sender is flagged AFTER its
// order already passed screening (and is thus in one of rescreen's own
// relevantStates -- "screened" here, not "dispatching": C3 itself never
// constructs the conversion entry screened->dispatching requires (that
// is C4/C5's job, out of C3's scope entirely per the build spec's own
// "WHAT C3 IS NOT" section), and rescreen's own relevantStates treats
// screened and dispatching identically, so screened alone already
// exercises the exact code path this scenario targets. Must produce a
// rescreen_flags row and, per FINAL ASSERTION 4, exactly zero automatic
// C1 transition calls.
func (h *harness) scenarioRescreenAfterAlreadyPassed() Result {
	const name = "RescreenAfterAlreadyPassed"
	addr, err := h.findAddressInBand("rescreen", 0, verdict.DefaultThresholds.PassBelow)
	if err != nil {
		return fail(name, err)
	}

	fe, err := h.createFundedOrder("rescreen", "cust-rescreen", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe.entry); err != nil {
		return fail(name, err)
	}
	order, err := h.ledger.getOrder(h.ctx, fe.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order.State != "screened" {
		return fail(name, fmt.Errorf("order state before re-screen = %s, want screened", order.State))
	}

	// The sender is flagged only now -- after already passing.
	h.mock.ForceFlagged(addr)

	if err := h.runRescreenTick(); err != nil {
		return fail(name, err)
	}

	flags, err := rescreen.List(h.ctx, h.pool, nil)
	if err != nil {
		return fail(name, err)
	}
	var found bool
	for _, f := range flags {
		if f.OrderID == fe.order.ID {
			found = true
			break
		}
	}
	if !found {
		return fail(name, fmt.Errorf("no rescreen_flags row recorded for order %d after its sender was flagged post-pass", fe.order.ID))
	}

	// The re-screen job must never itself transition the order --
	// confirmed here directly (this scenario's own before/after), and
	// again globally by FINAL ASSERTION 4 across every runRescreenTick
	// call this whole run made.
	order, err = h.ledger.getOrder(h.ctx, fe.order.ExternalID)
	if err != nil {
		return fail(name, err)
	}
	if order.State != "screened" {
		return fail(name, fmt.Errorf("order state after re-screen = %s, want unchanged screened", order.State))
	}
	return pass(name)
}

// scenarioIllegalTransitionRace: the order leaves `funded` (a customer
// cancel, reaching `refunded`) between enqueue and C3.4's own call --
// handled without a retry storm, queue row still reaches DONE.
func (h *harness) scenarioIllegalTransitionRace() Result {
	const name = "IllegalTransitionRace"
	addr := h.nextExternalID("race")

	fe, err := h.createFundedOrder("race", "cust-race", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := h.ledger.cancelFundedOrder(h.ctx, h.ledgerPool, fe.order); err != nil {
		return fail(name, fmt.Errorf("simulating the customer-cancel race: %w", err))
	}

	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe.entry); err != nil {
		return fail(name, fmt.Errorf("ScreenAndReport against an order that already left funded: %w", err))
	}

	entry, err := discovery.Get(h.ctx, h.pool, fe.order.ID)
	if err != nil {
		return fail(name, err)
	}
	if entry.Status != discovery.Done {
		return fail(name, fmt.Errorf("queue status = %s, want DONE even though the order raced out of funded", entry.Status))
	}
	if got := h.mock.ScreenCallCount(addr); got != 1 {
		return fail(name, fmt.Errorf("ScreenCallCount = %d, want exactly 1 -- no retry storm", got))
	}
	return pass(name)
}

// scenarioManualInvalidationThenFreshScreen: an operator invalidates a
// cached verdict; the next order from that same sender must get a real,
// fresh vendor call, not the invalidated row.
func (h *harness) scenarioManualInvalidationThenFreshScreen() Result {
	const name = "ManualInvalidationThenFreshScreen"
	addr, err := h.findAddressInBand("invalidate", 0, verdict.DefaultThresholds.PassBelow)
	if err != nil {
		return fail(name, err)
	}

	fe1, err := h.createFundedOrder("invalidate1", "cust-invalidate-1", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe1.entry); err != nil {
		return fail(name, err)
	}
	if got := h.mock.ScreenCallCount(addr); got != 1 {
		return fail(name, fmt.Errorf("ScreenCallCount after 1st order = %d, want 1", got))
	}

	if err := cache.Invalidate(h.ctx, h.pool, h.pipelineCfg.ProviderName, addr, "replay harness: manual invalidation scenario", "replay-operator"); err != nil {
		return fail(name, err)
	}

	fe2, err := h.createFundedOrder("invalidate2", "cust-invalidate-2", addr)
	if err != nil {
		return fail(name, err)
	}
	if err := pipeline.ScreenAndReport(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg, fe2.entry); err != nil {
		return fail(name, err)
	}
	if got := h.mock.ScreenCallCount(addr); got != 2 {
		return fail(name, fmt.Errorf("ScreenCallCount after invalidation + 2nd order = %d, want 2 (fresh screen, cache not reused)", got))
	}

	count, err := h.screeningResultsCountForAddress(addr)
	if err != nil {
		return fail(name, err)
	}
	if count != 2 {
		return fail(name, fmt.Errorf("screening_results rows for %s = %d, want exactly 2 (original + fresh)", addr, count))
	}
	return pass(name)
}

// scenarioConcurrentQueueProcessing is this chunk's own PERFORMANCE
// TARGET: real concurrency in the queue-processing loop -- several
// orders resolving under concurrent RunTick calls against the same
// database, plus a manual Release racing a concurrent, unrelated
// automatic pipeline pass -- not just a low-concurrency, one-at-a-time
// trust exercise.
func (h *harness) scenarioConcurrentQueueProcessing() Result {
	const name = "ConcurrentQueueProcessingAndRacingRelease"
	const n = 6

	var entries []discovery.QueueEntry
	for i := 0; i < n; i++ {
		addr := h.nextExternalID(fmt.Sprintf("conc%d", i))
		if i == 0 {
			// Exactly one flagged, so this batch also opens a hold this
			// scenario then races a Release against below.
			h.mock.ForceFlagged(addr)
		}
		fe, err := h.createFundedOrder(fmt.Sprintf("conc%d", i), fmt.Sprintf("cust-conc-%d", i), addr)
		if err != nil {
			return fail(name, err)
		}
		entries = append(entries, fe.entry)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pipeline.RunTick(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return fail(name, fmt.Errorf("concurrent RunTick: %w", err))
		}
	}

	for _, e := range entries {
		got, err := discovery.Get(h.ctx, h.pool, e.OrderID)
		if err != nil {
			return fail(name, err)
		}
		if got.Status != discovery.Done {
			return fail(name, fmt.Errorf("order %d status = %s after concurrent RunTick calls, want DONE", e.OrderID, got.Status))
		}
	}

	hold, err := h.findOpenHoldForOrder(entries[0].OrderID)
	if err != nil {
		return fail(name, err)
	}

	var relErr, tickErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		relErr = holds.Release(h.ctx, h.pool, h.client, hold.ID, "replay-concurrent-reviewer", "released while another tick runs")
	}()
	go func() {
		defer wg.Done()
		tickErr = pipeline.RunTick(h.ctx, h.pool, h.mock, h.client, h.pipelineCfg)
	}()
	wg.Wait()
	if relErr != nil {
		return fail(name, fmt.Errorf("Release racing a concurrent RunTick: %w", relErr))
	}
	if tickErr != nil {
		return fail(name, fmt.Errorf("RunTick racing a concurrent Release: %w", tickErr))
	}

	resolved, err := holds.Get(h.ctx, h.pool, hold.ID)
	if err != nil {
		return fail(name, err)
	}
	if resolved.Status != holds.StatusReleased {
		return fail(name, fmt.Errorf("hold status = %s, want RELEASED after the concurrent release", resolved.Status))
	}
	return pass(name)
}
