// Package provider defines the vendor-agnostic energy-provider
// interface C4 calls, and holds every implementation of it. No other
// package in this module may import a specific vendor's SDK -- see
// dependency_test.go, which enforces that mechanically, the same way
// C3's own internal/provider enforces its own vendor-SDK boundary.
package provider

import (
	"context"
	"errors"
	"time"

	"energybroker/internal/money"
)

// Quote is one provider's current price for TRON energy, at one point
// in time. It is never mutated after construction -- a re-check
// produces a new Quote, never an update to an old one, the same
// discipline C3's own provider.Verdict uses.
type Quote struct {
	ProviderName      string
	PricePerUnitSun   float64
	QuotedAt          time.Time
	MaxUnitsAvailable int64 // some providers cap a single call's size
}

// Delegation is one energy delegation this provider made (or attempted).
// ConfirmedAt stays nil until independently verified on-chain -- see
// EnergyProvider's own doc comment on why Verify is deliberately not
// part of this interface. A caller must never treat a non-nil
// Delegation, on its own, as proof the delegation actually landed.
//
// ExpiresAt is what the vendor actually granted, which a caller must use
// instead of independently recomputing an expiry from whatever duration
// it originally asked Delegate for: not every real vendor honors an
// arbitrary requested duration (CatFee only ever grants exactly 1 hour;
// Netts's /order1h is the same; only Tronsell accepts a genuinely
// custom leaseDurationSecond) -- see each vendor's own client file. A
// caller that assumed "the duration I asked for is the duration I got"
// would silently keep treating capacity as available long after it
// actually expired on-chain.
type Delegation struct {
	ID            string
	ProviderName  string
	TargetAddress string
	EnergyUnits   int64
	CostTRX       money.Amount
	RequestedAt   time.Time
	ExpiresAt     time.Time
	ConfirmedAt   *time.Time
}

// EnergyProvider is the one thing every vendor integration (real or
// fake) must implement. Quote and Delegate must both respect ctx
// cancellation/deadline -- a vendor call that ignores it and blocks past
// a caller's timeout is a bug in the implementation, not something
// callers should have to guard against separately.
//
// Verify is deliberately NOT part of this interface: on-chain
// verification is provider-agnostic (it's a TRON resource query against
// the target address, not something any vendor's own API reports back)
// and belongs in internal/buffer instead. A vendor claiming success in
// its own Delegate response and the delegation actually landing on-chain
// are different facts -- invariant 1 in this component's own build spec
// -- and this interface must not make it easy to conflate them by
// offering a same-package "verify" call that just re-asks the vendor.
//
// This interface used to also carry Redelegate, C4.4's first resolution
// of that chunk's own explicit open question ("buffer capacity is
// delegated to a broker-controlled staging address, not to any specific
// payout slot, so it must be retargeted at reservation time" -- design
// (b) in that chunk's own doc comment). Building real HTTP clients
// against Tronsell's, Netts's, and CatFee's actual current APIs (not
// their marketing copy) settled the question those docs left open:
// none of the three expose anything that retargets an existing
// delegation to a new address -- only "buy a new, fixed-receiver,
// vendor-priced order." A same-signature "Redelegate" against any of
// them could therefore only ever mean a second live purchase, which
// would put vendor latency back in every fast-path reservation's
// critical path (exactly what internal/buffer's staging design exists
// to avoid -- see "Read this third" in the build doc) and silently pay
// for the same energy twice. Rather than ship an interface method no
// real vendor actually supports, internal/buffer and
// internal/reservations were reworked to design (a) from that same
// open question instead: one buffer per known payout slot address,
// pre-acquired already pointed at its final destination, so a
// reservation is a pure database claim against an already-verified row
// -- never a second vendor call. See internal/buffer's own
// Config.SlotAddresses and internal/reservations' confirmFastPath.
type EnergyProvider interface {
	Quote(ctx context.Context) (Quote, error)
	Delegate(ctx context.Context, target string, units int64, duration time.Duration) (Delegation, error)
}

// Provider name constants -- the four named slots this component's own
// routing config wires by exact string (§B's own
// `routing_weights: tronsell: 0.60` etc.). Tronsell, Netts, and Catfee
// are authenticated HTTP integrations against a funded account balance;
// JustLendManual is the one exception -- see ErrManualFallbackRequired's
// own doc comment -- and is NEVER wired to a real vendor client by this
// document's own default (see "Read this second" in
// docs/03-build/c4-energy-broker-build-prompts.md).
const (
	Tronsell       = "tronsell"
	Netts          = "netts"
	Catfee         = "catfee"
	JustLendManual = "justlend_manual"
)

// ErrManualFallbackRequired is NoOpProvider's only possible result: the
// placeholder for JustLendDAO's own automated path, which this document
// deliberately does not build (JustLendDAO is a smart contract, not a
// vendor API -- acquiring energy from it means signing and broadcasting
// a TRON transaction, a capability nothing else in this component's
// scope otherwise requires). A caller reaching this slot must fall back
// to the manual runbook (C4.6), never retry expecting a different
// outcome.
var ErrManualFallbackRequired = errors.New(
	"provider: justlend_manual requires the manual runbook -- automated on-chain signing is not built here (see \"Read this second\" in docs/03-build/c4-energy-broker-build-prompts.md)")

// NoOpProvider is the justlend_manual slot's only implementation at
// MVP. Every call fails with ErrManualFallbackRequired -- reachable
// through the same EnergyProvider interface as a real vendor, but never
// silently succeeding, so a caller that forgets to special-case this
// slot fails loudly instead of believing energy was actually delegated.
type NoOpProvider struct{}

// Quote implements EnergyProvider.
func (NoOpProvider) Quote(ctx context.Context) (Quote, error) {
	return Quote{}, ErrManualFallbackRequired
}

// Delegate implements EnergyProvider.
func (NoOpProvider) Delegate(ctx context.Context, target string, units int64, duration time.Duration) (Delegation, error) {
	return Delegation{}, ErrManualFallbackRequired
}
