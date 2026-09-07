package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCatfeeProvider_Quote_ComputesPerUnitPriceFromTotalEstimate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/estimate" {
			t.Fatalf("path = %q, want /v1/estimate", r.URL.Path)
		}
		if got := r.URL.Query().Get("quantity"); got != "65000" {
			t.Fatalf("quantity = %q, want 65000 (catfeeMinQuantity)", got)
		}
		if got := r.URL.Query().Get("duration"); got != "1h" {
			t.Fatalf("duration = %q, want 1h", got)
		}
		for _, h := range []string{"CF-ACCESS-KEY", "CF-ACCESS-SIGN", "CF-ACCESS-TIMESTAMP"} {
			if r.Header.Get(h) == "" {
				t.Fatalf("missing header %s", h)
			}
		}
		w.Write([]byte(`{"code":0,"data":15600}`)) // 15600 sun total for 65000 units -> 0.24 sun/unit
	}))
	defer srv.Close()

	p := NewCatfeeProvider(CatfeeConfig{BaseURL: srv.URL, APIKey: "key", APISecret: "secret"})
	q, err := p.Quote(context.Background())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.ProviderName != Catfee {
		t.Fatalf("ProviderName = %q, want %q", q.ProviderName, Catfee)
	}
	want := 15600.0 / 65000.0
	if q.PricePerUnitSun != want {
		t.Fatalf("PricePerUnitSun = %v, want %v", q.PricePerUnitSun, want)
	}
}

func TestCatfeeProvider_Quote_PropagatesVendorErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":201,"msg":"insufficient balance"}`))
	}))
	defer srv.Close()

	p := NewCatfeeProvider(CatfeeConfig{BaseURL: srv.URL, APIKey: "key", APISecret: "secret"})
	_, err := p.Quote(context.Background())
	if err == nil {
		t.Fatal("expected an error for a non-zero code, got nil")
	}
}

func TestCatfeeProvider_Quote_MalformedBodyIsATypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	p := NewCatfeeProvider(CatfeeConfig{BaseURL: srv.URL, APIKey: "key", APISecret: "secret"})
	_, err := p.Quote(context.Background())
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("error = %v, want wrapping ErrMalformedResponse", err)
	}
}

func TestCatfeeProvider_Delegate_SignsRequestAndParsesRealCost(t *testing.T) {
	const apiKey, apiSecret = "test-key", "test-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/order":
			if got := r.Header.Get("CF-ACCESS-KEY"); got != apiKey {
				t.Fatalf("CF-ACCESS-KEY = %q, want %q", got, apiKey)
			}
			timestamp := r.Header.Get("CF-ACCESS-TIMESTAMP")
			wantSig := func() string {
				mac := hmac.New(sha256.New, []byte(apiSecret))
				mac.Write([]byte(timestamp + http.MethodPost + r.URL.RequestURI()))
				return base64.StdEncoding.EncodeToString(mac.Sum(nil))
			}()
			if got := r.Header.Get("CF-ACCESS-SIGN"); got != wantSig {
				t.Fatalf("CF-ACCESS-SIGN = %q, want %q (recomputed HMAC)", got, wantSig)
			}
			if got := r.URL.Query().Get("receiver"); got != "TTargetAddress0000000000000001" {
				t.Fatalf("receiver = %q, want the target address", got)
			}
			if got := r.URL.Query().Get("quantity"); got != "65000" {
				t.Fatalf("quantity = %q, want 65000", got)
			}
			w.Write([]byte(`{"code":0,"data":{"id":"cf-order-1","pay_amount_sun":15600,"status":"DELEGATE_SUCCESS","confirm_status":"UNCONFIRMED"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := NewCatfeeProvider(CatfeeConfig{BaseURL: srv.URL, APIKey: apiKey, APISecret: apiSecret})
	d, err := p.Delegate(context.Background(), "TTargetAddress0000000000000001", 65000, time.Hour)
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if d.ID != "cf-order-1" {
		t.Fatalf("ID = %q, want cf-order-1", d.ID)
	}
	if d.ProviderName != Catfee {
		t.Fatalf("ProviderName = %q, want %q", d.ProviderName, Catfee)
	}
	if d.CostTRX != 15600 { // 15600 sun = 0.015600 TRX = 15600 minor units at 6 decimals
		t.Fatalf("CostTRX = %v, want 15600 (15600 sun -> 0.015600 TRX)", d.CostTRX)
	}
	if d.ConfirmedAt != nil {
		t.Fatal("ConfirmedAt must be nil -- on-chain verification is not this client's job")
	}
	if d.ExpiresAt.Sub(d.RequestedAt) != time.Hour {
		t.Fatalf("ExpiresAt - RequestedAt = %v, want exactly 1h (CatFee only ever grants 1h)", d.ExpiresAt.Sub(d.RequestedAt))
	}
}

func TestCatfeeProvider_Delegate_EmptyOrderIDIsATypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"id":"","pay_amount_sun":100}}`))
	}))
	defer srv.Close()

	p := NewCatfeeProvider(CatfeeConfig{BaseURL: srv.URL, APIKey: "k", APISecret: "s"})
	_, err := p.Delegate(context.Background(), "T1", 65000, time.Hour)
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("error = %v, want wrapping ErrMalformedResponse", err)
	}
}
