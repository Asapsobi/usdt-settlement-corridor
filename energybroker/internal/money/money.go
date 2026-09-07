// Package money is C4's minimal money primitive. This service only ever
// handles one asset for cost-accounting purposes -- TRX, the energy
// delegation cost it attributes to C1 (§A: "Amounts are TRX, decimal
// strings, 6 decimal places") -- so unlike C1's own money.Amount
// (ledger/internal/money), there is no Asset field here: tracking a
// field that can only ever hold one value would be dead weight, not
// symmetry with C1. What this package keeps identical to C1's is the
// representation itself: signed minor units in an int64, never a float.
//
// This intentionally does not carry an on-chain-units converter the way
// depositwatcher's own money package does (FromOnChainUnits): C4 never
// parses a raw on-chain amount -- vendor price quotes are a per-unit sun
// rate (a plain float64, see internal/provider.Quote), not a token
// transfer amount -- so that conversion has no caller here and would be
// unused surface, not symmetry.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrInvalidDecimal is returned by ParseDecimal for anything that isn't a
// plain decimal string within this package's own precision.
var ErrInvalidDecimal = errors.New("money: invalid decimal string")

// Decimals is this service's minor-unit precision for TRX, matching §A's
// "6 decimal places" requirement for every amount crossing the HTTP
// boundary to C1.
const Decimals = 6

// Amount is a quantity of TRX in this package's own minor units (see
// Decimals): Amount(1) is one millionth of one TRX, Amount(1_000000) is
// 1.000000 TRX.
type Amount int64

// ParseDecimal parses a plain decimal string ("24.500000", "-0.5", "12")
// into an Amount, the inverse of Format. A string with more fractional
// digits than Decimals is rejected outright, matching every prior
// component's own money.ParseDecimal discipline exactly (mirrored, not
// shared: this is a different module).
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
// fractional digits (e.g. Amount(24_500000).Format() -> "24.500000"),
// the shape §A requires for every amount crossing the HTTP boundary to
// C1 -- never a JSON number, never routed through a float here either.
// a may be negative (an entry line credits one account with the negated
// amount of the other, per E4's own two-line shape); FormatInt handles
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
