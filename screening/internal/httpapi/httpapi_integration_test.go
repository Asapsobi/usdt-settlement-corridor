//go:build integration

// Requires a real, reachable Postgres 16 instance (SCREENING_TEST_DATABASE_URL);
// LEDGER_TEST_DATABASE_URL plus a sibling ledger checkout are needed
// only for the tests that exercise release against a real C1. Run via
// `make test-integration`.
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

	"screening/internal/cache"
	"screening/internal/db"
	"screening/internal/holds"
	"screening/internal/httpapi"
	"screening/internal/ledgerclient"
	"screening/internal/provider"
	"screening/internal/rescreen"
	"screening/internal/testledger"
	"screening/internal/verdict"
)

const (
	testToken        = "c38-test-token"
	testActor        = "screening"
	httpLedgerListen = ":18440"
	httpLedgerToken  = "c38-integration-test-token"
	httpLedgerActor  = "screening"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("SCREENING_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SCREENING_TEST_DATABASE_URL not set; skipping integration test")
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

// testServer spins up the real router (httptest.NewServer, real HTTP
// over a loopback socket) against the shared screening_test database.
// ledgerClient may be nil for tests that never reach a release/reject
// call.
func testServer(t *testing.T, ledgerClient *ledgerclient.Client) (baseURL string, pool *db.Pool) {
	baseURL, pool, _ = testServerWithMetrics(t, ledgerClient)
	return baseURL, pool
}

func testServerWithMetrics(t *testing.T, ledgerClient *ledgerclient.Client) (baseURL string, pool *db.Pool, server *httpapi.Server) {
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

	if _, err := rawPool.Exec(ctx, `TRUNCATE rescreen_flags, holds, screening_queue, screening_results, screening_result_invalidations`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}

	server = &httpapi.Server{
		Pool:         wrapped,
		Auth:         httpapi.AuthConfig{Tokens: map[string]string{testToken: testActor}},
		LedgerClient: ledgerClient,
		BuildInfo:    func() (string, string) { return "test", "test" },
	}
	router := httpapi.NewRouter(server)

	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)
	return ts.URL, wrapped, server
}

func doRequest(t *testing.T, method, url, token string, body any) *http.Response {
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
	baseURL, _, server := testServerWithMetrics(t, nil)

	// A CounterVec (VerdictsTotal) and plain Counters produce NO output
	// line at all in Prometheus's exposition format until at least one
	// increment has actually happened -- drive each one once so this
	// test asserts on real behavior, not an artifact of a freshly
	// constructed, never-used registry.
	server.Metrics.VerdictReported(verdict.Pass)
	server.Metrics.HoldOpened()
	server.Metrics.HoldReleased()
	server.Metrics.HoldRejected()
	server.Metrics.FlagRecorded()
	server.Metrics.VendorUnavailable()

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
		"screening_verdicts_total", "screening_holds_opened_total", "screening_holds_released_total",
		"screening_holds_rejected_total", "screening_rescreen_flags_total", "screening_vendor_unavailable_total",
		"screening_queue_depth", "screening_queue_oldest_pending_age_seconds",
	} {
		if !bytes.Contains(body, []byte(name)) {
			t.Errorf("/metrics output missing %q", name)
		}
	}
}

func TestErrorUnauthorized(t *testing.T) {
	baseURL, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/holds", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if decodeError(t, resp).Error.Code != "unauthorized" {
		t.Fatalf("code = %q, want unauthorized", decodeError(t, resp).Error.Code)
	}
}

