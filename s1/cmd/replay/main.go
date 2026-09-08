// Command replay is the S1.6 replay harness: the deterministic simulator
// that drives S1's whole surface (auto-sign, the approval queue, KMS
// failure recovery) through the build spec's scenario list and checks
// every FINAL ASSERTION afterward. It is S1's own ship gate -- exit code
// 0 means every assertion held, non-zero means it didn't (see the
// printed report for which one, and the seed to reproduce it).
//
// S1_DATABASE_URL must point at a database with S1's own migrations
// already applied and otherwise empty -- this run's assertions are only
// meaningful against a database this run had entirely to itself, the
// same requirement every prior component's own ship gate has.
//
// This binary signs against an in-process FakeKMSClient, never a real
// KMS -- there is no real cloud KMS adapter in this module yet (see
// cmd/s1d's own doc comment).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"s1/internal/db"
	"s1/internal/replay"
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
	flag.Parse()
	cfg.Seed = *seed

	ctx := context.Background()

	dbCfg, err := db.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	pool, err := db.Open(ctx, dbCfg)
	if err != nil {
		return fmt.Errorf("replay: connecting to S1 database: %w", err)
	}
	defer pool.Close()

	report, runErr := replay.Run(ctx, pool, cfg)
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
