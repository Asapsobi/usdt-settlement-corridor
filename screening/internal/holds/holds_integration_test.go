//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package holds_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"screening/internal/cache"
	"screening/internal/holds"
	"screening/internal/ledgerclient"
	"screening/internal/provider"
	"screening/internal/verdict"
)

var orderSeq int64

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("SCREENING_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("SCREENING_TEST_DATABASE_URL not set; skipping integration test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(context.Background(), `TRUNCATE rescreen_flags, holds, screening_queue, screening_results, screening_result_invalidations`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

// uniqueOrder returns a fresh, guaranteed-unique OrderRef -- an atomic
// counter, not time.Now().UnixNano() alone: two calls close together in
// the same test (this file's own TestListOpen_NeverReturnsResolvedHolds,
// concretely) can otherwise land on the same nanosecond timestamp and
// collide, silently reusing one order's hold for what was meant to be a
// second, distinct one.
func uniqueOrder(t *testing.T) ledgerclient.OrderRef {
	t.Helper()
	n := atomic.AddInt64(&orderSeq, 1)*1_000_000_000 + time.Now().UnixNano()%1_000_000_000
	return ledgerclient.OrderRef{OrderID: n, ExternalID: fmt.Sprintf("ext-%s-%d", t.Name(), n)}
}

// holdDecision returns a Hold Decision backed by a REAL screening_results
// row -- holds.screening_result_id is a genuine foreign key (migration
// 0004), so a fabricated id would violate it, exactly the way a real
// caller's decision.ScreeningResultID always traces back to an actual
// cache.Put.
func holdDecision(t *testing.T, pool *pgxpool.Pool) verdict.Decision {
	t.Helper()
	ctx := context.Background()
	address := fmt.Sprintf("0xHoldTestAddr-%s-%d", t.Name(), time.Now().UnixNano())
	v := provider.Verdict{RiskScore: 0.9, Flagged: true, ProviderName: "mock", CheckedAt: time.Now().UTC()}
	id, err := cache.Put(ctx, pool, "mock", address, v, time.Hour)
	if err != nil {
		t.Fatalf("seeding a real screening_results row: %v", err)
	}
	return verdict.Decision{Classification: verdict.Hold, ReasonCode: verdict.ReasonHoldFlagged, ScreeningResultID: id}
}

// fakeReleaser/fakeRejecter script C1's own response, so these tests can
// verify holds' own local-DB logic without a live ledgerd.
type fakeReleaser struct {
	err   error
	calls int
	mu    sync.Mutex
}

func (f *fakeReleaser) ReleaseHold(ctx context.Context, externalID string, orderID, holdID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.err
}

type fakeRejecter struct {
	err        error
	calls      int
	gotEntry   map[string]any
	entryError error
}

func (f *fakeRejecter) RejectHold(ctx context.Context, externalID string, orderID, holdID int64, entry map[string]any) error {
	f.calls++
	f.gotEntry = entry
	return f.err
}

type fakeEntryBuilder struct {
	entry map[string]any
	err   error
}

func (f fakeEntryBuilder) BuildRefundEntry(ctx context.Context, order ledgerclient.OrderRef) (map[string]any, error) {
	return f.entry, f.err
}

func TestOpen_CreatesExactlyOneRowAndIsIdempotentOnRetry(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	decision := holdDecision(t, pool)

	first, err := holds.Open(ctx, pool, order, decision)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if first.Status != holds.StatusOpen {
		t.Fatalf("status = %v, want StatusOpen", first.Status)
	}
	if first.ReasonCode != decision.ReasonCode {
		t.Fatalf("reason_code = %q, want %q", first.ReasonCode, decision.ReasonCode)
	}
	if first.ScreeningResultID == nil || *first.ScreeningResultID != decision.ScreeningResultID {
		t.Fatalf("screening_result_id = %v, want %d", first.ScreeningResultID, decision.ScreeningResultID)
	}

	// Simulates a retried C3.4 pipeline run for the same order.
	second, err := holds.Open(ctx, pool, order, decision)
	if err != nil {
		t.Fatalf("Open (retry): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("Open (retry) returned a different hold (id %d), want the same one (id %d) -- no duplicate OPEN hold", second.ID, first.ID)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM holds WHERE order_id = $1`, order.OrderID).Scan(&count); err != nil {
		t.Fatalf("counting holds rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("got %d holds rows for one order after a retried Open, want exactly 1", count)
	}
}

func TestOpen_RejectsNonHoldClassification(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	passDecision := verdict.Decision{Classification: verdict.Pass, ReasonCode: verdict.ReasonPass}

	if _, err := holds.Open(ctx, pool, order, passDecision); err == nil {
		t.Fatal("expected an error opening a hold for a Pass classification")
	}
}

func TestOpen_NullScreeningResultIDForUnavailableHolds(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	decision := verdict.Unavailable() // ScreeningResultID stays 0 -- see verdict.Unavailable's own doc comment

	h, err := holds.Open(ctx, pool, order, decision)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if h.ScreeningResultID != nil {
		t.Fatalf("screening_result_id = %v, want nil for a *_unavailable hold with no backing screening_results row", h.ScreeningResultID)
	}
}

func TestRelease_UpdatesLocalStateAfterCallingC1(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	h, err := holds.Open(ctx, pool, order, holdDecision(t, pool))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	releaser := &fakeReleaser{}
	if err := holds.Release(ctx, pool, releaser, h.ID, "operator:alice", "looks fine on manual review"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if releaser.calls != 1 {
		t.Fatalf("ReleaseHold called %d times, want 1", releaser.calls)
	}

	got, err := holds.Get(ctx, pool, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != holds.StatusReleased {
		t.Fatalf("status = %v, want StatusReleased", got.Status)
	}
	if got.ResolvedBy == nil || *got.ResolvedBy != "operator:alice" {
		t.Fatalf("resolved_by = %v, want operator:alice", got.ResolvedBy)
	}
	if got.ResolvedAt == nil {
		t.Fatal("resolved_at is nil, want set")
	}
	if got.ResolutionNote == nil || *got.ResolutionNote != "looks fine on manual review" {
		t.Fatalf("resolution_note = %v, want the given note", got.ResolutionNote)
	}
}

func TestRelease_RejectsEmptyReviewerBeforeCallingC1(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	h, err := holds.Open(ctx, pool, order, holdDecision(t, pool))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	releaser := &fakeReleaser{}
	err = holds.Release(ctx, pool, releaser, h.ID, "", "note")
	if !errors.Is(err, holds.ErrEmptyReviewer) {
		t.Fatalf("Release with empty reviewer = %v, want ErrEmptyReviewer", err)
	}
	if releaser.calls != 0 {
		t.Fatalf("ReleaseHold called %d times, want 0 -- must never call C1 with no reviewer identity", releaser.calls)
	}
}

func TestRelease_RejectsAlreadyResolvedHold(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	h, err := holds.Open(ctx, pool, order, holdDecision(t, pool))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	releaser := &fakeReleaser{}
	if err := holds.Release(ctx, pool, releaser, h.ID, "operator:alice", ""); err != nil {
		t.Fatalf("Release (first): %v", err)
	}
	err = holds.Release(ctx, pool, releaser, h.ID, "operator:bob", "")
	if !errors.Is(err, holds.ErrNotOpen) {
		t.Fatalf("Release (second, already resolved) = %v, want ErrNotOpen", err)
	}
	if releaser.calls != 1 {
		t.Fatalf("ReleaseHold called %d times, want exactly 1 (second attempt must not reach C1 again)", releaser.calls)
	}
}

func TestReject_BuildsEntryAndUpdatesLocalStateAfterCallingC1(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	h, err := holds.Open(ctx, pool, order, holdDecision(t, pool))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	entry := map[string]any{"entry_type": "manual_refund_test", "lines": []map[string]any{}}
	rejecter := &fakeRejecter{}
	builder := fakeEntryBuilder{entry: entry}

	if err := holds.Reject(ctx, pool, rejecter, builder, h.ID, "operator:alice", "clearly bad actor"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejecter.calls != 1 {
		t.Fatalf("RejectHold called %d times, want 1", rejecter.calls)
	}
	if rejecter.gotEntry["entry_type"] != "manual_refund_test" {
		t.Fatalf("RejectHold received entry %+v, want the one BuildRefundEntry produced", rejecter.gotEntry)
	}

	got, err := holds.Get(ctx, pool, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != holds.StatusRejected {
		t.Fatalf("status = %v, want StatusRejected", got.Status)
	}
	if got.ResolvedBy == nil || *got.ResolvedBy != "operator:alice" {
		t.Fatalf("resolved_by = %v, want operator:alice", got.ResolvedBy)
	}
}

func TestReject_StubRefundEntryBuilderFailsAsDocumented(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	h, err := holds.Open(ctx, pool, order, holdDecision(t, pool))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rejecter := &fakeRejecter{}
	err = holds.Reject(ctx, pool, rejecter, holds.StubRefundEntryBuilder{}, h.ID, "operator:alice", "")
	if !errors.Is(err, holds.ErrRefundEntryNotImplemented) {
		t.Fatalf("Reject with StubRefundEntryBuilder = %v, want an error wrapping ErrRefundEntryNotImplemented", err)
	}
	if rejecter.calls != 0 {
		t.Fatalf("RejectHold called %d times, want 0 -- entry building must fail before ever calling C1", rejecter.calls)
	}

	got, err := holds.Get(ctx, pool, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != holds.StatusOpen {
		t.Fatalf("status = %v, want StatusOpen -- a failed reject must not resolve the hold", got.Status)
	}
}

func TestReject_RejectsEmptyReviewerBeforeCallingC1(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	h, err := holds.Open(ctx, pool, order, holdDecision(t, pool))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	rejecter := &fakeRejecter{}
	err = holds.Reject(ctx, pool, rejecter, fakeEntryBuilder{entry: map[string]any{}}, h.ID, "", "")
	if !errors.Is(err, holds.ErrEmptyReviewer) {
		t.Fatalf("Reject with empty reviewer = %v, want ErrEmptyReviewer", err)
	}
	if rejecter.calls != 0 {
		t.Fatalf("RejectHold called %d times, want 0", rejecter.calls)
	}
}

func TestListOpen_NeverReturnsResolvedHolds(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	openOrder := uniqueOrder(t)
	releasedOrder := uniqueOrder(t)
	rejectedOrder := uniqueOrder(t)

	openHold, err := holds.Open(ctx, pool, openOrder, holdDecision(t, pool))
	if err != nil {
		t.Fatalf("Open (openHold): %v", err)
	}
	releasedHold, err := holds.Open(ctx, pool, releasedOrder, holdDecision(t, pool))
	if err != nil {
		t.Fatalf("Open (releasedHold): %v", err)
	}
	rejectedHold, err := holds.Open(ctx, pool, rejectedOrder, holdDecision(t, pool))
	if err != nil {
		t.Fatalf("Open (rejectedHold): %v", err)
	}

	if err := holds.Release(ctx, pool, &fakeReleaser{}, releasedHold.ID, "operator:alice", ""); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := holds.Reject(ctx, pool, &fakeRejecter{}, fakeEntryBuilder{entry: map[string]any{}}, rejectedHold.ID, "operator:alice", ""); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	open, err := holds.ListOpen(ctx, pool)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	var sawOpen, sawReleased, sawRejected bool
	for _, h := range open {
		switch h.ID {
		case openHold.ID:
			sawOpen = true
		case releasedHold.ID:
			sawReleased = true
		case rejectedHold.ID:
			sawRejected = true
		}
	}
	if !sawOpen {
		t.Error("ListOpen did not return the still-open hold")
	}
	if sawReleased {
		t.Error("ListOpen returned a RELEASED hold")
	}
	if sawRejected {
		t.Error("ListOpen returned a REJECTED hold")
	}
}

func TestOpen_ConcurrentCallsForSameOrderProduceExactlyOneOpenHold(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	order := uniqueOrder(t)
	decision := holdDecision(t, pool)

	var wg sync.WaitGroup
	ids := make([]int64, 20)
	errs := make([]error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := holds.Open(ctx, pool, order, decision)
			ids[i] = h.ID
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Open (goroutine %d): %v", i, err)
		}
	}
	first := ids[0]
	for i, id := range ids {
		if id != first {
			t.Fatalf("goroutine %d got hold id %d, want %d -- all concurrent Opens for the same order must agree on one row", i, id, first)
		}
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM holds WHERE order_id = $1`, order.OrderID).Scan(&count); err != nil {
		t.Fatalf("counting holds rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("got %d holds rows after 20 concurrent Opens for one order, want exactly 1", count)
	}
}
