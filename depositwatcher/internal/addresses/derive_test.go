package addresses

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"

	secp256k1 "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/tyler-smith/go-bip32"
)

// testXpub returns a real, valid BIP32 extended PUBLIC key for tests.
// Generated from a fixed, clearly-throwaway seed inside the test itself --
// this secures nothing, it exists purely as a fixture for exercising this
// package's own public-derivation math, so a hardcoded, obviously-fake
// seed is the right choice, not a security concern.
func testXpub(t *testing.T) string {
	t.Helper()
	master, err := bip32.NewMasterKey([]byte("depositwatcher test fixture -- not a real seed, never use"))
	if err != nil {
		t.Fatalf("generating test master key: %v", err)
	}
	return master.PublicKey().B58Serialize()
}

func TestDeriveAddress_DeterministicValidNoCollisions(t *testing.T) {
	xpub := testXpub(t)
	seed := int64(1)
	rng := rand.New(rand.NewSource(seed))
	seen := make(map[Address]uint32, 10000)

	defer func() {
		if t.Failed() {
			t.Logf("seed=%d", seed)
		}
	}()

	for i := 0; i < 10000; i++ {
		idx := uint32(rng.Int31()) // always < hardenedOffset (2^31)

		addr1, err := DeriveAddress(xpub, idx)
		if err != nil {
			t.Fatalf("index %d: %v", idx, err)
		}
		addr2, err := DeriveAddress(xpub, idx)
		if err != nil {
			t.Fatalf("index %d (second call): %v", idx, err)
		}
		if addr1 != addr2 {
			t.Fatalf("index %d: not deterministic: %s vs %s", idx, addr1, addr2)
		}
		if !ValidChecksum(addr1) {
			t.Fatalf("index %d: %s is not a valid EIP-55 checksum address", idx, addr1)
		}
		if prevIdx, dup := seen[addr1]; dup {
			t.Fatalf("collision: indices %d and %d both produced %s", prevIdx, idx, addr1)
		}
		seen[addr1] = idx
	}
}

