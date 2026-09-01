package replay

import (
	"context"
	"fmt"
	"math/rand"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/orders"
)

// Run executes one full replay against pool: seeds the fixed chart of
// accounts, resets halt state to a known baseline, sets up the shared
// account pool, builds the scenario plan, runs it across cfg.Workers
// concurrent goroutines, and checks every FINAL ASSERTION. pool should
// point at a database with nothing in it but the schema -- see this
// package's doc comment on why VerifyBalances-style global checks (and
// assertion 5's exact journal_entries row count) require an otherwise
// empty database, not the shared test database other packages' test
// suites use.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config) (*Report, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	start := time.Now()
	runTag := fmt.Sprintf("%d", cfg.Seed)

	if err := accounts.Seed(ctx, pool); err != nil {
		return nil, fmt.Errorf("replay: seeding chart of accounts: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE system_state
		SET halted = false, halt_reason = NULL, halt_detail = NULL, halted_at = NULL, halted_by = NULL
		WHERE id = 1
	`); err != nil {
		return nil, fmt.Errorf("replay: resetting halt state: %w", err)
	}
	haltCache, err := halt.NewCache(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("replay: creating halt cache: %w", err)
	}
	orders.SetHaltCache(haltCache)

	shared, err := setupSharedAccounts(ctx, pool, runTag)
	if err != nil {
		return nil, fmt.Errorf("replay: setting up shared accounts: %w", err)
	}

	plans := buildPlan(cfg)
	results := make([]execResult, len(plans))

	jobs := make(chan int, len(plans))
	for i := range plans {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				// Each order gets its own RNG, deterministically derived
				// from the run seed and the order's plan index -- not a
				// shared *rand.Rand across workers, which isn't safe for
				// concurrent use, and not the worker's own single RNG
				// reused across many orders, which would make an
				// individual order's random choices depend on which
				// worker happened to pick it up and in what order,
				// breaking "same seed = byte-identical run" the moment
				// goroutine scheduling changes.
				rng := newRNG(cfg.Seed + int64(plans[idx].index) + 1)
				results[idx] = runOneOrder(ctx, pool, shared, runTag, plans[idx], rng)
			}
		}()
	}
	wg.Wait()

	duration := time.Since(start)

	report := buildReport(cfg, plans, results, duration)
	if err := runAssertions(ctx, pool, cfg, plans, results, report); err != nil {
		return report, err
	}
	return report, nil
}

// runOneOrder wraps runScenario with a recover, so a panic anywhere in
// one order's scenario becomes that order's execResult.err (visible in
// the report, and caught by FINAL ASSERTION 9's "zero panics") instead of
// crashing the whole harness and losing every other worker's progress.
func runOneOrder(ctx context.Context, pool *pgxpool.Pool, shared *sharedAccounts, runTag string, plan orderPlan, rng *rand.Rand) (res execResult) {
	defer func() {
		if r := recover(); r != nil {
			res = execResult{plan: plan, err: fmt.Errorf("replay: panic: %v\n%s", r, debug.Stack())}
		}
	}()
	return runScenario(ctx, pool, shared, runTag, plan, rng)
}
