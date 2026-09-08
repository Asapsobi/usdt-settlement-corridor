//go:build integration

package dispatch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/fbsobreira/gotron-sdk/pkg/proto/core"
	"google.golang.org/protobuf/proto"

	"dispatcher/internal/db"
	"dispatcher/internal/dispatch"
	"dispatcher/internal/money"
	"dispatcher/internal/signing"
	"dispatcher/internal/txbuild"
)

// mustUnsignedTx builds a real, valid unsigned TRC20 transfer via C5.4's
// own txbuild.BuildTransfer -- Broadcast's reconstructTransaction step
// does real protobuf unmarshaling of this value, so an arbitrary byte
// string will not do; salt varies the block reference so two calls
// produce two genuinely different (and differently content-addressed)
// transfers when the test needs that.
func mustUnsignedTx(t *testing.T, amount string, salt int64) []byte {
	t.Helper()
	amt, err := money.ParseDecimal(amount)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", amount, err)
	}
	ref := txbuild.BlockReference{
		BlockNumber: 1000 + salt,
		Timestamp:   time.UnixMilli(1700000000000 + salt),
		Expiration:  time.UnixMilli(1700000060000 + salt),
	}
	tx, err := txbuild.BuildTransfer("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", amt, ref)
	if err != nil {
		t.Fatalf("BuildTransfer: %v", err)
	}
	return tx
}

// fakeBroadcastClient stands in for a real TRON node's broadcast surface.
// txid is derived deterministically from the transaction's own raw_data,
// so two calls for the identical transfer would (if this test's own
// locking discipline ever broke) at least produce the same txid rather
// than masking a bug with random ids.
type fakeBroadcastClient struct {
	mu    sync.Mutex
	calls int
	delay time.Duration
}

