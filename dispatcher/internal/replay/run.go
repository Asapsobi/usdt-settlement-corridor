package replay

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dispatcher/internal/db"
)

// Run executes one full replay against pool (this run's own dispatcher
// database), ledgerPool (a raw connection to a real, already-running
// ledgerd's database, fixture use only), and cfg (which names that
// ledgerd's own HTTP address). Runs every scenario in the C5.11 build
// spec's own SCENARIO MIX, then every FINAL ASSERTION, and returns a
// Report regardless of whether everything passed -- callers check
// Report.Passed(), same split every prior component's own replay
// harness uses.
func Run(ctx context.Context, pool *db.Pool, ledgerPool *pgxpool.Pool, cfg Config) (*Report, error) {
	h := newHarness(ctx, cfg, pool, ledgerPool)
	if err := h.ledger.waitHealthy(ctx, 10*time.Second); err != nil {
		return nil, err
	}
	report := &Report{Seed: cfg.Seed}

	scenarios := []struct {
		name string
		run  func() Result
	}{
		{"CleanDirectStandardDispatchMajority", h.scenarioCleanDirectStandardMajority},
		{"CleanSweepBatchNoPartialFailure", h.scenarioCleanSweepBatchNoPartialFailure},
		{"SweepBatchWithPartialSettlement", h.scenarioSweepBatchPartialSettlement},
		{"EnergyReservationFailedBeforeAnyC1Commitment", h.scenarioEnergyReservationFailedZeroLedgerImpact},
		{"EnergyReservationTimeoutDistinctFromFailed", h.scenarioEnergyReservationTimeoutDistinctFromFailed},
		{"NonRetryableDispatchFailureHeldViaTwoCallPathWithSimulatedCrash", h.scenarioNonRetryableDispatchFailureHeldViaTwoCallPath},
		{"DuplicateBroadcastRetryStormExactlyOneRealBroadcast", h.scenarioDuplicateBroadcastRetryStorm},
		{"SlotFreezePreBroadcastReattemptOnFreshSlotNoLoss", h.scenarioSlotFreezePreBroadcast},
		{"SlotFreezePostBroadcastPreConfirmationAlertZeroTransitions", h.scenarioSlotFreezePostBroadcast},
		{"AllSlotsOverCapErrNoEligibleSlotZeroAttempts", h.scenarioAllSlotsOverCap},
		{"FakeSigningServiceDuplicateSignatureInjectionSystemStillBroadcastsExactlyOnce", h.scenarioForcedDuplicateSignature},
	}
	for _, s := range scenarios {
		report.Scenarios = append(report.Scenarios, runGuarded(s.name, s.run))
	}

	report.Assertions = h.finalAssertions(ctx)
	report.Assertions = append(report.Assertions, h.assertNoPanicsOccurred(report))

	if !report.Passed() {
		return report, fmt.Errorf("replay: one or more scenarios/assertions failed (seed %d)", cfg.Seed)
	}
	return report, nil
}

// runGuarded is the ship gate's own "zero panics" enforcement: a
// scenario that panics must fail that one scenario and let every other
// scenario still run, never bring down the whole harness.
func runGuarded(name string, run func() Result) (result Result) {
	defer func() {
		if r := recover(); r != nil {
			result = Result{Name: name, Passed: false, Detail: fmt.Sprintf("PANICKED: %v\n%s", r, debug.Stack())}
		}
	}()
	return run()
}
