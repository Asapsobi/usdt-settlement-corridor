//go:build integration

// Requires a real, reachable Postgres 16 instance (BROKER_TEST_DATABASE_URL).
// TestPostReservation_AgainstRealC1 additionally needs
// LEDGER_TEST_DATABASE_URL plus a sibling ledger checkout -- see
// internal/testledger's own requirement. Run via `make test-integration`.
package httpapi_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"energybroker/internal/buffer"
	"energybroker/internal/db"
	"energybroker/internal/httpapi"
	"energybroker/internal/ledgerclient"
	"energybroker/internal/pricing"
	"energybroker/internal/provider"
	"energybroker/internal/reservations"
	"energybroker/internal/routing"
	"energybroker/internal/testledger"
)

const (
	testToken        = "c48-test-token"
	testActor        = "energy-broker-http-test"
	httpLedgerListen = ":18441"
	httpLedgerToken  = "c48-integration-test-token"
	httpLedgerActor  = "energy-broker"
	ceiling          = 25.7
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("BROKER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("BROKER_TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

func applyMigrations(t *testing.T, dbURL string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir(t)); err != nil {
		t.Fatalf("running migrations: %v", err)
	}
}

type fakeOrderResolver struct {
	order ledgerclient.Order
	err   error
}

func (f fakeOrderResolver) GetOrder(ctx context.Context, externalID string) (ledgerclient.Order, error) {
	return f.order, f.err
}

func defaultWeights() routing.RoutingWeights {
	return routing.RoutingWeights{
		provider.Tronsell: 0.60,
		provider.Netts:    0.35,
		provider.Catfee:   0.05,
	}
}

type stubDemandObserver struct{}

func (stubDemandObserver) RecentReservedUnits(ctx context.Context, window time.Duration, targetAddress string) (int64, error) {
	return 0, nil
}

// testServer spins up the real router (httptest.NewServer, real HTTP
// over a loopback socket) against the shared broker_test database, with
// a real Postgres-backed Buffer and Router (so /v1/buffer,
// /v1/system/*, and the reservation fast/slow paths all behave exactly
// as they do in production) around a set of MockProviders this test
// controls, and orders resolved via a caller-supplied OrderResolver --
// a fakeOrderResolver{} (nil orders) for tests that never reach
// POST /v1/reservations against a real order, or a real
// ledgerclient.Client against a real ledgerd for the one live-C1 test.
func testServer(t *testing.T, orders reservations.OrderResolver) (baseURL string, pool *db.Pool, server *httpapi.Server) {
	t.Helper()
	dbURL := testDatabaseURL(t)
	applyMigrations(t, dbURL)

	ctx := context.Background()
	rawPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(rawPool.Close)
	wrapped := &db.Pool{Pool: rawPool}

	if _, err := rawPool.Exec(ctx, `TRUNCATE energy_buffer, buffer_allocations, reservations, vendor_overcharge_events, manual_fallback_events RESTART IDENTITY`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}

	providers := map[string]provider.EnergyProvider{
		provider.Tronsell: provider.NewMockProvider(provider.Tronsell, 1, 24.0),
		provider.Netts:    provider.NewMockProvider(provider.Netts, 2, 28.0),
		provider.Catfee:   provider.NewMockProvider(provider.Catfee, 3, 30.0),
	}

	poller := pricing.NewPoller(wrapped, providers, time.Hour)
	if err := poller.PollAll(ctx); err != nil {
		t.Fatalf("PollAll: %v", err)
	}
	router := routing.NewRouter(poller, wrapped, 1)

	reader := buffer.NewFakeTronReader()
	reader.AutoConfirm(1_000_000)
	buf, err := buffer.NewBuffer(wrapped, providers, router, stubDemandObserver{}, reader, nil, buffer.Config{
		SlotAddresses: []string{"TSlotHTTPTest0000000000000001"},
		Ceiling:       ceiling,
	})
	if err != nil {
		t.Fatalf("NewBuffer: %v", err)
	}

	if orders == nil {
		orders = fakeOrderResolver{}
	}
	svc, err := reservations.NewService(wrapped, orders, buf, router, nil, nil, providers, reservations.Config{
		Weights: defaultWeights(), Ceiling: ceiling,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	server = &httpapi.Server{
		Pool:         wrapped,
		Auth:         httpapi.AuthConfig{Tokens: map[string]string{testToken: testActor}},
		Reservations: svc,
		Buffer:       buf,
		Router:       router,
		Poller:       poller,
		Weights:      defaultWeights(),
		Ceiling:      ceiling,
		BuildInfo:    func() (string, string) { return "test", "test" },
	}
	r := httpapi.NewRouter(server)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts.URL, wrapped, server
}

// seedAvailableRow inserts an AVAILABLE energy_buffer row directly,
// slotted under slotAddress -- these tests care about the HTTP
// boundary's own translation, not about how a warm buffer got that way
// (internal/buffer's and internal/reservations' own tests already cover
// Replenish/Create exhaustively). A row only serves Reserve for a
// reservation whose own target_address matches slotAddress exactly.
func seedAvailableRow(t *testing.T, pool *db.Pool, providerName, delegationID, slotAddress string, units, costTRX int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO energy_buffer (provider_name, delegation_id, slot_address, units, acquired_at, cost_trx, expires_at, status)
		VALUES ($1, $2, $3, $4, now(), $5, now() + interval '1 hour', 'AVAILABLE')
	`, providerName, delegationID, slotAddress, units, costTRX)
	if err != nil {
		t.Fatalf("seeding available row: %v", err)
	}
}

// seedReservationRow inserts a reservation row directly, bypassing
// Create() entirely -- for OC.5's own list/reconcile tests, which need a
// row already sitting at a specific status (most usefully FAILED), not
// one that reached it through a real vendor call.
func seedReservationRow(t *testing.T, pool *db.Pool, idempotencyKey, externalID string, orderID int64, targetAddress, status string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO reservations (idempotency_key, external_id, order_id, target_address, energy_units, tier, status, deadline)
		VALUES ($1, $2, $3, $4, 65000, 'DIRECT', $5, now() + interval '1 hour')
		RETURNING id
	`, idempotencyKey, externalID, orderID, targetAddress, status).Scan(&id)
	if err != nil {
		t.Fatalf("seeding reservation row: %v", err)
	}
	return id
}

func doRequest(t *testing.T, method, url, token string, body any) *http.Response {
	t.Helper()
	return doRequestWithIdempotencyKey(t, method, url, token, "", body)
}

func doRequestWithIdempotencyKey(t *testing.T, method, url, token, idempotencyKey string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeError(t *testing.T, resp *http.Response) errorResponse {
	t.Helper()
	var e errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	return e
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

func TestHealthzReadyzMetricsUnauthenticated(t *testing.T) {
	baseURL, _, server := testServer(t, nil)

	// A CounterVec and plain Counters produce NO output line at all in
	// Prometheus's exposition format until at least one increment has
	// actually happened -- drive each one once so this test asserts on
	// real behavior, not an artifact of a freshly constructed, never-used
	// registry.
	server.Metrics.ReservationConfirmed(true)
	server.Metrics.ReservationFailed()
	server.Metrics.CeilingRejected(provider.Tronsell)
	server.Metrics.ManualFallbackEventTriggered()
	server.Metrics.ReplenishCost(provider.Tronsell, 1)

	resp := doRequest(t, http.MethodGet, baseURL+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}

	resp = doRequest(t, http.MethodGet, baseURL+"/readyz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want 200", resp.StatusCode)
	}

	resp = doRequest(t, http.MethodGet, baseURL+"/metrics", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	body := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	for _, name := range []string{
		"broker_reservations_total", "broker_buffer_level", "broker_buffer_target",
		"broker_replenish_cost_trx_total", "broker_manual_fallback_events_total",
		"broker_ceiling_rejections_total", "broker_price_staleness_seconds",
	} {
		if !bytes.Contains(body, []byte(name)) {
			t.Errorf("/metrics output missing %q", name)
		}
	}
}

func TestErrorUnauthorized(t *testing.T) {
	baseURL, _, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/buffer", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if decodeError(t, resp).Error.Code != "unauthorized" {
		t.Fatalf("code = %q, want unauthorized", decodeError(t, resp).Error.Code)
	}
}

func TestPostReservation_RequiresIdempotencyKey(t *testing.T) {
	baseURL, _, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/reservations", testToken, map[string]any{
		"external_id": "no-idem-key", "target_address": "TX", "energy_units": 1, "tier": "STANDARD",
		"deadline": time.Now().Add(time.Second),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if decodeError(t, resp).Error.Code != "invalid_request" {
		t.Fatalf("code = %q, want invalid_request", decodeError(t, resp).Error.Code)
	}
}

func TestPostReservation_FastPath_ThenGetReservation(t *testing.T) {
	baseURL, pool, _ := testServer(t, fakeOrderResolver{order: ledgerclient.Order{ID: 501, ExternalID: "http-fast-1"}})
	seedAvailableRow(t, pool, provider.Tronsell, "seed-http-1", "TPayoutHTTPTest0000000000001", 500, 1_200000)

	resp := doRequestWithIdempotencyKey(t, http.MethodPost, baseURL+"/v1/reservations", testToken, "dispatch:http-fast-1:1", map[string]any{
		"external_id":    "http-fast-1",
		"target_address": "TPayoutHTTPTest0000000000001",
		"energy_units":   500,
		"tier":           "STANDARD",
		"deadline":       time.Now().Add(5 * time.Second),
	})
	if resp.StatusCode != http.StatusCreated {
		errBody := decodeError(t, resp)
		t.Fatalf("status = %d, code = %s, message = %s", resp.StatusCode, errBody.Error.Code, errBody.Error.Message)
	}
	var created struct {
		ID       int64   `json:"id"`
		OrderID  int64   `json:"order_id"`
		Status   string  `json:"status"`
		Vendor   *string `json:"vendor"`
		FastPath *bool   `json:"fast_path"`
	}
	decodeInto(t, resp, &created)
	if created.Status != "CONFIRMED" {
		t.Fatalf("status = %q, want CONFIRMED", created.Status)
	}
	if created.Vendor == nil || *created.Vendor != provider.Tronsell {
		t.Fatalf("vendor = %v, want tronsell", created.Vendor)
	}
	if created.FastPath == nil || !*created.FastPath {
		t.Fatalf("fast_path = %v, want true", created.FastPath)
	}
	if created.OrderID != 501 {
		t.Fatalf("order_id = %d, want 501", created.OrderID)
	}

	getResp := doRequest(t, http.MethodGet, fmt.Sprintf("%s/v1/reservations/%d", baseURL, created.ID), testToken, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
	}
	var fetched struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	decodeInto(t, getResp, &fetched)
	if fetched.ID != created.ID || fetched.Status != "CONFIRMED" {
		t.Fatalf("fetched %+v, want id=%d status=CONFIRMED", fetched, created.ID)
	}
}

func TestGetReservation_NotFound(t *testing.T) {
	baseURL, _, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/reservations/999999999", testToken, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if decodeError(t, resp).Error.Code != "reservation_not_found" {
		t.Fatalf("code = %q, want reservation_not_found", decodeError(t, resp).Error.Code)
	}
}

func TestGetBuffer(t *testing.T) {
	baseURL, pool, _ := testServer(t, nil)
	seedAvailableRow(t, pool, provider.Tronsell, "seed-buf-1", "TPayoutHTTPBuf00000000000001", 300, 700000)

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/buffer", testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result struct {
		Providers []struct {
			ProviderName string `json:"provider_name"`
			Available    int64  `json:"available"`
			Reserved     int64  `json:"reserved"`
		} `json:"providers"`
	}
	decodeInto(t, resp, &result)
	var found bool
	for _, p := range result.Providers {
		if p.ProviderName == provider.Tronsell && p.Available >= 300 {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded tronsell availability not reflected in GET /v1/buffer: %+v", result.Providers)
	}
}

func TestManualFallbackEvents_ListAndResolve(t *testing.T) {
	baseURL, _, server := testServer(t, nil)
	ctx := context.Background()
	event, err := server.Router.OnFallbackTriggered(ctx, routing.ReasonManualRequired, nil)
	if err != nil {
		t.Fatalf("OnFallbackTriggered: %v", err)
	}

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/manual-fallback-events?resolved=false", testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var listResult struct {
		Events []struct {
			ID int64 `json:"id"`
		} `json:"events"`
	}
	decodeInto(t, resp, &listResult)
	var listed bool
	for _, e := range listResult.Events {
		if e.ID == event.ID {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("seeded fallback event %d not found in resolved=false listing", event.ID)
	}

	resolveResp := doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/manual-fallback-events/%d/resolve", baseURL, event.ID), testToken,
		map[string]any{"resolution": "vendors recovered", "actor": "operator:bob"})
	if resolveResp.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200", resolveResp.StatusCode)
	}
	var resolved struct {
		ResolvedBy *string `json:"resolved_by"`
		Resolution *string `json:"resolution"`
	}
	decodeInto(t, resolveResp, &resolved)
	if resolved.ResolvedBy == nil || *resolved.ResolvedBy != "operator:bob" {
		t.Fatalf("resolved_by = %v, want operator:bob", resolved.ResolvedBy)
	}
	if resolved.Resolution == nil || *resolved.Resolution != "vendors recovered" {
		t.Fatalf("resolution = %v, want %q", resolved.Resolution, "vendors recovered")
	}
}