func (f *fakeBroadcastClient) Broadcast(ctx context.Context, tx *core.Transaction) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	raw, err := proto.Marshal(tx.RawData)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (f *fakeBroadcastClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newDispatcherForBroadcast(pool *db.Pool, signer *signing.FakeSigningService) *dispatch.Dispatcher {
	return dispatch.NewDispatcher(nil, dispatch.NewStore(pool), dispatch.NewAttemptStore(pool), signer)
}

func TestBroadcast_HappyPath(t *testing.T) {
	signer := signing.NewFakeSigningService()
	pool := testPool(t)
	d := newDispatcherForBroadcast(pool, signer)
	chain := &fakeBroadcastClient{}

	unsignedTx := mustUnsignedTx(t, "1.000000", 1)
	attempt, err := d.Broadcast(context.Background(), 1, 1, 1, unsignedTx, 100.0, chain)
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if attempt.Status != dispatch.BroadcastBroadcast {
		t.Fatalf("Status = %q, want BROADCAST", attempt.Status)
	}
	if attempt.TronTxID == nil || *attempt.TronTxID == "" {
		t.Fatal("TronTxID is empty")
	}
	if attempt.SignedTx == nil || *attempt.SignedTx == "" {
		t.Fatal("SignedTx is empty")
	}
	if chain.callCount() != 1 {
		t.Fatalf("chain.callCount() = %d, want 1", chain.callCount())
	}
}

// TestBroadcast_ConcurrentCallsExactlyOneChainCall is C5.5's own named
// hard-parts proof: N goroutines calling Broadcast for the identical
// logical transfer (same order, same attempt, same unsignedTx) must
// result in exactly one real chain call and one BROADCAST row -- the
// same concurrency-proof discipline as C1.3's own 1,000-goroutine
// idempotency test, adapted to the chain-broadcast layer.
func TestBroadcast_ConcurrentCallsExactlyOneChainCall(t *testing.T) {
	signer := signing.NewFakeSigningService()
	pool := testPool(t)
	d := newDispatcherForBroadcast(pool, signer)
	// An artificial delay inside the fake chain call widens the race
	// window so a lock-discipline bug (e.g. missing FOR UPDATE) would
	// actually be caught, not just accidentally avoided by fast goroutine
	// scheduling.
	chain := &fakeBroadcastClient{delay: 150 * time.Millisecond}

	unsignedTx := mustUnsignedTx(t, "2.000000", 2)
	const n = 20
	results := make([]dispatch.BroadcastAttempt, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = d.Broadcast(context.Background(), 2, 1, 1, unsignedTx, 100.0, chain)
		}(i)
	}
	wg.Wait()

	var firstTxID *string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Broadcast (goroutine %d): %v", i, err)
		}
		if results[i].Status != dispatch.BroadcastBroadcast {
			t.Fatalf("Broadcast (goroutine %d) status = %q, want BROADCAST", i, results[i].Status)
		}
		if firstTxID == nil {
			firstTxID = results[i].TronTxID
		} else if results[i].TronTxID == nil || *results[i].TronTxID != *firstTxID {
			t.Fatalf("Broadcast (goroutine %d) TronTxID = %v, want %v (every goroutine must see the same outcome)", i, results[i].TronTxID, firstTxID)
		}
	}
	if got := chain.callCount(); got != 1 {
		t.Fatalf("chain.callCount() = %d, want exactly 1", got)
	}

	var rowCount int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM dispatch_attempts WHERE order_id = 2`).Scan(&rowCount); err != nil {
		t.Fatalf("counting dispatch_attempts rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("dispatch_attempts row count for order 2 = %d, want exactly 1", rowCount)
	}
}

// TestBroadcast_ResumeAfterCrashBetweenSignedAndBroadcastNeverResigns
// simulates the crash this chunk names explicitly: a first Broadcast call
// reaches SIGNED (committed, durable) but never reaches BROADCAST (this
// test uses a chain client that always fails, standing in for the
// process dying before observing any broadcast response). A resumed
// Broadcast call, now given a working chain client, must broadcast using
// the SAME stored signature -- proven here by asserting the fake
// SigningService's own RequestSignature call count never exceeds 1.
func TestBroadcast_ResumeAfterCrashBetweenSignedAndBroadcastNeverResigns(t *testing.T) {
	signer := signing.NewFakeSigningService()
	pool := testPool(t)
	d := newDispatcherForBroadcast(pool, signer)
	unsignedTx := mustUnsignedTx(t, "3.000000", 3)

	failingChain := &alwaysFailChain{}
	_, err := d.Broadcast(context.Background(), 3, 1, 1, unsignedTx, 100.0, failingChain)
	if err == nil {
		t.Fatal("Broadcast (1st, with a failing chain client): want an error, got nil")
	}
	if failingChain.callCount() != 1 {
		t.Fatalf("failingChain.callCount() = %d, want 1", failingChain.callCount())
	}

	signCallsAfterFirstAttempt := signer.RequestSignatureCallCount()
	if signCallsAfterFirstAttempt != 1 {
		t.Fatalf("RequestSignature call count after 1st Broadcast = %d, want 1", signCallsAfterFirstAttempt)
	}

	workingChain := &fakeBroadcastClient{}
	attempt, err := d.Broadcast(context.Background(), 3, 1, 1, unsignedTx, 100.0, workingChain)
	if err != nil {
		t.Fatalf("Broadcast (2nd, resumed): %v", err)
	}
	if attempt.Status != dispatch.BroadcastBroadcast {
		t.Fatalf("Status = %q, want BROADCAST", attempt.Status)
	}
	if workingChain.callCount() != 1 {
		t.Fatalf("workingChain.callCount() = %d, want 1", workingChain.callCount())
	}
	if got := signer.RequestSignatureCallCount(); got != 1 {
		t.Fatalf("RequestSignature call count after resumed Broadcast = %d, want still 1 (never re-signed)", got)
	}
}

// TestBroadcast_ForcedDuplicateSignatureStillReachesExactlyOneBroadcast
// uses FakeSigningService's own forced-duplicate-signature mode (built
// for exactly this) to prove the SYSTEM -- not just this package's own
// retry logic -- behaves correctly even if S1 itself were to misbehave
// and hand back a byte-identical signature for what should be a distinct
// request. This attempt's own unsigned_tx_hash uniqueness is what still
// protects it: a duplicated signature does not, by itself, cause a
// duplicate broadcast for its own attempt.
func TestBroadcast_ForcedDuplicateSignatureStillReachesExactlyOneBroadcast(t *testing.T) {
	signer := signing.NewFakeSigningService()
	pool := testPool(t)
	d := newDispatcherForBroadcast(pool, signer)
	chain := &fakeBroadcastClient{}

	signer.ForceDuplicateSignature("dispatcher:sign:4:1", "dispatcher:sign:5:1")

	unsignedTxA := mustUnsignedTx(t, "4.000000", 40)
	if _, err := d.Broadcast(context.Background(), 5, 1, 1, mustUnsignedTx(t, "5.000000", 50), 100.0, chain); err != nil {
		t.Fatalf("Broadcast (order 5, establishes the signature to be duplicated): %v", err)
	}

	attempt, err := d.Broadcast(context.Background(), 4, 1, 1, unsignedTxA, 100.0, chain)
	if err != nil {
		t.Fatalf("Broadcast (order 4, forced duplicate signature): %v", err)
	}
	if attempt.Status != dispatch.BroadcastBroadcast {
		t.Fatalf("Status = %q, want BROADCAST", attempt.Status)
	}
	if chain.callCount() != 2 {
		t.Fatalf("chain.callCount() = %d, want 2 (one broadcast per distinct attempt, even with a shared signature)", chain.callCount())
	}
}

type alwaysFailChain struct {
	mu    sync.Mutex
	calls int
}

func (f *alwaysFailChain) Broadcast(ctx context.Context, tx *core.Transaction) (string, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return "", context.DeadlineExceeded
}

func (f *alwaysFailChain) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
