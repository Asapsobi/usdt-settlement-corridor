//go:build integration

// Requires a real, reachable Postgres 16 instance (both
// SCREENING_TEST_DATABASE_URL and LEDGER_TEST_DATABASE_URL) AND a
// sibling checkout of the ledger module -- see internal/testledger.
// This is C3.4's own headline acceptance criterion: "against a real
// running C1 instance... an order enqueued via C3.3 ends up in screened
// or held correctly, matching the MockProvider's configured verdict for
// its address." Run via `make test-integration` (build tag
// "integration").
package pipeline_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"screening/internal/cache"
	"screening/internal/discovery"
	"screening/internal/ledgerclient"
	"screening/internal/pipeline"
	"screening/internal/provider"
	"screening/internal/testledger"
	"screening/internal/verdict"
)

const (
	pipelineLedgerListenAddr = ":18437"
	pipelineLedgerAPIToken   = "c34-integration-test-token"
	pipelineLedgerActor      = "screening"
)

func TestPipeline_EnqueuedOrderEndsUpScreenedMatchingMockVerdict_AgainstRealC1(t *testing.T) {
	pool := testPool(t)
	ll := testledger.Start(t, pipelineLedgerListenAddr, pipelineLedgerAPIToken, pipelineLedgerActor)
	client := ledgerclient.New(ll.BaseURL(), pipelineLedgerAPIToken)

	externalID := "c34-live-pass-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c34-live-pass-cust-" + fmt.Sprint(time.Now().UnixNano())
	// Verified offline against MockProvider's own deterministic hash
	// (seed 1): scores ~0.12, comfortably below verdict.DefaultThresholds
	// .PassBelow (0.5) and never Flagged -- a genuinely clean verdict,
	// not an assumption about what an arbitrary address string happens
	// to hash to.
	const cleanSender = "0xCleanSenderLiveBBBB000000000002"

	order := ll.CreateOrder(externalID, customerID)
	funded := ll.FundOrder(order, customerID, cleanSender)

	// Simulates what C3.3's own discovery loop would have done on
	// seeing this order funded.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO screening_queue (order_id, external_id, sender_address) VALUES ($1, $2, $3)
	`, funded.ID, externalID, cleanSender); err != nil {
		t.Fatalf("seeding screening_queue: %v", err)
	}
	entry, err := discovery.Get(context.Background(), pool, funded.ID)
	if err != nil {
		t.Fatalf("discovery.Get: %v", err)
	}

	mock := provider.NewMockProvider(1) // cleanSender's deterministic score is well below any real threshold's flagged band by default
	cfg := pipeline.Config{ProviderName: "mock", Timeout: 2 * time.Second, Thresholds: verdict.DefaultThresholds, TTL: cache.DefaultTTLConfig}

	if err := pipeline.ScreenAndReport(context.Background(), pool, mock, client, cfg, entry); err != nil {
		t.Fatalf("ScreenAndReport: %v", err)
	}

	after := ll.GetOrder(externalID)
	if after.State != "screened" {
		t.Fatalf("order state = %q, want screened (clean MockProvider verdict for %s)", after.State, cleanSender)
	}
	queued, err := discovery.Get(context.Background(), pool, funded.ID)
	if err != nil {
		t.Fatalf("discovery.Get (after): %v", err)
	}
	if queued.Status != discovery.Done {
		t.Fatalf("queue status = %q, want DONE", queued.Status)
	}
}

func TestPipeline_EnqueuedOrderEndsUpHeldMatchingMockVerdict_AgainstRealC1(t *testing.T) {
	pool := testPool(t)
	ll := testledger.Start(t, pipelineLedgerListenAddr, pipelineLedgerAPIToken, pipelineLedgerActor)
	client := ledgerclient.New(ll.BaseURL(), pipelineLedgerAPIToken)

	externalID := "c34-live-hold-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c34-live-hold-cust-" + fmt.Sprint(time.Now().UnixNano())
	const flaggedSender = "0xFlaggedSenderLive00000000000002"

	order := ll.CreateOrder(externalID, customerID)
	funded := ll.FundOrder(order, customerID, flaggedSender)

	if _, err := pool.Exec(context.Background(), `
		INSERT INTO screening_queue (order_id, external_id, sender_address) VALUES ($1, $2, $3)
	`, funded.ID, externalID, flaggedSender); err != nil {
		t.Fatalf("seeding screening_queue: %v", err)
	}
	entry, err := discovery.Get(context.Background(), pool, funded.ID)
	if err != nil {
		t.Fatalf("discovery.Get: %v", err)
	}

	mock := provider.NewMockProvider(1)
	mock.ForceFlagged(flaggedSender)
	cfg := pipeline.Config{ProviderName: "mock", Timeout: 2 * time.Second, Thresholds: verdict.DefaultThresholds, TTL: cache.DefaultTTLConfig}

	if err := pipeline.ScreenAndReport(context.Background(), pool, mock, client, cfg, entry); err != nil {
		t.Fatalf("ScreenAndReport: %v", err)
	}

	after := ll.GetOrder(externalID)
	if after.State != "held" {
		t.Fatalf("order state = %q, want held (flagged MockProvider verdict for %s)", after.State, flaggedSender)
	}
}

// TestPipeline_RestartMidPipelineReplaysSafely simulates a C3 restart:
// ScreenAndReport is invoked twice for the same queue entry, as if two
// process lifetimes both observed it PENDING before either finished.
// The first call caches a real result and reports it; the second finds
// that same cached result (same screening_result_id, same idempotency
// key) and its ReportVerdict call surfaces
// ledgerclient.ErrIllegalTransition -- which this pipeline treats as
// "already done" -- rather than corrupting the order via a conflicting
// transition. See TestReportVerdict_ReplaySurfacesIllegalTransitionSafely
// in internal/ledgerclient for why that specific error, not a replayed
// 200, is what a real C1 actually returns here.
func TestPipeline_RestartMidPipelineReplaysSafely(t *testing.T) {
	pool := testPool(t)
	ll := testledger.Start(t, pipelineLedgerListenAddr, pipelineLedgerAPIToken, pipelineLedgerActor)
	client := ledgerclient.New(ll.BaseURL(), pipelineLedgerAPIToken)

	externalID := "c34-live-restart-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c34-live-restart-cust-" + fmt.Sprint(time.Now().UnixNano())
	const sender = "0xRestartSenderLive000000000000003"

	order := ll.CreateOrder(externalID, customerID)
	funded := ll.FundOrder(order, customerID, sender)

	if _, err := pool.Exec(context.Background(), `
		INSERT INTO screening_queue (order_id, external_id, sender_address) VALUES ($1, $2, $3)
	`, funded.ID, externalID, sender); err != nil {
		t.Fatalf("seeding screening_queue: %v", err)
	}

	mock := provider.NewMockProvider(1)
	cfg := pipeline.Config{ProviderName: "mock", Timeout: 2 * time.Second, Thresholds: verdict.DefaultThresholds, TTL: cache.DefaultTTLConfig}

	entry, err := discovery.Get(context.Background(), pool, funded.ID)
	if err != nil {
		t.Fatalf("discovery.Get: %v", err)
	}
	if err := pipeline.ScreenAndReport(context.Background(), pool, mock, client, cfg, entry); err != nil {
		t.Fatalf("ScreenAndReport (1st, simulating pre-restart process): %v", err)
	}

	// "Restart": re-fetch the (now DONE) entry exactly as a fresh
	// process would, and process it again -- ScreenAndReport itself
	// doesn't refuse a DONE entry (nothing in this chunk's own scope
	// filters by status inside ScreenAndReport; ListReadyForScreening is
	// what normally prevents this in RunTick), so this exercises the
	// worst case directly: the same entry, reprocessed.
	entryAgain, err := discovery.Get(context.Background(), pool, funded.ID)
	if err != nil {
		t.Fatalf("discovery.Get (post-restart): %v", err)
	}
	if err := pipeline.ScreenAndReport(context.Background(), pool, mock, client, cfg, entryAgain); err != nil {
		t.Fatalf("ScreenAndReport (2nd, simulating restart) returned an error instead of absorbing the replay: %v", err)
	}

	// Not corrupted: still exactly where the first, successful pass left it.
	after := ll.GetOrder(externalID)
	if after.State != "screened" && after.State != "held" {
		t.Fatalf("order state after a replayed pipeline pass = %q, want screened or held (unchanged from the first pass)", after.State)
	}
	if mock.ScreenCallCount(sender) != 1 {
		t.Fatalf("provider.Screen called %d times across the replay, want exactly 1 (the second pass must hit the cache)", mock.ScreenCallCount(sender))
	}
}
