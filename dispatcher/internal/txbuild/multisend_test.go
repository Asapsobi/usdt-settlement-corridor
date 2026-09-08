package txbuild

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

const multisendContractAddress = "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj" // provisional, does not exist on any real network

func testRecipients(t *testing.T, n int) []Recipient {
	t.Helper()
	out := make([]Recipient, n)
	for i := range out {
		out[i] = Recipient{Address: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", Amount: mustAmount(t, "10.000000")}
	}
	return out
}

func TestBuildMultisend_RejectsOverLimitBatchBeforeConstruction(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	_, err := BuildMultisend("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", multisendContractAddress, testRecipients(t, MaxMultisendRecipients+1), ref)
	if !errors.Is(err, ErrTooManyRecipients) {
		t.Fatalf("BuildMultisend error = %v, want ErrTooManyRecipients", err)
	}
}

func TestBuildMultisend_AtExactlyTheLimitSucceeds(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	_, err := BuildMultisend("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", multisendContractAddress, testRecipients(t, MaxMultisendRecipients), ref)
	if err != nil {
		t.Fatalf("BuildMultisend at exactly the limit: %v", err)
	}
}

func TestBuildMultisend_EmptyRecipientsRejected(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	_, err := BuildMultisend("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", multisendContractAddress, nil, ref)
	if !errors.Is(err, ErrNoRecipients) {
		t.Fatalf("BuildMultisend error = %v, want ErrNoRecipients", err)
	}
}

func TestBuildMultisend_MalformedRecipientRejected(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	recipients := []Recipient{
		{Address: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", Amount: mustAmount(t, "1.000000")},
		{Address: "not-a-real-address", Amount: mustAmount(t, "1.000000")},
	}
	_, err := BuildMultisend("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", multisendContractAddress, recipients, ref)
	if !errors.Is(err, ErrInvalidAddress) {
		t.Fatalf("BuildMultisend error = %v, want ErrInvalidAddress", err)
	}
}

func TestBuildMultisend_NonPositiveAmountRejected(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	recipients := []Recipient{
		{Address: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", Amount: mustAmount(t, "0.000000")},
	}
	_, err := BuildMultisend("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", multisendContractAddress, recipients, ref)
	if !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("BuildMultisend error = %v, want ErrNonPositiveAmount", err)
	}
}

func TestBuildMultisend_Deterministic(t *testing.T) {
	ref := BlockReference{BlockNumber: 12345, Timestamp: time.UnixMilli(1700000000000), Expiration: time.UnixMilli(1700000060000)}
	recipients := []Recipient{
		{Address: "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", Amount: mustAmount(t, "10.000000")},
		{Address: "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", Amount: mustAmount(t, "20.500000")},
	}

	first, err := BuildMultisend("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", multisendContractAddress, recipients, ref)
	if err != nil {
		t.Fatalf("BuildMultisend (1st): %v", err)
	}
	second, err := BuildMultisend("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", multisendContractAddress, recipients, ref)
	if err != nil {
		t.Fatalf("BuildMultisend (2nd): %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("BuildMultisend is not deterministic:\n1st: %x\n2nd: %x", first, second)
	}
}

func TestBuildMultisend_DifferentRecipientCountsProduceDifferentBytes(t *testing.T) {
	ref := BlockReference{BlockNumber: 1, Timestamp: time.UnixMilli(1), Expiration: time.UnixMilli(2)}
	two, err := BuildMultisend("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", multisendContractAddress, testRecipients(t, 2), ref)
	if err != nil {
		t.Fatalf("BuildMultisend (2 recipients): %v", err)
	}
	three, err := BuildMultisend("TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH", multisendContractAddress, testRecipients(t, 3), ref)
	if err != nil {
		t.Fatalf("BuildMultisend (3 recipients): %v", err)
	}
	if bytes.Equal(two, three) {
		t.Fatal("BuildMultisend produced identical bytes for different recipient counts")
	}
}
