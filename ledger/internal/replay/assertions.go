package replay

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/journal"
	"ledger/internal/money"
)

// entriesPerOrder is how many journal_entries rows ONE successfully
// completed order of scenario s produces. Computed statically from each
// scenario's own design (see scenarios.go), not tracked at runtime --
// deliberately: assertion 5 below compares this prediction against the
// database's actual count, so the two must be derived independently for
// the comparison to mean anything. This function, and the loss-per-order
// constant in checkLossAccounting, both assume zero unexpected errors --
// which assertion 9 checks independently, so a run that violates that
// assumption already fails the gate for a more fundamental reason before
// this mismatch would matter.
func entriesPerOrder(s scenarioType) int {
	switch s {
	case scenarioReorgBeforeDispatch, scenarioScreeningHoldReject:
		return 2 // deposit + its reversal
	case scenarioReorgAfterSettlement:
		return 5 // deposit, conversion, payout, reversal-of-deposit, loss entry
	case scenarioQuoteExpiry:
		return 0 // no money ever moves; a late-deposit sub-case fails before Post runs
	default:
		return 3 // happy path, duplicate, screening-hold-release, amount-variance, sweep (either outcome), non-retryable-failure
	}
}

// legalPairs is copied independently from internal/orders' own
// (unexported) transitionTable -- the point of checking against a
// hand-copied list, same as internal/orders' own cross-product test, is
// verifying actual runtime behavior against the spec, not that the
// production map agrees with itself.
var legalPairs = map[[2]string]bool{
	{"quoted", "funded"}:        true,
	{"quoted", "expired"}:       true,
	{"funded", "screened"}:      true,
	{"funded", "held"}:          true,
	{"funded", "refunded"}:      true,
	{"funded", "quoted"}:        true,
	{"held", "screened"}:        true,
	{"held", "refunded"}:        true,
	{"screened", "dispatching"}: true,
	{"dispatching", "settled"}:  true,
	{"dispatching", "held"}:     true,
}

