package replay

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"energybroker/internal/buffer"
	"energybroker/internal/db"
	"energybroker/internal/ledgerclient"
	"energybroker/internal/money"
	"energybroker/internal/pricing"
	"energybroker/internal/provider"
	"energybroker/internal/reservations"
	"energybroker/internal/routing"
)

// harness bundles everything a scenario function needs: a real
// ledgerFixture and *ledgerclient.Client wired against a real, running
// C1 (this chunk's whole point -- "not a mock of your own assumptions
// about C1"), a shared FakeTronReader standing in for the
// forked/simulated TRON node the build spec names (see faketron.go's own
// doc comment on why a real chain client is a later, separate concern),
// and the bookkeeping the final assertions check afterward.
//
// Unlike screening's own harness (one shared MockProvider trio reused
// across every scenario, since its MockProvider is forced per-ADDRESS),
// this harness gives each scenario its own fresh rig (see newRig): C4's
// own MockProvider forces health/price/timeout per PROVIDER INSTANCE,
// globally, with no way to un-force it -- reusing one instance across
// scenarios would let an earlier scenario's "force this vendor
// unhealthy" leak into a later scenario that needs it healthy again. A
// fresh rig's own Poller/Router/Buffer/Service still all talk to the
// SAME real, shared Postgres pool and the SAME shared FakeTronReader, so
// every scenario is still exercising this component's real, persistent
// state -- only the in-memory vendor-side fakes are scenario-local.
type harness struct {
	ctx  context.Context
	rng  *rand.Rand
	seed int64

	pool       *db.Pool      // C4's own database
	ledgerPool *pgxpool.Pool // raw connection to C1's database, fixture account/journal access only
	ledger     *ledgerFixture
	orders     *ledgerclient.Client // real OrderResolver against the real, running C1
	reader     *recordingReader     // shared FakeTronReader, wrapped to record every call for FINAL ASSERTION 2

	runIDOffsetRigs int64

	mu          sync.Mutex
	seq         int64
	costReports []costReport
	stagingSeq  int64
	rigSeq      int64
}

// costReport is one recordingCostReporter.ReportEnergyCost call this run
// made -- FINAL ASSERTIONS 1 and 3's own data.
type costReport struct {
	delegation provider.Delegation
	orderID    int64
	ceiling    float64
	idemKey    string
}

func newHarness(ctx context.Context, cfg Config, pool *db.Pool, ledgerPool *pgxpool.Pool) *harness {
	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000_000) // healthy by default; adversarial scenarios override specific (address, delegationID) pairs

	return &harness{
		ctx: ctx, rng: rand.New(rand.NewSource(cfg.Seed)), seed: cfg.Seed,
		pool: pool, ledgerPool: ledgerPool,
		ledger: newLedgerFixture(cfg.LedgerBaseURL, cfg.LedgerToken),
		orders: ledgerclient.New(cfg.LedgerBaseURL, cfg.LedgerToken),
		reader: &recordingReader{inner: reader},
		// runIDOffsetRigs seeds this run's own disjoint delegation-id
		// namespace (see nextRigIDBase) from the real wall clock, NOT
		// from cfg.Seed: cfg.Seed governs reproducible BUSINESS-LOGIC
		// randomness (which provider a weighted draw picks, and so on),
		// but two runs using the SAME seed (harness_integration_test.go's
		// own fixed seed, concretely) must still get DIFFERENT delegation
		// ids -- LEDGER_TEST_DATABASE_URL is shared, persistent state
		// across repeated runs (see that test's own doc comment), and a
		// deterministic id would replay the exact same
		// "broker:energy_cost:<id>" idempotency key into a real C1 that
		// already has a row under it from a PRIOR run, with a different
		// payload (a different order id, a different occurred_at) --
		// a genuine, spurious idempotency conflict, not a safe replay.
		// Bounded to keep newRig's own warmup loop (which must actually
		// call Redelegate this many times) fast.
		runIDOffsetRigs: rand.New(rand.NewSource(time.Now().UnixNano())).Int63n(1000),
	}
}

func (h *harness) nextID(prefix string) string {
	h.mu.Lock()
	h.seq++
	seq := h.seq
	h.mu.Unlock()
	return fmt.Sprintf("replay-%s-%d-%d", prefix, time.Now().UnixNano(), seq)
}

// nextStagingAddress hands out a fresh, unique TRON address for one
// rig's own Buffer.Config.StagingAddress -- unique per rig so the shared
// FakeTronReader (keyed by address+delegationID) never sees a key
// collision between two different scenarios' otherwise-identically-
// numbered MockProvider delegation ids (each fresh MockProvider restarts
// its own delegation id sequence at 1).
func (h *harness) nextStagingAddress() string {
	h.mu.Lock()
	h.stagingSeq++
	seq := h.stagingSeq
	h.mu.Unlock()
	return fmt.Sprintf("TReplayStaging%016d", seq)
}

