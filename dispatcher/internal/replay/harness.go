package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/fbsobreira/gotron-sdk/pkg/address"
	"github.com/jackc/pgx/v5/pgxpool"

	"dispatcher/internal/db"
	"dispatcher/internal/dispatch"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/money"
	"dispatcher/internal/slots"
	"dispatcher/internal/txbuild"
)

func parseMoney(s string) (money.Amount, error) { return money.ParseDecimal(s) }

func mustParseMoney(s string) money.Amount {
	amt, err := money.ParseDecimal(s)
	if err != nil {
		panic(fmt.Sprintf("replay: internal error parsing constant %q: %v", s, err))
	}
	return amt
}

// hashHexForReplay mirrors internal/dispatch's own unexported hashHex --
// this package needs to simulate an AttemptStore.Create call directly
// (a handful of scenarios construct a dispatch_attempts row themselves,
// the same way this module's own _test.go files do) without access to
// dispatch's unexported helper.
func hashHexForReplay(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// harness bundles everything every scenario needs: a real Postgres-backed
// dispatch.Dispatcher and slots.Store against a real, running C1 (this
// harness's own ledgerFixture and *ledgerclient.Client), plus in-process
// fakes for every OTHER external boundary -- S1 (no real implementation
// exists anywhere, see "Read this first"), C4's own reservation client,
// and the chain itself (broadcast, finality). Each scenario gets its own
// fresh FakeSigningService, fakeEnergyReserver, fakeChain, and slot(s)
// (see newSlot) -- sharing those across scenarios would let one
// scenario's forced failure/rejection leak into another's, the same
// reasoning S1's and C4's own harnesses already settled on.
type harness struct {
	ctx  context.Context
	rng  *rand.Rand
	seed int64

	pool   *db.Pool // dispatcher's own database
	ledger *ledgerFixture
	orders *ledgerclient.Client // real client against the real, running C1
	slots  *slots.Store

	mu      sync.Mutex
	seq     int64
	slotSeq int

	// State the FINAL ASSERTIONS check, appended to by scenarios as they
	// run.
	dispatched []dispatchedOrder
	settled    []settledOrder
	held       []heldOrder
	batchRuns  []batchRun
}

// dispatchedOrder is one order this run pushed through EnterDispatching.
// expectAmbiguous marks the one scenario (slot freeze post-broadcast,
// pre-confirmation) that deliberately leaves an order `dispatching` with
// an open BROADCAST attempt awaiting a human -- FINAL ASSERTION 1 must
// not treat that as a violation.
type dispatchedOrder struct {
	externalID      string
	orderID         int64
	expectAmbiguous bool
}

type settledOrder struct {
	externalID       string
	settleKey        string
	trcLiabilityCode string
	amountOutUnits   int64
}

type heldOrder struct {
	externalID         string
	conversionEntryKey string
}

type batchRun struct {
	recipientCount         int
	energyReservationCalls int
}

// markStillInFlightOK flips an already-recorded dispatched order's own
// expectAmbiguous flag -- for a Sweep-tier order requeued after a
// partial-batch failure, which legitimately stays `dispatching` at the
// end of THIS run (awaiting the next batch window, which this run never
// cuts), the same as a genuinely ambiguous frozen-slot order does for a
// different reason. Both cases mean the same thing to FINAL ASSERTION 1:
// "still dispatching is fine here, don't flag it."
func (h *harness) markStillInFlightOK(externalID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.dispatched {
		if h.dispatched[i].externalID == externalID {
			h.dispatched[i].expectAmbiguous = true
		}
	}
}

func (h *harness) recordDispatched(externalID string, orderID int64, ambiguous bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dispatched = append(h.dispatched, dispatchedOrder{externalID: externalID, orderID: orderID, expectAmbiguous: ambiguous})
}

func (h *harness) recordSettled(externalID, settleKey, trcLiabilityCode string, amountOutUnits int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settled = append(h.settled, settledOrder{externalID: externalID, settleKey: settleKey, trcLiabilityCode: trcLiabilityCode, amountOutUnits: amountOutUnits})
}

func (h *harness) recordHeld(externalID, conversionEntryKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = append(h.held, heldOrder{externalID: externalID, conversionEntryKey: conversionEntryKey})
}

func (h *harness) recordBatchRun(recipientCount, energyReservationCalls int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.batchRuns = append(h.batchRuns, batchRun{recipientCount: recipientCount, energyReservationCalls: energyReservationCalls})
}

func newHarness(ctx context.Context, cfg Config, pool *db.Pool, ledgerPool *pgxpool.Pool) *harness {
	return &harness{
		ctx: ctx, rng: rand.New(rand.NewSource(cfg.Seed)), seed: cfg.Seed,
		pool:   pool,
		ledger: newLedgerFixture(cfg.LedgerBaseURL, cfg.LedgerToken, ledgerPool),
		orders: ledgerclient.New(cfg.LedgerBaseURL, cfg.LedgerToken),
		slots:  slots.NewStore(pool),
	}
}

// nextID returns a deterministic (seeded), collision-free identifier for
// this run -- external ids, customer ids, idempotency-key components.
func (h *harness) nextID(prefix string) string {
	h.mu.Lock()
	h.seq++
	seq := h.seq
	h.mu.Unlock()
	return fmt.Sprintf("replay-%s-%d-%d", prefix, h.seed, seq)
}

// newSlot registers a fresh, real slot -- a unique id and a genuinely
// real, checksummed TRON address (a fresh secp256k1 keypair's own
// derived address, via the exact same address.BTCECPrivkeyToAddress
// txbuild itself is built on -- not a fixed, reused string, since
// slots.Store's own UNIQUE constraint on tron_address means this harness
// needs as many distinct real addresses as scenarios need slots, up to
// and including "all six slots" in one run) -- and ensures its C1 ledger
// account exists. Every scenario's own starting point.
func (h *harness) newSlot() (slots.Slot, error) {
	h.mu.Lock()
	h.slotSeq++
	id := h.slotSeq
	h.mu.Unlock()

	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return slots.Slot{}, fmt.Errorf("replay: generating a real address for slot %d: %w", id, err)
	}
	tronAddress := address.BTCECPrivkeyToAddress(priv).String()

	slot, err := h.slots.Create(h.ctx, id, tronAddress, time.Now().UTC())
	if err != nil {
		return slots.Slot{}, fmt.Errorf("replay: registering slot %d: %w", id, err)
	}

	code := fmt.Sprintf("asset:tron:slot:%d", id)
	if err := h.orders.EnsureAccount(h.ctx, code, ledgerclient.AccountAsset, "USDT_TRC20", h.nextID("ensure-account")); err != nil {
		return slots.Slot{}, fmt.Errorf("replay: ensuring %s exists: %w", code, err)
	}
	return slot, nil
}

