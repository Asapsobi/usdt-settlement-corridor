// Package money implements the minor-unit integer money primitive used
// everywhere in the ledger's write path. No floating point, ever.
package money

import (
	"errors"
	"fmt"
)

// Asset is a closed set of the currencies this ledger knows how to hold.
// Deliberately a defined type over string, not a bare string, so a typo
// cannot silently pass as a valid asset anywhere in the codebase.
type Asset string

const (
	USDT_BEP20 Asset = "USDT_BEP20"
	USDT_TRC20 Asset = "USDT_TRC20"
	TRX        Asset = "TRX"
	BNB        Asset = "BNB"
)

var ErrUnknownAsset = errors.New("money: unknown asset")

// Decimals returns the number of minor-unit decimal places for the asset,
// or ErrUnknownAsset if the asset is not one of the closed set above.
//
// BNB is truncated to 9 decimals (gwei-equivalent), not its native 18.
// BNB only ever appears in this system as a BSC gas expense line, so the
// sub-gwei precision native BNB carries is deliberately discarded rather
// than modeled.
func (a Asset) Decimals() (int, error) {
	switch a {
	case USDT_BEP20, USDT_TRC20, TRX:
		return 6, nil
	case BNB:
		return 9, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownAsset, string(a))
	}
}

// Valid reports whether a is one of the closed set of known assets.
func (a Asset) Valid() bool {
	_, err := a.Decimals()
	return err == nil
}
