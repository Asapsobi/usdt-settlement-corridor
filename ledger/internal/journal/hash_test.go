package journal

import (
	"bytes"
	"testing"
	"time"

	"ledger/internal/money"
)

func mustAmount(t *testing.T, asset money.Asset, units int64) money.Amount {
	t.Helper()
	return money.Amount{Asset: asset, Units: units}
}

func baseRequest(t *testing.T) EntryRequest {
	t.Helper()
	return EntryRequest{
		IdempotencyKey: "test:key:1",
		EntryType:      "conversion",
		Actor:          "test",
		OccurredAt:     time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
		Lines: []Line{
			{AccountCode: "liability:customer:acme", Amount: mustAmount(t, money.USDT_BEP20, 3000_000000)},
			{AccountCode: "position:corridor:USDT_BEP20", Amount: mustAmount(t, money.USDT_BEP20, -3000_000000)},
		},
	}
}

func TestCanonicalHashLineOrderIndependent(t *testing.T) {
	req := baseRequest(t)
	h1, err := canonicalHash(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reordered := req
	reordered.Lines = []Line{req.Lines[1], req.Lines[0]}
	h2, err := canonicalHash(reordered)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !bytes.Equal(h1, h2) {
		t.Error("reordering lines changed the hash, want identical")
	}
}

func TestCanonicalHashOccurredAtSubMicrosecondIgnored(t *testing.T) {
	req := baseRequest(t)
	h1, err := canonicalHash(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req2 := req
	req2.OccurredAt = req.OccurredAt.Add(400 * time.Nanosecond)
	h2, err := canonicalHash(req2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(h1, h2) {
		t.Error("sub-microsecond occurred_at difference changed the hash, want identical")
	}

	req3 := req
	req3.OccurredAt = req.OccurredAt.Add(2 * time.Microsecond)
	h3, err := canonicalHash(req3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bytes.Equal(h1, h3) {
		t.Error("2-microsecond occurred_at difference did not change the hash, want different")
	}
}

func TestCanonicalHashDiffersOnContent(t *testing.T) {
	req := baseRequest(t)
	base, err := canonicalHash(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entryTypeChanged := req
	entryTypeChanged.EntryType = "payout_settled"
	if h, err := canonicalHash(entryTypeChanged); err != nil || bytes.Equal(base, h) {
		t.Error("changing entry_type did not change the hash")
	}

	orderID := int64(42)
	orderIDChanged := req
	orderIDChanged.OrderID = &orderID
	if h, err := canonicalHash(orderIDChanged); err != nil || bytes.Equal(base, h) {
		t.Error("changing order_id did not change the hash")
	}

	amountChanged := req
	amountChanged.Lines = []Line{
		req.Lines[0],
		{AccountCode: req.Lines[1].AccountCode, Amount: mustAmount(t, money.USDT_BEP20, -2999_000000)},
	}
	if h, err := canonicalHash(amountChanged); err != nil || bytes.Equal(base, h) {
		t.Error("changing a line amount did not change the hash")
	}
}
