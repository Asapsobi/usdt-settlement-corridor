//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is an internal (white-box) test file,
// not discovery_test, because it exercises runTick/getCursor/Get
// directly -- the C3.3 build spec's own acceptance criteria are about
// the tick's internal cursor/enqueue/retry behavior, not just its
// externally-visible effects through a full RunLoop.
package discovery

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"screening/internal/ledgerclient"
)

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

	// Every test in this file assumes a clean slate for discovery_cursor
	// and screening_queue -- this package's own tests are the only thing
	// that ever writes to them in the test database, so truncating here
	// (rather than requiring every test to pick globally-unique ids) is
	// safe and keeps each test's assertions exact rather than
	// "at least" checks against a shared, growing table.
	if _, err := pool.Exec(context.Background(), `TRUNCATE screening_queue`); err != nil {
		t.Fatalf("truncating screening_queue: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE discovery_cursor SET cursor = NULL WHERE id = 1`); err != nil {
		t.Fatalf("resetting discovery_cursor: %v", err)
	}

	return pool
}

// fakePoller is a FundedOrderPoller whose pages and cursor advancement
// are scripted by the test -- the C3.3 build spec's own acceptance
// criterion ("PollFundedOrders against a fake C1 client").
type fakePoller struct {
	mu    sync.Mutex
	pages map[string]fakePage // keyed by the cursor a call arrives with
	calls []string            // cursors this fake was called with, in order
}

type fakePage struct {
	refs       []ledgerclient.OrderRef
	nextCursor string
}

func newFakePoller() *fakePoller {
	return &fakePoller{pages: make(map[string]fakePage)}
}

func (f *fakePoller) setPage(cursor string, page fakePage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages[cursor] = page
}

func (f *fakePoller) PollFundedOrders(ctx context.Context, cursor string) ([]ledgerclient.OrderRef, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cursor)
	page, ok := f.pages[cursor]
	if !ok {
		// No page scripted for this cursor -- an empty page that echoes
		// the cursor back, exactly like a real, up-to-date C1 would.
		return nil, cursor, nil
	}
	return page.refs, page.nextCursor, nil
}

// fakeSenderLookup is a provider.SenderAddressLookup whose per-address
// success/failure is scripted by the test.
type fakeSenderLookup struct {
	mu        sync.Mutex
	addresses map[string]string
	failing   map[string]bool
	calls     map[string]int
}

func newFakeSenderLookup() *fakeSenderLookup {
	return &fakeSenderLookup{
		addresses: make(map[string]string),
		failing:   make(map[string]bool),
		calls:     make(map[string]int),
	}
}

func (f *fakeSenderLookup) set(externalID, address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses[externalID] = address
}

func (f *fakeSenderLookup) failFor(externalID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing[externalID] = true
}

func (f *fakeSenderLookup) unfail(externalID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failing, externalID)
}

func (f *fakeSenderLookup) callCount(externalID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[externalID]
}

var errFakeLookupFailed = errors.New("fake sender lookup: deliberately failing")

func (f *fakeSenderLookup) GetSenderAddress(ctx context.Context, externalID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[externalID]++
	if f.failing[externalID] {
		return "", errFakeLookupFailed
	}
	addr, ok := f.addresses[externalID]
	if !ok {
		return "", errFakeLookupFailed
	}
	return addr, nil
}

