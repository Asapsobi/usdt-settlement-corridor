package replay

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultScenarioCounts_SumsToNAndSweepIsAMultipleOf50(t *testing.T) {
	for _, n := range []int{1, 50, 100, 777, 2500, 10000, 123456} {
		c := DefaultScenarioCounts(n)
		assert.Equalf(t, n, c.Total(), "n=%d: counts sum to %d, want %d", n, c.Total(), n)
		assert.Zerof(t, c.SweepBatch%50, "n=%d: SweepBatch=%d is not a multiple of 50", n, c.SweepBatch)
		assert.GreaterOrEqualf(t, c.HappyPath, 0, "n=%d: HappyPath went negative absorbing the rounding remainder", n)
	}
}

func TestConfig_Validate(t *testing.T) {
	valid := func() Config {
		return Config{Seed: 1, NumOrders: 100, Workers: 4, Scenarios: DefaultScenarioCounts(100)}
	}

	t.Run("valid config passes", func(t *testing.T) {
		require.NoError(t, valid().validate())
	})
	t.Run("non-positive NumOrders", func(t *testing.T) {
		c := valid()
		c.NumOrders = 0
		assert.Error(t, c.validate())
	})
	t.Run("non-positive Workers", func(t *testing.T) {
		c := valid()
		c.Workers = 0
		assert.Error(t, c.validate())
	})
	t.Run("SweepBatch not a multiple of 50", func(t *testing.T) {
		c := valid()
		c.Scenarios.SweepBatch = 51
		assert.Error(t, c.validate())
	})
	t.Run("scenario counts don't sum to NumOrders", func(t *testing.T) {
		c := valid()
		c.Scenarios.HappyPath++
		assert.Error(t, c.validate())
	})
}

func TestBuildPlan_MatchesConfiguredCounts(t *testing.T) {
	cfg := Config{Seed: 42, NumOrders: 1000, Workers: 8, Scenarios: DefaultScenarioCounts(1000)}
	plans := buildPlan(cfg)
	require.Len(t, plans, cfg.NumOrders)

	got := ScenarioCounts{}
	sweepSettle, sweepFail := 0, 0
	for _, p := range plans {
		switch p.scenario {
		case scenarioHappyPath:
			got.HappyPath++
		case scenarioDuplicateIdempotency:
			got.DuplicateIdempotency++
		case scenarioReorgBeforeDispatch:
			got.ReorgBeforeDispatch++
		case scenarioReorgAfterSettlement:
			got.ReorgAfterSettlement++
		case scenarioScreeningHoldRelease:
			got.ScreeningHoldRelease++
		case scenarioScreeningHoldReject:
			got.ScreeningHoldReject++
		case scenarioQuoteExpiry:
			got.QuoteExpiry++
		case scenarioAmountVariance:
			got.AmountVariance++
		case scenarioNonRetryableFailure:
			got.NonRetryableFailure++
		case scenarioSweepBatch:
			got.SweepBatch++
			if p.sweepFails {
				sweepFail++
			} else {
				sweepSettle++
			}
		default:
			t.Fatalf("unexpected scenario type %v", p.scenario)
		}
	}
	assert.Equal(t, cfg.Scenarios, got)
	// Every sweep batch is exactly 40 settle / 10 fail, per the build spec.
	assert.Equal(t, cfg.Scenarios.SweepBatch/50*40, sweepSettle)
	assert.Equal(t, cfg.Scenarios.SweepBatch/50*10, sweepFail)
}

func TestBuildPlan_IndexIsFinalPositionAfterShuffle(t *testing.T) {
	cfg := Config{Seed: 7, NumOrders: 500, Workers: 4, Scenarios: DefaultScenarioCounts(500)}
	plans := buildPlan(cfg)
	for i, p := range plans {
		require.Equal(t, i, p.index, "plan at position %d has index %d", i, p.index)
	}
}

// TestBuildPlan_SameSeedIsDeterministic is the property the whole harness
// leans on for "print the seed on failure" to be worth anything: a
// failing run must be reproducible from its seed alone.
func TestBuildPlan_SameSeedIsDeterministic(t *testing.T) {
	cfg := Config{Seed: 999, NumOrders: 500, Workers: 4, Scenarios: DefaultScenarioCounts(500)}
	a := buildPlan(cfg)
	b := buildPlan(cfg)
	require.Equal(t, len(a), len(b))
	for i := range a {
		assert.Equalf(t, a[i], b[i], "position %d differs between two runs of the same seed", i)
	}
}

func TestBuildPlan_DifferentSeedsUsuallyDiffer(t *testing.T) {
	cfgA := Config{Seed: 1, NumOrders: 500, Workers: 4, Scenarios: DefaultScenarioCounts(500)}
	cfgB := cfgA
	cfgB.Seed = 2
	a, b := buildPlan(cfgA), buildPlan(cfgB)

	differs := false
	for i := range a {
		if a[i].scenario != b[i].scenario {
			differs = true
			break
		}
	}
	assert.True(t, differs, "two different seeds produced the identical scenario sequence")
}
