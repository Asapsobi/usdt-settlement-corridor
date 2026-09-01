package replay

import "fmt"

type scenarioType int

const (
	scenarioHappyPath scenarioType = iota
	scenarioDuplicateIdempotency
	scenarioReorgBeforeDispatch
	scenarioReorgAfterSettlement
	scenarioScreeningHoldRelease
	scenarioScreeningHoldReject
	scenarioQuoteExpiry
	scenarioAmountVariance
	scenarioSweepBatch
	scenarioNonRetryableFailure
)

func (s scenarioType) String() string {
	switch s {
	case scenarioHappyPath:
		return "happy_path"
	case scenarioDuplicateIdempotency:
		return "duplicate_idempotency"
	case scenarioReorgBeforeDispatch:
		return "reorg_before_dispatch"
	case scenarioReorgAfterSettlement:
		return "reorg_after_settlement"
	case scenarioScreeningHoldRelease:
		return "screening_hold_release"
	case scenarioScreeningHoldReject:
		return "screening_hold_reject"
	case scenarioQuoteExpiry:
		return "quote_expiry"
	case scenarioAmountVariance:
		return "amount_variance"
	case scenarioSweepBatch:
		return "sweep_batch"
	case scenarioNonRetryableFailure:
		return "non_retryable_failure"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// orderPlan is one order's assignment before it runs: which scenario,
// and for sweep batches, which batch it belongs to and whether it's one
// of the 40 that settle or one of the 10 that fail.
type orderPlan struct {
	index        int
	scenario     scenarioType
	sweepBatchID int
	sweepFails   bool
}

// buildPlan expands Config.Scenarios into exactly NumOrders plans and
// shuffles them with the seeded PRNG, so scenario types are interleaved
// across the run (and across workers) rather than landing in solid
// blocks. Sweep batches get their internal 40-settle/10-fail split
// decided here, before shuffling, so each batch's composition is fixed
// regardless of shuffle order; only which slot each individual sweep
// order lands in (and thus which worker picks it up, and when) is
// shuffled.
func buildPlan(cfg Config) []orderPlan {
	plans := make([]orderPlan, 0, cfg.NumOrders)
	add := func(t scenarioType, n int) {
		for i := 0; i < n; i++ {
			plans = append(plans, orderPlan{scenario: t})
		}
	}
	add(scenarioHappyPath, cfg.Scenarios.HappyPath)
	add(scenarioDuplicateIdempotency, cfg.Scenarios.DuplicateIdempotency)
	add(scenarioReorgBeforeDispatch, cfg.Scenarios.ReorgBeforeDispatch)
	add(scenarioReorgAfterSettlement, cfg.Scenarios.ReorgAfterSettlement)
	add(scenarioScreeningHoldRelease, cfg.Scenarios.ScreeningHoldRelease)
	add(scenarioScreeningHoldReject, cfg.Scenarios.ScreeningHoldReject)
	add(scenarioQuoteExpiry, cfg.Scenarios.QuoteExpiry)
	add(scenarioAmountVariance, cfg.Scenarios.AmountVariance)
	add(scenarioNonRetryableFailure, cfg.Scenarios.NonRetryableFailure)

	numBatches := cfg.Scenarios.SweepBatch / 50
	for b := 0; b < numBatches; b++ {
		for i := 0; i < 40; i++ {
			plans = append(plans, orderPlan{scenario: scenarioSweepBatch, sweepBatchID: b, sweepFails: false})
		}
		for i := 0; i < 10; i++ {
			plans = append(plans, orderPlan{scenario: scenarioSweepBatch, sweepBatchID: b, sweepFails: true})
		}
	}

	rng := newRNG(cfg.Seed)
	rng.Shuffle(len(plans), func(i, j int) { plans[i], plans[j] = plans[j], plans[i] })
	for i := range plans {
		plans[i].index = i
	}
	return plans
}
