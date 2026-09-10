//go:build integration

package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gateway/internal/customers"
	"gateway/internal/ratelimit"
)

// testRouter builds a minimal router exercising exactly this package's
// own auth stack -- authMiddleware, requireActiveCustomer -- against one
// read route and one write route, standing in for C6.2/C6.3's own real
// routes (not yet built at this chunk). Never registered in NewRouter
// itself; this file's own test fixture only.
func testRouter(store *customers.Store, limiter *ratelimit.Limiter) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(authMiddleware(store, limiter))
		r.Get("/read", func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusOK, map[string]string{"customer": customerFromContext(r.Context()).Name})
		})
		r.With(requireActiveCustomer).Post("/write", func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusCreated, map[string]string{"customer": customerFromContext(r.Context()).Name})
		})
	})
	return r
}

func TestAuth_MissingOrInvalidKeyRejected(t *testing.T) {
	pool := testPool(t)
	router := testRouter(customers.NewStore(pool), ratelimit.New())

	for _, auth := range []string{"", "Bearer ", "Bearer not-a-real-key"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/read", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization=%q: status = %d, want 401", auth, rec.Code)
		}
	}
}

func TestAuth_ActiveCustomerCanReadAndWrite(t *testing.T) {
	pool := testPool(t)
	store := customers.NewStore(pool)
	router := testRouter(store, ratelimit.New())

	_, rawKey, err := store.Create(t.Context(), "active-co")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/v1/read", http.StatusOK},
		{http.MethodPost, "/v1/write", http.StatusCreated},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+rawKey)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

// TestAuth_SuspendedCustomerReadsButNotWrites is C6.1's own acceptance
// criterion verbatim: "Suspended customer: 403 on quote/order creation,
// 200 on read/status endpoints."
func TestAuth_SuspendedCustomerReadsButNotWrites(t *testing.T) {
	pool := testPool(t)
	store := customers.NewStore(pool)
	router := testRouter(store, ratelimit.New())

	c, rawKey, err := store.Create(t.Context(), "suspended-co")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Suspend(t.Context(), c.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	readReq := httptest.NewRequest(http.MethodGet, "/v1/read", nil)
	readReq.Header.Set("Authorization", "Bearer "+rawKey)
	readRec := httptest.NewRecorder()
	router.ServeHTTP(readRec, readReq)
	if readRec.Code != http.StatusOK {
		t.Errorf("suspended customer GET /v1/read: status = %d, want 200", readRec.Code)
	}

	writeReq := httptest.NewRequest(http.MethodPost, "/v1/write", nil)
	writeReq.Header.Set("Authorization", "Bearer "+rawKey)
	writeRec := httptest.NewRecorder()
	router.ServeHTTP(writeRec, writeReq)
	if writeRec.Code != http.StatusForbidden {
		t.Errorf("suspended customer POST /v1/write: status = %d, want 403", writeRec.Code)
	}
}

// TestAuth_RateLimitExceeded is C6.1's own acceptance criterion: 429
// with a Retry-After header once a customer's own configured limit is
// exhausted.
func TestAuth_RateLimitExceeded(t *testing.T) {
	pool := testPool(t)
	store := customers.NewStore(pool)
	router := testRouter(store, ratelimit.New())

	c, rawKey, err := store.Create(t.Context(), "low-limit-co")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lowLimit := 2
	if _, err := pool.Exec(t.Context(), `UPDATE customers SET rate_limit_per_minute = $1 WHERE id = $2`, lowLimit, c.ID); err != nil {
		t.Fatalf("setting rate_limit_per_minute: %v", err)
	}

	var lastRec *httptest.ResponseRecorder
	for i := 0; i < lowLimit+1; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/read", nil)
		req.Header.Set("Authorization", "Bearer "+rawKey)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		lastRec = rec
	}
	if lastRec.Code != http.StatusTooManyRequests {
		t.Fatalf("request past the configured limit: status = %d, want 429", lastRec.Code)
	}
	if lastRec.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
	if _, err := strconv.Atoi(lastRec.Header().Get("Retry-After")); err != nil {
		t.Errorf("Retry-After = %q, want a plain integer seconds value", lastRec.Header().Get("Retry-After"))
	}
}

// TestAuth_APIKeyNeverLogged is C6.1's own acceptance criterion, taken
// literally: grep the actual log output for the raw key value, don't
// just eyeball the code. Drives one full request cycle (success and
// failure) with logging captured to a buffer, then asserts the raw key
// never appears in it.
func TestAuth_APIKeyNeverLogged(t *testing.T) {
	pool := testPool(t)
	store := customers.NewStore(pool)
	router := testRouter(store, ratelimit.New())

	c, rawKey, err := store.Create(t.Context(), "logged-co")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	// A successful call, a call with the raw key but suspended (403),
	// and a call with an outright wrong key (401) -- every path that
	// might be tempted to log "here's the key that was rejected".
	for _, req := range []*http.Request{
		authedRequest(http.MethodGet, "/v1/read", rawKey),
		authedRequest(http.MethodGet, "/v1/read", "sk_live_definitely_wrong"),
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
	}
	if err := store.Suspend(t.Context(), c.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, authedRequest(http.MethodPost, "/v1/write", rawKey))

	logged := logBuf.String()
	if strings.Contains(logged, rawKey) {
		t.Fatalf("raw API key appeared in log output: %q", logged)
	}
	if strings.Contains(logged, "sk_live_definitely_wrong") {
		t.Fatalf("rejected raw API key appeared in log output: %q", logged)
	}
}

func authedRequest(method, path, rawKey string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	return req
}