func seedHold(t *testing.T, pool *db.Pool, orderID int64, externalID string) holds.Hold {
	t.Helper()
	ctx := context.Background()
	address := fmt.Sprintf("0xHttpTestAddr-%s-%d", t.Name(), time.Now().UnixNano())
	v := provider.Verdict{RiskScore: 0.9, Flagged: true, ProviderName: "mock", CheckedAt: time.Now().UTC()}
	resultID, err := cache.Put(ctx, pool, "mock", address, v, time.Hour)
	if err != nil {
		t.Fatalf("seeding screening_results: %v", err)
	}
	decision := verdict.Decision{Classification: verdict.Hold, ReasonCode: verdict.ReasonHoldFlagged, ScreeningResultID: resultID}
	h, err := holds.Open(ctx, pool, ledgerclient.OrderRef{OrderID: orderID, ExternalID: externalID}, decision)
	if err != nil {
		t.Fatalf("seeding hold: %v", err)
	}
	return h
}

func TestGetHolds_FiltersByStatus(t *testing.T) {
	baseURL, pool := testServer(t, nil)
	n := time.Now().UnixNano()
	seedHold(t, pool, n, fmt.Sprintf("ext-%d-a", n))

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/holds?status=OPEN", testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result struct {
		Holds []map[string]any `json:"holds"`
	}
	decodeInto(t, resp, &result)
	if len(result.Holds) != 1 {
		t.Fatalf("got %d holds, want 1", len(result.Holds))
	}

	resp = doRequest(t, http.MethodGet, baseURL+"/v1/holds?status=RELEASED", testToken, nil)
	decodeInto(t, resp, &result)
	if len(result.Holds) != 0 {
		t.Fatalf("got %d RELEASED holds, want 0", len(result.Holds))
	}
}

func TestPostReleaseHold_RequiresReviewer(t *testing.T) {
	baseURL, pool := testServer(t, nil)
	n := time.Now().UnixNano()
	h := seedHold(t, pool, n, fmt.Sprintf("ext-%d-b", n))

	resp := doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/holds/%d/release", baseURL, h.ID), testToken,
		map[string]any{"reviewer": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if decodeError(t, resp).Error.Code != "invalid_request" {
		t.Fatalf("code = %q, want invalid_request", decodeError(t, resp).Error.Code)
	}
}

func TestPostReleaseHold_NotFound(t *testing.T) {
	baseURL, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/holds/999999999/release", testToken,
		map[string]any{"reviewer": "operator:alice"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if decodeError(t, resp).Error.Code != "hold_not_found" {
		t.Fatalf("code = %q, want hold_not_found", decodeError(t, resp).Error.Code)
	}
}

func TestPostRejectHold_ReturnsNotImplemented(t *testing.T) {
	baseURL, pool := testServer(t, nil)
	n := time.Now().UnixNano()
	h := seedHold(t, pool, n, fmt.Sprintf("ext-%d-c", n))

	resp := doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/holds/%d/reject", baseURL, h.ID), testToken,
		map[string]any{"reviewer": "operator:alice", "note": "clearly bad"})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
	if decodeError(t, resp).Error.Code != "refund_entry_not_implemented" {
		t.Fatalf("code = %q, want refund_entry_not_implemented", decodeError(t, resp).Error.Code)
	}

	// The hold must NOT have been resolved by the failed attempt.
	getResp := doRequest(t, http.MethodGet, baseURL+"/v1/holds?status=OPEN", testToken, nil)
	var result struct {
		Holds []map[string]any `json:"holds"`
	}
	decodeInto(t, getResp, &result)
	var stillOpen bool
	for _, hh := range result.Holds {
		if int64(hh["id"].(float64)) == h.ID {
			stillOpen = true
		}
	}
	if !stillOpen {
		t.Fatal("hold was resolved despite the refund entry failing to build")
	}
}

func TestScreeningResults_RequiresSenderAddress(t *testing.T) {
	baseURL, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/screening-results", testToken, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestScreeningResults_ListAndInvalidate(t *testing.T) {
	baseURL, pool := testServer(t, nil)
	ctx := context.Background()
	address := fmt.Sprintf("0xHttpScreeningResultAddr%d", time.Now().UnixNano())
	v := provider.Verdict{RiskScore: 0.1, Flagged: false, ProviderName: "mock", CheckedAt: time.Now().UTC()}
	resultID, err := cache.Put(ctx, pool, "mock", address, v, time.Hour)
	if err != nil {
		t.Fatalf("seeding screening_results: %v", err)
	}

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/screening-results?sender_address="+address, testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var listResult struct {
		ScreeningResults []map[string]any `json:"screening_results"`
	}
	decodeInto(t, resp, &listResult)
	if len(listResult.ScreeningResults) != 1 {
		t.Fatalf("got %d results, want 1", len(listResult.ScreeningResults))
	}

	invalidateResp := doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/screening-results/%d/invalidate", baseURL, resultID),
		testToken, map[string]any{"reason": "operator distrusts this", "actor": "operator:alice"})
	if invalidateResp.StatusCode != http.StatusOK {
		t.Fatalf("invalidate status = %d, want 200", invalidateResp.StatusCode)
	}

	got, err := cache.Get(ctx, pool, "mock", address)
	if err != nil {
		t.Fatalf("cache.Get: %v", err)
	}
	if got != nil {
		t.Fatal("cache.Get returned a result for an address just invalidated via HTTP")
	}
}

