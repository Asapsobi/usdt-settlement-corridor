package replay

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"screening/internal/cache"
	"screening/internal/db"
	"screening/internal/discovery"
	"screening/internal/ledgerclient"
	"screening/internal/pipeline"
	"screening/internal/provider"
	"screening/internal/rescreen"
	"screening/internal/verdict"
)

// harness bundles everything a scenario function needs: a real
// ledgerFixture and *ledgerclient.Client wired against a real, running
// C1 (this chunk's whole point -- see C3.9's own build spec: "not a
// mock of your own assumptions about C1"), a seeded MockProvider (the
// one vendor-side fake C3.0 shipped specifically so this harness never
// needs a real vendor), and the bookkeeping the final assertions check
// afterward.
type harness struct {
	ctx  context.Context
	rng  *rand.Rand
	seed int64

	pool       *db.Pool      // screening's own database
	ledgerPool *pgxpool.Pool // raw connection to C1's database, fixture account creation only
	ledger     *ledgerFixture
	client     *countingLedgerClient
	mock       *provider.MockProvider

	pipelineCfg pipeline.Config
	rescreenCfg rescreen.Config

	mu                 sync.Mutex
	seq                int64
	rescreenCallDeltas []int
	trackedOrderIDs    []int64
}

// trackedOrderIDsSnapshot returns every order id createFundedOrder ever
// produced this run -- FINAL ASSERTION 1's own data: every one of these
// must reach a DONE screening_queue row.
func (h *harness) trackedOrderIDsSnapshot() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int64, len(h.trackedOrderIDs))
	copy(out, h.trackedOrderIDs)
	return out
}

// rescreenCallDeltaSnapshot returns a copy of every recorded
// before/after transitionCallCount delta from every runRescreenTick
// call this run made -- FINAL ASSERTION 4's own data, checked
// independently of whichever scenario happened to call it (see
// scenarios.go's runRescreenTick doc comment).
func (h *harness) rescreenCallDeltaSnapshot() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int, len(h.rescreenCallDeltas))
	copy(out, h.rescreenCallDeltas)
	return out
}

func newHarness(ctx context.Context, cfg Config, pool *db.Pool, ledgerPool *pgxpool.Pool) *harness {
	client := &countingLedgerClient{Client: ledgerclient.New(cfg.LedgerBaseURL, cfg.LedgerToken)}
	mock := provider.NewMockProvider(cfg.Seed)

	return &harness{
		ctx: ctx, rng: rand.New(rand.NewSource(cfg.Seed)), seed: cfg.Seed,
		pool: pool, ledgerPool: ledgerPool,
		ledger: newLedgerFixture(cfg.LedgerBaseURL, cfg.LedgerToken),
		client: client, mock: mock,
		pipelineCfg: pipeline.Config{
			ProviderName: "mock",
			Thresholds:   verdict.DefaultThresholds,
			TTL:          cache.DefaultTTLConfig,
			OutagePolicy: provider.FailClosed,
		},
		rescreenCfg: rescreen.Config{
			ProviderName: "mock",
			Thresholds:   verdict.DefaultThresholds,
			TTL:          cache.DefaultTTLConfig,
		},
	}
}

// countingLedgerClient wraps a real *ledgerclient.Client, promoting
// every read (GetOrder, GetSenderAddress, ListOrdersByState,
// PollFundedOrders) for free -- satisfying discovery.FundedOrderPoller,
// provider.SenderAddressLookup, and rescreen.OrderLister without this
// harness re-implementing any of them as fakes, which would defeat the
// point of gating C3 on a REAL C1 -- while counting every call that
// actually transitions an order on C1 (ReportVerdict, ReleaseHold,
// RejectHold). FINAL ASSERTION 4 needs that count, specifically:
// snapshot it immediately before and after one rescreen.RunTick call and
// assert it did not move, since rescreen must never itself transition an
// order -- see internal/rescreen's own package doc comment.
type countingLedgerClient struct {
	*ledgerclient.Client

	mu              sync.Mutex
	transitionCalls int
}

func (c *countingLedgerClient) ReportVerdict(ctx context.Context, externalID string, decision verdict.Decision) error {
	c.mu.Lock()
	c.transitionCalls++
	c.mu.Unlock()
	return c.Client.ReportVerdict(ctx, externalID, decision)
}

