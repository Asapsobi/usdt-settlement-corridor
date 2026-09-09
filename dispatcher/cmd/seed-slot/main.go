// Command seed-slot registers one payout slot -- the one, real,
// operational gap the MVP proof run's own docker-compose.yml surfaced:
// dispatcher's own HTTP boundary (internal/httpapi) has no POST /v1/slots
// route. A slot only ever comes into existence via slots.Store.Create,
// called directly by Go code today (internal/replay's own harness does
// exactly this for its fixtures) -- there was no standalone tool for a
// real deployment to do the same thing for a real slot. This mirrors
// ledger/cmd/seed-console's own precedent: a small, typed Go tool
// calling the real internal package directly, run once by an operator,
// not a manual SQL statement and not a new production HTTP endpoint.
//
// Usage:
//
//	DISPATCHER_DATABASE_URL=... DISPATCHER_LEDGER_BASE_URL=... DISPATCHER_LEDGER_API_TOKEN=... \
//	  seed-slot -id 1 -address <real TRON address S1 derived for this slot>
//
// The TRON address must already exist -- query it from S1's own
// GET /v1/slots/{id}/address (S1's own slot numbering is independent of
// this command's -id, which is C5's own slot identity, per
// component-map.md: "C5's own slot-identity registry" is explicitly
// separate from S1's key custody).
//
// Safe to run more than once for the same -id: slots.Store.Create itself
// is NOT idempotent (a second INSERT for an existing id errors,
// slots.ErrDuplicateSlot), so this command catches that specific error
// and reports the slot as it already exists instead of failing --
// ledgerclient.EnsureAccount genuinely is idempotent on its own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"dispatcher/internal/db"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/slots"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	id := flag.Int("id", 0, "this slot's own numeric id in C5's slot-identity registry (required, > 0)")
	address := flag.String("address", "", "the real TRON address S1 derived for this slot (required)")
	flag.Parse()

	if *id <= 0 {
		return fmt.Errorf("seed-slot: -id is required and must be > 0")
	}
	if *address == "" {
		return fmt.Errorf("seed-slot: -address is required")
	}

	ctx := context.Background()

	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	ledgerBaseURL := os.Getenv("DISPATCHER_LEDGER_BASE_URL")
	ledgerToken := os.Getenv("DISPATCHER_LEDGER_API_TOKEN")
	if ledgerBaseURL == "" || ledgerToken == "" {
		return fmt.Errorf("seed-slot: DISPATCHER_LEDGER_BASE_URL and DISPATCHER_LEDGER_API_TOKEN are both required")
	}
	ledgerClient := ledgerclient.New(ledgerBaseURL, ledgerToken)

	slotStore := slots.NewStore(pool)
	slot, err := slotStore.Create(ctx, *id, *address, time.Now().UTC())
	if err != nil {
		if !errors.Is(err, slots.ErrDuplicateSlot) {
			return fmt.Errorf("seed-slot: registering slot %d: %w", *id, err)
		}
		slot, err = slotStore.Get(ctx, *id)
		if err != nil {
			return fmt.Errorf("seed-slot: slot %d already registered, but fetching it failed: %w", *id, err)
		}
		if slot.TronAddress != *address {
			return fmt.Errorf("seed-slot: slot %d is already registered with a DIFFERENT address (%s) than requested (%s) -- refusing to proceed",
				*id, slot.TronAddress, *address)
		}
		fmt.Printf("ok   slot %d already registered, tron_address=%s, status=%s\n", slot.ID, slot.TronAddress, slot.Status)
	} else {
		fmt.Printf("ok   slot %d registered, tron_address=%s, status=%s\n", slot.ID, slot.TronAddress, slot.Status)
	}

	code := fmt.Sprintf("asset:tron:slot:%d", *id)
	if err := ledgerClient.EnsureAccount(ctx, code, ledgerclient.AccountAsset, "USDT_TRC20", fmt.Sprintf("seed-slot:%d", *id)); err != nil {
		return fmt.Errorf("seed-slot: ensuring %s exists in C1: %w", code, err)
	}
	fmt.Printf("ok   %s exists in C1\n", code)

	return nil
}
