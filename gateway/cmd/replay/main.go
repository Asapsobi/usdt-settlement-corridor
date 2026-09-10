// Command replay is the C6.9 replay harness: the deterministic
// simulator that drives C6's whole HTTP boundary (quote, order, status,
// webhooks, sandbox) through the build spec's SCENARIO MIX and checks
// every FINAL ASSERTION afterward. It is the C6 ship gate -- exit code
// 0 means every assertion held, non-zero means it didn't (see the
// printed report for which one, and the seed to reproduce it).
//
// This connects to THREE databases and TWO running services:
//   - GATEWAY_DATABASE_URL: this run's own database. Must be empty of
//     everything but the schema, the same reason every prior
//     component's own C*.9 gate needs a fresh database.
//   - LEDGER_DATABASE_URL: a raw connection to a REAL, ALREADY RUNNING
//     ledgerd's database, used only for the account-creation fixture
//     (there is no public HTTP endpoint for it).
//   - -ledger-url / -ledger-token (or LEDGER_BASE_URL / LEDGER_API_TOKEN)
//     and -watcher-url / -watcher-token (or WATCHER_BASE_URL /
//     WATCHER_API_TOKEN): where a real, already-running ledgerd and
//     watcherd are reachable over HTTP. Starting them is this command's
//     caller's job (an operator, or a CI script), not this binary's --
//     same reasoning every prior component's own cmd/replay gives for
//     not managing its own dependencies' lifecycles.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"gateway/internal/db"
	"gateway/internal/replay"
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
	watcherURL := flag.String("watcher-url", os.Getenv("WATCHER_BASE_URL"), "base URL of a real, already-running watcherd (or WATCHER_BASE_URL)")
	watcherToken := flag.String("watcher-token", os.Getenv("WATCHER_API_TOKEN"), "bearer token for that watcherd (or WATCHER_API_TOKEN)")
	flag.Parse()

	cfg.Seed = *seed
	cfg.LedgerBaseURL = *ledgerURL
	cfg.LedgerToken = *ledgerToken
	cfg.WatcherBaseURL = *watcherURL
	cfg.WatcherToken = *watcherToken

	ctx := context.Background()

	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return fmt.Errorf("replay: connecting to gateway database: %w", err)
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
	if report != nil && !report.Passed() {
		os.Exit(1)
	}
	return nil
}
