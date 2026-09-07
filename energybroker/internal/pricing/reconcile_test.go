package pricing

import (
	"testing"

	"energybroker/internal/money"
	"energybroker/internal/provider"
)

// delegationChargedAt builds a Delegation whose CostTRX corresponds to
// exactly priceSun per unit -- the same formula every real
// EnergyProvider implementation in this module uses (see
// chargedPriceSun's own doc comment): CostTRX minor units ~= priceSun *
// units, since the sun-to-TRX and TRX-to-minor-units conversions cancel
// exactly (1 TRX = 1,000,000 sun, and money.Decimals is 6).
func delegationChargedAt(priceSun float64, units int64) provider.Delegation {
	return provider.Delegation{
		ProviderName: provider.Tronsell,
		EnergyUnits:  units,
		CostTRX:      money.Amount(priceSun * float64(units)),
	}
}

func TestReconcileCharge_HonoredQuoteIsNotMismatched(t *testing.T) {
	d := delegationChargedAt(24.0, 65000)
	recon := ReconcileCharge(24.0, d, 25.7)
	if recon.Mismatched {
		t.Fatalf("Mismatched = true for a delegation charged exactly what was quoted")
	}
	if recon.OverCeiling {
		t.Fatalf("OverCeiling = true for a charge well under ceiling")
	}
}

func TestReconcileCharge_MismatchedButStillUnderCeilingIsFlaggedNotRefused(t *testing.T) {
	// Quoted 24, but the vendor actually charged as though the price
	// were 25.5 -- a real quote/charge divergence, but 25.5 is still
	// under the 25.7 ceiling.
	d := delegationChargedAt(25.5, 65000)
	recon := ReconcileCharge(24.0, d, 25.7)
	if !recon.Mismatched {
		t.Fatal("Mismatched = false, want true -- the vendor did not honor its own quote")
	}
	if recon.OverCeiling {
		t.Fatal("OverCeiling = true, want false -- 25.5 is still under the 25.7 ceiling")
	}
	if recon.ChargedPriceSun != 25.5 {
		t.Fatalf("ChargedPriceSun = %v, want 25.5", recon.ChargedPriceSun)
	}
}

func TestReconcileCharge_ChargedAboveCeilingIsFlaggedAndOverCeiling(t *testing.T) {
	// Quoted 24 (well under ceiling), but the vendor actually charged as
	// though the price were 30 -- above the 25.7 ceiling. This is the
	// scenario invariant 3 exists to prevent from being silently
	// accepted, even though the vendor's own call already "succeeded".
	d := delegationChargedAt(30.0, 65000)
	recon := ReconcileCharge(24.0, d, 25.7)
	if !recon.Mismatched {
		t.Fatal("Mismatched = false, want true")
	}
	if !recon.OverCeiling {
		t.Fatal("OverCeiling = false, want true -- 30 exceeds the 25.7 ceiling")
	}
}

func TestReconcileCharge_HonoredQuoteAtExactlyCeilingIsNotOverCeiling(t *testing.T) {
	// Boundary case, mirroring TestUnderCeiling's own "exactly at
	// ceiling is under it" case: a charge exactly equal to ceiling must
	// not be flagged OverCeiling.
	d := delegationChargedAt(25.7, 65000)
	recon := ReconcileCharge(25.7, d, 25.7)
	if recon.OverCeiling {
		t.Fatal("OverCeiling = true for a charge exactly at ceiling, want false")
	}
}

func TestReconcileCharge_ClearsTheCeilingByOneUnitOfPrecision(t *testing.T) {
	// C4.7's own named boundary scenario: a spike that clears the
	// ceiling by the smallest meaningful margin must still be caught,
	// proving no float-comparison slop hides a bypass.
	const ceiling = 25.7
	const justOver = 25.700001
	d := delegationChargedAt(justOver, 1_000_000) // large unit count keeps the minor-units rounding well below the margin being tested
	recon := ReconcileCharge(justOver, d, ceiling)
	if !recon.OverCeiling {
		t.Fatalf("OverCeiling = false for a charge of %v against ceiling %v, want true", justOver, ceiling)
	}
}
