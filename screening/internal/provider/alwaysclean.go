package provider

import (
	"context"
	"time"
)

// AlwaysCleanProviderName is what every AlwaysCleanProvider verdict
// reports as its own ProviderName -- deliberately not a real vendor's
// name (Chainalysis/TRM/Elliptic, per component-map.md), so a cached
// row or an audit log entry produced by this provider can never be
// mistaken for a real vendor's own result.
const AlwaysCleanProviderName = "always_clean_placeholder"

// AlwaysCleanProvider is NOT a real screening decision. It exists only
// to unblock C1's own funded->screened transition (which cannot be
// bypassed without touching the ledger's transition table -- out of
// scope) for the MVP proof run described in
// docs/03-build/mvp-proof-run-plan.md, in place of a real AML vendor
// contract (Chainalysis/TRM/Elliptic) this project has deliberately not
// yet chosen -- see component-map.md's own C3 entry and this module's
// own cmd/screend doc comment.
//
// Every address, without exception, comes back clean: RiskScore 0,
// Flagged false. Wiring this into a production compliance path for real
// customer volume would be a compliance decision, not a simplification
// -- it must never be mistaken for one. Never use this provider unless
// the caller is a supervised, no-real-customer-traffic proof run.
type AlwaysCleanProvider struct{}

// Screen implements ScreeningProvider, unconditionally clean.
func (AlwaysCleanProvider) Screen(ctx context.Context, address string) (Verdict, error) {
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	return Verdict{
		RiskScore:    0,
		Flagged:      false,
		ReasonCodes:  nil,
		RawResponse:  []byte(`{"provider":"always_clean_placeholder","note":"not a real AML decision -- see AlwaysCleanProvider's own doc comment"}`),
		ProviderName: AlwaysCleanProviderName,
		CheckedAt:    time.Now().UTC(),
	}, nil
}
