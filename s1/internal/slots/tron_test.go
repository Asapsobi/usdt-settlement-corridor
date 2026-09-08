package slots

import (
	"bytes"
	"crypto/sha256"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// TestBase58Encode_KnownVector checks base58Encode against a widely-cited
// external test vector (encoding of the ASCII string "Hello World"),
// independent of this package's own decode logic -- a self-round-trip
// alone could pass even if both directions shared the same bug.
func TestBase58Encode_KnownVector(t *testing.T) {
	got := base58Encode([]byte("Hello World"))
	want := "JxF12TrwUP45BMd"
	if got != want {
		t.Fatalf("base58Encode(%q) = %q, want %q", "Hello World", got, want)
	}
}

func TestBase58Encode_LeadingZeroBytesBecomeLeadingOnes(t *testing.T) {
	got := base58Encode([]byte{0x00, 0x00, 0x01})
	if len(got) < 2 || got[0] != '1' || got[1] != '1' {
		t.Fatalf("base58Encode([0,0,1]) = %q, want it to start with two '1's for the two leading zero bytes", got)
	}
}

// base58Decode is a small, independent reference decoder used only by
// this test -- structurally the inverse operation (multiply-and-add
// versus base58Encode's own divide-and-collect-remainder), not a call
// into base58Encode's own machinery, so a round-trip proof here isn't
// just "the encoder undoes itself."
func base58DecodeForTest(t *testing.T, s string) []byte {
	t.Helper()
	num := new(big.Int)
	base := big.NewInt(58)
	for _, c := range []byte(s) {
		idx := bytes.IndexByte([]byte(base58Alphabet), c)
		if idx < 0 {
			t.Fatalf("invalid base58 character %q", c)
		}
		num.Mul(num, base)
		num.Add(num, big.NewInt(int64(idx)))
	}
	decoded := num.Bytes()

	leadingOnes := 0
	for i := 0; i < len(s) && s[i] == '1'; i++ {
		leadingOnes++
	}
	out := make([]byte, leadingOnes+len(decoded))
	copy(out[leadingOnes:], decoded)
	return out
}

func TestBase58CheckEncode_RoundTripsAndChecksumIsValid(t *testing.T) {
	payload := []byte{0x41, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	encoded := base58CheckEncode(payload)

	decoded := base58DecodeForTest(t, encoded)
	if len(decoded) != len(payload)+4 {
		t.Fatalf("decoded length = %d, want %d (payload + 4-byte checksum)", len(decoded), len(payload)+4)
	}
	gotPayload := decoded[:len(payload)]
	gotChecksum := decoded[len(payload):]
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("decoded payload = %x, want %x", gotPayload, payload)
	}

	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	wantChecksum := second[:4]
	if !bytes.Equal(gotChecksum, wantChecksum) {
		t.Fatalf("decoded checksum = %x, want %x (double-SHA256 of the payload)", gotChecksum, wantChecksum)
	}
}

func TestDeriveTronAddress_DeterministicForTheSameKey(t *testing.T) {
	priv := secp256k1.PrivKeyFromBytes([]byte{
		1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16,
		17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32,
	})
	addr1 := deriveTronAddress(priv.PubKey())
	addr2 := deriveTronAddress(priv.PubKey())
	if addr1 != addr2 {
		t.Fatalf("deriveTronAddress is not deterministic: %q != %q", addr1, addr2)
	}
	if addr1[0] != 'T' {
		t.Fatalf("address = %q, want it to start with 'T'", addr1)
	}
}

func TestDeriveTronAddress_DifferentKeysYieldDifferentAddresses(t *testing.T) {
	priv1 := secp256k1.PrivKeyFromBytes(bytes.Repeat([]byte{1}, 32))
	priv2 := secp256k1.PrivKeyFromBytes(bytes.Repeat([]byte{2}, 32))
	if deriveTronAddress(priv1.PubKey()) == deriveTronAddress(priv2.PubKey()) {
		t.Fatal("different public keys produced the same TRON address")
	}
}
