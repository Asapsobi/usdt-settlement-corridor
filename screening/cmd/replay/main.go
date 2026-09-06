// Command replay is the C3.9 replay harness: the deterministic
// simulator that drives C3's whole pipeline (discovery, screening
// against a fake vendor, the hold queue, re-screen) through the build
// spec's scenario mix and checks every FINAL ASSERTION afterward. It is
// the C3 ship gate -- exit code 0 means every assertion held, non-zero
// means it didn't (see the printed report for which one, and the seed
// to reproduce it).
//
// Unlike screend, this connects to TWO databases and a running C1:
//   - SCREENING_DATABASE_URL: this run's own database. Must be empty of
//     everything but the schema, the same reason C1.9's and C2.10's own
//     gates need a fresh database -- this run's assertions are only
//     meaningful against a database this run had entirely to itself.
//   - LEDGER_DATABASE_URL: a raw connection to a REAL, ALREADY RUNNING
//     ledgerd's database, used only for the account-creation fixture
//     (there is no public HTTP endpoint for it -- see
//     internal/replay/ledgerfixture.go's own doc comment).
//   - -ledger-url / -ledger-token (or LEDGER_BASE_URL / LEDGER_API_TOKEN):
//     where that running ledgerd is reachable over HTTP. Starting it is
//     this command's caller's job (an operator, or a CI script), not
//     this binary's -- same reasoning cmd/screend gives for not managing
//     its own dependencies' lifecycles.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"screening/internal/db"
	"screening/internal/replay"
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

	ctx := context.Background()

	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return fmt.Errorf("replay: connecting to screening database: %w", err)
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
