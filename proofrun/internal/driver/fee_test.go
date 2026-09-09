package driver_test

import (
	"context"
	"testing"

	"proofrun/internal/driver"
	"proofrun/internal/money"
)

func TestCreatePayout_AmountTooSmallIsRejectedBeforeAnyUpstreamCall(t *testing.T) {
	d := &driver.Driver{
		Cfg: driver.Config{FeeBasisPoints: 25, NetworkFeeUnits: mustAmount(t, "1.800000")},
	}
	_, err := d.CreatePayout(context.Background(), driver.CreatePayoutRequest{
		ExternalID: "x", CustomerID: "c", RecipientTronAddress: "T...", AmountIn: "1.000000",
	})
	if err == nil {
		t.Fatal("CreatePayout with amount_in too small to cover fee+network_fee: want an error, got nil")
	}
}

func TestCreatePayout_InvalidAmountRejected(t *testing.T) {
	d := &driver.Driver{
		Cfg: driver.Config{FeeBasisPoints: 25, NetworkFeeUnits: mustAmount(t, "1.800000")},
	}
	_, err := d.CreatePayout(context.Background(), driver.CreatePayoutRequest{
		ExternalID: "x", CustomerID: "c", RecipientTronAddress: "T...", AmountIn: "not-a-number",
	})
	if err == nil {
		t.Fatal("CreatePayout with a malformed amount_in: want an error, got nil")
	}
}

func mustAmount(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return a
}
