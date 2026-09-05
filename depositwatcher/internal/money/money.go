// Package money is depositwatcher's minimal money primitive. This service
// only ever handles one asset -- USDT_BEP20, watched deposits into it, never
// anything else -- so unlike C1's own money.Amount (ledger/internal/money),
// there is no Asset field here: tracking a field that can only ever hold one
// value would be dead weight, not symmetry with C1. What this package does
// keep identical to C1's is the representation itself: signed minor units in
// an int64, never a float, per the C2 build spec's own instruction ("C2's
// own money type should therefore be the same int64 minor-units
// representation C1 uses internally").
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// ErrInvalidDecimal is returned by ParseDecimal for anything that isn't a
// plain decimal string within this package's own precision.
var ErrInvalidDecimal = errors.New("money: invalid decimal string")

// Decimals is this service's minor-unit precision for USDT_BEP20, matching
// C1's money.Asset.Decimals() for that asset exactly. It is NOT the token
// contract's own on-chain decimals -- see chain.ParseTransferLog, which is
// where that distinction actually matters and is verified.
const Decimals = 6

// Amount is a quantity of USDT_BEP20 in the ledger's minor units (see
// Decimals): Amount(1) is one millionth of one USDT, Amount(1_000000) is
// 1.000000 USDT.
type Amount int64

// FromOnChainUnits converts raw -- an amount already known to be expressed
// in onChainDecimals fractional digits -- into this package's Decimals-place
// minor-unit convention. raw must be non-negative (an ERC20 Transfer's
// amount is a uint256; a caller passing anything else has a bug upstream)
// and onChainDecimals must be at least Decimals.
//
// Fractional precision below Decimals is truncated, never rounded, and
// truncating all the way down to zero is not an error: a real, nonzero
// on-chain transfer legitimately can be too small to represent at the
// ledger's precision, and that is exactly the case classification's
// ZeroValue/Dust outcomes exist to name rather than silently discard as a
// bug. What IS an error is a value that, even after truncation, does not
// fit in an int64 -- returned rather than silently wrapped, the same
// discipline C1's own money package uses for overflow.
func FromOnChainUnits(raw *big.Int, onChainDecimals int) (Amount, error) {
	if raw == nil {
		return 0, fmt.Errorf("money: nil raw amount")
	}
	if raw.Sign() < 0 {
		return 0, fmt.Errorf("money: negative on-chain amount %s", raw)
	}
	if onChainDecimals < Decimals {
		return 0, fmt.Errorf("money: onChainDecimals %d is finer than this package's own %d-decimal floor",
			onChainDecimals, Decimals)
	}

	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(onChainDecimals-Decimals)), nil)
	scaled := new(big.Int).Quo(raw, divisor)
	if !scaled.IsInt64() {
		return 0, fmt.Errorf("money: on-chain amount %s overflows int64 minor units", raw)
	}
	return Amount(scaled.Int64()), nil
}

// ParseDecimal parses a plain decimal string ("3000.000000", "-0.5", "12")
// into an Amount, the inverse of Format. This exists for the candidate
// pipeline (C2.10): the ONE place this service ever needs the quoted
// amount_in it did not derive itself is C1's own order record (GET
// /v1/orders/{external_id}), which returns it as exactly this shape --
// never rounds, never a float at any point. A string with more
// fractional digits than Decimals is rejected outright, matching C1's
// own money.ParseDecimal's discipline exactly (mirrored, not shared:
// this is a different module).
func ParseDecimal(s string) (Amount, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty string", ErrInvalidDecimal)
	}

	neg := false
	rest := s
	switch rest[0] {
	case '-':
		neg = true
		rest = rest[1:]
	case '+':
		rest = rest[1:]
	}
	if rest == "" {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}

	intPart, fracPart, hasDot := strings.Cut(rest, ".")
	if hasDot && strings.Contains(fracPart, ".") {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if intPart == "" && fracPart == "" {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if intPart == "" {
		intPart = "0"
	}
	if !isDigits(intPart) || (fracPart != "" && !isDigits(fracPart)) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidDecimal, s)
	}
	if len(fracPart) > Decimals {
		return 0, fmt.Errorf("%w: %q has more than %d decimal places", ErrInvalidDecimal, s, Decimals)
	}

	digits := intPart + fracPart + strings.Repeat("0", Decimals-len(fracPart))
	units, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %v", ErrInvalidDecimal, s, err)
	}
	if neg {
		units = -units
	}
	return Amount(units), nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Format renders a as a plain decimal string with exactly Decimals
// fractional digits (e.g. Amount(3000_000000).Format() -> "3000.000000"),
// the shape C1's own money.Format uses and §A requires for every amount
// crossing the HTTP boundary -- never a JSON number, never routed
// through a float here either. a may be negative (a Line request credits
// one account with the negated amount of the other); FormatInt handles
// the math.MinInt64 edge case correctly, unlike negating a first would.
func (a Amount) Format() string {
	neg := a < 0
	digits := strconv.FormatInt(int64(a), 10)
	if neg {
		digits = digits[1:]
	}
	for len(digits) < Decimals+1 {
		digits = "0" + digits
	}
	intPart, fracPart := digits[:len(digits)-Decimals], digits[len(digits)-Decimals:]
	out := intPart + "." + fracPart
	if neg {
		out = "-" + out
	}
	return out
}