// rigIDSpace is how many Delegate/Redelegate ids newRig's own warmup
// reserves per provider, per rig -- see newRig's own doc comment on why
// this needs to exist at all: MockProvider's delegation id sequence is
// per-instance and always starts at 1, but this harness's own
// ReportEnergyCost calls post real, idempotency-keyed entries
// ("broker:energy_cost:<delegation.ID>") against ONE real C1 database
// that outlives any single run (see runIDOffsetRigs's own doc comment) --
// two different rigs' otherwise-identically-sequenced ids would collide
// there. No single scenario in this harness makes anywhere close to 20
// real Delegate/Redelegate calls, so a disjoint 20-wide id space per rig
// per provider leaves a comfortable margin while keeping newRig's own
// warmup loop (bounded by runIDOffsetRigs, up to ~1000 rig-slots) fast.
const rigIDSpace = 20

func (h *harness) nextRigIDBase() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	base := (h.runIDOffsetRigs + h.rigSeq) * rigIDSpace
	h.rigSeq++
	return base
}

func (h *harness) recordCostReport(c costReport) {
	h.mu.Lock()
	h.costReports = append(h.costReports, c)
	h.mu.Unlock()
}

func (h *harness) costReportsSnapshot() []costReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]costReport, len(h.costReports))
	copy(out, h.costReports)
	return out
}

// recordingReader wraps the harness's own shared FakeTronReader,
// recording every DelegationUnits call and its result -- FINAL ASSERTION
// 2's own data ("no reservation ever confirms without a successful
// VerifyOnChain call backing it -- assert by call-count/audit-trail, not
// by outcome alone").
type recordingReader struct {
	inner *buffer.FakeTronReader

	mu        sync.Mutex
	calls     int
	successes int
}

func (r *recordingReader) DelegationUnits(ctx context.Context, address, delegationID string) (int64, error) {
	units, err := r.inner.DelegationUnits(ctx, address, delegationID)
	r.mu.Lock()
	r.calls++
	// VerifyOnChain's own success condition is `total >= d.EnergyUnits`,
	// which this wrapper cannot see (only the raw on-chain units this
	// call reported) -- units > 0 is this wrapper's own necessary (not
	// sufficient) proxy for "this call could plausibly have backed a
	// confirmation"; a genuinely wrong confirmation is still caught
	// because CONFIRMED reservations are cross-checked against journal
	// entries by FINAL ASSERTION 1, and a confirmation with zero
	// verify-success calls behind it at all is exactly the gap this
	// assertion exists to catch.
	if err == nil && units > 0 {
		r.successes++
	}
	r.mu.Unlock()
	return units, err
}

func (r *recordingReader) successCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.successes
}

// recordingCostReporter wraps the harness's own real *ledgerclient.Client,
// promoting ReportEnergyCost while recording every call this run makes
// into the harness -- FINAL ASSERTIONS 1 and 3's own data.
type recordingCostReporter struct {
	h       *harness
	ceiling float64
	client  *ledgerclient.Client
}

func (r *recordingCostReporter) ReportEnergyCost(ctx context.Context, delegation provider.Delegation, orderID int64) error {
	err := r.client.ReportEnergyCost(ctx, delegation, orderID)
	if err == nil {
		r.h.recordCostReport(costReport{
			delegation: delegation, orderID: orderID, ceiling: r.ceiling,
			idemKey: "broker:energy_cost:" + delegation.ID,
		})
	}
	return err
}

// rig is one scenario's own fresh, isolated set of business-logic
// objects -- see harness's own doc comment for why these are built fresh
// per scenario rather than shared for the whole run.
type rig struct {
	tronsell  *provider.MockProvider
	netts     *provider.MockProvider
	catfee    *provider.MockProvider
	providers map[string]provider.EnergyProvider

	poller *pricing.Poller
	router *routing.Router
	buffer *buffer.Buffer
	svc    *reservations.Service

	stagingAddress string
	ceiling        float64
	idBase         int64
}

// nextDelegationID predicts the delegation id providerName's own Delegate
// or Redelegate call will produce THE NEXT TIME it is called on this
// rig's own provider instance -- MockProvider's own id format
// ("mock-<name>-<seq>") is deterministic and documented, so a scenario
// that needs to pre-register the shared FakeTronReader for one specific,
// not-yet-made call (see scenarioDelegationNeverLandedOnChain) can
// compute it ahead of time, as long as it is truly this rig's own
// FIRST call to that provider after construction (idBase accounts for
// newRig's own warmup, so "first" here means first since newRig
// returned, not first ever on the underlying struct).
func (r *rig) nextDelegationID(providerName string) string {
	return fmt.Sprintf("mock-%s-%d", providerName, r.idBase+1)
}

type stubDemandObserver struct{ floor int64 }

func (s stubDemandObserver) RecentReservedUnits(ctx context.Context, window time.Duration) (int64, error) {
	return 0, nil
}