func TestDeriveAddress_DifferentXpubsDifferentAddresses(t *testing.T) {
	master1, err := bip32.NewMasterKey([]byte("fixture seed one"))
	if err != nil {
		t.Fatal(err)
	}
	master2, err := bip32.NewMasterKey([]byte("fixture seed two"))
	if err != nil {
		t.Fatal(err)
	}
	addr1, err := DeriveAddress(master1.PublicKey().B58Serialize(), 0)
	if err != nil {
		t.Fatal(err)
	}
	addr2, err := DeriveAddress(master2.PublicKey().B58Serialize(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if addr1 == addr2 {
		t.Fatalf("two different master keys produced the same address at index 0: %s", addr1)
	}
}

func TestDeriveAddress_RejectsHardenedIndex(t *testing.T) {
	xpub := testXpub(t)
	_, err := DeriveAddress(xpub, hardenedOffset)
	if !errors.Is(err, ErrHardenedIndex) {
		t.Fatalf("expected ErrHardenedIndex, got %v", err)
	}
	_, err = DeriveAddress(xpub, hardenedOffset+5)
	if !errors.Is(err, ErrHardenedIndex) {
		t.Fatalf("expected ErrHardenedIndex, got %v", err)
	}
}

func TestDeriveAddress_RejectsExtendedPrivateKey(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("fixture seed for xprv rejection test"))
	if err != nil {
		t.Fatal(err)
	}
	xprv := master.B58Serialize() // the PRIVATE extended key, not .PublicKey()

	_, err = DeriveAddress(xprv, 0)
	if !errors.Is(err, ErrPrivateKeyMaterial) {
		t.Fatalf("expected ErrPrivateKeyMaterial for an xprv, got %v", err)
	}
}

func TestDeriveAddress_RejectsWIFShapedInput(t *testing.T) {
	// A real Bitcoin mainnet WIF (compressed), well-known test-vector shaped
	// but not tied to any real funds -- shape is all that matters here.
	wif := "L1aW4aubDFB7yfras2S1mN3bqg9nwySY8nkoLmJebSLD5BWv3ENZ"
	_, err := DeriveAddress(wif, 0)
	if !errors.Is(err, ErrPrivateKeyMaterial) {
		t.Fatalf("expected ErrPrivateKeyMaterial for a WIF-shaped string, got %v", err)
	}
}

func TestDeriveAddress_RejectsMnemonicShapedInput(t *testing.T) {
	mnemonic := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	_, err := DeriveAddress(mnemonic, 0)
	if !errors.Is(err, ErrPrivateKeyMaterial) {
		t.Fatalf("expected ErrPrivateKeyMaterial for a mnemonic-shaped string, got %v", err)
	}
}

func TestDeriveAddress_RejectsGarbageInput(t *testing.T) {
	for _, bad := range []string{"", "not-a-key-at-all", "0x1234"} {
		if _, err := DeriveAddress(bad, 0); err == nil {
			t.Fatalf("expected an error for garbage input %q, got none", bad)
		}
	}
}

func TestValidChecksum_RejectsWrongCaseAndMalformed(t *testing.T) {
	xpub := testXpub(t)
	addr, err := DeriveAddress(xpub, 0)
	if err != nil {
		t.Fatal(err)
	}
	lower := Address(fmt.Sprintf("0x%s", string(addr)[2:]))
	// Force lowercase every letter -- guaranteed to differ from the real
	// checksum casing for at least one letter in a 40-char address in
	// practice, and if it every somehow doesn't, ValidChecksum recomputes
	// deterministically anyway so this is not a flaky assertion.
	forcedLower := make([]byte, len(lower))
	for i, c := range []byte(lower) {
		if c >= 'A' && c <= 'F' {
			c = c - 'A' + 'a'
		}
		forcedLower[i] = c
	}
	if Address(forcedLower) != addr && ValidChecksum(Address(forcedLower)) {
		t.Fatalf("an all-lowercase rendering of a checksummed address must not itself validate: %s", forcedLower)
	}
	if ValidChecksum("0xnothex000000000000000000000000000000000") {
		t.Fatal("non-hex address must not validate")
	}
	if ValidChecksum("0x1234") {
		t.Fatal("wrong-length address must not validate")
	}
}

// TestCKDPub_MatchesIndependentImplementation cross-validates this
// package's own CKDpub math (bip32.go) against tyler-smith/go-bip32's
// entirely separate implementation of the same BIP32 derivation formula.
// This is the strongest correctness check available without a network
// connection to pull published BIP32 test vectors: elliptic-curve
// derivation bugs (wrong byte order into the HMAC, wrong operand order in
// the point addition) tend to produce a point that still LOOKS like a
// plausible public key -- valid length, on-curve or close to it -- while
// being cryptographically wrong. Landing on the exact same point as a
// second, independently-implemented codebase for the same (seed, index)
// is not something a subtly-wrong implementation could do by chance.
//
// go-bip32 here is exercised purely as an oracle for this one test -- see
// go.mod's comment on why it's a test-only dependency, never a production
// one, for exactly the reasons this file's own DeriveAddress avoids it.
func TestCKDPub_MatchesIndependentImplementation(t *testing.T) {
	master, err := bip32.NewMasterKey([]byte("cross-validation fixture seed -- not a real seed, never use"))
	if err != nil {
		t.Fatal(err)
	}
	xpub := master.PublicKey().B58Serialize()

	parsed, err := parseXpub(xpub)
	if err != nil {
		t.Fatalf("parseXpub: %v", err)
	}

	for _, idx := range []uint32{0, 1, 2, 100, 1 << 20, hardenedOffset - 1} {
		theirChild, err := master.PublicKey().NewChildKey(idx)
		if err != nil {
			t.Fatalf("index %d: go-bip32 NewChildKey: %v", idx, err)
		}

		ourUncompressed, err := ckdPub(parsed, idx)
		if err != nil {
			t.Fatalf("index %d: our ckdPub: %v", idx, err)
		}

		ourPub, err := secp256k1.ParsePubKey(ourUncompressed)
		if err != nil {
			t.Fatalf("index %d: parsing our derived point: %v", idx, err)
		}
		theirPub, err := secp256k1.ParsePubKey(theirChild.Key)
		if err != nil {
			t.Fatalf("index %d: parsing go-bip32's derived point: %v", idx, err)
		}

		if !ourPub.IsEqual(theirPub) {
			t.Fatalf("index %d: derived a DIFFERENT public key than go-bip32:\n  ours:  %x\n  theirs: %x",
				idx, ourUncompressed, theirChild.Key)
		}
	}
}
