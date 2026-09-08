// Package money is C5's minimal money primitive, mirroring every prior
// component's own internal/money exactly: signed minor units in an
// int64, never a float, 6 decimal places (USDT's own precision on both
// BEP20 and TRC20, and TRX's). Duplicated, not shared -- separate Go
// modules, no common internal package between them, the same convention
// this whole project uses everywhere else.
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

// Decimals is this service's minor-unit precision, matching every prior
// component's own "6 decimal places" convention for every amount
// crossing an HTTP boundary in this project.
const Decimals = 6

// Amount is a quantity of money in this package's own minor units:
// Amount(1) is one millionth of a unit, Amount(1_000000) is 1.000000.
type Amount int64

// ParseDecimal parses a plain decimal string ("2990.700000", "-0.5",
// "12") into an Amount, the inverse of Format.
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
// fractional digits.
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
