// Package pricing is C6's own foundation, built from scratch as this
// component's job -- no shared pricing library exists anywhere else in
// this repository (component-map.md's "calls a pricing library" line
// describes something that was never built; see this project's own
// c6-api-gateway-build-prompts.md, "Read this first"). Pure functions,
// no I/O: the number a customer is quoted here is the exact number C1's
// journal ends up booking, on every single order, so this package is
// tested in isolation before anything else in C6 is allowed to depend
// on it.
package pricing

import (
	"errors"
	"fmt"

	"gateway/internal/money"
)

// Tier mirrors C1's own closed set (ledger/docs/openapi.yaml's
// PostOrderRequest.tier enum) exactly -- these three string values are
// what actually gets sent to C1, never re-encoded.
type Tier string

const (
	Direct   Tier = "DIRECT"
	Standard Tier = "STANDARD"
	Sweep    Tier = "SWEEP"
)

// networkFeeUnits is fixed per tier -- product-operations-architecture.md
// decision 5 / findings-and-recommendation.md's own tier network fees.
var networkFeeUnits = map[Tier]money.Amount{
	Direct:   2_540000, // $2.54
	Standard: 1_800000, // $1.80
	Sweep:    1_000000, // $1.00
}

// feeFloor is the minimum fee, regardless of tier or amount --
// findings-and-recommendation.md: "max(flat floor, declining bp tier)...
// Floor never below $1.80."
const feeFloor = money.Amount(1_800000) // $1.80

// bracket is one tier boundary in the declining bp schedule. upperBound
// is exclusive (amountIn strictly below it stays in this bracket); the
// last bracket's upperBound is unused (0, meaning "no upper bound").
// rateTenthsOfBp is the bracket's own rate in units of 0.1 basis point
// (so 3.5bp is representable as an exact integer, 35, with no float
// anywhere in this file) -- feeUnits = amountIn * rateTenthsOfBp /
// 100_000 (100_000 = 10_000 [1bp = 1/10_000] * 10 [tenths]).
type bracket struct {
	upperBound     money.Amount
	rateTenthsOfBp int64
}

// brackets is findings-and-recommendation.md's own declining bp
// schedule: "40 bp under $1k · 25 bp to $10k · 12 bp to $100k · 6 bp to
// $1M · 3.5 bp above."
//
// Applied as a FLAT rate selected by which single bracket amountIn
// falls into, not marginally across bracket boundaries -- this
// deliberately does NOT follow c6-api-gateway-build-prompts.md's own
// prose instruction ("apply marginally across the bracket boundaries,
// not as a single flat rate"), because that prose contradicts its own
// golden test. A marginal calculation for $3,000 (STANDARD) gives
// $4.00 (first $1k at 40bp) + $5.00 (next $2k at 25bp) = $9.00; the
// spec's own required result is $7.50. $7.50 is exactly $3,000 * 25bp
// applied flat -- and findings-and-recommendation.md itself confirms
// this reading directly: "the 25 bp ($7.50) this report proposes
// charging in the same volume tier." Built against the verified worked
// example and its own authoritative source, not the contradicting
// prose -- the same "the code/doc that's actually been checked wins"
// discipline this whole project applies everywhere else.
var brackets = []bracket{
	{upperBound: 1_000_000000, rateTenthsOfBp: 400},    // under $1k: 40bp
	{upperBound: 10_000_000000, rateTenthsOfBp: 250},   // $1k-$10k: 25bp
	{upperBound: 100_000_000000, rateTenthsOfBp: 120},  // $10k-$100k: 12bp
	{upperBound: 1_000_000_000000, rateTenthsOfBp: 60}, // $100k-$1M: 6bp
	{upperBound: 0, rateTenthsOfBp: 35},                // $1M and above: 3.5bp
}

// ErrUnknownTier means tier is not one of Direct, Standard, Sweep.
var ErrUnknownTier = errors.New("pricing: unknown tier")

// ErrNonPositiveAmount means amountIn was zero or negative -- never a
// valid quote input.
var ErrNonPositiveAmount = errors.New("pricing: amount_in must be positive")

// ErrAmountTooSmall means amountIn, after this tier's own fee and
// network fee, would leave a non-positive amountOut -- never a valid
// payout, rejected here rather than returned as a negative or zero
// quote.
var ErrAmountTooSmall = errors.New("pricing: amount_in is too small to cover this tier's fee and network_fee")

// Quote is one priced request -- everything C1's own PostOrderRequest
// needs for its own amount_out/fee_units/network_fee_units fields,
// computed once and never recomputed at order-creation time (C6.2's own
// "quote-then-order, never quote-inside-order" contract).
type Quote struct {
	Tier            Tier
	AmountIn        money.Amount
	FeeUnits        money.Amount
	NetworkFeeUnits money.Amount
	AmountOut       money.Amount
}

// ComputeQuote prices amountIn at tier, per §B of
// c6-api-gateway-build-prompts.md: feeUnits = max(floor, bracket rate
// applied flat to amountIn); amountOut = amountIn - feeUnits -
// networkFeeUnits.
func ComputeQuote(tier Tier, amountIn money.Amount) (Quote, error) {
	netFee, ok := networkFeeUnits[tier]
	if !ok {
		return Quote{}, fmt.Errorf("%w: %q", ErrUnknownTier, tier)
	}
	if amountIn <= 0 {
		return Quote{}, ErrNonPositiveAmount
	}

	rate := rateFor(amountIn)
	fee := money.Amount(int64(amountIn) * rate / 100_000)
	if fee < feeFloor {
		fee = feeFloor
	}

	amountOut := amountIn - fee - netFee
	if amountOut <= 0 {
		return Quote{}, ErrAmountTooSmall
	}

	return Quote{Tier: tier, AmountIn: amountIn, FeeUnits: fee, NetworkFeeUnits: netFee, AmountOut: amountOut}, nil
}

// rateFor returns amountIn's own bracket rate (tenths of a basis
// point) -- the LAST bracket whose upperBound is 0 always matches
// (amountIn >= every finite upperBound before it), so this never falls
// through without a result.
func rateFor(amountIn money.Amount) int64 {
	for _, b := range brackets {
		if b.upperBound == 0 || amountIn < b.upperBound {
			return b.rateTenthsOfBp
		}
	}
	panic("pricing: unreachable -- brackets' own last entry has upperBound 0")
}
