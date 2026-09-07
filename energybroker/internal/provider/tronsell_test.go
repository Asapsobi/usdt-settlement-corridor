package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewTronsellProvider_RejectsEmptyBaseURL(t *testing.T) {
	_, err := NewTronsellProvider(TronsellConfig{APIKey: "k"})
	if !errors.Is(err, ErrTronsellBaseURLNotConfigured) {
		t.Fatalf("error = %v, want ErrTronsellBaseURLNotConfigured", err)
	}
}

func TestTronsellProvider_Quote_ErrorsUntilAPriceHasBeenObserved(t *testing.T) {
	p, err := NewTronsellProvider(TronsellConfig{BaseURL: "http://unused.invalid", APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTronsellProvider: %v", err)
	}
	_, err = p.Quote(context.Background())
	if !errors.Is(err, ErrTronsellNoPriceObserved) {
		t.Fatalf("error = %v, want ErrTronsellNoPriceObserved before any Delegate call", err)
	}
}

func TestTronsellProvider_Delegate_ConvertsThousandthsOfSunAndWarmsQuote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/order/useRent" {
			t.Fatalf("path = %q, want /v1/order/useRent", r.URL.Path)
		}
		if got := r.Header.Get("key"); got != "test-key" {
			t.Fatalf("key header = %q, want test-key", got)
		}
		var body struct {
			ResourceType        int    `json:"resourceType"`
			ReceiveAddress      string `json:"receiveAddress"`
			ResourceValue       int64  `json:"resourceValue"`
			LeaseDurationSecond int64  `json:"leaseDurationSecond"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.ResourceType != 1 {
			t.Fatalf("resourceType = %d, want 1 (energy)", body.ResourceType)
		}
		if body.ResourceValue != 65000 {
			t.Fatalf("resourceValue = %d, want 65000", body.ResourceValue)
		}
		if body.LeaseDurationSecond != 3600 {
			t.Fatalf("leaseDurationSecond = %d, want 3600 (1h)", body.LeaseDurationSecond)
		}
		w.Write([]byte(`{
			"code": 99999,
			"msg": "success",
			"data": {
				"orderNo": "8fe2a3b7-c995-40ec-9cb6-67a3c90ed998",
				"priceInSun": 56000,
				"resourceValue": 65000,
				"rentTimeSecond": 3600,
				"payAmount": 3.9,
				"status": 10
			}
		}`))
	}))
	defer srv.Close()

	p, err := NewTronsellProvider(TronsellConfig{BaseURL: srv.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("NewTronsellProvider: %v", err)
	}

	d, err := p.Delegate(context.Background(), "TTargetAddress0000000000000001", 65000, time.Hour)
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if d.ID != "8fe2a3b7-c995-40ec-9cb6-67a3c90ed998" {
		t.Fatalf("ID = %q, want the orderNo", d.ID)
	}
	if d.CostTRX != 3_900000 {
		t.Fatalf("CostTRX = %v, want 3900000 (payAmount 3.9 TRX)", d.CostTRX)
	}
	if d.ExpiresAt.Sub(d.RequestedAt) != time.Hour {
		t.Fatalf("ExpiresAt - RequestedAt = %v, want 1h (rentTimeSecond 3600)", d.ExpiresAt.Sub(d.RequestedAt))
	}
	if d.ConfirmedAt != nil {
		t.Fatal("ConfirmedAt must be nil -- on-chain verification is not this client's job")
	}

	// priceInSun (56000, thousandths of a sun) must be divided by 1000
	// to recover the real 56 sun/unit rate, and Quote must now report it
	// instead of erroring -- see setLastPriceSun's own doc comment.
	q, err := p.Quote(context.Background())
	if err != nil {
		t.Fatalf("Quote after a real Delegate call: %v", err)
	}
	if q.PricePerUnitSun != 56 {
		t.Fatalf("PricePerUnitSun = %v, want 56 (56000 thousandths-of-a-sun / 1000)", q.PricePerUnitSun)
	}
}

func TestTronsellProvider_Delegate_NonSuccessCodeIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":10100,"msg":"general error"}`))
	}))
	defer srv.Close()

	p, err := NewTronsellProvider(TronsellConfig{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTronsellProvider: %v", err)
	}
	_, err = p.Delegate(context.Background(), "T1", 65000, time.Hour)
	if err == nil {
		t.Fatal("expected an error for a non-99999 code, got nil")
	}
}

func TestTronsellProvider_Delegate_NonPositiveRentTimeFallsBackToRequestedDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":99999,"data":{"orderNo":"o1","priceInSun":1000,"payAmount":1.0,"rentTimeSecond":0}}`))
	}))
	defer srv.Close()

	p, err := NewTronsellProvider(TronsellConfig{BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("NewTronsellProvider: %v", err)
	}
	d, err := p.Delegate(context.Background(), "T1", 65000, 45*time.Minute)
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if d.ExpiresAt.Sub(d.RequestedAt) != 45*time.Minute {
		t.Fatalf("ExpiresAt - RequestedAt = %v, want the requested 45m fallback since rentTimeSecond was 0", d.ExpiresAt.Sub(d.RequestedAt))
	}
}