// newRig builds one scenario's own fresh Poller/Router/Buffer/Service,
// wired against the harness's own shared pool, shared FakeTronReader,
// and a real C1 -- weights and ceiling are the scenario's own choice
// (some scenarios deliberately configure only one or two named
// providers, or an unusually tight ceiling, to exercise a specific
// branch). minimumFloor sizes the buffer's own TargetLevel for
// scenarios that call Replenish; 0 is fine for scenarios that seed
// energy_buffer rows directly instead.
func (h *harness) newRig(weights routing.RoutingWeights, ceiling float64, minimumFloor int64) (*rig, error) {
	seed := func() int64 { h.mu.Lock(); defer h.mu.Unlock(); return h.rng.Int63() }

	tronsell := provider.NewMockProvider(provider.Tronsell, seed(), 24.0)
	netts := provider.NewMockProvider(provider.Netts, seed(), 28.0)
	catfee := provider.NewMockProvider(provider.Catfee, seed(), 30.0)
	providers := map[string]provider.EnergyProvider{
		provider.Tronsell: tronsell,
		provider.Netts:    netts,
		provider.Catfee:   catfee,
	}

	// Reserve this rig's own disjoint delegation-id range on every
	// provider before anything else touches them -- see rigIDSpace's own
	// doc comment. Redelegate is a pure in-memory, no-I/O call on a
	// freshly constructed MockProvider (no ForceTimeout/ForceMalformed
	// has been set yet), so warming up rigIDSpace ids on all three costs
	// microseconds, not a real chain or HTTP call.
	idBase := h.nextRigIDBase()
	for i := int64(0); i < idBase; i++ {
		_, _ = tronsell.Redelegate(context.Background(), "warmup", "Twarmup", 1)
		_, _ = netts.Redelegate(context.Background(), "warmup", "Twarmup", 1)
		_, _ = catfee.Redelegate(context.Background(), "warmup", "Twarmup", 1)
	}

	poller := pricing.NewPoller(h.pool, providers, time.Hour)
	if err := poller.PollAll(h.ctx); err != nil {
		return nil, fmt.Errorf("replay: PollAll: %w", err)
	}
	router := routing.NewRouter(poller, h.pool, seed())

	stagingAddress := h.nextStagingAddress()
	buf, err := buffer.NewBuffer(h.pool, providers, router, stubDemandObserver{floor: minimumFloor}, h.reader, nil, buffer.Config{
		StagingAddress: stagingAddress,
		Weights:        weights,
		Ceiling:        ceiling,
		MinimumFloor:   minimumFloor,
	})
	if err != nil {
		return nil, fmt.Errorf("replay: NewBuffer: %w", err)
	}

	costReporter := &recordingCostReporter{h: h, ceiling: ceiling, client: h.orders}
	svc, err := reservations.NewService(h.pool, h.orders, buf, router, costReporter, nil, providers, reservations.Config{
		Weights: weights, Ceiling: ceiling,
	})
	if err != nil {
		return nil, fmt.Errorf("replay: NewService: %w", err)
	}

	return &rig{
		tronsell: tronsell, netts: netts, catfee: catfee, providers: providers,
		poller: poller, router: router, buffer: buf, svc: svc,
		stagingAddress: stagingAddress, ceiling: ceiling, idBase: idBase,
	}, nil
}

// createOrder creates externalID's order on a real, running C1 --
// C4's own OrderResolver needs nothing more than a real, resolvable
// order (see ledgerfixture.go's own doc comment on why C4 never needs to
// fund one).
func (h *harness) createOrder(prefix string) (fixtureOrder, error) {
	externalID := h.nextID(prefix)
	return h.ledger.createOrder(h.ctx, externalID, "cust-"+externalID)
}

// seedAvailableRow inserts an AVAILABLE energy_buffer row directly
// against rig's own staging address -- for scenarios that need a warm
// buffer without going through a full Replenish cycle first (the same
// direct-seed discipline this module's own reservations/buffer
// integration tests already use). delegationID must be unique across
// the whole run (h.nextID handles that).
// drainBuffer marks every currently-AVAILABLE energy_buffer row EXPIRED
// directly -- energy_buffer is one continuous, real table shared by
// every rig this whole run ever builds (a Buffer object is scoped per
// scenario; the Postgres table it reads and writes is not), so a
// scenario that left AVAILABLE surplus behind (scenarioCleanFastPathMajority's
// own floor, concretely) would otherwise silently hand that leftover
// capacity to a LATER scenario's own Reserve call -- which is exactly
// wrong for any scenario whose own point is "the buffer has nothing to
// offer, so this must go through the slow path" (or, for
// scenarioConcurrentReservationsRacingBuffer's own scarcity phase, "the
// buffer has EXACTLY this much and no more"). Scenarios that actually
// want a warm buffer seed their own rows immediately afterward, so
// draining first is always safe to call unconditionally.
func (h *harness) drainBuffer() error {
	_, err := h.pool.Exec(h.ctx, `UPDATE energy_buffer SET status = 'EXPIRED' WHERE status = 'AVAILABLE'`)
	return err
}

func (h *harness) seedAvailableRow(providerName, delegationID string, units int64, costTRX money.Amount, expiresIn time.Duration) error {
	_, err := h.pool.Exec(h.ctx, `
		INSERT INTO energy_buffer (provider_name, delegation_id, units, acquired_at, cost_trx, expires_at, status)
		VALUES ($1, $2, $3, now(), $4, $5, 'AVAILABLE')
	`, providerName, delegationID, units, int64(costTRX), time.Now().UTC().Add(expiresIn))
	return err
}
