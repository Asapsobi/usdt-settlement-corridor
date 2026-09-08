package ledgerclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeC1Server stands in for a real C1 (ledger) instance, built to C1's
// actual shipped response shapes (ledger/internal/httpapi) -- verified by
// reading errors.go, entries.go, reversals.go, orders_handlers.go, and
// balances.go directly, not guessed.
func fakeC1Server(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)
	return ts
}

func TestGetOrder_DecodesOrderFields(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/orders/order-1" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(orderResponse{
			ID: 42, ExternalID: "order-1", CustomerID: "cust-1", Tier: "STANDARD", State: "screened",
			AmountIn: "3000.000000", AmountOut: "2990.700000", FeeUnits: "7.500000", NetworkFeeUnits: "1.800000",
			RecipientAddress: "Trecipient", Version: 3,
		})
	})

	c := New(ts.URL, "test-token")
	order, err := c.GetOrder(t.Context(), "order-1")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if order.ID != 42 || order.State != "screened" || order.Version != 3 {
		t.Fatalf("GetOrder result = %+v, want ID=42 State=screened Version=3", order)
	}
	if order.AmountIn.Format() != "3000.000000" {
		t.Fatalf("AmountIn = %v, want 3000.000000", order.AmountIn.Format())
	}
}

func TestGetOrder_NotFoundClassifiesAsErrOrderNotFound(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "order_not_found", Message: "no order exists with that id"}})
	})

	c := New(ts.URL, "test-token")
	_, err := c.GetOrder(t.Context(), "missing")
	if !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("GetOrder error = %v, want ErrOrderNotFound", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("GetOrder error = %v, want it to also unwrap to *APIError", err)
	}
	if apiErr.Status != http.StatusNotFound || apiErr.Code != "order_not_found" {
		t.Fatalf("APIError = %+v, want Status=404 Code=order_not_found", apiErr)
	}
}

func TestGetAccountBalance_DecodesBalance(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/accounts/asset:tron:slot:1/balance" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(balanceResponse{AccountCode: "asset:tron:slot:1", Asset: "USDT_TRC20", Balance: "1234.560000"})
	})

	c := New(ts.URL, "test-token")
	bal, err := c.GetAccountBalance(t.Context(), "asset:tron:slot:1")
	if err != nil {
		t.Fatalf("GetAccountBalance: %v", err)
	}
	if bal.Format() != "1234.560000" {
		t.Fatalf("balance = %v, want 1234.560000", bal.Format())
	}
}

func TestGetAccountBalance_AccountNotFoundIsAPIError(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "account_not_found", Message: "no account exists with that code"}})
	})

	c := New(ts.URL, "test-token")
	_, err := c.GetAccountBalance(t.Context(), "asset:tron:slot:99")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("GetAccountBalance error = %v, want *APIError", err)
	}
	if apiErr.Code != "account_not_found" {
		t.Fatalf("APIError.Code = %q, want account_not_found", apiErr.Code)
	}
}

func TestEnsureAccount_PostsToAccountsEndpoint(t *testing.T) {
	var gotBody postAccountRequest
	var gotIdemKey string
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/accounts" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotIdemKey = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(accountResponse{ID: 1, Code: gotBody.Code, Type: gotBody.Type, Asset: gotBody.Asset})
	})

	c := New(ts.URL, "test-token")
	err := c.EnsureAccount(t.Context(), "liability:customer:acme:USDT_TRC20", AccountLiability, "USDT_TRC20", "dispatcher:ensure-account:liability:customer:acme:USDT_TRC20")
	if err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	if gotBody.Code != "liability:customer:acme:USDT_TRC20" || gotBody.Type != "LIABILITY" || gotBody.Asset != "USDT_TRC20" {
		t.Fatalf("request body = %+v", gotBody)
	}
	if gotIdemKey == "" {
		t.Fatal("Idempotency-Key header was not sent")
	}
}

func TestEnsureAccount_NonCreatedStatusIsAnError(t *testing.T) {
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(apiErrorEnvelope{Error: apiErrorBody{Code: "invalid_request", Message: "bad type"}})
	})

	c := New(ts.URL, "test-token")
	err := c.EnsureAccount(t.Context(), "bogus", AccountAsset, "USDT_TRC20", "idem-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "invalid_request" {
		t.Fatalf("EnsureAccount error = %v, want *APIError with code invalid_request", err)
	}
}

func TestClient_SendsBearerToken(t *testing.T) {
	var gotAuth string
	ts := fakeC1Server(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(entryResponse{ID: 1, IdempotencyKey: "k1", Outcome: "created"})
	})

	c := New(ts.URL, "test-token")
	if _, err := c.GetEntryByIdempotencyKey(t.Context(), "k1"); err != nil {
		t.Fatalf("GetEntryByIdempotencyKey: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization header = %q, want Bearer test-token", gotAuth)
	}
}
