package replay

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyler-smith/go-bip32"

	"depositwatcher/internal/addresses"
	"depositwatcher/internal/candidates"
	"depositwatcher/internal/chain"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/ledgerclient"
	"depositwatcher/internal/orphaned"
)

// contractAddress and transferTopic are the real, verified USDT BEP20
// values -- see chain.ParseTransferLog's own doc comment for how
// transferTopic was confirmed independently of this document's citation.
var (
	contractAddress = common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")
	transferTopic   = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
)

const dustFloorUnits = 1_000000 // 1.000000 USDT_BEP20, this harness's own DustFloor

// finalAttempt is one OnFinal call this run's Tracker made, kept so the
// FINAL ASSERTIONS can check "exactly once" and "never for a filtered
// candidate" against what actually happened, not just what a single
// scenario locally observed.
type finalAttempt struct {
	Candidate finality.Candidate
	Err       error
}

// harness bundles everything a scenario function needs: the sim chain,
// a real ledgerd fixture and client, C2's own address book and
// finality.Tracker (wired with the REAL ledgerclient/orphaned
// implementations, not fakes -- this is the point of C2.10), and the
// bookkeeping the final assertions check afterward.
type harness struct {
	ctx context.Context
	rng *rand.Rand

	watcherPool *pgxpool.Pool
	ledgerPool  *pgxpool.Pool
	ledger      *ledgerFixture
	ledgerClnt  *ledgerclient.Client

	chain   *simChain
	tracker *finality.Tracker

	mu            sync.Mutex
	finalAttempts []finalAttempt
	reorgReports  []string // external IDs ReportReorg was actually called for
	seq           int64
}

func newHarness(ctx context.Context, cfg Config, watcherPool, ledgerPool *pgxpool.Pool) (*harness, error) {
	master, err := bip32.NewMasterKey([]byte("c2.10 replay harness fixture -- never use"))
	if err != nil {
		return nil, fmt.Errorf("replay: generating fixture xpub: %w", err)
	}
	if err := addresses.Configure(master.PublicKey().B58Serialize()); err != nil {
		return nil, fmt.Errorf("replay: configuring addresses: %w", err)
	}

	sc, err := newSimChain(chain.Config{MinAgreement: 2})
	if err != nil {
		return nil, fmt.Errorf("replay: building sim chain: %w", err)
	}

	h := &harness{
		ctx: ctx, rng: rand.New(rand.NewSource(cfg.Seed)),
		watcherPool: watcherPool, ledgerPool: ledgerPool,
		ledger:     newLedgerFixture(cfg.LedgerBaseURL, cfg.LedgerToken),
		ledgerClnt: ledgerclient.New(cfg.LedgerBaseURL, cfg.LedgerToken),
		chain:      sc,
	}

	tracker, err := finality.New(finality.Config{
		ContractAddress:         contractAddress,
		TransferTopic:           transferTopic,
		OnFinal:                 h.onFinal,
		ReorgReporter:           reorgReporterFunc(h.reportReorg),
		OrphanedDepositRecorder: orphanedRecorderFunc(h.recordOrphaned),
	})
	if err != nil {
		sc.Close()
		return nil, fmt.Errorf("replay: building tracker: %w", err)
	}
	h.tracker = tracker
	return h, nil
}

func (h *harness) close() {
	h.chain.Close()
}

// onFinal is Config.OnFinal: the REAL ledgerclient.ReportDepositFinal,
// wrapped only to record what happened for the final assertions --
// exactly the reporting path a real watcherd would use, never a fake.
func (h *harness) onFinal(ctx context.Context, c finality.Candidate) error {
	err := h.ledgerClnt.ReportDepositFinal(ctx, c)
	h.mu.Lock()
	h.finalAttempts = append(h.finalAttempts, finalAttempt{Candidate: c, Err: err})
	h.mu.Unlock()
	return err
}

type reorgReporterFunc func(ctx context.Context, externalID, originalEntryKey string) error

func (f reorgReporterFunc) ReportReorg(ctx context.Context, externalID, originalEntryKey string) error {
	return f(ctx, externalID, originalEntryKey)
}

func (h *harness) reportReorg(ctx context.Context, externalID, originalEntryKey string) error {
	err := h.ledgerClnt.ReportReorg(ctx, externalID, originalEntryKey)
	if err == nil {
		h.mu.Lock()
		h.reorgReports = append(h.reorgReports, externalID)
		h.mu.Unlock()
	}
	return err
}

type orphanedRecorderFunc func(ctx context.Context, c finality.Candidate, c1Error error) error

func (f orphanedRecorderFunc) RecordOrphanedDeposit(ctx context.Context, c finality.Candidate, c1Error error) error {
	return f(ctx, c, c1Error)
}

// recordOrphaned is the REAL Config.OrphanedDepositRecorder: fetches the
// order's current state from C1 (best-effort -- "unknown" if even that
// fails, per finality.OrphanedDepositRecorder's own doc comment) and
// persists via orphaned.Record against the watcher database.
func (h *harness) recordOrphaned(ctx context.Context, c finality.Candidate, c1Error error) error {
	state := "unknown"
	if order, err := h.ledger.getOrder(ctx, c.ExternalID); err == nil {
		state = order.State
	}
	return orphaned.Record(ctx, h.watcherPool, orphaned.Deposit{
		OrderID: c.OrderID, ExternalID: c.ExternalID, TxHash: c.TxHash.Hex(), LogIndex: int(c.LogIndex),
		Amount: int64(c.Amount), DetectedAt: time.Now().UTC(), OrderStateAtDetection: state,
	})
}

// tick runs the candidate pipeline for [fromHeight, toHeight] and then
// checks finality once -- the two steps a real watcherd will eventually
// run continuously (see cmd/watcherd's own doc comment on why that
// wiring isn't built yet); this harness drives them explicitly and
// deterministically instead of on a background ticker, so a scenario
// controls exactly when each step happens.
func (h *harness) tick(fromHeight, toHeight uint64) error {
	cfg := candidates.Config{ContractAddress: contractAddress, TransferTopic: transferTopic, DustFloor: dustFloorUnits}
	if err := candidates.ScanRange(h.ctx, h.chain.pool, h.watcherPool, h.ledgerClnt, h.tracker, cfg, fromHeight, toHeight); err != nil {
		return fmt.Errorf("replay: scanning [%d,%d]: %w", fromHeight, toHeight, err)
	}
	if err := h.tracker.CheckFinality(h.ctx, h.chain.pool); err != nil {
		// Expected in scenarios that deliberately induce disagreement --
		// scenarios assert on this themselves; tick just surfaces it.
		return err
	}
	return nil
}

func (h *harness) nextExternalID(prefix string) string {
	h.mu.Lock()
	h.seq++
	seq := h.seq
	h.mu.Unlock()
	return fmt.Sprintf("replay-%s-%d-%d", prefix, time.Now().UnixNano(), seq)
}

func (h *harness) finalAttemptsFor(externalID string) []finalAttempt {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []finalAttempt
	for _, a := range h.finalAttempts {
		if a.Candidate.ExternalID == externalID {
			out = append(out, a)
		}
	}
	return out
}
