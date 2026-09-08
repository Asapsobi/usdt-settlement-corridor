package kmssign

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

func TestFakeKMSClient_SameSeedAndKeyIDYieldsSameKeypair(t *testing.T) {
	ctx := context.Background()
	a := NewFakeKMSClient(42)
	b := NewFakeKMSClient(42)

	derA, err := a.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("a.GetPublicKey: %v", err)
	}
	derB, err := b.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("b.GetPublicKey: %v", err)
	}
	if string(derA) != string(derB) {
		t.Fatal("same seed and keyID produced different public keys")
	}
}

func TestFakeKMSClient_DifferentKeyIDsYieldDifferentKeypairs(t *testing.T) {
	ctx := context.Background()
	f := NewFakeKMSClient(1)

	der1, err := f.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey(slot-1): %v", err)
	}
	der2, err := f.GetPublicKey(ctx, "slot-2")
	if err != nil {
		t.Fatalf("GetPublicKey(slot-2): %v", err)
	}
	if string(der1) == string(der2) {
		t.Fatal("different keyIDs produced the same public key")
	}
}

func TestFakeKMSClient_SignProducesAnIndependentlyVerifiableSignature(t *testing.T) {
	ctx := context.Background()
	f := NewFakeKMSClient(7)
	digest := [32]byte{1, 2, 3, 4, 5}

	derPub, err := f.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	compressed, err := parseDERPublicKey(derPub)
	if err != nil {
		t.Fatalf("parseDERPublicKey: %v", err)
	}
	pubKey, err := secp256k1.ParsePubKey(compressed[:])
	if err != nil {
		t.Fatalf("ParsePubKey: %v", err)
	}

	derSig, err := f.Sign(ctx, "slot-1", digest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sig, err := ecdsa.ParseDERSignature(derSig)
	if err != nil {
		t.Fatalf("ParseDERSignature: %v", err)
	}
	if !sig.Verify(digest[:], pubKey) {
		t.Fatal("signature does not verify against the digest and public key returned for the same keyID")
	}
}

func TestFakeKMSClient_ForceTimeoutRespectsContextDeadline(t *testing.T) {
	f := NewFakeKMSClient(1)
	f.ForceTimeout("slot-1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := f.Sign(ctx, "slot-1", [32]byte{})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sign() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Sign() took %v to respect a 30ms deadline", elapsed)
	}
}

func TestFakeKMSClient_ForceMalformedIsATypedError(t *testing.T) {
	f := NewFakeKMSClient(1)
	f.ForceMalformed("slot-1")

	if _, err := f.Sign(context.Background(), "slot-1", [32]byte{}); !errors.Is(err, ErrForcedMalformed) {
		t.Fatalf("Sign() error = %v, want ErrForcedMalformed", err)
	}
	if _, err := f.GetPublicKey(context.Background(), "slot-1"); !errors.Is(err, ErrForcedMalformed) {
		t.Fatalf("GetPublicKey() error = %v, want ErrForcedMalformed", err)
	}
}

func TestFakeKMSClient_ForceWrongDigestSignatureStillVerifiesAsSignatureJustNotForTheRequestedDigest(t *testing.T) {
	ctx := context.Background()
	f := NewFakeKMSClient(1)
	f.ForceWrongDigestSignature("slot-1")
	digest := [32]byte{9, 9, 9}

	derPub, err := f.GetPublicKey(ctx, "slot-1")
	if err != nil {
		t.Fatalf("GetPublicKey: %v", err)
	}
	compressed, err := parseDERPublicKey(derPub)
	if err != nil {
		t.Fatalf("parseDERPublicKey: %v", err)
	}
	pubKey, err := secp256k1.ParsePubKey(compressed[:])
	if err != nil {
		t.Fatalf("ParsePubKey: %v", err)
	}

	derSig, err := f.Sign(ctx, "slot-1", digest)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sig, err := ecdsa.ParseDERSignature(derSig)
	if err != nil {
		t.Fatalf("ParseDERSignature: %v", err)
	}
	if sig.Verify(digest[:], pubKey) {
		t.Fatal("forced-wrong-digest signature verified against the ORIGINAL digest, want it to verify against a different one instead")
	}
}