func (c *countingLedgerClient) ReleaseHold(ctx context.Context, externalID string, orderID, holdID int64) error {
	c.mu.Lock()
	c.transitionCalls++
	c.mu.Unlock()
	return c.Client.ReleaseHold(ctx, externalID, orderID, holdID)
}

func (c *countingLedgerClient) RejectHold(ctx context.Context, externalID string, orderID, holdID int64, entry map[string]any) error {
	c.mu.Lock()
	c.transitionCalls++
	c.mu.Unlock()
	return c.Client.RejectHold(ctx, externalID, orderID, holdID, entry)
}

func (c *countingLedgerClient) transitionCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.transitionCalls
}

func (h *harness) nextExternalID(prefix string) string {
	h.mu.Lock()
	h.seq++
	seq := h.seq
	h.mu.Unlock()
	return fmt.Sprintf("replay-%s-%d-%d", prefix, time.Now().UnixNano(), seq)
}

// mockScore reproduces MockProvider's own unexported deterministicScore
// formula (documented on that method: FNV-1a of "seed:address", mapped
// to [0,1)) so scenarios can search for a fixture address that lands
// naturally in a specific risk band -- clean, ambiguous, or
// naturally-high -- without forcing anything and without spending a real
// Screen call (and thus a ScreenCallCount) on addresses this run never
// actually uses.
func mockScore(seed int64, address string) float64 {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%s", seed, address)
	sum := h.Sum64()
	return float64(sum) / float64(^uint64(0))
}

// findAddressInBand searches deterministically (seeded off h.rng, so a
// run is still fully reproducible from its own seed) for an address
// whose natural mock score falls in [lo, hi) -- e.g. the ambiguous band
// between verdict.DefaultThresholds' two cutoffs, or strictly below
// PassBelow for a guaranteed-clean address. Bounded: this system's own
// score distribution is uniform-ish over a 64-bit hash, so a band as
// narrow as the default ambiguous one (0.5-0.85) is found within a few
// hundred attempts essentially always; a search that somehow never
// converges is a real bug (or a pathologically narrow band), not
// something to paper over with an infinite loop.
func (h *harness) findAddressInBand(prefix string, lo, hi float64) (string, error) {
	for i := 0; i < 100_000; i++ {
		candidate := fmt.Sprintf("0x%s-%d", prefix, h.rng.Int63())
		score := mockScore(h.seed, candidate)
		if score >= lo && score < hi {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("replay: no address found with mock score in [%f, %f) after 100000 attempts", lo, hi)
}

// fundedEntry is one order this harness pushed all the way from created
// through funded through screening_queue, ready for the pipeline.
type fundedEntry struct {
	order         fixtureOrder
	senderAddress string
	entry         discovery.QueueEntry
}

// createFundedOrder creates externalID's order on C1, funds it with
// senderAddress (the real transition wire shape -- see
// ledgerfixture.go's own doc comment), then drives one real
// discovery.RunTick so the order lands in screening_queue exactly the
// way a real C3 process running its discovery loop would notice it --
// never inserted into screening_queue directly, which would bypass the
// very component (C3.3) this harness exists to gate.
func (h *harness) createFundedOrder(prefix, customerID, senderAddress string) (fundedEntry, error) {
	externalID := h.nextExternalID(prefix)
	order, err := h.ledger.createOrder(h.ctx, externalID, customerID)
	if err != nil {
		return fundedEntry{}, fmt.Errorf("replay: creating order %s: %w", externalID, err)
	}
	funded, err := h.ledger.fundOrder(h.ctx, h.ledgerPool, order, senderAddress)
	if err != nil {
		return fundedEntry{}, fmt.Errorf("replay: funding order %s: %w", externalID, err)
	}
	if err := discovery.RunTick(h.ctx, h.pool, h.client, h.client); err != nil {
		return fundedEntry{}, fmt.Errorf("replay: discovery tick after funding %s: %w", externalID, err)
	}
	entry, err := discovery.Get(h.ctx, h.pool, funded.ID)
	if err != nil {
		return fundedEntry{}, fmt.Errorf("replay: fetching queue entry for order %d (%s): %w", funded.ID, externalID, err)
	}
	if entry.SenderAddress == nil {
		return fundedEntry{}, fmt.Errorf("replay: order %d (%s) enqueued with no sender_address resolved", funded.ID, externalID)
	}

	h.mu.Lock()
	h.trackedOrderIDs = append(h.trackedOrderIDs, funded.ID)
	h.mu.Unlock()

	return fundedEntry{order: funded, senderAddress: senderAddress, entry: entry}, nil
}
