package routing

import (
	"context"
	"fmt"
	"log/slog"

	"energybroker/internal/pricing"
)

// ErrChargedAboveCeiling is what a caller (internal/buffer's Replenish,
// internal/reservations' slow path) checks for after calling
// RecordVendorOvercharge with a ChargeReconciliation whose OverCeiling
// is true: the signal to refuse the delegation as normal inventory or a
// normal confirmed reservation, per invariant 3 -- the on-chain
// delegation already happened and cannot be undone, but C4's own
// bookkeeping must never treat an over-ceiling charge as a routine,
// accepted outcome just because the vendor's API call itself succeeded.
var ErrChargedAboveCeiling = fmt.Errorf("%w: charged price exceeds ceiling", ErrInvalidCeiling)

// RecordVendorOvercharge persists recon as a new vendor_overcharge_events
// row -- C4.7's own "the delegation is flagged" acceptance criterion --
// whenever recon shows any real discrepancy (Mismatched or OverCeiling).
// A no-op (never called by convention, but harmless if it is) when
// neither is true. Always logs loudly: Warn for a mismatch that still
// stayed under ceiling (the vendor didn't honor its own quote, worth an
// operator's attention but not urgent), Error for OverCeiling (a real
// invariant-3 exposure -- the vendor charged more than this system is
// configured to ever pay, regardless of what caused it).
//
// Returns ErrChargedAboveCeiling when recon.OverCeiling is true -- AFTER
// the row is successfully recorded, never instead of it -- so a caller
// can record-and-decide in one call: `if errors.Is(err,
// ErrChargedAboveCeiling) { refuse this delegation }`. This never
// blocks, reverses, or refunds the underlying delegation itself -- the
// energy was already delegated on-chain by the time this is called.
// Recovering the overcharge is a dispute to take up WITH the vendor (see
// docs/runbook-energy-fallback.md's own posture on manual, out-of-band
// vendor problems), not something this function can undo. What the
// CALLER refuses, on ErrChargedAboveCeiling, is treating the delegation
// as ordinary going forward: marking buffer capacity AVAILABLE, or
// confirming a reservation, on the strength of a charge invariant 3
// never allowed in the first place.
func (r *Router) RecordVendorOvercharge(ctx context.Context, recon pricing.ChargeReconciliation, delegationID string, energyUnits int64, orderID *int64) error {
	if !recon.Mismatched && !recon.OverCeiling {
		return nil
	}

	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO vendor_overcharge_events
			(provider_name, delegation_id, order_id, energy_units, quoted_price_sun, charged_price_sun, ceiling_sun, over_ceiling)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id
	`, recon.ProviderName, delegationID, orderID, energyUnits, recon.QuotedPriceSun, recon.ChargedPriceSun, recon.Ceiling, recon.OverCeiling).Scan(&id)
	if err != nil {
		return fmt.Errorf("routing: recording vendor overcharge for delegation %s: %w", delegationID, err)
	}

	if recon.OverCeiling {
		slog.Error("routing: VENDOR CHARGED ABOVE CEILING -- invariant 3 exposure, see docs/runbook-energy-fallback.md",
			"event_id", id, "provider", recon.ProviderName, "delegation_id", delegationID,
			"quoted_price_sun", recon.QuotedPriceSun, "charged_price_sun", recon.ChargedPriceSun, "ceiling_sun", recon.Ceiling)
		return fmt.Errorf("%w: provider %s delegation %s charged %v sun/unit against a %v ceiling",
			ErrChargedAboveCeiling, recon.ProviderName, delegationID, recon.ChargedPriceSun, recon.Ceiling)
	}
	slog.Warn("routing: vendor charged a different price than quoted (still under ceiling)",
		"event_id", id, "provider", recon.ProviderName, "delegation_id", delegationID,
		"quoted_price_sun", recon.QuotedPriceSun, "charged_price_sun", recon.ChargedPriceSun)
	return nil
}