func TestManualFallbackEvents_ResolveRequiresActorAndResolution(t *testing.T) {
	baseURL, _, server := testServer(t, nil)
	event, err := server.Router.OnFallbackTriggered(context.Background(), routing.ReasonFallbackLadder, nil)
	if err != nil {
		t.Fatalf("OnFallbackTriggered: %v", err)
	}

	resp := doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/manual-fallback-events/%d/resolve", baseURL, event.ID), testToken,
		map[string]any{"resolution": "", "actor": "operator:bob"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty resolution: status = %d, want 400", resp.StatusCode)
	}

	resp = doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/manual-fallback-events/%d/resolve", baseURL, event.ID), testToken,
		map[string]any{"resolution": "recovered", "actor": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty actor: status = %d, want 400", resp.StatusCode)
	}
}

func TestManualFallbackEvents_ResolveNotFound(t *testing.T) {
	baseURL, _, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/manual-fallback-events/999999999/resolve", testToken,
		map[string]any{"resolution": "x", "actor": "operator:bob"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestGetSystemPrices(t *testing.T) {
	baseURL, _, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/system/prices", testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result struct {
		Providers []struct {
			ProviderName string `json:"provider_name"`
			Healthy      bool   `json:"healthy"`
		} `json:"providers"`
		CeilingSunPerUnit float64 `json:"ceiling_sun_per_unit"`
	}
	decodeInto(t, resp, &result)
	if len(result.Providers) != 3 {
		t.Fatalf("got %d providers, want 3", len(result.Providers))
	}
	for _, p := range result.Providers {
		if !p.Healthy {
			t.Errorf("provider %q reported unhealthy right after PollAll", p.ProviderName)
		}
	}
	if result.CeilingSunPerUnit != ceiling {
		t.Fatalf("ceiling_sun_per_unit = %v, want %v", result.CeilingSunPerUnit, ceiling)
	}
}

func TestGetSystemInvariants(t *testing.T) {
	baseURL, pool, _ := testServer(t, fakeOrderResolver{order: ledgerclient.Order{ID: 900, ExternalID: "http-inv-1"}})
	seedAvailableRow(t, pool, provider.Tronsell, "seed-inv-1", "TPayoutHTTPInvariants000000001", 500, 1_000000)

	resp := doRequestWithIdempotencyKey(t, http.MethodPost, baseURL+"/v1/reservations", testToken, "dispatch:http-inv-1:1", map[string]any{
		"external_id":    "http-inv-1",
		"target_address": "TPayoutHTTPInvariants000000001",
		"energy_units":   500,
		"tier":           "STANDARD",
		"deadline":       time.Now().Add(5 * time.Second),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seeding reservation: status = %d", resp.StatusCode)
	}

	resp = doRequest(t, http.MethodGet, baseURL+"/v1/system/invariants", testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result struct {
		BufferAvailable        int64    `json:"buffer_available"`
		FastPathConfirmedCount int64    `json:"fast_path_confirmed_count"`
		FastPathRatio          *float64 `json:"fast_path_ratio"`
	}
	decodeInto(t, resp, &result)
	if result.FastPathConfirmedCount < 1 {
		t.Fatalf("fast_path_confirmed_count = %d, want >= 1", result.FastPathConfirmedCount)
	}
	if result.FastPathRatio == nil || *result.FastPathRatio != 1.0 {
		t.Fatalf("fast_path_ratio = %v, want 1.0 (only a fast-path confirmation happened)", result.FastPathRatio)
	}
}

// TestPostReservation_AgainstRealC1 is this chunk's own live proof: the
// full HTTP path (auth -> decode -> reservations.Create -> a real
// ledgerclient -> a real ledgerd) actually resolves a real C1 order and
// confirms a reservation against it, mirroring internal/reservations'
// own live tests but through the HTTP boundary this chunk adds on top
// of them.
func TestPostReservation_AgainstRealC1(t *testing.T) {
	ll := testledger.Start(t, httpLedgerListen, httpLedgerToken, httpLedgerActor)
	client := ledgerclient.New(ll.BaseURL(), httpLedgerToken)
	baseURL, pool, _ := testServer(t, client)

	externalID := "c48-http-live-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c48-http-live-cust-" + fmt.Sprint(time.Now().UnixNano())
	order := ll.CreateOrder(externalID, customerID)

	seedAvailableRow(t, pool, provider.Tronsell, "seed-live-1", "TPayoutHTTPLive00000000000001", 500, 1_200000)

	resp := doRequestWithIdempotencyKey(t, http.MethodPost, baseURL+"/v1/reservations", testToken, "dispatch:"+externalID+":1", map[string]any{
		"external_id":    externalID,
		"target_address": "TPayoutHTTPLive00000000000001",
		"energy_units":   500,
		"tier":           "STANDARD",
		"deadline":       time.Now().Add(5 * time.Second),
	})
	if resp.StatusCode != http.StatusCreated {
		errBody := decodeError(t, resp)
		t.Fatalf("status = %d, code = %s, message = %s", resp.StatusCode, errBody.Error.Code, errBody.Error.Message)
	}
	var result struct {
		OrderID int64  `json:"order_id"`
		Status  string `json:"status"`
	}
	decodeInto(t, resp, &result)
	if result.OrderID != order.ID {
		t.Fatalf("order_id = %d, want %d (the real C1 order's own id)", result.OrderID, order.ID)
	}
	if result.Status != "CONFIRMED" {
		t.Fatalf("status = %q, want CONFIRMED", result.Status)
	}
}

func TestPostReservation_OrderNotFoundOnRealC1(t *testing.T) {
	ll := testledger.Start(t, httpLedgerListen, httpLedgerToken, httpLedgerActor)
	client := ledgerclient.New(ll.BaseURL(), httpLedgerToken)
	baseURL, _, _ := testServer(t, client)

	resp := doRequestWithIdempotencyKey(t, http.MethodPost, baseURL+"/v1/reservations", testToken, "dispatch:no-such-order:1", map[string]any{
		"external_id":    "c48-http-no-such-order-" + fmt.Sprint(time.Now().UnixNano()),
		"target_address": "TPayoutHTTPMissing000000000001",
		"energy_units":   500,
		"tier":           "STANDARD",
		"deadline":       time.Now().Add(5 * time.Second),
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if decodeError(t, resp).Error.Code != "order_not_found" {
		t.Fatalf("code = %q, want order_not_found", decodeError(t, resp).Error.Code)
	}
}

func TestGetReservations_RequiresAtLeastOneStatus(t *testing.T) {
	baseURL, _, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/reservations", testToken, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGetReservations_FiltersByStatus(t *testing.T) {
	baseURL, pool, _ := testServer(t, nil)
	failedID := seedReservationRow(t, pool, "list-test-failed", "list-test-failed-ext", 9001, "TListTestFailed00000000000001", "FAILED")
	seedReservationRow(t, pool, "list-test-pending", "list-test-pending-ext", 9002, "TListTestPending0000000000001", "PENDING")

	var got struct {
		Reservations []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"reservations"`
	}
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/reservations?status=FAILED", testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	decodeInto(t, resp, &got)

	found := false
	for _, r := range got.Reservations {
		if r.ID == failedID {
			found = true
			if r.Status != "FAILED" {
				t.Errorf("status = %q, want FAILED", r.Status)
			}
		}
		if r.Status != "FAILED" {
			t.Errorf("filtering on status=FAILED returned a %s row", r.Status)
		}
	}
	if !found {
		t.Errorf("seeded FAILED reservation %d not present in status=FAILED results", failedID)
	}
}

func TestPostReservationReconcile_ConfirmsAFailedRow(t *testing.T) {
	baseURL, pool, _ := testServer(t, nil)
	id := seedReservationRow(t, pool, "reconcile-test-1", "reconcile-test-1-ext", 9101, "TReconcileTest000000000000001", "FAILED")

	resp := doRequestWithIdempotencyKey(t, http.MethodPost, fmt.Sprintf("%s/v1/reservations/%d/reconcile", baseURL, id), testToken, "reconcile:1", map[string]any{
		"order_id":       9101,
		"provider":       "catfee",
		"delegation_id":  "manual-test-delegation-1",
		"target_address": "TReconcileTest000000000000001",
		"energy_units":   65000,
		"cost_trx":       "1.950000",
	})
	if resp.StatusCode != http.StatusOK {
		errBody := decodeError(t, resp)
		t.Fatalf("status = %d, code = %s, message = %s", resp.StatusCode, errBody.Error.Code, errBody.Error.Message)
	}
	var got struct {
		Status  string  `json:"status"`
		Vendor  *string `json:"vendor"`
		CostTRX *string `json:"cost_trx"`
	}
	decodeInto(t, resp, &got)
	if got.Status != "CONFIRMED" {
		t.Fatalf("status = %q, want CONFIRMED", got.Status)
	}
	if got.Vendor == nil || *got.Vendor != "catfee" {
		t.Fatalf("vendor = %v, want catfee", got.Vendor)
	}
	if got.CostTRX == nil || *got.CostTRX != "1.950000" {
		t.Fatalf("cost_trx = %v, want 1.950000", got.CostTRX)
	}
}

func TestPostReservationReconcile_MissingFieldsReturns400(t *testing.T) {
	baseURL, pool, _ := testServer(t, nil)
	id := seedReservationRow(t, pool, "reconcile-test-2", "reconcile-test-2-ext", 9102, "TReconcileTest000000000000002", "FAILED")

	resp := doRequestWithIdempotencyKey(t, http.MethodPost, fmt.Sprintf("%s/v1/reservations/%d/reconcile", baseURL, id), testToken, "reconcile:2", map[string]any{
		"order_id": 9102,
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
