//go:build integration

package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGetOrderStatus_AddressPendingNeverCallsC1Get(t *testing.T) {
	c1f := &fakeC1Full{}
	c1 := newFakeC1Full(t, c1f)
	c2f := &fakeC2{failAssign: true}
	c2 := newFakeC2(t, c2f)
	s, custStore := newOrdersServer(t, c1.URL, c2.URL)
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "status-pending-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}
	quote := issueQuote(t, router, rawKey)
	externalID := uniqueExternalID("ext-status-pending")
	body := `{"quote_id":` + strconv.Itoa(int(quote["quote_id"].(float64))) + `,"external_id":"` + externalID + `"}`

	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Idempotency-Key", "status-idem-1")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating order: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/v1/orders/"+externalID, nil)
	getReq.Header.Set("Authorization", "Bearer "+rawKey)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status: code = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	var resp orderStatusResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Status != "address_pending" {
		t.Errorf("Status = %q, want address_pending", resp.Status)
	}
	if resp.DepositAddress != nil {
		t.Errorf("DepositAddress = %v, want nil", resp.DepositAddress)
	}
	if c1f.getCalls != 0 {
		t.Errorf("C1 GET /v1/orders/{id} call count = %d, want 0 (no C1 order to read yet from the customer's own perspective)", c1f.getCalls)
	}
}

func TestGetOrderStatus_TranslatesC1State(t *testing.T) {
	c1f := &fakeC1Full{}
	c1 := newFakeC1Full(t, c1f)
	c2f := &fakeC2{}
	c2 := newFakeC2(t, c2f)
	s, custStore := newOrdersServer(t, c1.URL, c2.URL)
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "status-settled-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}
	quote := issueQuote(t, router, rawKey)
	externalID := uniqueExternalID("ext-status-settled")
	body := `{"quote_id":` + strconv.Itoa(int(quote["quote_id"].(float64))) + `,"external_id":"` + externalID + `"}`

	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Idempotency-Key", "status-idem-2")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating order: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	c1f.getState.Store("settled")
	getReq := httptest.NewRequest(http.MethodGet, "/v1/orders/"+externalID, nil)
	getReq.Header.Set("Authorization", "Bearer "+rawKey)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status: code = %d, body = %s", getRec.Code, getRec.Body.String())
	}
	var resp orderStatusResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Status != "settled" {
		t.Errorf("Status = %q, want settled", resp.Status)
	}
	if resp.DepositAddress == nil {
		t.Errorf("DepositAddress = nil, want a real address once C2 assigned one")
	}
	if c1f.getCalls != 1 {
		t.Errorf("C1 GET /v1/orders/{id} call count = %d, want 1", c1f.getCalls)
	}
}

func TestGetOrderStatus_UnrecognizedC1StateRefusesToPassThrough(t *testing.T) {
	c1f := &fakeC1Full{}
	c1 := newFakeC1Full(t, c1f)
	c2f := &fakeC2{}
	c2 := newFakeC2(t, c2f)
	s, custStore := newOrdersServer(t, c1.URL, c2.URL)
	router := NewRouter(s)

	_, rawKey, err := custStore.Create(t.Context(), "status-unknown-co")
	if err != nil {
		t.Fatalf("Create customer: %v", err)
	}
	quote := issueQuote(t, router, rawKey)
	externalID := uniqueExternalID("ext-status-unknown")
	body := `{"quote_id":` + strconv.Itoa(int(quote["quote_id"].(float64))) + `,"external_id":"` + externalID + `"}`

	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Idempotency-Key", "status-idem-3")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating order: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	c1f.getState.Store("some_future_state_this_gateway_has_never_heard_of")
	getReq := httptest.NewRequest(http.MethodGet, "/v1/orders/"+externalID, nil)
	getReq.Header.Set("Authorization", "Bearer "+rawKey)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500 (refused, not passed through), body = %s", getRec.Code, getRec.Body.String())
	}
	if strings.Contains(getRec.Body.String(), "some_future_state") {
		t.Errorf("unrecognized internal state string leaked into the customer-facing response body: %s", getRec.Body.String())
	}
}

func TestGetOrderStatus_OwnershipAndMissingBoth404(t *testing.T) {
	c1f := &fakeC1Full{}
	c1 := newFakeC1Full(t, c1f)
	c2f := &fakeC2{}
	c2 := newFakeC2(t, c2f)
	s, custStore := newOrdersServer(t, c1.URL, c2.URL)
	router := NewRouter(s)

	_, ownerKey, err := custStore.Create(t.Context(), "status-owner-co")
	if err != nil {
		t.Fatalf("Create owner customer: %v", err)
	}
	_, otherKey, err := custStore.Create(t.Context(), "status-other-co")
	if err != nil {
		t.Fatalf("Create other customer: %v", err)
	}

	quote := issueQuote(t, router, ownerKey)
	externalID := uniqueExternalID("ext-status-owned")
	body := `{"quote_id":` + strconv.Itoa(int(quote["quote_id"].(float64))) + `,"external_id":"` + externalID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ownerKey)
	req.Header.Set("Idempotency-Key", "status-idem-4")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating order: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// A completely nonexistent external_id, from the owner's own key.
	missingReq := httptest.NewRequest(http.MethodGet, "/v1/orders/"+uniqueExternalID("ext-status-nonexistent"), nil)
	missingReq.Header.Set("Authorization", "Bearer "+ownerKey)
	missingRec := httptest.NewRecorder()
	router.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Errorf("nonexistent external_id: code = %d, want 404", missingRec.Code)
	}

	// The owner's real order, queried with a different customer's key.
	otherReq := httptest.NewRequest(http.MethodGet, "/v1/orders/"+externalID, nil)
	otherReq.Header.Set("Authorization", "Bearer "+otherKey)
	otherRec := httptest.NewRecorder()
	router.ServeHTTP(otherRec, otherReq)
	if otherRec.Code != http.StatusNotFound {
		t.Errorf("another customer's external_id: code = %d, want 404", otherRec.Code)
	}
}
