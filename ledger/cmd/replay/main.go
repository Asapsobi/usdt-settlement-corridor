// Command replay is the C1.9 replay harness: the deterministic simulator
// that hammers the ledger with realistic scenario mix and checks every
// FINAL ASSERTION afterward. It is the C1 ship gate ("make gate") -- exit
// code 0 means every assertion held, non-zero means it didn't (see the
// printed report for which one, and the seed to reproduce it).
//
// It connects to LEDGER_DATABASE_URL like every other command in this
// module, but -- unlike ledgerd -- that database must be empty of
// everything except the schema: several assertions (exact journal_entries
// count, whole-database VerifyBalances/TrialBalance) are only meaningful
// against a database this run had entirely to itself. "make gate"
// provisions a fresh throwaway database and points LEDGER_DATABASE_URL at
// it for exactly this reason; running this binary directly against
// ledger_dev or ledger_test will produce spurious assertion failures from
// whatever data already lives there, not a real bug.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"ledger/internal/db"
	"ledger/internal/replay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg := replay.DefaultConfig()

	seed := flag.Int64("seed", cfg.Seed, "PRNG seed; same seed replays byte-identically (default: time-based)")
	numOrders := flag.Int("orders", cfg.NumOrders, "total number of orders to simulate")
	workers := flag.Int("workers", cfg.Workers, "number of concurrent worker goroutines")
	flag.Parse()

	cfg.Seed = *seed
	if *numOrders != cfg.NumOrders {
		cfg.NumOrders = *numOrders
		cfg.Scenarios = replay.DefaultScenarioCounts(*numOrders)
	}
	cfg.Workers = *workers

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

	report, runErr := replay.Run(ctx, pool.Pool, cfg)
	if report != nil {
		report.Print(os.Stdout)
	}
	if runErr != nil {
		if report == nil {
			return runErr
		}
		os.Exit(1)
	}
	return nil
}
