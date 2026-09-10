package replay

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gateway/internal/c1client"
	"gateway/internal/c2client"
	"gateway/internal/customers"
	"gateway/internal/db"
	"gateway/internal/httpapi"
	"gateway/internal/orders"
	"gateway/internal/quotes"
	"gateway/internal/ratelimit"
	"gateway/internal/reconcile"
	"gateway/internal/sandbox"
	"gateway/internal/webhooks"
)

type harness struct {
	ctx context.Context
	rng *rand.Rand

	gatewayPool *db.Pool
	ledgerPool  *pgxpool.Pool
	ledger      *ledgerFixture

	cfg    Config
	server *httpapi.Server
	router http.Handler

	customersStore *customers.Store
	quotesStore    *quotes.Store
	ordersStore    *orders.Store
	sandboxStore   *sandbox.Store
	webhooksStore  *webhooks.Store
	trigger        *webhooks.Trigger
	deliverer      *webhooks.Deliverer
	reconciler     *reconcile.Reconciler

	slotCounter int64

	prodExternalIDs    []string // tracked for FINAL ASSERTION 1
	sandboxExternalIDs []string // tracked for FINAL ASSERTION 3
}

// Run wires one harness against a real gateway database and a real
// ledger database (for account-fixture creation only -- see
// ledgerFixture's own doc comment), drives every scenario in the
// SCENARIO MIX, checks every FINAL ASSERTION, and returns the report.
func Run(ctx context.Context, gatewayPool *db.Pool, ledgerPool *pgxpool.Pool, cfg Config) (*Report, error) {
	h := &harness{
		ctx: ctx, rng: rand.New(rand.NewSource(cfg.Seed)), cfg: cfg,
		gatewayPool: gatewayPool, ledgerPool: ledgerPool,
		ledger: newLedgerFixture(cfg.LedgerBaseURL, cfg.LedgerToken),
	}

	h.customersStore = customers.NewStore(gatewayPool)
	h.quotesStore = quotes.NewStore(gatewayPool)
	h.ordersStore = orders.NewStore(gatewayPool)
	h.sandboxStore = sandbox.NewStore(gatewayPool)
	h.webhooksStore = webhooks.NewStore(gatewayPool)

	ledgerClient := c1client.New(cfg.LedgerBaseURL, cfg.LedgerToken)
	watcherClient := c2client.New(cfg.WatcherBaseURL, cfg.WatcherToken)

	h.server = &httpapi.Server{
		Pool: gatewayPool, Customers: h.customersStore, RateLimiter: ratelimit.New(),
		Quotes: h.quotesStore, Orders: h.ordersStore, Ledger: ledgerClient, Watcher: watcherClient, Sandbox: h.sandboxStore,
	}
	h.router = httpapi.NewRouter(h.server)
	h.reconciler = reconcile.NewReconciler(h.ordersStore, h.quotesStore, watcherClient, nil, reconcile.Config{GracePeriod: time.Millisecond})
	h.trigger = webhooks.NewTrigger(gatewayPool, ledgerClient, h.webhooksStore, h.customersStore)
	h.deliverer = webhooks.NewDeliverer(h.webhooksStore, h.customersStore, nil, nil)

	report := &Report{Seed: cfg.Seed}
	scenarios := []func() Result{
		h.scenarioCleanSettleWithWebhook,
		h.scenarioQuoteExpiresBeforeOrder,
		h.scenarioAddressPendingResolvedByReconcile,
		h.scenarioWebhookExhaustsThenBackstopAgrees,
		h.scenarioConcurrentOrderCreationRacesQuote,
		h.scenarioSystemHaltedRefusesQuoteAndOrder,
		h.scenarioAllFourSandboxTriggersIsolated,
	}
	for _, s := range scenarios {
		report.Scenarios = append(report.Scenarios, s())
	}
	report.Assertions = h.finalAssertions()

	return report, nil
}

func (h *harness) uniqueExternalID(prefix string) string {
	return fmt.Sprintf("replay-%s-%d-%d", prefix, time.Now().UnixNano(), h.rng.Int63())
}

func (h *harness) nextSlotID() int {
	h.slotCounter++
	return int(h.slotCounter)
}

// runGuarded recovers a panic inside fn and reports it as a failed
// Result rather than crashing the whole run -- FINAL ASSERTION 5's own
// "zero panics" would otherwise mean one scenario's own panic prevents
// every later scenario and every final assertion from ever running.
func runGuarded(name string, fn func() error) (result Result) {
	defer func() {
		if r := recover(); r != nil {
			result = fail(name, fmt.Errorf("panic: %v", r))
		}
	}()
	if err := fn(); err != nil {
		return fail(name, err)
	}
	return pass(name)
}
