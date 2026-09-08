//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package requests_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"s1/internal/db"
	"s1/internal/kmssign"
	"s1/internal/requests"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("S1_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("S1_TEST_DATABASE_URL not set; skipping integration test")
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

	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `TRUNCATE signing_requests, signing_audit_log, signing_approvals RESTART IDENTITY`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

// fakeSlotKeyGetter is a minimal, in-memory requests.SlotKeyGetter --
// this package's own tests care about the request/approval queue logic,
// not internal/slots' own storage (that has its own, separately-tested
// integration tests).
type fakeSlotKeyGetter struct {
	keys map[int]requests.SlotKeyInfo
}

func (f fakeSlotKeyGetter) Get(ctx context.Context, slotID int) (requests.SlotKeyInfo, error) {
	key, ok := f.keys[slotID]
	if !ok {
		return requests.SlotKeyInfo{}, fmt.Errorf("no key configured for slot %d", slotID)
	}
	return key, nil
}

// countingSigner wraps a real Signer, counting every call -- lets these
// tests assert "exactly one real KMS call happened" the same
// call-count-not-just-outcome discipline energybroker's own MockProvider
// tests use. failNextCalls, when positive, makes that many subsequent
// calls fail (decremented per call) before falling through to inner --
// used to prove a KMS failure mid-approval doesn't lose the recorded
// approvals (S1.4's own acceptance criterion).
type countingSigner struct {
	inner         requests.Signer
	calls         int64
	failNextCalls int64
}

func (c *countingSigner) Sign(ctx context.Context, keyID string, digest [32]byte, expectedPubKey [33]byte) ([65]byte, error) {
	atomic.AddInt64(&c.calls, 1)
	if atomic.LoadInt64(&c.failNextCalls) > 0 {
		atomic.AddInt64(&c.failNextCalls, -1)
		return [65]byte{}, fmt.Errorf("countingSigner: forced failure")
	}
	return c.inner.Sign(ctx, keyID, digest, expectedPubKey)
}

func (c *countingSigner) forceFailNext(n int64) { atomic.StoreInt64(&c.failNextCalls, n) }

func (c *countingSigner) callCount() int64 { return atomic.LoadInt64(&c.calls) }

func newTestStore(t *testing.T, pool *db.Pool, thresholdUSD float64) (*requests.Store, *countingSigner, requests.SlotKeyInfo) {
	t.Helper()
	fakeKMS := kmssign.NewFakeKMSClient(1)
	wrapper := kmssign.NewWrapper(fakeKMS)
	pubKey, err := wrapper.GetPublicKey(context.Background(), "kms-key-slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	slotKey := requests.SlotKeyInfo{KMSKeyID: "kms-key-slot-1", PublicKey: pubKey, TronAddress: "TFakeSlotAddress00000000000001"}

	signer := &countingSigner{inner: wrapper}
	slotGetter := fakeSlotKeyGetter{keys: map[int]requests.SlotKeyInfo{1: slotKey}}
	store := requests.NewStore(pool, slotGetter, signer, requests.Config{ApprovalThresholdUSD: thresholdUSD})
	return store, signer, slotKey
}

func TestRequestSignature_UnderThresholdSignsSynchronously(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, signer, _ := newTestStore(t, pool, 10000)

	req, err := store.RequestSignature(ctx, 1, [32]byte{1, 2, 3}, 5000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}
	if req.Status != requests.StatusSigned {
		t.Fatalf("Status = %s, want SIGNED", req.Status)
	}
	if req.SignedTx == ([65]byte{}) {
		t.Fatal("SignedTx is empty on a SIGNED request")
	}
	if got := signer.callCount(); got != 1 {
		t.Fatalf("Signer was called %d times, want exactly 1", got)
	}

	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM signing_audit_log WHERE signing_request_id = $1`, req.ID).Scan(&auditCount); err != nil {
		t.Fatalf("counting audit rows: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("signing_audit_log rows for request %d = %d, want exactly 1", req.ID, auditCount)
	}
}

func TestRequestSignature_AtOrAboveThresholdStaysPendingWithZeroKMSCalls(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, signer, _ := newTestStore(t, pool, 10000)

	req, err := store.RequestSignature(ctx, 1, [32]byte{1}, 10000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}
	if req.Status != requests.StatusPending {
		t.Fatalf("Status = %s, want PENDING for a request at the threshold", req.Status)
	}
	if got := signer.callCount(); got != 0 {
		t.Fatalf("Signer was called %d times, want 0", got)
	}
}

func TestRequestSignature_IdempotentReplayNeverSignsTwice(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, signer, _ := newTestStore(t, pool, 10000)

	first, err := store.RequestSignature(ctx, 1, [32]byte{1}, 5000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature (1st): %v", err)
	}
	second, err := store.RequestSignature(ctx, 1, [32]byte{9, 9}, 999999, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature (2nd, replayed): %v", err)
	}
	if second.ID != first.ID || second.Status != first.Status || second.SignedTx != first.SignedTx {
		t.Fatalf("replayed request = %+v, want identical to first %+v", second, first)
	}
	if got := signer.callCount(); got != 1 {
		t.Fatalf("Signer was called %d times across both calls, want exactly 1", got)
	}
}

func TestRequestSignature_ConcurrentSameIdempotencyKeyYieldsOneRequestAndOneSignCall(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, signer, _ := newTestStore(t, pool, 10000)

	const n = 20
	var wg sync.WaitGroup
	ids := make([]int64, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := store.RequestSignature(ctx, 1, [32]byte{1}, 5000, "idem-concurrent")
			ids[i] = req.ID
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("RequestSignature (goroutine %d): %v", i, err)
		}
	}
	first := ids[0]
	for _, id := range ids {
		if id != first {
			t.Fatalf("concurrent calls with the same idempotency key produced different request ids: %v", ids)
		}
	}
	if got := signer.callCount(); got != 1 {
		t.Fatalf("Signer was called %d times across %d concurrent requests, want exactly 1", got, n)
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM signing_requests WHERE idempotency_key = 'idem-concurrent'`).Scan(&rowCount); err != nil {
		t.Fatalf("counting signing_requests: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("signing_requests rows = %d, want exactly 1", rowCount)
	}
}

func TestGetSignature_UnknownIDReturnsErrRequestNotFound(t *testing.T) {
	pool := testPool(t)
	store, _, _ := newTestStore(t, pool, 10000)

	if _, err := store.GetSignature(context.Background(), 999999); err != requests.ErrRequestNotFound {
		t.Fatalf("GetSignature error = %v, want ErrRequestNotFound", err)
	}
}

func TestSlotAddress_ResolvesFromSlotKeyGetter(t *testing.T) {
	pool := testPool(t)
	store, _, slotKey := newTestStore(t, pool, 10000)

	addr, err := store.SlotAddress(context.Background(), 1)
	if err != nil {
		t.Fatalf("SlotAddress: %v", err)
	}
	if addr != slotKey.TronAddress {
		t.Fatalf("SlotAddress = %q, want %q", addr, slotKey.TronAddress)
	}
}

func TestApprove_OneApprovalStaysPending(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, signer, _ := newTestStore(t, pool, 10000)

	req, err := store.RequestSignature(ctx, 1, [32]byte{1}, 50000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}

	got, err := store.Approve(ctx, req.ID, "alice")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got.Status != requests.StatusPending {
		t.Fatalf("Status = %s, want PENDING after one approval", got.Status)
	}
	if signer.callCount() != 0 {
		t.Fatalf("Signer was called %d times after one approval, want 0", signer.callCount())
	}
}

func TestApprove_TwoDistinctApproversSigns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, signer, _ := newTestStore(t, pool, 10000)

	req, err := store.RequestSignature(ctx, 1, [32]byte{1}, 50000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}
	if _, err := store.Approve(ctx, req.ID, "alice"); err != nil {
		t.Fatalf("Approve (alice): %v", err)
	}
	got, err := store.Approve(ctx, req.ID, "bob")
	if err != nil {
		t.Fatalf("Approve (bob): %v", err)
	}
	if got.Status != requests.StatusSigned {
		t.Fatalf("Status = %s, want SIGNED after two distinct approvals", got.Status)
	}
	if signer.callCount() != 1 {
		t.Fatalf("Signer was called %d times, want exactly 1", signer.callCount())
	}

	var approvers []string
	rows, err := pool.Query(ctx, `SELECT approvers FROM signing_audit_log WHERE signing_request_id = $1`, req.ID)
	if err != nil {
		t.Fatalf("querying audit log: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no audit log row for request")
	}
	if err := rows.Scan(&approvers); err != nil {
		t.Fatalf("scanning approvers: %v", err)
	}
	if len(approvers) != 2 {
		t.Fatalf("audit log approvers = %v, want exactly 2 names", approvers)
	}
}

func TestApprove_SameApproverTwiceStaysPending(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, signer, _ := newTestStore(t, pool, 10000)

	req, err := store.RequestSignature(ctx, 1, [32]byte{1}, 50000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}
	if _, err := store.Approve(ctx, req.ID, "alice"); err != nil {
		t.Fatalf("Approve (1st): %v", err)
	}
	got, err := store.Approve(ctx, req.ID, "alice")
	if err != nil {
		t.Fatalf("Approve (2nd, same approver): %v", err)
	}
	if got.Status != requests.StatusPending {
		t.Fatalf("Status = %s, want still PENDING -- the same approver approving twice must not count as two votes", got.Status)
	}
	if signer.callCount() != 0 {
		t.Fatalf("Signer was called %d times, want 0", signer.callCount())
	}
}

func TestReject_VetoesRegardlessOfPriorApprovals(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, signer, _ := newTestStore(t, pool, 10000)

	req, err := store.RequestSignature(ctx, 1, [32]byte{1}, 50000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}
	if _, err := store.Approve(ctx, req.ID, "alice"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	got, err := store.Reject(ctx, req.ID, "carol")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if got.Status != requests.StatusRejected {
		t.Fatalf("Status = %s, want REJECTED", got.Status)
	}
	if signer.callCount() != 0 {
		t.Fatalf("Signer was called %d times, want 0 -- a rejected request must never sign", signer.callCount())
	}

	if _, err := store.Approve(ctx, req.ID, "bob"); err != requests.ErrRequestAlreadyResolved {
		t.Fatalf("Approve after REJECTED error = %v, want ErrRequestAlreadyResolved", err)
	}

	// Idempotent: rejecting an already-REJECTED request is a no-op, not
	// an error.
	got2, err := store.Reject(ctx, req.ID, "dave")
	if err != nil {
		t.Fatalf("Reject (already REJECTED): %v", err)
	}
	if got2.Status != requests.StatusRejected {
		t.Fatalf("Status = %s, want still REJECTED", got2.Status)
	}
}

func TestApprove_KMSFailureOnSecondApprovalKeepsBothApprovalsForARetry(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store, signer, _ := newTestStore(t, pool, 10000)

	req, err := store.RequestSignature(ctx, 1, [32]byte{1}, 50000, "idem-1")
	if err != nil {
		t.Fatalf("RequestSignature: %v", err)
	}
	if _, err := store.Approve(ctx, req.ID, "alice"); err != nil {
		t.Fatalf("Approve (alice): %v", err)
	}

	signer.forceFailNext(1)
	if _, err := store.Approve(ctx, req.ID, "bob"); err == nil {
		t.Fatal("Approve (bob, forced KMS failure): want an error, got nil")
	}

	got, err := store.GetSignature(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetSignature: %v", err)
	}
	if got.Status != requests.StatusPending {
		t.Fatalf("Status after a failed sign attempt = %s, want still PENDING", got.Status)
	}

	var approvalCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM signing_approvals WHERE signing_request_id = $1 AND decision = 'APPROVE'`, req.ID).Scan(&approvalCount); err != nil {
		t.Fatalf("counting approvals: %v", err)
	}
	if approvalCount != 2 {
		t.Fatalf("recorded approvals after a failed sign = %d, want both (alice, bob) still recorded", approvalCount)
	}

	// Retry: a third approver's own approval completes the sign (still
	// only needs >= 2 distinct approvers; the retry doesn't require
	// re-approving from alice/bob specifically).
	got2, err := store.Approve(ctx, req.ID, "carol")
	if err != nil {
		t.Fatalf("Approve (carol, retry): %v", err)
	}
	if got2.Status != requests.StatusSigned {
		t.Fatalf("Status after retry = %s, want SIGNED", got2.Status)
	}
}
