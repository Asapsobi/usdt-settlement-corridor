package replay

import (
	"context"
	"fmt"
	"runtime/debug"

	"s1/internal/db"
)

// Run executes one full replay against pool (this run's own S1 database)
// and an in-process FakeKMSClient seeded from cfg.Seed. Runs every
// scenario in the S1.6 build spec's own SCENARIO list, then every FINAL
// ASSERTION, and returns a Report regardless of whether everything
// passed -- callers check Report.Passed(), same split every prior
// component's own replay harness uses.
func Run(ctx context.Context, pool *db.Pool, cfg Config) (*Report, error) {
	h := newHarness(ctx, cfg, pool)
	report := &Report{Seed: cfg.Seed}

	scenarios := []struct {
		name string
		run  func() Result
	}{
		{"CleanUnderThresholdMajorityAllAutoSignAndVerify", h.scenarioCleanUnderThresholdMajority},
		{"OverThresholdTwoDistinctApproversSigns", h.scenarioOverThresholdTwoApproversSigns},
		{"OverThresholdOneRejectVetoesRegardlessOfPriorApprovals", h.scenarioOverThresholdRejectVetoes},
		{"ConcurrentDuplicateIdempotencyKeyUnderThresholdSignsExactlyOnce", h.scenarioConcurrentDuplicateIdempotencyKey},
		{"KMSFailureMidApprovalKeepsApprovalsAndRecoversOnRetry", h.scenarioKMSFailureMidApprovalRecoversOnRetry},
	}
	for _, s := range scenarios {
		report.Scenarios = append(report.Scenarios, runGuarded(s.name, s.run))
	}

	assertions, err := h.finalAssertions(ctx)
	if err != nil {
		return report, fmt.Errorf("replay: computing final assertions: %w", err)
	}
	report.Assertions = assertions
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

func (h *harness) assertNoPanicsOccurred(report *Report) Result {
	const name = "FinalAssertion_ZeroPanics"
	for _, s := range report.Scenarios {
		if !s.Passed && len(s.Detail) >= 9 && s.Detail[:9] == "PANICKED:" {
			return fail(name, fmt.Errorf("scenario %s panicked: %s", s.Name, s.Detail))
		}
	}
	return pass(name)
}
