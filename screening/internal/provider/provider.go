// Package provider defines the vendor-agnostic screening interface C3
// calls, and holds every implementation of it. No other package in this
// module may import a specific vendor's SDK -- see no_vendor_sdk_test.go,
// which enforces that mechanically, the same way depositwatcher's
// internal/addresses enforces "never a private key" for its own package.
package provider

import (
	"context"
	"encoding/json"
	"time"
)

// Verdict is one provider's answer for one address, at one point in time.
// It is never mutated after construction -- a re-check produces a new
// Verdict, never an update to an old one (see internal/cache, C3.1).
type Verdict struct {
	RiskScore    float64 // 0.0-1.0, provider-normalized
	Flagged      bool
	ReasonCodes  []string
	RawResponse  json.RawMessage // stored verbatim for audit, never parsed downstream
	ProviderName string
	CheckedAt    time.Time
}

// ScreeningProvider is the one thing every vendor integration (real or
// fake) must implement. Screen must respect ctx cancellation/deadline --
// a vendor call that ignores it and blocks past a caller's timeout is a
// bug in the implementation, not something callers should have to guard
// against separately.
type ScreeningProvider interface {
	Screen(ctx context.Context, address string) (Verdict, error)
}

// SenderAddressLookup resolves an order's deposit sender address.
//
// This is a stub for a real gap: C1's orders table has no sender_address
// column yet, and C2's report-to-C1 call doesn't send one -- see
// docs/03-build/c3-screening-build-prompts.md's "Read this first" section.
// C3.0-C3.2 use FakeSenderAddressLookup, configured per external_id. The
// real implementation (querying C1's GET /v1/orders/{external_id} once it
// exposes sender_address) is C3.3's job, once C1's addition ships -- not
// this chunk's, and not worked around by giving this package chain access
// of its own, which would silently recreate C2's job inside C3.
type SenderAddressLookup interface {
	GetSenderAddress(ctx context.Context, externalID string) (string, error)
}