// runAssertions checks all 9 FINAL ASSERTIONS and fills in report's
// AssertionResults, TrialBalance, HaltSetCount, and HaltClearCount. It
// returns a non-nil error iff the report ends up not Passed(), so
// cmd/replay can set a non-zero exit code from one call.
func runAssertions(ctx context.Context, pool *pgxpool.Pool, cfg Config, plans []orderPlan, results []execResult, report *Report) error {
	check := func(name string, fn func() (bool, string, error)) {
		ok, detail, err := fn()
		if err != nil {
			report.AssertionResults = append(report.AssertionResults, assertionResult{Name: name, Passed: false, Detail: "error checking: " + err.Error()})
			return
		}
		report.AssertionResults = append(report.AssertionResults, assertionResult{Name: name, Passed: ok, Detail: detail})
	}

	check("1. TrialBalance() == 0 for every asset", func() (bool, string, error) {
		trial, err := journal.TrialBalance(ctx, pool)
		if err != nil {
			return false, "", err
		}
		report.TrialBalance = make(map[string]string, len(trial))
		ok := true
		var detail string
		for asset, units := range trial {
			formatted, ferr := money.Format(money.Amount{Asset: asset, Units: units})
			if ferr != nil {
				return false, "", ferr
			}
			report.TrialBalance[string(asset)] = formatted
			if units != 0 {
				ok = false
				detail += fmt.Sprintf("%s=%s ", asset, formatted)
			}
		}
		return ok, detail, nil
	})

	check("2. VerifyBalances() returns nothing", func() (bool, string, error) {
		discrepancies, err := journal.VerifyBalances(ctx, pool)
		if err != nil {
			return false, "", err
		}
		if len(discrepancies) == 0 {
			return true, "", nil
		}
		return false, fmt.Sprintf("%d discrepancies, first: %+v", len(discrepancies), discrepancies[0]), nil
	})

	check("3. every order is terminal or held", func() (bool, string, error) {
		var count int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM orders
			WHERE state NOT IN ('settled', 'refunded', 'expired', 'held')
		`).Scan(&count)
		if err != nil {
			return false, "", err
		}
		if count == 0 {
			return true, "", nil
		}
		return false, fmt.Sprintf("%d orders in a non-terminal, non-held state", count), nil
	})

	check("4. order_transitions has no pair outside the transition table", func() (bool, string, error) {
		rows, err := pool.Query(ctx, `SELECT DISTINCT from_state, to_state FROM order_transitions`)
		if err != nil {
			return false, "", err
		}
		defer rows.Close()
		var bad []string
		for rows.Next() {
			var from, to string
			if err := rows.Scan(&from, &to); err != nil {
				return false, "", err
			}
			if !legalPairs[[2]string{from, to}] {
				bad = append(bad, from+"->"+to)
			}
		}
		if err := rows.Err(); err != nil {
			return false, "", err
		}
		if len(bad) == 0 {
			return true, "", nil
		}
		return false, fmt.Sprintf("illegal pairs found: %v", bad), nil
	})

	check("5. journal_entries row count == expected distinct entries", func() (bool, string, error) {
		expected := 0
		for _, p := range plans {
			expected += entriesPerOrder(p.scenario)
		}
		var actual int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries`).Scan(&actual); err != nil {
			return false, "", err
		}
		if actual == expected {
			return true, fmt.Sprintf("%d", actual), nil
		}
		return false, fmt.Sprintf("expected %d, got %d", expected, actual), nil
	})

	check("6. every reversal has exactly one parent, and is never itself reversed", func() (bool, string, error) {
		var multiParent int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM (
				SELECT reversal_of FROM journal_entries
				WHERE reversal_of IS NOT NULL
				GROUP BY reversal_of HAVING count(*) > 1
			) x
		`).Scan(&multiParent)
		if err != nil {
			return false, "", err
		}

		var reversedReversal int
		err = pool.QueryRow(ctx, `
			SELECT count(*) FROM journal_entries child
			JOIN journal_entries parent ON parent.id = child.reversal_of
			WHERE parent.reversal_of IS NOT NULL
		`).Scan(&reversedReversal)
		if err != nil {
			return false, "", err
		}

		if multiParent == 0 && reversedReversal == 0 {
			return true, "", nil
		}
		return false, fmt.Sprintf("multi-parent reversals=%d, reversed-reversals=%d", multiParent, reversedReversal), nil
	})

	check("7. expense:loss:reorg == sum(amount_out) across scenario-B orders", func() (bool, string, error) {
		_, baseAmountOut, _, _ := baseAmounts()
		expected := baseAmountOut.Units * int64(cfg.Scenarios.ReorgAfterSettlement)

		actual, err := journal.Balance(ctx, pool, "expense:loss:reorg")
		if err != nil {
			return false, "", err
		}
		if actual.Units == expected {
			return true, "", nil
		}
		return false, fmt.Sprintf("expected %d, got %d", expected, actual.Units), nil
	})

	check("8. halt count == injected halt-trigger count", func() (bool, string, error) {
		var setCount, clearCount int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM halt_log WHERE action = 'set'`).Scan(&setCount); err != nil {
			return false, "", err
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM halt_log WHERE action = 'clear'`).Scan(&clearCount); err != nil {
			return false, "", err
		}
		report.HaltSetCount, report.HaltClearCount = setCount, clearCount

		if setCount == cfg.Scenarios.ReorgAfterSettlement {
			return true, fmt.Sprintf("%d", setCount), nil
		}
		return false, fmt.Sprintf("expected %d, got %d", cfg.Scenarios.ReorgAfterSettlement, setCount), nil
	})

	check("9. zero deadlocks, panics, or unexplained errors", func() (bool, string, error) {
		if len(report.UnexpectedErrors) == 0 {
			return true, "", nil
		}
		return false, fmt.Sprintf("%d unexpected errors (see report)", len(report.UnexpectedErrors)), nil
	})

	if report.Passed() {
		return nil
	}
	return fmt.Errorf("replay: gate failed, seed=%d", cfg.Seed)
}
