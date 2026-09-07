package pricing

import (
	"math"

	"energybroker/internal/provider"
)

// chargeEqualityEpsilonSun is how close two per-unit sun prices must be
// to count as "the same price," absorbing float64/decimal-string
// round-trip noise (a Delegation's own CostTRX passed through
// money.ParseDecimal's 6-decimal-place rounding) without producing a
// false Mismatched on a real, honored quote.
const chargeEqualityEpsilonSun = 1e-6

// ChargeReconciliation is C4.7's own adversarial-scenario finding:
// comparing what a provider quoted before a Delegate call against what
// it actually charged, and independently confirming the actual charge
// still respects ceiling -- a vendor claiming success, and a vendor
// honoring its own quote, are two different facts, neither of which
// invariant 3 lets slide just because the on-chain delegation already
// landed.
type ChargeReconciliation struct {
	ProviderName    string
	QuotedPriceSun  float64
	ChargedPriceSun float64
	Ceiling         float64
	// Mismatched is true the instant the actual charge differs from the
	// quote by more than rounding noise -- worth auditing even when
	// still under ceiling, since it means the vendor did not honor its
	// own Quote response.
	Mismatched bool
	// OverCeiling is true when the ACTUAL charged price -- never the
	// stale quote -- exceeds ceiling. This is the invariant-3 violation:
	// a caller must refuse to treat the delegation as normal inventory
	// or a normal confirmed reservation when this is true, no matter how
	// the vendor's own API call itself responded.
	OverCeiling bool
}

// ReconcileCharge derives d's own actual per-unit price from
// CostTRX/EnergyUnits (the inverse of how every EnergyProvider
// implementation in this module computes CostTRX from a per-unit sun
// price -- see provider.MockProvider.Delegate's own doc comment) and
// compares it against quotedPriceSun (whatever Quote returned
// immediately before the Delegate call that produced d) and ceiling.
// Pure and stateless, mirroring UnderCeiling's own placement here:
// internal/buffer's Replenish and internal/reservations' slow path both
// need this exact same reconciliation, never two implementations that
// could drift.
func ReconcileCharge(quotedPriceSun float64, d provider.Delegation, ceiling float64) ChargeReconciliation {
	charged := chargedPriceSun(d)
	return ChargeReconciliation{
		ProviderName:    d.ProviderName,
		QuotedPriceSun:  quotedPriceSun,
		ChargedPriceSun: charged,
		Ceiling:         ceiling,
		Mismatched:      math.Abs(charged-quotedPriceSun) > chargeEqualityEpsilonSun,
		OverCeiling:     !UnderCeiling(provider.Quote{PricePerUnitSun: charged}, ceiling),
	}
}

// chargedPriceSun recovers d's own actual per-unit sun price from its
// CostTRX (minor units) and EnergyUnits. Every EnergyProvider
// implementation in this module computes CostTRX as approximately
// pricePerUnitSun * energyUnits minor units (the sun-to-TRX and
// TRX-to-minor-units conversions cancel exactly -- 1 TRX = 1,000,000
// sun, and money.Decimals is 6), so this is the exact inverse, not an
// approximation invented for this function alone.
func chargedPriceSun(d provider.Delegation) float64 {
	if d.EnergyUnits == 0 {
		return 0
	}
	return float64(d.CostTRX) / float64(d.EnergyUnits)
}
