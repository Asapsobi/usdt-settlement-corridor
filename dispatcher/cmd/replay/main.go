// Command replay is the C5.11 replay harness: the deterministic
// simulator that drives C5's whole surface (dispatch, broadcast,
// finality, batching, failure handling, freeze handling) through the
// build spec's own scenario mix and checks every FINAL ASSERTION
// afterward. It is C5's own ship gate -- exit code 0 means every
// assertion held, non-zero means it didn't (see the printed report for
// which one, and the seed to reproduce it).
//
// Unlike dispatchd, this connects to TWO databases and a running C1:
//   - DISPATCHER_DATABASE_URL: this run's own database. Must be empty
//     of everything but the schema -- this run's assertions are only
//     meaningful against a database this run had entirely to itself,
//     the same requirement every prior component's own ship gate has.
//   - LEDGER_DATABASE_URL: a raw connection to a REAL, ALREADY RUNNING
//     ledgerd's database, used only for fixture account creation and
//     journal assertions (there is no public HTTP endpoint for either).
//   - -ledger-url / -ledger-token (or LEDGER_BASE_URL / LEDGER_API_TOKEN):
//     where that running ledgerd is reachable over HTTP. Starting it is
//     this command's caller's job (an operator, or a CI script), not
//     this binary's -- same reasoning cmd/dispatchd gives for not
//     managing its own dependencies' lifecycles.
//
// Nothing here touches a real TRON node or a real S1: no S1 exists
// anywhere yet (see "Read this first" in the build spec), and this
// harness does not assume a reachable TRON node any more than C4's own
// did. Every other external boundary is an in-process fake.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"dispatcher/internal/db"
	"dispatcher/internal/replay"
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
	ledgerURL := flag.String("ledger-url", os.Getenv("LEDGER_BASE_URL"), "base URL of a real, already-running ledgerd (or LEDGER_BASE_URL)")
	ledgerToken := flag.String("ledger-token", os.Getenv("LEDGER_API_TOKEN"), "bearer token for that ledgerd (or LEDGER_API_TOKEN)")
	flag.Parse()

	cfg.Seed = *seed
	cfg.LedgerBaseURL = *ledgerURL
	cfg.LedgerToken = *ledgerToken
	if cfg.LedgerBaseURL == "" || cfg.LedgerToken == "" {
		return fmt.Errorf("replay: -ledger-url/-ledger-token (or LEDGER_BASE_URL/LEDGER_API_TOKEN) are required")
	}

	ctx := context.Background()

	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return fmt.Errorf("replay: connecting to dispatcher database: %w", err)
	}
	defer pool.Close()

	ledgerDBURL := os.Getenv("LEDGER_DATABASE_URL")
	if ledgerDBURL == "" {
		return fmt.Errorf("replay: LEDGER_DATABASE_URL is not set")
	}
	ledgerPool, err := pgxpool.New(ctx, ledgerDBURL)
	if err != nil {
		return fmt.Errorf("replay: connecting to ledger database: %w", err)
	}
	defer ledgerPool.Close()

	report, runErr := replay.Run(ctx, pool, ledgerPool, cfg)
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