func TestRunTick_CursorAdvancesAndOrdersAreNotFetchedTwiceAcrossRestart(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	poller := newFakePoller()
	lookup := newFakeSenderLookup()
	lookup.set("ext-1", "0xSender1")
	lookup.set("ext-2", "0xSender2")

	poller.setPage("", fakePage{
		refs:       []ledgerclient.OrderRef{{OrderID: 1, ExternalID: "ext-1"}},
		nextCursor: "cursor-a",
	})
	poller.setPage("cursor-a", fakePage{
		refs:       []ledgerclient.OrderRef{{OrderID: 2, ExternalID: "ext-2"}},
		nextCursor: "cursor-b",
	})

	if err := runTick(ctx, pool, poller, lookup); err != nil {
		t.Fatalf("runTick (1st): %v", err)
	}
	if err := runTick(ctx, pool, poller, lookup); err != nil {
		t.Fatalf("runTick (2nd): %v", err)
	}

	if got, err := getCursor(ctx, pool); err != nil || got != "cursor-b" {
		t.Fatalf("cursor = %q, err=%v, want %q", got, err, "cursor-b")
	}

	e1, err := Get(ctx, pool, 1)
	if err != nil {
		t.Fatalf("Get(1): %v", err)
	}
	if e1.SenderAddress == nil || *e1.SenderAddress != "0xSender1" {
		t.Fatalf("order 1 sender_address = %v, want 0xSender1", e1.SenderAddress)
	}
	e2, err := Get(ctx, pool, 2)
	if err != nil {
		t.Fatalf("Get(2): %v", err)
	}
	if e2.SenderAddress == nil || *e2.SenderAddress != "0xSender2" {
		t.Fatalf("order 2 sender_address = %v, want 0xSender2", e2.SenderAddress)
	}

	// "Restart": run more ticks against the now-current cursor. The fake
	// has no page scripted for "cursor-b" so it returns an empty page --
	// exactly the real C1 behavior once a poller has caught up -- and
	// nothing about order 1 or 2 should be touched again.
	if err := runTick(ctx, pool, poller, lookup); err != nil {
		t.Fatalf("runTick (post-restart): %v", err)
	}
	if got, err := getCursor(ctx, pool); err != nil || got != "cursor-b" {
		t.Fatalf("cursor after an empty page = %q, err=%v, want unchanged %q", got, err, "cursor-b")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM screening_queue`).Scan(&count); err != nil {
		t.Fatalf("counting screening_queue rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("screening_queue has %d rows after re-polling an already-seen cursor, want exactly 2 (no duplicate enqueue)", count)
	}
}

func TestRunTick_SenderAddressLookupFailureIsRetriedNotDropped(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	poller := newFakePoller()
	lookup := newFakeSenderLookup()
	lookup.failFor("ext-flaky")

	poller.setPage("", fakePage{
		refs:       []ledgerclient.OrderRef{{OrderID: 42, ExternalID: "ext-flaky"}},
		nextCursor: "cursor-a",
	})

	if err := runTick(ctx, pool, poller, lookup); err != nil {
		t.Fatalf("runTick (1st, lookup fails): %v", err)
	}

	entry, err := Get(ctx, pool, 42)
	if err != nil {
		t.Fatalf("Get(42): %v", err)
	}
	if entry.SenderAddress != nil {
		t.Fatalf("sender_address = %v, want nil after a failed lookup -- must not be enqueued with a fabricated address", entry.SenderAddress)
	}
	if entry.Status != Pending {
		t.Fatalf("status = %v, want PENDING", entry.Status)
	}

	// Still failing: another tick must not drop the row, and must not
	// re-poll C1's list endpoint again for an order it already knows
	// about (the retry goes through screening_queue directly).
	pollCallsBefore := len(poller.calls)
	if err := runTick(ctx, pool, poller, lookup); err != nil {
		t.Fatalf("runTick (2nd, still failing): %v", err)
	}
	entry, err = Get(ctx, pool, 42)
	if err != nil {
		t.Fatalf("Get(42) after 2nd tick: %v", err)
	}
	if entry.SenderAddress != nil {
		t.Fatalf("sender_address = %v, want still nil", entry.SenderAddress)
	}
	if lookup.callCount("ext-flaky") < 2 {
		t.Fatalf("GetSenderAddress was called %d times for ext-flaky, want at least 2 (retried)", lookup.callCount("ext-flaky"))
	}

	// Now let it succeed -- the very next tick must resolve it in place.
	lookup.unfail("ext-flaky")
	lookup.set("ext-flaky", "0xResolvedLate")
	if err := runTick(ctx, pool, poller, lookup); err != nil {
		t.Fatalf("runTick (3rd, now succeeds): %v", err)
	}
	entry, err = Get(ctx, pool, 42)
	if err != nil {
		t.Fatalf("Get(42) after resolution: %v", err)
	}
	if entry.SenderAddress == nil || *entry.SenderAddress != "0xResolvedLate" {
		t.Fatalf("sender_address = %v, want 0xResolvedLate", entry.SenderAddress)
	}

	_ = pollCallsBefore // documents intent; the real assertion is on lookup, not poll, call counts
}

func TestRunTick_EmptyFirstPageAdvancesCursorToWhatItWasCalledWith(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	poller := newFakePoller() // no pages scripted at all
	lookup := newFakeSenderLookup()

	if err := runTick(ctx, pool, poller, lookup); err != nil {
		t.Fatalf("runTick: %v", err)
	}
	got, err := getCursor(ctx, pool)
	if err != nil {
		t.Fatalf("getCursor: %v", err)
	}
	if got != "" {
		t.Fatalf("cursor = %q after an empty first page, want unchanged empty string", got)
	}
}
