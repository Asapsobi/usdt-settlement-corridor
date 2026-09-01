package replay

import (
	"fmt"
	"io"
	"sort"
	"time"
)

type unexpectedError struct {
	OrderIndex int
	Scenario   string
	ExternalID string
	Err        error
}

type assertionResult struct {
	Name   string
	Passed bool
	Detail string
}

// Report is the one-page summary the DELIVERABLE section asks for:
// scenario counts, halt events, and final trial balance per asset, plus
// enough detail on any failure to actually debug it -- including the
// seed, per "print the seed on failure."
type Report struct {
	Config           Config
	Duration         time.Duration
	ScenarioCounts   map[string]int
	ProvokedErrors   int
	UnexpectedErrors []unexpectedError
	AssertionResults []assertionResult
	TrialBalance     map[string]string // formatted, filled in by the TrialBalance assertion
	HaltSetCount     int
	HaltClearCount   int
}

// Passed reports whether every assertion held and there were zero
// unexpected errors -- the single boolean `make gate` exits non-zero on.
func (r *Report) Passed() bool {
	if len(r.UnexpectedErrors) > 0 {
		return false
	}
	for _, a := range r.AssertionResults {
		if !a.Passed {
			return false
		}
	}
	return true
}

func buildReport(cfg Config, plans []orderPlan, results []execResult, duration time.Duration) *Report {
	r := &Report{
		Config:         cfg,
		Duration:       duration,
		ScenarioCounts: make(map[string]int),
	}
	for _, p := range plans {
		r.ScenarioCounts[p.scenario.String()]++
	}
	for i, res := range results {
		r.ProvokedErrors += res.provoked
		if res.err != nil {
			r.UnexpectedErrors = append(r.UnexpectedErrors, unexpectedError{
				OrderIndex: i, Scenario: plans[i].scenario.String(), ExternalID: res.externalID, Err: res.err,
			})
		}
	}
	sort.Slice(r.UnexpectedErrors, func(i, j int) bool { return r.UnexpectedErrors[i].OrderIndex < r.UnexpectedErrors[j].OrderIndex })
	return r
}

// Print writes the one-page report DELIVERABLE calls for.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintln(w, "=== C1.9 replay harness report ===")
	fmt.Fprintf(w, "seed=%d orders=%d workers=%d duration=%s\n", r.Config.Seed, r.Config.NumOrders, r.Config.Workers, r.Duration)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "-- scenario counts --")
	names := []string{
		scenarioHappyPath.String(), scenarioDuplicateIdempotency.String(), scenarioReorgBeforeDispatch.String(),
		scenarioReorgAfterSettlement.String(), scenarioScreeningHoldRelease.String(), scenarioScreeningHoldReject.String(),
		scenarioQuoteExpiry.String(), scenarioAmountVariance.String(), scenarioSweepBatch.String(), scenarioNonRetryableFailure.String(),
	}
	for _, n := range names {
		fmt.Fprintf(w, "  %-24s %d\n", n, r.ScenarioCounts[n])
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "-- halt events --")
	fmt.Fprintf(w, "  set:   %d\n", r.HaltSetCount)
	fmt.Fprintf(w, "  clear: %d\n", r.HaltClearCount)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "-- trial balance --")
	for asset, bal := range r.TrialBalance {
		fmt.Fprintf(w, "  %-12s %s\n", asset, bal)
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "-- provoked errors (expected, not failures): %d --\n", r.ProvokedErrors)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "-- assertions --")
	for _, a := range r.AssertionResults {
		status := "PASS"
		if !a.Passed {
			status = "FAIL"
		}
		fmt.Fprintf(w, "  [%s] %s", status, a.Name)
		if a.Detail != "" {
			fmt.Fprintf(w, ": %s", a.Detail)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)

	if len(r.UnexpectedErrors) > 0 {
		fmt.Fprintf(w, "-- %d unexpected error(s) (first 20 shown) --\n", len(r.UnexpectedErrors))
		for i, e := range r.UnexpectedErrors {
			if i >= 20 {
				break
			}
			fmt.Fprintf(w, "  order[%d] scenario=%s external_id=%s: %v\n", e.OrderIndex, e.Scenario, e.ExternalID, e.Err)
		}
		fmt.Fprintln(w)
	}

	if r.Passed() {
		fmt.Fprintln(w, "RESULT: PASS")
	} else {
		fmt.Fprintf(w, "RESULT: FAIL -- seed was %d\n", r.Config.Seed)
	}
}
