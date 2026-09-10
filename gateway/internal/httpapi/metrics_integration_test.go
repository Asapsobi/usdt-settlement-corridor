//go:build integration

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestMetrics_QuoteAndOrderCountersIncrement is C6.8's own acceptance
// criterion made concrete: quotes_issued_total and orders_created_total
// are real, live counters, not just declared.
func TestMetrics_QuoteAndOrderCountersIncrement(t *testing.T) {
	c1f := &fakeC1Full{}
	c1 := newFakeC1Full(t, c1f)
	c2f := &fakeC2{}
	c2 := newFakeC2(t, c2f)
	s, custStore := newOrdersServer(t, c1.URL, c2.URL)
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "metrics-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}

	before := scrapeCounterValue(t, router, "gateway_quotes_issued_total")
	quote := issueQuote(t, router, rawKey)
	afterQuote := scrapeCounterValue(t, router, "gateway_quotes_issued_total")
	if afterQuote != before+1 {
		t.Errorf("gateway_quotes_issued_total went from %v to %v, want +1", before, afterQuote)
	}

	beforeOrders := scrapeCounterValue(t, router, "gateway_orders_created_total")
	externalID := uniqueExternalID("ext-metrics")
	body := `{"quote_id":` + strconv.Itoa(int(quote["quote_id"].(float64))) + `,"external_id":"` + externalID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Idempotency-Key", "metrics-idem-1")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating order: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	afterOrders := scrapeCounterValue(t, router, "gateway_orders_created_total")
	if afterOrders != beforeOrders+1 {
		t.Errorf("gateway_orders_created_total went from %v to %v, want +1", beforeOrders, afterOrders)
	}

	// A retry of the same order must NOT increment the counter again --
	// only a genuine new C1 creation counts.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+rawKey)
	req2.Header.Set("Idempotency-Key", "metrics-idem-1")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("retrying order: status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	afterRetry := scrapeCounterValue(t, router, "gateway_orders_created_total")
	if afterRetry != afterOrders {
		t.Errorf("gateway_orders_created_total changed on a retry: %v -> %v", afterOrders, afterRetry)
	}
}

// scrapeCounterValue GETs /metrics and returns the value of a
// label-less counter by name (the last whitespace-separated field on
// its own, non-comment line).
func scrapeCounterValue(t *testing.T, router http.Handler, name string) float64 {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: status = %d", rec.Code)
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			v, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				t.Fatalf("parsing metric value %q for %s: %v", fields[1], name, err)
			}
			return v
		}
	}
	return 0
}