func TestScreeningResults_InvalidateNotFound(t *testing.T) {
	baseURL, _ := testServer(t, nil)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/screening-results/999999999/invalidate", testToken,
		map[string]any{"reason": "x", "actor": "operator:alice"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func seedRescreenFlag(t *testing.T, pool *db.Pool) rescreen.Flag {
	t.Helper()
	ctx := context.Background()
	address := fmt.Sprintf("0xHttpRescreenAddr%d", time.Now().UnixNano())
	clean := provider.Verdict{RiskScore: 0.1, Flagged: false, ProviderName: "mock", CheckedAt: time.Now().UTC()}
	prevID, err := cache.Put(ctx, pool, "mock", address, clean, time.Hour)
	if err != nil {
		t.Fatalf("seeding previous verdict: %v", err)
	}
	flagged := provider.Verdict{RiskScore: 0.99, Flagged: true, ProviderName: "mock", CheckedAt: time.Now().UTC()}
	newID, err := cache.Put(ctx, pool, "mock", address, flagged, time.Hour)
	if err != nil {
		t.Fatalf("seeding new verdict: %v", err)
	}
	n := time.Now().UnixNano()
	_, err = pool.Exec(ctx, `
		INSERT INTO rescreen_flags (order_id, external_id, order_state_at_detection, previous_verdict_id, new_verdict_id)
		VALUES ($1, $2, 'screened', $3, $4)
	`, n, fmt.Sprintf("ext-rescreen-%d", n), prevID, newID)
	if err != nil {
		t.Fatalf("seeding rescreen_flags row: %v", err)
	}
	f, err := rescreen.Get(ctx, pool, mustGetFlagID(t, pool, n))
	if err != nil {
		t.Fatalf("rescreen.Get: %v", err)
	}
	return f
}

func mustGetFlagID(t *testing.T, pool *db.Pool, orderID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM rescreen_flags WHERE order_id = $1`, orderID).Scan(&id); err != nil {
		t.Fatalf("looking up seeded flag id: %v", err)
	}
	return id
}

func TestRescreenFlags_ListAndResolve(t *testing.T) {
	baseURL, pool := testServer(t, nil)
	f := seedRescreenFlag(t, pool)

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/rescreen-flags?resolved=false", testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var listResult struct {
		RescreenFlags []map[string]any `json:"rescreen_flags"`
	}
	decodeInto(t, resp, &listResult)
	var found bool
	for _, ff := range listResult.RescreenFlags {
		if int64(ff["id"].(float64)) == f.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("seeded flag not found in resolved=false listing")
	}

	resolveResp := doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/rescreen-flags/%d/resolve", baseURL, f.ID), testToken,
		map[string]any{"resolution": "confirmed, releasing manually", "actor": "operator:alice"})
	if resolveResp.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200", resolveResp.StatusCode)
	}

	after, err := rescreen.Get(context.Background(), pool, f.ID)
	if err != nil {
		t.Fatalf("rescreen.Get: %v", err)
	}
	if after.Resolution == nil {
		t.Fatal("flag was not resolved")
	}
}

func TestRescreenFlags_ResolveRequiresActorAndResolution(t *testing.T) {
	baseURL, pool := testServer(t, nil)
	f := seedRescreenFlag(t, pool)

	resp := doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/rescreen-flags/%d/resolve", baseURL, f.ID), testToken,
		map[string]any{"resolution": "", "actor": "operator:alice"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty resolution: status = %d, want 400", resp.StatusCode)
	}

	resp = doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/rescreen-flags/%d/resolve", baseURL, f.ID), testToken,
		map[string]any{"resolution": "confirmed", "actor": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty actor: status = %d, want 400", resp.StatusCode)
	}
}

func TestGetSystemQueue(t *testing.T) {
	baseURL, pool := testServer(t, nil)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO screening_queue (order_id, external_id, sender_address, status)
		VALUES ($1, $2, $3, 'PENDING')
	`, time.Now().UnixNano(), fmt.Sprintf("ext-queue-%d", time.Now().UnixNano()), "0xQueueAddr"); err != nil {
		t.Fatalf("seeding screening_queue: %v", err)
	}

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/system/queue", testToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var result struct {
		DepthByStatus           map[string]int `json:"depth_by_status"`
		OldestPendingAgeSeconds *float64       `json:"oldest_pending_age_seconds"`
	}
	decodeInto(t, resp, &result)
	if result.DepthByStatus["PENDING"] < 1 {
		t.Fatalf("depth_by_status[PENDING] = %d, want >= 1", result.DepthByStatus["PENDING"])
	}
	if result.OldestPendingAgeSeconds == nil {
		t.Fatal("oldest_pending_age_seconds is nil, want set (a PENDING row exists)")
	}
}

