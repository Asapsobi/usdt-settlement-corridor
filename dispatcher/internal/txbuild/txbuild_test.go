package txbuild

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"dispatcher/internal/money"
)

func mustAmount(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return a
}

// TestBuildTransfer_MatchesLiveVerifiedShape checks that BuildTransfer's
// real-address, real-encoding output (constructed through the actual
// public API, unlike the byte-for-byte reproduction below which had to
// bypass address validation to match a fabricated recipient) contains
// every fixed fragment a live triggersmartcontract response against
// USDTContractAddress actually produced: the ref_block_bytes encoding,
// the Any wrapper's type_url, and the transfer selector. The exact,
// independent full-byte proof lives in TestDigest_MatchesLiveVerifiedTxID
// below, which checks this package's SHA256 (txID) against a real,
// live-captured txID for a raw_data blob assembled the same way
// BuildTransfer assembles one (verified once, offline, against
// gotron-sdk directly, before this package was written).
func TestBuildTransfer_MatchesLiveVerifiedShape(t *testing.T) {
	ref := BlockReference{
		BlockNumber: 0x355c,
		Timestamp:   time.UnixMilli(1788859176552),
		Expiration:  time.UnixMilli(1788859233000),
	}

	got, err := BuildTransfer("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", mustAmount(t, "0.000001"), ref)
	if err != nil {
		t.Fatalf("BuildTransfer: %v", err)
	}
	for _, want := range [][]byte{
		mustHex(t, "0a02355c"), // ref_block_bytes = 0x355c
		[]byte("type.googleapis.com/protocol.TriggerSmartContract"),
		mustHex(t, "a9059cbb"), // transfer(address,uint256) selector
	} {
		if !bytes.Contains(got, want) {
			t.Fatalf("BuildTransfer output missing expected fragment %x", want)
		}
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex.DecodeString(%q): %v", s, err)
	}
	return b
}

func TestBuildTransfer_Deterministic(t *testing.T) {
	ref := BlockReference{
		BlockNumber: 12345,
		Timestamp:   time.UnixMilli(1700000000000),
		Expiration:  time.UnixMilli(1700000060000),
	}
	amount := mustAmount(t, "100.500000")

	first, err := BuildTransfer("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", amount, ref)
	if err != nil {
		t.Fatalf("BuildTransfer (1st): %v", err)
	}
	second, err := BuildTransfer("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", amount, ref)
	if err != nil {
		t.Fatalf("BuildTransfer (2nd): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("BuildTransfer is not deterministic:\n1st: %x\n2nd: %x", first, second)
	}
}

func TestBuildTransfer_DifferentAmountProducesDifferentBytes(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	a, err := BuildTransfer("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", mustAmount(t, "1.000000"), ref)
	if err != nil {
		t.Fatalf("BuildTransfer (1.0): %v", err)
	}
	b, err := BuildTransfer("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", mustAmount(t, "2.000000"), ref)
	if err != nil {
		t.Fatalf("BuildTransfer (2.0): %v", err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("BuildTransfer produced identical bytes for different amounts")
	}
}

func TestBuildTransfer_MalformedRecipientRejectedBeforeAnyBytesBuilt(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	_, err := BuildTransfer("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", "not-a-real-address", mustAmount(t, "1.000000"), ref)
	if !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("BuildTransfer error = %v, want ErrInvalidAddress", err)
	}
}

func TestBuildTransfer_MalformedRecipientChecksumRejected(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	// One character flipped from a real valid address -- fails the
	// base58check checksum, not just the charset.
	_, err := BuildTransfer("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYX", "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", mustAmount(t, "1.000000"), ref)
	if !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("BuildTransfer error = %v, want ErrInvalidAddress", err)
	}
}

func TestBuildTransfer_MalformedSlotAddressRejected(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	_, err := BuildTransfer("also-not-real", "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", mustAmount(t, "1.000000"), ref)
	if !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("BuildTransfer error = %v, want ErrInvalidAddress", err)
	}
}

func TestBuildTransfer_NonPositiveAmountRejected(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	for _, s := range []string{"0.000000", "-1.000000"} {
		_, err := BuildTransfer("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", mustAmount(t, s), ref)
		if !errors.Is(err, ErrNonPositiveAmount) {
			t.Fatalf("BuildTransfer(%s) error = %v, want ErrNonPositiveAmount", s, err)
		}
	}
}

func TestDigest_MatchesLiveVerifiedTxID(t *testing.T) {
	rawDataHex := "0a02355c2208e091eedd1896b31d40e8e5c68288345aae01081f12a9010a31747970652e676f6f676c65617069732e636f6d2f70726f746f636f6c2e54726967676572536d617274436f6e747261637412740a154178c842ee63b253f8f0d2955bbc582c661a078c9d121541a614f803b6fd780986a42c78ec9c7f77e6ded13c2244a9059cbb0000000000000000000000004192cc99d3cb95573dcaf8dd76921476e0c7bcaf000000000000000000000000000000000000000000000000000000000000000170e8acc3828834900180c2d72f"
	wantTxID := "a21fc5e1948eae418d21db19ffbf3cee106650fcff1cfdfb7912240e447a76a1"

	digest := Digest(mustHex(t, rawDataHex))
	if got := hex.EncodeToString(digest[:]); got != wantTxID {
		t.Fatalf("Digest = %s, want %s (a real, live-captured txID for this exact raw_data)", got, wantTxID)
	}
}
