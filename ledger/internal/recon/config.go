package recon

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"ledger/internal/money"
)

// Config controls the reconciler: how often the self-check ticker runs,
// how much TRX drift to tolerate, and where position:corridor's alert
// ceilings sit.
type Config struct {
	// Interval between self-check ticks (TrialBalance, VerifyBalances,
	// corridor ceiling). Default 60s, per the build spec.
	Interval time.Duration

	// TRXToleranceUnits is the only nonzero tolerance this system allows,
	// and only for TRX -- to absorb energy/bandwidth rounding. Every USDT
	// asset's tolerance is hardcoded to zero in checkDrift, not
	// configurable: USDT is 6dp and exact, so any nonzero drift there is
	// real by construction, never a rounding artifact.
	TRXToleranceUnits int64

	// CorridorCeilings maps an asset to the maximum tolerated magnitude of
	// position:corridor for that asset before the self-check ticker logs
	// an alert (never a halt -- see runSelfChecks). An asset absent from
	// this map, or mapped to 0, has no ceiling check.
	CorridorCeilings map[money.Asset]int64
}

const defaultInterval = 60 * time.Second

// ConfigFromEnv reads Config from the environment:
//
//	RECON_INTERVAL_SECONDS               default 60
//	RECON_TRX_TOLERANCE_UNITS             default 0
//	RECON_CORRIDOR_CEILING_BEP20_UNITS    default unset (no check)
//	RECON_CORRIDOR_CEILING_TRC20_UNITS    default unset (no check)
//
// A nonzero TRX tolerance is logged at Warn on the way out, per the build
// spec: "it must be logged loudly at startup when set."
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Interval:         defaultInterval,
		CorridorCeilings: map[money.Asset]int64{},
	}

	if v := os.Getenv("RECON_INTERVAL_SECONDS"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs <= 0 {
			return Config{}, fmt.Errorf("recon: RECON_INTERVAL_SECONDS must be a positive integer, got %q", v)
		}
		cfg.Interval = time.Duration(secs) * time.Second
	}

	if v := os.Getenv("RECON_TRX_TOLERANCE_UNITS"); v != "" {
		units, err := strconv.ParseInt(v, 10, 64)
		if err != nil || units < 0 {
			return Config{}, fmt.Errorf("recon: RECON_TRX_TOLERANCE_UNITS must be a non-negative integer, got %q", v)
		}
		cfg.TRXToleranceUnits = units
		if units > 0 {
			slog.Warn("non-zero TRX reconciliation tolerance configured", "units", units)
		}
	}

	if v := os.Getenv("RECON_CORRIDOR_CEILING_BEP20_UNITS"); v != "" {
		units, err := strconv.ParseInt(v, 10, 64)
		if err != nil || units <= 0 {
			return Config{}, fmt.Errorf("recon: RECON_CORRIDOR_CEILING_BEP20_UNITS must be a positive integer, got %q", v)
		}
		cfg.CorridorCeilings[money.USDT_BEP20] = units
	}
	if v := os.Getenv("RECON_CORRIDOR_CEILING_TRC20_UNITS"); v != "" {
		units, err := strconv.ParseInt(v, 10, 64)
		if err != nil || units <= 0 {
			return Config{}, fmt.Errorf("recon: RECON_CORRIDOR_CEILING_TRC20_UNITS must be a positive integer, got %q", v)
		}
		cfg.CorridorCeilings[money.USDT_TRC20] = units
	}

	return cfg, nil
}

// toleranceFor returns the configured drift tolerance for asset. Every
// USDT asset is hardcoded to zero regardless of Config; only TRX ever
// uses TRXToleranceUnits.
func (c Config) toleranceFor(asset money.Asset) int64 {
	if asset == money.TRX {
		return c.TRXToleranceUnits
	}
	return 0
}