// newDispatcher builds a fresh Dispatcher sharing this harness's own
// pool/ledger client, with a caller-supplied SigningService (a fresh
// FakeSigningService per scenario, or one pre-configured to force a
// specific failure).
func (h *harness) newDispatcher(signer dispatch.SigningService) *dispatch.Dispatcher {
	d := dispatch.NewDispatcher(h.orders, dispatch.NewStore(h.pool), dispatch.NewAttemptStore(h.pool), signer)
	d.Batches = dispatch.NewBatchStore(h.pool)
	d.HaltBackoff = 50 * time.Millisecond
	d.FinalityStaleCeiling = time.Hour
	return d
}

// buildTransfer is C5.4's own BuildTransfer, given a deterministic block
// reference derived from this harness's own seeded sequence -- every
// scenario needing an unsigned tx goes through this, never hand-crafted
// bytes, so Broadcast's real proto.Unmarshal path is always exercised
// for real.
func (h *harness) buildTransfer(slotAddress, recipientAddress, amount string) ([]byte, error) {
	h.mu.Lock()
	h.seq++
	n := h.seq
	h.mu.Unlock()
	amt, err := parseMoney(amount)
	if err != nil {
		return nil, err
	}
	ref := txbuild.BlockReference{
		BlockNumber: 1_000_000 + n,
		Timestamp:   time.Now().UTC(),
		Expiration:  time.Now().UTC().Add(time.Minute),
	}
	return txbuild.BuildTransfer(slotAddress, recipientAddress, amt, ref)
}
