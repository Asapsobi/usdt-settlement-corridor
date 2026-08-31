package recon

import (
	"testing"
	"time"

	"ledger/internal/money"
)

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Interval != defaultInterval {
		t.Errorf("Interval = %v, want default %v", cfg.Interval, defaultInterval)
	}
	if cfg.TRXToleranceUnits != 0 {
		t.Errorf("TRXToleranceUnits = %d, want 0", cfg.TRXToleranceUnits)
	}
	if len(cfg.CorridorCeilings) != 0 {
		t.Errorf("CorridorCeilings = %v, want empty", cfg.CorridorCeilings)
	}
}

func TestConfigFromEnvOverrides(t *testing.T) {
	t.Setenv("RECON_INTERVAL_SECONDS", "30")
	t.Setenv("RECON_TRX_TOLERANCE_UNITS", "5")
	t.Setenv("RECON_CORRIDOR_CEILING_BEP20_UNITS", "1000")
	t.Setenv("RECON_CORRIDOR_CEILING_TRC20_UNITS", "2000")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want 30s", cfg.Interval)
	}
	if cfg.TRXToleranceUnits != 5 {
		t.Errorf("TRXToleranceUnits = %d, want 5", cfg.TRXToleranceUnits)
	}
	if cfg.CorridorCeilings[money.USDT_BEP20] != 1000 {
		t.Errorf("CorridorCeilings[BEP20] = %d, want 1000", cfg.CorridorCeilings[money.USDT_BEP20])
	}
	if cfg.CorridorCeilings[money.USDT_TRC20] != 2000 {
		t.Errorf("CorridorCeilings[TRC20] = %d, want 2000", cfg.CorridorCeilings[money.USDT_TRC20])
	}
}

func TestConfigFromEnvRejectsInvalidInterval(t *testing.T) {
	t.Setenv("RECON_INTERVAL_SECONDS", "not-a-number")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected an error for a non-numeric interval")
	}

	t.Setenv("RECON_INTERVAL_SECONDS", "0")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected an error for a zero interval")
	}

	t.Setenv("RECON_INTERVAL_SECONDS", "-5")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected an error for a negative interval")
	}
}

func TestConfigFromEnvRejectsInvalidTRXTolerance(t *testing.T) {
	t.Setenv("RECON_TRX_TOLERANCE_UNITS", "-1")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected an error for a negative TRX tolerance")
	}
}

func TestToleranceForOnlyAppliesToTRX(t *testing.T) {
	cfg := Config{TRXToleranceUnits: 7}
	if got := cfg.toleranceFor(money.TRX); got != 7 {
		t.Errorf("toleranceFor(TRX) = %d, want 7", got)
	}
	for _, asset := range []money.Asset{money.USDT_BEP20, money.USDT_TRC20, money.BNB} {
		if got := cfg.toleranceFor(asset); got != 0 {
			t.Errorf("toleranceFor(%s) = %d, want 0 (every non-TRX asset is hardcoded to zero tolerance)", asset, got)
		}
	}
}
