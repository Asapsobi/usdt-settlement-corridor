// Package accounts owns the chart of accounts: creating, looking up, and
// listing accounts. It does not post journal entries or track balances --
// that is internal/journal (C1.2).
package accounts

import (
	"errors"
	"fmt"
	"time"

	"ledger/internal/money"
)

// Type is the closed set of account types. Each implies a fixed NormalSide
// via NormalSideFor -- callers never choose the sign independently.
type Type string

const (
	Asset     Type = "ASSET"
	Liability Type = "LIABILITY"
	Revenue   Type = "REVENUE"
	Expense   Type = "EXPENSE"
	Equity    Type = "EQUITY"
	Position  Type = "POSITION"
)

var ErrUnknownAccountType = errors.New("accounts: unknown account type")

// NormalSideFor returns the sign an account of type t carries by
// definition: +1 (debit-normal) for ASSET/EXPENSE, -1 (credit-normal) for
// LIABILITY/REVENUE/EQUITY, 0 for POSITION. This mirrors the CHECK
// constraint on the accounts table -- the two must never disagree.
func NormalSideFor(t Type) (int16, error) {
	switch t {
	case Asset, Expense:
		return 1, nil
	case Liability, Revenue, Equity:
		return -1, nil
	case Position:
		return 0, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrUnknownAccountType, string(t))
	}
}

// Valid reports whether t is one of the closed set of known account types.
func (t Type) Valid() bool {
	_, err := NormalSideFor(t)
	return err == nil
}

// Account is a row of the chart of accounts. It carries exactly one asset
// for its whole life -- USDT_BEP20 and USDT_TRC20 balances for what is
// conceptually "the same" wallet live in two different Account rows, never
// one row summed across assets.
type Account struct {
	ID         int64
	Code       string
	Type       Type
	Asset      money.Asset
	NormalSide int16
	IsActive   bool
	OpenedAt   time.Time
	Metadata   map[string]any
}
