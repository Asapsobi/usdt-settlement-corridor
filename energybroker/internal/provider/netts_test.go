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

func TestNettsProvider_Quote_ReadsCurrentPeriodPrice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pricing" {
			t.Fatalf("path = %q, want /pricing", r.URL.Path)
		}
		if got := r.Header.Get("X-API-KEY"); got != "test-key" {
			t.Fatalf("X-API-KEY = %q, want test-key", got)
		}
		if got := r.Header.Get("X-Real-IP"); got != "203.0.113.10" {
			t.Fatalf("X-Real-IP = %q, want 203.0.113.10", got)
		}
		w.Write([]byte(`{
			"success": true,
			"data": {
				"services": {
					"energy_1h": {
						"unit": "sun",
						"periods": [
							{"id": "nighttime", "is_current": false, "price": 20},
							{"id": "daytime", "is_current": true, "price": 27}
						]
					}
				}
			}
		}`))
	}))
	defer srv.Close()

	p := NewNettsProvider(NettsConfig{BaseURL: srv.URL, APIKey: "test-key", RealIP: "203.0.113.10"})
	q, err := p.Quote(context.Background())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.PricePerUnitSun != 27 {
		t.Fatalf("PricePerUnitSun = %v, want 27 (the is_current period)", q.PricePerUnitSun)
	}
	if q.ProviderName != Netts {
		t.Fatalf("ProviderName = %q, want %q", q.ProviderName, Netts)
	}
}

func TestNettsProvider_Quote_NoCurrentPeriodIsATypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"services":{"energy_1h":{"unit":"sun","periods":[{"id":"x","is_current":false,"price":1}]}}}}`))
	}))
	defer srv.Close()

	p := NewNettsProvider(NettsConfig{BaseURL: srv.URL, APIKey: "k", RealIP: "1.2.3.4"})
	_, err := p.Quote(context.Background())
	if !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("error = %v, want wrapping ErrMalformedResponse", err)
	}
}

func TestNettsProvider_Delegate_PostsOrder1hAndParsesRealCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/order1h" {
			t.Fatalf("path = %q, want /order1h", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("X-API-KEY"); got != "test-key" {
			t.Fatalf("X-API-KEY = %q, want test-key", got)
		}
		var body struct {
			Amount         int64  `json:"amount"`
			ReceiveAddress string `json:"receiveAddress"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		if body.Amount != 131000 {
			t.Fatalf("amount = %d, want 131000", body.Amount)
		}
		if body.ReceiveAddress != "TTargetAddress0000000000000001" {
			t.Fatalf("receiveAddress = %q, want the target address", body.ReceiveAddress)
		}
		w.Write([]byte(`{"detail":{"code":10000,"msg":"Successful, 2.23 TRX deducted","data":{"orderId":"1H70bcc7962a","paidTRX":2.535,"hash":"h","delegateAddress":"TTargetAddress0000000000000001","energy":131050}}}`))
	}))
	defer srv.Close()

	p := NewNettsProvider(NettsConfig{BaseURL: srv.URL, APIKey: "test-key", RealIP: "203.0.113.10"})
	d, err := p.Delegate(context.Background(), "TTargetAddress0000000000000001", 131000, time.Hour)
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if d.ID != "1H70bcc7962a" {
		t.Fatalf("ID = %q, want 1H70bcc7962a", d.ID)
	}
	if d.CostTRX != 2_535000 {
		t.Fatalf("CostTRX = %v, want 2535000 (2.535 TRX)", d.CostTRX)
	}
	if d.ExpiresAt.Sub(d.RequestedAt) != time.Hour {
		t.Fatalf("ExpiresAt - RequestedAt = %v, want exactly 1h", d.ExpiresAt.Sub(d.RequestedAt))
	}
	if d.ConfirmedAt != nil {
		t.Fatal("ConfirmedAt must be nil -- on-chain verification is not this client's job")
	}
}

func TestNettsProvider_Delegate_NonSuccessCodeIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"detail":{"code":40001,"msg":"insufficient balance","data":{}}}`))
	}))
	defer srv.Close()

	p := NewNettsProvider(NettsConfig{BaseURL: srv.URL, APIKey: "k", RealIP: "1.2.3.4"})
	_, err := p.Delegate(context.Background(), "T1", 65000, time.Hour)
	if err == nil {
		t.Fatal("expected an error for a non-10000 code, got nil")
	}
}
