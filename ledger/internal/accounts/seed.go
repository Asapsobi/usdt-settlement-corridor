package accounts

import (
	"context"
	"fmt"

	"ledger/internal/money"
)

// seedAccount is one row of the fixed part of the chart of accounts: every
// code from docs/03-build/c1-ledger-build-prompts.md §A that has no
// variable segment (no <order_id>, <slot_id>, <venue>, or customer id).
type seedAccount struct {
	code  string
	typ   Type
	asset money.Asset
}

// seedChart is the fixed chart of accounts. Two entries deviate from a
// literal reading of §A, both forced by the same rule that table states
// explicitly: "An account has exactly ONE asset for its whole life."
// §A marks position:corridor's asset as "both" and equity:opening's as
// "all", and §B's worked example debits and credits "position:corridor" in
// two different assets within one entry -- which only typechecks if that
// is shorthand for two distinct accounts, not literally one row holding
// two assets. So each is seeded once per asset it needs, with the asset
// appended as a trailing code segment (position:corridor:USDT_BEP20, etc),
// consistent with the colon-delimited, extensible code scheme the rest of
// the chart already uses. Every other seeded code here is copied verbatim
// from §A.
var seedChart = []seedAccount{
	{"asset:tron:energy_wallet", Asset, money.TRX},

	{"position:corridor:USDT_BEP20", Position, money.USDT_BEP20},
	{"position:corridor:USDT_TRC20", Position, money.USDT_TRC20},

	{"revenue:fee", Revenue, money.USDT_TRC20},
	{"revenue:network_fee", Revenue, money.USDT_TRC20},

	{"expense:energy", Expense, money.TRX},
	{"expense:bandwidth", Expense, money.TRX},
	{"expense:gas", Expense, money.BNB},
	{"expense:rebalance", Expense, money.USDT_TRC20},
	{"expense:loss:reorg", Expense, money.USDT_TRC20},
	{"expense:loss:freeze", Expense, money.USDT_TRC20},

	{"equity:opening:USDT_BEP20", Equity, money.USDT_BEP20},
	{"equity:opening:USDT_TRC20", Equity, money.USDT_TRC20},
	{"equity:opening:TRX", Equity, money.TRX},
	{"equity:opening:BNB", Equity, money.BNB},
}

// Seed creates every account in seedChart. It is safe to call repeatedly:
// Create is idempotent on code, so a second Seed is a no-op.
func Seed(ctx context.Context, q Queryer) error {
	for _, s := range seedChart {
		if _, err := Create(ctx, q, s.code, s.typ, s.asset); err != nil {
			return fmt.Errorf("accounts: seed %q: %w", s.code, err)
		}
	}
	return nil
}
