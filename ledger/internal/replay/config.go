// Package replay is C1.9: the deterministic simulator that hammers the
// ledger with the scenarios that actually happen, and the acceptance gate
// for C1 as a whole. It imports every other internal package as a client
// would -- it is not itself imported by anything else.
package replay

import (
	"fmt"
	"math/rand"
	"time"
)

// Config controls one run. The zero Config is not usable; use
// DefaultConfig and override fields as needed.
type Config struct {
	Seed      int64 // same seed = byte-identical run
	NumOrders int   // default 10,000
	Workers   int   // default 32

	// Counts, not probabilities: the spec's percentages are exact target
	// counts of a fixed NumOrders, not a per-order coin flip that would
	// let repeated runs of the same seed still drift in mix. Counts must
	// sum to NumOrders; NewScenarioCounts enforces this.
	Scenarios ScenarioCounts
}

// ScenarioCounts is the exact number of orders each scenario gets, per
// the build spec's default mix (percentages of 10,000):
//
//	72%    HappyPath
//	 6%    DuplicateIdempotency (half concurrent, half sequential)
//	 4%    ReorgBeforeDispatch
//	 0.5%  ReorgAfterSettlement  -- must halt every time
//	 5%    ScreeningHoldRelease
//	 2%    ScreeningHoldReject
//	 4%    QuoteExpiry           -- includes a deposit landing after expiry
//	 3%    AmountVariance        -- overpay / underpay / dust
//	 2%    SweepBatch            -- groups of 50, 40 settle / 10 fail
//	 1.5%  NonRetryableFailure   -- held, conversion reversed
type ScenarioCounts struct {
	HappyPath            int
	DuplicateIdempotency int
	ReorgBeforeDispatch  int
	ReorgAfterSettlement int
	ScreeningHoldRelease int
	ScreeningHoldReject  int
	QuoteExpiry          int
	AmountVariance       int
	SweepBatch           int
	NonRetryableFailure  int
}

// Total returns the sum of every scenario count.
func (s ScenarioCounts) Total() int {
	return s.HappyPath + s.DuplicateIdempotency + s.ReorgBeforeDispatch + s.ReorgAfterSettlement +
		s.ScreeningHoldRelease + s.ScreeningHoldReject + s.QuoteExpiry + s.AmountVariance +
		s.SweepBatch + s.NonRetryableFailure
}

// DefaultScenarioCounts computes the spec's default mix for n orders.
// SweepBatch is rounded down to a multiple of 50 (batches of 50
// recipients are the unit that scenario models); any remainder from that
// rounding, and from every other percentage's rounding, is folded into
// HappyPath so the total always comes out to exactly n.
func DefaultScenarioCounts(n int) ScenarioCounts {
	sweep := (int(float64(n)*0.02) / 50) * 50

	c := ScenarioCounts{
		DuplicateIdempotency: int(float64(n) * 0.06),
		ReorgBeforeDispatch:  int(float64(n) * 0.04),
		ReorgAfterSettlement: int(float64(n) * 0.005),
		ScreeningHoldRelease: int(float64(n) * 0.05),
		ScreeningHoldReject:  int(float64(n) * 0.02),
		QuoteExpiry:          int(float64(n) * 0.04),
		AmountVariance:       int(float64(n) * 0.03),
		SweepBatch:           sweep,
		NonRetryableFailure:  int(float64(n) * 0.015),
	}
	rest := c.DuplicateIdempotency + c.ReorgBeforeDispatch + c.ReorgAfterSettlement +
		c.ScreeningHoldRelease + c.ScreeningHoldReject + c.QuoteExpiry + c.AmountVariance +
		c.SweepBatch + c.NonRetryableFailure
	c.HappyPath = n - rest
	return c
}

// DefaultConfig returns the spec's default: 10,000 orders, 32 workers,
// the default scenario mix, and a seed derived from the current time (so
// two runs without an explicit seed still differ, per "print the seed on
// failure" -- there has to be a fresh one to print).
func DefaultConfig() Config {
	n := 10000
	return Config{
		Seed:      time.Now().UnixNano(),
		NumOrders: n,
		Workers:   32,
		Scenarios: DefaultScenarioCounts(n),
	}
}

func (c Config) validate() error {
	if c.NumOrders <= 0 {
		return fmt.Errorf("replay: NumOrders must be positive, got %d", c.NumOrders)
	}
	if c.Workers <= 0 {
		return fmt.Errorf("replay: Workers must be positive, got %d", c.Workers)
	}
	if c.Scenarios.SweepBatch%50 != 0 {
		return fmt.Errorf("replay: SweepBatch count must be a multiple of 50 (batches of 50 recipients), got %d", c.Scenarios.SweepBatch)
	}
	if total := c.Scenarios.Total(); total != c.NumOrders {
		return fmt.Errorf("replay: scenario counts sum to %d, want NumOrders %d", total, c.NumOrders)
	}
	return nil
}

// newRNG returns a fresh, seeded PRNG. Every call with the same seed
// value produces the same sequence -- callers derive independent RNGs
// per order (see plan.go) rather than sharing one across goroutines,
// since a shared *rand.Rand is not safe for concurrent use and taking a
// lock around it would serialize all 32 workers on every random call.
func newRNG(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}
