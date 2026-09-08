package kmssign

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"sync"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// ErrForcedTimeout and ErrForcedMalformed are FakeKMSClient's own typed
// errors for its two forced-failure modes -- distinct types so a test can
// assert which one it triggered, not just that "an error happened".
var (
	ErrForcedTimeout   = fmt.Errorf("kmssign: fake KMS call forced to time out")
	ErrForcedMalformed = fmt.Errorf("kmssign: fake KMS call forced to return malformed DER")
)

// FakeKMSClient is a deterministic, seeded KMSClient: the same (seed,
// keyID) pair always generates the same in-memory secp256k1 keypair,
// first-use-lazily, and every Sign call produces a REAL, independently
// verifiable DER signature over the real requested digest -- never a
// placeholder -- so code built against this fake (Wrapper, in S1.1) is
// tested against genuinely correct cryptography, not an approximation of
// it. Private key material lives only in this fake's own process memory
// for the life of the test process; no method on this type ever returns
// it, mirroring the one real guarantee a production KMS provides.
type FakeKMSClient struct {
	seed int64

	mu             sync.Mutex
	keys           map[string]*secp256k1.PrivateKey
	forceTimeout   map[string]bool
	forceMalformed map[string]bool
	forceWrongSig  map[string]bool
	signCalls      map[string]int
}

// NewFakeKMSClient returns a FakeKMSClient seeded by seed. Two
// FakeKMSClients constructed with the same seed generate the same keypair
// for the same keyID.
func NewFakeKMSClient(seed int64) *FakeKMSClient {
	return &FakeKMSClient{
		seed:           seed,
		keys:           make(map[string]*secp256k1.PrivateKey),
		forceTimeout:   make(map[string]bool),
		forceMalformed: make(map[string]bool),
		forceWrongSig:  make(map[string]bool),
		signCalls:      make(map[string]int),
	}
}

// ForceTimeout configures every subsequent call for keyID to block until
// ctx is done, returning ctx.Err() -- exactly what a hung real KMS call
// would look like to a caller that applied its own timeout, the same
// discipline energybroker's own MockProvider.ForceTimeout established.
func (f *FakeKMSClient) ForceTimeout(keyID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceTimeout[keyID] = true
}

// ForceMalformed configures every subsequent Sign/GetPublicKey call for
// keyID to return ErrForcedMalformed -- simulating a KMS response this
// client can't parse, wrapped by the real caller (Wrapper, S1.1) into its
// own ErrMalformedSignature/ErrMalformedPublicKey.
func (f *FakeKMSClient) ForceMalformed(keyID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceMalformed[keyID] = true
}

// ForceWrongDigestSignature configures every subsequent Sign call for
// keyID to return a syntactically valid DER signature that verifies
// against a DIFFERENT digest than the one requested -- the negative case
// Wrapper.Sign's own recovery-id matching (S1.1) must reject via
// ErrSignatureDoesNotMatchKey, proving that check is real and not just
// theoretically reachable.
func (f *FakeKMSClient) ForceWrongDigestSignature(keyID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forceWrongSig[keyID] = true
}

// ClearForced removes every Force* condition configured for keyID --
// lets a test or the replay harness simulate a KMS outage recovering,
// rather than being stuck forced for the rest of a shared instance's
// life.
func (f *FakeKMSClient) ClearForced(keyID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.forceTimeout, keyID)
	delete(f.forceMalformed, keyID)
	delete(f.forceWrongSig, keyID)
}

// SignCallCount reports how many times Sign has been called for keyID --
// exported, mirroring energybroker's own MockProvider.DelegateCallCount,
// so a caller can assert a call count directly rather than only an
// outcome.
func (f *FakeKMSClient) SignCallCount(keyID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signCalls[keyID]
}

func (f *FakeKMSClient) keyFor(keyID string) *secp256k1.PrivateKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key, ok := f.keys[keyID]; ok {
		return key
	}
	// Deterministic per (f.seed, keyID): a seeded PRNG keyed off both,
	// producing 32 bytes reduced mod the curve order by PrivKeyFromBytes
	// -- fine for a test fixture (a very slight modular bias is
	// irrelevant here), never used for anything real.
	h := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", f.seed, keyID)))
	r := rand.New(rand.NewSource(int64(binaryLE(h[:8]))))
	var seedBytes [32]byte
	_, _ = r.Read(seedBytes[:])
	key := secp256k1.PrivKeyFromBytes(seedBytes[:])
	f.keys[keyID] = key
	return key
}

func binaryLE(b []byte) uint64 {
	var v uint64
	for i, c := range b {
		v |= uint64(c) << (8 * i)
	}
	return v
}

// GetPublicKey implements KMSClient.
func (f *FakeKMSClient) GetPublicKey(ctx context.Context, keyID string) ([]byte, error) {
	f.mu.Lock()
	timeout := f.forceTimeout[keyID]
	malformed := f.forceMalformed[keyID]
	f.mu.Unlock()

	if timeout {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if malformed {
		return nil, ErrForcedMalformed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key := f.keyFor(keyID)
	var compressed [33]byte
	copy(compressed[:], key.PubKey().SerializeCompressed())
	return marshalDERPublicKey(compressed)
}

// Sign implements KMSClient.
func (f *FakeKMSClient) Sign(ctx context.Context, keyID string, digest [32]byte) ([]byte, error) {
	f.mu.Lock()
	f.signCalls[keyID]++
	timeout := f.forceTimeout[keyID]
	malformed := f.forceMalformed[keyID]
	wrongSig := f.forceWrongSig[keyID]
	f.mu.Unlock()

	if timeout {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if malformed {
		return nil, ErrForcedMalformed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key := f.keyFor(keyID)
	signDigest := digest
	if wrongSig {
		// Sign a DIFFERENT digest than the one requested -- a real,
		// valid signature, just not for this input. Derived from digest
		// itself so it's still deterministic per call.
		signDigest = sha256.Sum256(digest[:])
	}
	sig := ecdsa.Sign(key, signDigest[:])
	return sig.Serialize(), nil
}
