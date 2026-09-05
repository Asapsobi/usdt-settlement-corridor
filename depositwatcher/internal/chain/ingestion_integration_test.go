//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. package chain (not chain_test): the kill-
// and-restart test below calls runIngestionTick directly for
// deterministic single-tick control, and that's unexported.
package chain

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"depositwatcher/internal/db"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("WATCHER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("WATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

func applyMigrations(t *testing.T, dbURL string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir(t)); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
}

// resetIngestionState clears seen_blocks and resets the cursor to 0, so
// each test starts from a clean slate against the shared watcher_test
// database rather than wherever a previous test run left off.
func resetIngestionState(t *testing.T, database *db.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Exec(ctx, `DELETE FROM seen_blocks`); err != nil {
		t.Fatalf("resetting seen_blocks: %v", err)
	}
	if _, err := database.Exec(ctx, `UPDATE ingestion_cursor SET last_scanned = 0 WHERE id = 1`); err != nil {
		t.Fatalf("resetting ingestion_cursor: %v", err)
	}
}

func testDBPool(t *testing.T, dbURL string) *db.Pool {
	t.Helper()
	pool, err := db.Open(context.Background(), db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// chainOfBlocks builds n consistently-linked headers (each one's
// ParentHash is the true, computed Hash() of the previous one) and
// registers them on node at heights 1..n, with node's tip set to n.
// Returns the height->hash map for callers that want to assert against
// specific stored values.
func chainOfBlocks(node *fakeNode, n uint64) map[uint64]common.Hash {
	hashes := make(map[uint64]common.Hash, n)
	var parent common.Hash
	for h := uint64(1); h <= n; h++ {
		hdr := testHeader(h, parent)
		node.setBlock(h, hdr)
		hashes[h] = hdr.Hash()
		parent = hashes[h]
	}
	node.setTip(n)
	return hashes
}

// twoProviderPoolFor builds a chain.Pool with two providers both pointing
// at the SAME fake node -- fine for these tests, which are about cursor
// and reorg-detection correctness (single-primary-provider concerns, see
// headers.go), not about re-exercising C2.2's multi-provider agreement.
func twoProviderPoolFor(t *testing.T, node *fakeNode) *Pool {
	t.Helper()
	c1, srv1 := node.client()
	c2, srv2 := node.client()
	t.Cleanup(srv1.Close)
	t.Cleanup(srv2.Close)
	pool, err := NewPool([]Provider{{Name: "A", Client: c1}, {Name: "B", Client: c2}}, Config{MinAgreement: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool
}

func seenBlocksRows(t *testing.T, database *db.Pool) (count int, minH, maxH int64) {
	t.Helper()
	ctx := context.Background()
	err := database.QueryRow(ctx, `
		SELECT count(*), COALESCE(min(height), 0), COALESCE(max(height), 0) FROM seen_blocks
	`).Scan(&count, &minH, &maxH)
	if err != nil {
		t.Fatalf("querying seen_blocks: %v", err)
	}
	return count, minH, maxH
}

func lastScanned(t *testing.T, database *db.Pool) uint64 {
	t.Helper()
	var v int64
	if err := database.QueryRow(context.Background(),
		`SELECT last_scanned FROM ingestion_cursor WHERE id = 1`).Scan(&v); err != nil {
		t.Fatalf("querying ingestion_cursor: %v", err)
	}
	return uint64(v)
}

// ---------------------------------------------------------------------
// Forward scan and cursor correctness
// ---------------------------------------------------------------------

func TestRunIngestionLoop_ScansForwardAndAdvancesCursor(t *testing.T) {
	dbURL := testDatabaseURL(t)
	applyMigrations(t, dbURL)
	database := testDBPool(t, dbURL)
	resetIngestionState(t, database)

	node := newFakeNode()
	hashes := chainOfBlocks(node, 5)
	pool := twoProviderPoolFor(t, node)

	if err := runIngestionTick(context.Background(), pool, database, DefaultSeenBlocksWindow); err != nil {
		t.Fatalf("tick failed: %v", err)
	}

	if got := lastScanned(t, database); got != 5 {
		t.Fatalf("last_scanned = %d, want 5", got)
	}
	count, minH, maxH := seenBlocksRows(t, database)
	if count != 5 || minH != 1 || maxH != 5 {
		t.Fatalf("seen_blocks: count=%d min=%d max=%d, want count=5 min=1 max=5", count, minH, maxH)
	}

	// Spot-check the stored hash for one height matches what was
	// actually derived from the header, not some placeholder.
	var stored string
	if err := database.QueryRow(context.Background(),
		`SELECT hash FROM seen_blocks WHERE height = 3`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != hashes[3].Hex() {
		t.Fatalf("seen_blocks height 3 hash = %s, want %s", stored, hashes[3].Hex())
	}

	// A second tick with nothing new must be a no-op, not an error and
	// not a re-scan.
	if err := runIngestionTick(context.Background(), pool, database, DefaultSeenBlocksWindow); err != nil {
		t.Fatalf("second (no-op) tick failed: %v", err)
	}
	if got := lastScanned(t, database); got != 5 {
		t.Fatalf("after no-op tick, last_scanned = %d, want still 5", got)
	}
}

func TestRunIngestionLoop_IsResumable(t *testing.T) {
	dbURL := testDatabaseURL(t)
	applyMigrations(t, dbURL)
	database := testDBPool(t, dbURL)
	resetIngestionState(t, database)

	node := newFakeNode()
	chainOfBlocks(node, 10)
	pool := twoProviderPoolFor(t, node)

	// Simulate the chain having only reached height 4 on the first tick
	// (e.g., that's all that existed yet), then advancing later.
	node.setTip(4)
	if err := runIngestionTick(context.Background(), pool, database, DefaultSeenBlocksWindow); err != nil {
		t.Fatal(err)
	}
	if got := lastScanned(t, database); got != 4 {
		t.Fatalf("last_scanned = %d, want 4", got)
	}

	node.setTip(10)
	if err := runIngestionTick(context.Background(), pool, database, DefaultSeenBlocksWindow); err != nil {
		t.Fatal(err)
	}
	if got := lastScanned(t, database); got != 10 {
		t.Fatalf("last_scanned = %d, want 10 after resuming", got)
	}
	count, minH, maxH := seenBlocksRows(t, database)
	if count != 10 || minH != 1 || maxH != 10 {
		t.Fatalf("seen_blocks: count=%d min=%d max=%d, want count=10 min=1 max=10 -- "+
			"a gap or a skip means resumption re-derived the wrong starting point", count, minH, maxH)
	}
}

// ---------------------------------------------------------------------
// Pre-final reorg detection (advisory only)
// ---------------------------------------------------------------------

func TestRunIngestionLoop_DetectsParentHashMismatch(t *testing.T) {
	dbURL := testDatabaseURL(t)
	applyMigrations(t, dbURL)
	database := testDBPool(t, dbURL)
	resetIngestionState(t, database)

	node := newFakeNode()
	chainOfBlocks(node, 5)
	pool := twoProviderPoolFor(t, node)

	if err := runIngestionTick(context.Background(), pool, database, DefaultSeenBlocksWindow); err != nil {
		t.Fatal(err)
	}
	if got := lastScanned(t, database); got != 5 {
		t.Fatalf("last_scanned = %d, want 5", got)
	}

	// Block 6 claims a parent hash that does NOT match what was actually
	// stored for block 5 -- a deliberately injected pre-final reorg,
	// exactly inside the seen_blocks window.
	var wrongParent common.Hash
	wrongParent[31] = 0xEE
	badHeader := testHeader(6, wrongParent)
	node.setBlock(6, badHeader)
	node.setTip(6)

	logs := captureSlog(t)
	if err := runIngestionTick(context.Background(), pool, database, DefaultSeenBlocksWindow); err != nil {
		t.Fatalf("tick with a mismatch must not error -- advisory only, loop must continue: %v", err)
	}

	if got := lastScanned(t, database); got != 6 {
		t.Fatalf("last_scanned = %d, want 6 -- the loop must continue past a pre-final reorg, not stall", got)
	}
	count, _, maxH := seenBlocksRows(t, database)
	if count != 6 || maxH != 6 {
		t.Fatalf("seen_blocks: count=%d max=%d, want count=6 max=6", count, maxH)
	}
	if !bytes.Contains(logs.Bytes(), []byte("pre-final reorg detected")) {
		t.Fatalf("expected a pre-final reorg warning to be logged; got:\n%s", logs.String())
	}
}

// ---------------------------------------------------------------------
// Pruning stays bounded
// ---------------------------------------------------------------------

func TestRunIngestionLoop_SeenBlocksNeverGrowsUnbounded(t *testing.T) {
	dbURL := testDatabaseURL(t)
	applyMigrations(t, dbURL)
	database := testDBPool(t, dbURL)
	resetIngestionState(t, database)

	node := newFakeNode()
	const totalBlocks = 1000
	const window = 10
	chainOfBlocks(node, totalBlocks)
	pool := twoProviderPoolFor(t, node)

	// One tick that has to walk all 1000 blocks -- functionally
	// equivalent to 1000 separate ticks arriving one block apart, and
	// much faster to run repeatedly than an actual wall-clock soak,
	// while exercising the exact same repeated insert-then-prune path
	// on every single block, not just a single before/after snapshot.
	if err := runIngestionTick(context.Background(), pool, database, window); err != nil {
		t.Fatal(err)
	}

	if got := lastScanned(t, database); got != totalBlocks {
		t.Fatalf("last_scanned = %d, want %d", got, totalBlocks)
	}
	count, minH, maxH := seenBlocksRows(t, database)
	if count != window {
		t.Fatalf("seen_blocks has %d rows after scanning %d blocks with window=%d -- it must stay bounded at the window size, not grow with total blocks scanned",
			count, totalBlocks, window)
	}
	if maxH != totalBlocks || minH != totalBlocks-window+1 {
		t.Fatalf("seen_blocks retains heights [%d,%d], want the most recent %d heights ([%d,%d])",
			minH, maxH, window, totalBlocks-window+1, totalBlocks)
	}
}

// ---------------------------------------------------------------------
// slog capture
// ---------------------------------------------------------------------

// captureSlog redirects the default slog logger to a buffer for the
// duration of the test, restoring the previous default on cleanup.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// ---------------------------------------------------------------------
// Real SIGKILL mid-scan, then resume
// ---------------------------------------------------------------------

// waitForCursorBetween polls ingestion_cursor until last_scanned is
// strictly between low and high (exclusive), returning that value. Fails
// the test if timeout elapses first. This is how the parent process
// synchronizes with the subprocess worker without any IPC beyond the
// shared database both of them are already talking to.
func waitForCursorBetween(t *testing.T, database *db.Pool, low, high uint64, timeout time.Duration) uint64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		v := lastScanned(t, database)
		if v > low && v < high {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("last_scanned never reached (%d,%d) within %s (stuck at %d)",
		low, high, timeout, lastScanned(t, database))
	return 0
}

// TestRunIngestionLoop_KillMidScanAndRestart is the acceptance criterion
// that requires a REAL SIGKILL of a REAL separate process, not a
// cancelled context -- an in-process goroutine cancellation is a graceful
// shutdown, a materially different (and already well-covered) code path.
// This spawns the test binary itself as a subprocess (the standard Go
// pattern for testing process-kill semantics), running
// TestHelperIngestionWorker below against a fake node this process keeps
// alive on a real local TCP port, kills it while genuinely mid-scan
// (synchronized via polling the shared database, not a timing guess), and
// verifies both that nothing was corrupted by the kill and that resuming
// afterward completes the job with no block skipped or duplicated.
func TestRunIngestionLoop_KillMidScanAndRestart(t *testing.T) {
	dbURL := testDatabaseURL(t)
	applyMigrations(t, dbURL)
	database := testDBPool(t, dbURL)
	resetIngestionState(t, database)

	node := newFakeNode()
	const totalBlocks = 40
	chainOfBlocks(node, totalBlocks)
	// An artificial per-block delay so a 40-block scan takes long enough
	// (~800ms) to reliably catch mid-scan, rather than racing to finish
	// before the parent's first poll.
	node.setFinalizedDelay(20 * time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(node.handle))
	defer srv.Close()

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperIngestionWorker", "-test.v")
	cmd.Env = append(os.Environ(),
		"GO_WANT_INGESTION_WORKER=1",
		"FAKE_NODE_URL="+srv.URL,
		"WATCHER_TEST_DATABASE_URL="+dbURL,
	)
	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting worker subprocess: %v", err)
	}

	killedAt := waitForCursorBetween(t, database, 3, totalBlocks-3, 10*time.Second)
	t.Logf("observed last_scanned=%d, sending SIGKILL now", killedAt)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("killing worker: %v", err)
	}
	_ = cmd.Wait() // expected to report a kill-related error; not asserted on -- the DB state is what matters

	// The cursor may have advanced slightly further than killedAt if a
	// block or two committed between the poll and the kill signal
	// actually landing -- that's fine, it only needs to be consistent
	// with whatever seen_blocks actually holds.
	afterKill := lastScanned(t, database)
	count, minH, maxH := seenBlocksRows(t, database)
	if int64(afterKill) != maxH || count != int(afterKill) || minH != 1 {
		t.Fatalf("post-kill state is inconsistent: cursor=%d, seen_blocks count=%d min=%d max=%d "+
			"-- these must agree exactly (cursor == max height == row count == 1..cursor with no gaps)\nworker stdout:\n%s\nworker stderr:\n%s",
			afterKill, count, minH, maxH, stdout.String(), stderr.String())
	}
	if afterKill >= totalBlocks {
		t.Fatalf("worker was not actually caught mid-scan (cursor already at %d of %d) -- "+
			"increase the artificial delay or block count so the kill lands earlier", afterKill, totalBlocks)
	}

	// Resume -- in-process this time; only the KILL needed to be a real
	// separate OS process. Remove the artificial delay so this completes
	// quickly.
	node.setFinalizedDelay(0)
	pool := twoProviderPoolFor(t, node)
	if err := runIngestionTick(context.Background(), pool, database, DefaultSeenBlocksWindow); err != nil {
		t.Fatalf("resumed tick failed: %v", err)
	}

	if got := lastScanned(t, database); got != totalBlocks {
		t.Fatalf("last_scanned = %d after resuming, want %d", got, totalBlocks)
	}
	count, minH, maxH = seenBlocksRows(t, database)
	if count != totalBlocks || minH != 1 || maxH != totalBlocks {
		t.Fatalf("after resuming: seen_blocks count=%d min=%d max=%d, want count=%d min=1 max=%d "+
			"-- a mismatch here means a block was skipped or the kill left a corrupt/duplicated row",
			count, minH, maxH, totalBlocks, totalBlocks)
	}
}

// TestHelperIngestionWorker is not a real test on its own -- it is a
// subprocess entry point, invoked only when GO_WANT_INGESTION_WORKER=1,
// that runs the real ingestion loop against a real database and a fake
// node reachable over real HTTP, until killed by its parent
// (TestRunIngestionLoop_KillMidScanAndRestart). `go test -run
// TestHelperIngestionWorker` without that env var set is a harmless
// no-op skip, so this never interferes with a normal test run.
func TestHelperIngestionWorker(t *testing.T) {
	if os.Getenv("GO_WANT_INGESTION_WORKER") != "1" {
		t.Skip("only runs as a subprocess spawned by TestRunIngestionLoop_KillMidScanAndRestart")
	}

	ctx := context.Background()
	nodeURL := os.Getenv("FAKE_NODE_URL")
	dbURL := os.Getenv("WATCHER_TEST_DATABASE_URL")

	client1, err := ethclient.DialContext(ctx, nodeURL)
	if err != nil {
		t.Fatalf("dialing fake node (1): %v", err)
	}
	client2, err := ethclient.DialContext(ctx, nodeURL)
	if err != nil {
		t.Fatalf("dialing fake node (2): %v", err)
	}
	pool, err := NewPool([]Provider{{Name: "A", Client: client1}, {Name: "B", Client: client2}}, Config{MinAgreement: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	database, err := db.Open(ctx, db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}

	// Interval is irrelevant: the immediate first-tick scan alone walks
	// the whole configured chain and, with the parent's artificial
	// per-block delay, takes long enough to be killed mid-way through.
	// This call is expected to never return normally -- the parent kills
	// this process before ctx is ever cancelled.
	err = RunIngestionLoop(ctx, pool, database, IngestionConfig{Interval: time.Hour})
	fmt.Fprintf(os.Stderr, "RunIngestionLoop returned (unexpected if still alive): %v\n", err)
}