// TestPostReleaseHold_AgainstRealC1 is this chunk's own live proof: the
// full HTTP path (auth -> decode -> holds.Release -> a real ledgerclient
// -> a real ledgerd) actually moves an order held->screened, mirroring
// internal/holds' own live test but through the HTTP boundary this
// chunk adds on top of it.
func TestPostReleaseHold_AgainstRealC1(t *testing.T) {
	ll := testledger.Start(t, httpLedgerListen, httpLedgerToken, httpLedgerActor)
	client := ledgerclient.New(ll.BaseURL(), httpLedgerToken)
	baseURL, pool := testServer(t, client)

	externalID := "c38-release-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c38-release-cust-" + fmt.Sprint(time.Now().UnixNano())
	const sender = "0xHttpReleaseSenderLive00000001"
	order := ll.CreateOrder(externalID, customerID)
	funded := ll.FundOrder(order, customerID, sender)

	heldBody := map[string]any{
		"to_state": "held", "expected_version": funded.Version,
		"reason": "screening_hold_flagged", "occurred_at": time.Now().UTC(),
	}
	resp, body := ll.Do("POST", fmt.Sprintf("/v1/orders/%s/transitions", externalID),
		"screening:hold:"+fmt.Sprint(funded.ID)+":1", heldBody)
	if resp.StatusCode != 200 {
		t.Fatalf("moving order to held: status %d: %s", resp.StatusCode, body)
	}

	h := seedHold(t, pool, funded.ID, externalID)

	releaseResp := doRequest(t, http.MethodPost, fmt.Sprintf("%s/v1/holds/%d/release", baseURL, h.ID), testToken,
		map[string]any{"reviewer": "operator:alice", "note": "confirmed clean"})
	if releaseResp.StatusCode != http.StatusOK {
		errBody := decodeError(t, releaseResp)
		t.Fatalf("release status = %d, code = %s, message = %s", releaseResp.StatusCode, errBody.Error.Code, errBody.Error.Message)
	}

	after := ll.GetOrder(externalID)
	if after.State != "screened" {
		t.Fatalf("order state = %q, want screened", after.State)
	}
}
