// Command seed-sandbox-customer creates one sandbox customer (an
// sk_test_-keyed identity, C6.7's own fully separate namespace) --
// customers.Store.CreateSandbox exists and is tested, but nothing wraps
// it in an HTTP route or a CLI, the exact "no HTTP route for this
// operator action" gap dispatcher/cmd/seed-slot and s1/cmd/seed-slot-key
// already close for their own components. Needed to demo the pipeline
// (docs/03-build/ops-console-build-prompts.md's OC.12) without a real
// customer account or real money: a sandbox order never calls C1-C5 at
// all, per sandbox's own design.
//
// Usage:
//
//	GATEWAY_DATABASE_URL=... seed-sandbox-customer -name "Demo"
//
// The printed sk_test_ key is shown exactly once -- customers.Store
// stores only its hash, so there is no way to recover it later; run
// this again for a fresh customer if it's lost.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"gateway/internal/customers"
	"gateway/internal/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	name := flag.String("name", "", "a human-readable label for this sandbox customer (required)")
	flag.Parse()
	if *name == "" {
		return fmt.Errorf("seed-sandbox-customer: -name is required")
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

	store := customers.NewStore(pool)
	customer, apiKey, err := store.CreateSandbox(ctx, *name)
	if err != nil {
		return fmt.Errorf("seed-sandbox-customer: %w", err)
	}

	fmt.Printf("ok   sandbox customer %d (%s) created\n", customer.ID, customer.Name)
	fmt.Println("     API key (shown once, never recoverable -- store it now):", apiKey)
	return nil
}
