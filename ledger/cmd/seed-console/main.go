// Command seed-console creates the small, fixed set of accounts the HTML
// testing console (docs/console.html) needs to walk through an order's
// lifecycle over the HTTP API alone.
//
// There is no POST /accounts endpoint -- accounts come into existence
// only via internal/accounts.Create, called directly by whatever Go code
// needs a new one (internal/orders.Create does not create the deposit
// account it will eventually need; internal/replay's harness creates its
// own accounts directly against the pool, never through HTTP). A real
// deposit-watcher service (C2) would create a fresh
// asset:bsc:deposit:<order_id> account per order the same way. For a
// console meant to be clicked through by a person, reusing one fixed set
// of demo accounts across every order is simpler and just as valid --
// nothing in C1's own logic requires the deposit account's code to
// structurally match the order it funds.
//
// Safe to run repeatedly: accounts.Create is idempotent on code.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/accounts"
	"ledger/internal/money"
)

// consoleAccounts is the fixed set docs/console.html's JS hardcodes.
// Keep the two in sync if you ever change one.
var consoleAccounts = []struct {
	code  string
	typ   accounts.Type
	asset money.Asset
}{
	{"asset:bsc:deposit:console-demo", accounts.Asset, money.USDT_BEP20},
	{"liability:customer:console-demo:USDT_BEP20", accounts.Liability, money.USDT_BEP20},
	{"liability:customer:console-demo:USDT_TRC20", accounts.Liability, money.USDT_TRC20},
	{"asset:tron:slot:console-demo", accounts.Asset, money.USDT_TRC20},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	url := os.Getenv("LEDGER_DATABASE_URL")
	if url == "" {
		return fmt.Errorf("LEDGER_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer pool.Close()

	if err := accounts.Seed(ctx, pool); err != nil {
		return fmt.Errorf("seeding base chart of accounts: %w", err)
	}

	for _, a := range consoleAccounts {
		if _, err := accounts.Create(ctx, pool, a.code, a.typ, a.asset); err != nil {
			return fmt.Errorf("creating %q: %w", a.code, err)
		}
		fmt.Printf("ok   %s\n", a.code)
	}
	return nil
}
