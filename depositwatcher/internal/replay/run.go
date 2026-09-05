package replay

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Run executes one full replay against a real, running C1 (cfg's
// LedgerBaseURL/LedgerToken) and watcherPool/ledgerPool (this run's own
// database and a raw connection to C1's, for account-creation fixture
// setup only -- see ledgerFixture's own doc comment on why). It runs
// every scenario in the build spec's SCENARIO MIX, then every FINAL
// ASSERTION, and returns a Report regardless of whether everything
// passed -- callers check Report.Passed(), the same split C1.9 uses,
// so a failure is diagnosable from the printed report rather than only
// from a bare error.
func Run(ctx context.Context, watcherPool, ledgerPool *pgxpool.Pool, cfg Config) (*Report, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	h, err := newHarness(ctx, cfg, watcherPool, ledgerPool)
	if err != nil {
		return nil, err
	}
	defer h.close()

	report := &Report{Seed: cfg.Seed}

	scenarios := []struct {
		name string
		run  func() Result
	}{
		{"CleanDeposits", h.scenarioCleanDeposits},
		{"DepositToRetiredAddress", h.scenarioDepositToRetiredAddress},
		{"DepositAfterQuoteExpiry", h.scenarioDepositAfterQuoteExpiry},
		{"PreFinalReorg", h.scenarioPreFinalReorg},
		{"ProviderDisagreement", h.scenarioProviderDisagreement},
		{"ProviderDark", h.scenarioProviderDark},
		{"DuplicateLogDelivery", h.scenarioDuplicateLogDelivery},
		{"WrongTokenAndZeroValue", h.scenarioWrongTokenAndZeroValue},
		{"PostFinalReorg", h.scenarioPostFinalReorg},
	}
	for _, s := range scenarios {
		report.Scenarios = append(report.Scenarios, runGuarded(s.name, s.run))
	}

	report.Assertions = append(report.Assertions, h.finalAssertions()...)
	report.Assertions = append(report.Assertions, h.assertNoPanicsOccurred(report))

	if !report.Passed() {
		return report, fmt.Errorf("replay: one or more scenarios/assertions failed (seed %d)", cfg.Seed)
	}
	return report, nil
}

// runGuarded is FINAL ASSERTION 6's own enforcement mechanism ("zero
// deadlocks, zero panics, zero unexplained errors"): a scenario that
// panics must fail that one scenario and let every other scenario still
// run, not bring down the whole harness (and, worse, leave a caller
// unable to tell WHICH scenario broke).
func runGuarded(name string, run func() Result) (result Result) {
	defer func() {
		if r := recover(); r != nil {
			result = Result{Name: name, Passed: false, Detail: fmt.Sprintf("PANICKED: %v\n%s", r, debug.Stack())}
		}
	}()
	return run()
}

// assertNoPanicsOccurred is FINAL ASSERTION 6, checked from the
// scenarios that already ran: a scenario whose own detail starts with
// "PANICKED:" is exactly what runGuarded produces when recover() caught
// something.
func (h *harness) assertNoPanicsOccurred(report *Report) Result {
	const name = "FinalAssertion6_ZeroPanics"
	for _, s := range report.Scenarios {
		if !s.Passed && len(s.Detail) >= 9 && s.Detail[:9] == "PANICKED:" {
			return fail(name, fmt.Errorf("scenario %s panicked: %s", s.Name, s.Detail))
		}
	}
	return pass(name)
}
