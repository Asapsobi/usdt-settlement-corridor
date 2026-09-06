//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is C1.8's ship gate: a golden-file test
// for every stable error code -- exact status, exact code string --
// plus the two behavioral acceptance criteria the build spec names by
// name: a JSON number in an amount field is rejected, and replaying
// POST /entries with the same Idempotency-Key returns 200 with the
// original entry id and outcome=replayed, not 201.
package httpapi_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/db"
	"ledger/internal/halt"
	"ledger/internal/httpapi"
	"ledger/internal/journal"
	"ledger/internal/money"
	"ledger/internal/orders"
	"ledger/internal/recon"
)

const (
	testToken = "test-token"
	testActor = "test-actor"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

func applyMigrations(t *testing.T, dbURL string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(sqlDB, migrationsDir(t)))
}

// testServer spins up the real router (httptest.NewServer, real HTTP over
// a loopback socket, not an in-process ResponseRecorder) against a real
// Postgres database, with the halt cache and reconciler wired up exactly
// as cmd/ledgerd wires them.
// testServer spins up a router with the default recon.Config -- an
// Interval far longer than any test runs, so the background ticker never
// actually fires and interferes -- against the shared ledger_test
// database. Pass opts to override fields (e.g. CorridorCeilings) for
// tests that need specific reconciler behavior.
//
// Not safe for a test that asserts an exact value for a globally shared
// account (position:corridor:*, an asset total, ...): ledger_test is
// shared across this whole suite, go test parallelizes across packages
// by default, and another package's test can write to that same account
// in the window between this test's own reads. Use testServerIsolated
// instead for those.
func testServer(t *testing.T, opts ...func(*recon.Config)) (baseURL string, pool *pgxpool.Pool) {
	t.Helper()
	return testServerAt(t, testDatabaseURL(t), opts...)
}

// testServerIsolated is testServer against a freshly created throwaway
// database instead of the shared one, for tests that need it.
func testServerIsolated(t *testing.T, opts ...func(*recon.Config)) (baseURL string, pool *pgxpool.Pool) {
	t.Helper()
	return testServerAt(t, freshIsolatedDatabaseURL(t), opts...)
}

// freshIsolatedDatabaseURL creates a throwaway database on the same
// cluster as LEDGER_TEST_DATABASE_URL and returns its connection URL,
// with cleanup registered.
func freshIsolatedDatabaseURL(t *testing.T) string {
	t.Helper()
	baseURL := testDatabaseURL(t)
	u, err := url.Parse(baseURL)
	require.NoError(t, err)
	adminURL := *u
	adminURL.Path = "/postgres"
	ctx := context.Background()

	adminPool, err := pgxpool.New(ctx, adminURL.String())
	require.NoError(t, err)
	dbName := fmt.Sprintf("httpapi_isolated_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+dbName)
	require.NoError(t, err)
	adminPool.Close()

	t.Cleanup(func() {
		cleanupPool, err := pgxpool.New(context.Background(), adminURL.String())
		if err != nil {
			return
		}
		defer cleanupPool.Close()
		_, _ = cleanupPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	})

	freshURL := *u
	freshURL.Path = "/" + dbName
	return freshURL.String()
}

func testServerAt(t *testing.T, dbURL string, opts ...func(*recon.Config)) (baseURL string, pool *pgxpool.Pool) {
	t.Helper()
	applyMigrations(t, dbURL)

	ctx := context.Background()
	rawPool, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	require.NoError(t, accounts.Seed(ctx, rawPool))
	_, err = rawPool.Exec(ctx, `
		UPDATE system_state
		SET halted = false, halt_reason = NULL, halt_detail = NULL, halted_at = NULL, halted_by = NULL
		WHERE id = 1
	`)
	require.NoError(t, err)

	haltCache, err := halt.NewCache(ctx, rawPool)
	require.NoError(t, err)
	orders.SetHaltCache(haltCache)

	reconCfg := recon.Config{Interval: time.Hour}
	for _, opt := range opts {
		opt(&reconCfg)
	}
	reconciler := recon.NewReconciler(rawPool, reconCfg)

	server := &httpapi.Server{
		Pool:       &db.Pool{Pool: rawPool},
		Auth:       httpapi.AuthConfig{Tokens: map[string]string{testToken: testActor}},
		ReconCfg:   reconCfg,
		Reconciler: reconciler,
		BuildInfo:  func() (string, string) { return "test", "test" },
	}
	router := httpapi.NewRouter(server)

	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)
	return ts.URL, rawPool
}

var seq int64

func uniqueSuffix(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&seq, 1)
	return fmt.Sprintf("%s:%d:%d", t.Name(), time.Now().UnixNano(), n)
}

func idemKey(t *testing.T) string {
	return "httpapi_test:" + uniqueSuffix(t)
}

// doRequest is the one place every test builds and sends an HTTP
// request, so auth and Content-Type are never forgotten.
func doRequest(t *testing.T, method, url string, idempotencyKey string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if raw, ok := body.([]byte); ok {
		reader = bytes.NewReader(raw)
	} else if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
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
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&e))
	return e
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(v))
}

func twoTRXAccounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	suffix := uniqueSuffix(t)
	a := "liability:customer:" + suffix + ":a"
	b := "liability:customer:" + suffix + ":b"
	_, err := accounts.Create(ctx, pool, a, accounts.Liability, money.TRX)
	require.NoError(t, err)
	_, err = accounts.Create(ctx, pool, b, accounts.Liability, money.TRX)
	require.NoError(t, err)
	return a, b
}

func simpleEntryBody(t *testing.T, acc1, acc2 string) map[string]any {
	return map[string]any{
		"entry_type":  "http_test",
		"occurred_at": time.Now().Format(time.RFC3339),
		"lines": []map[string]any{
			{"account_code": acc1, "asset": "TRX", "amount": "1.000000"},
			{"account_code": acc2, "asset": "TRX", "amount": "-1.000000"},
		},
	}
}

// ---------------------------------------------------------------------
// Golden-file error tests: one per stable code, exact status + exact code.
// ---------------------------------------------------------------------

func TestErrorUnauthorized(t *testing.T) {
	baseURL, _ := testServer(t)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/entries", bytes.NewReader(nil))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, "unauthorized", decodeError(t, resp).Error.Code)
}

func TestErrorMissingIdempotencyKey(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", "", map[string]any{})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

func TestErrorInvalidAmountJSONNumber(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, ctxPool)

	// Built as raw bytes: a Go map can't represent "amount as a JSON
	// number" via json.Marshal of a string field, so this is hand-written
	// JSON with the amount field as a bare number, exactly what the
	// build spec's acceptance criterion describes.
	raw := fmt.Sprintf(`{
		"entry_type": "http_test",
		"occurred_at": %q,
		"lines": [
			{"account_code": %q, "asset": "TRX", "amount": 1.0},
			{"account_code": %q, "asset": "TRX", "amount": -1.0}
		]
	}`, time.Now().Format(time.RFC3339), acc1, acc2)

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), []byte(raw))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_amount", decodeError(t, resp).Error.Code)
}

func TestErrorInvalidAmountBadDecimalString(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, ctxPool)

	body := simpleEntryBody(t, acc1, acc2)
	body["lines"].([]map[string]any)[0]["amount"] = "1.0000001" // too many decimal places for TRX
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), body)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_amount", decodeError(t, resp).Error.Code)
}

func TestErrorUnbalancedEntry(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, ctxPool)

	body := simpleEntryBody(t, acc1, acc2)
	body["lines"].([]map[string]any)[1]["amount"] = "-0.999999" // doesn't balance
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	require.Equal(t, "unbalanced_entry", decodeError(t, resp).Error.Code)
}

func TestErrorAssetMismatch(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, ctxPool) // both accounts are TRX

	body := simpleEntryBody(t, acc1, acc2)
	body["lines"].([]map[string]any)[0]["asset"] = "BNB" // account is TRX, line claims BNB
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	require.Equal(t, "asset_mismatch", decodeError(t, resp).Error.Code)
}

func TestErrorInvalidEntryTooFewLines(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	acc1, _ := twoTRXAccounts(t, ctx, ctxPool)

	body := map[string]any{
		"entry_type":  "http_test",
		"occurred_at": time.Now().Format(time.RFC3339),
		"lines": []map[string]any{
			{"account_code": acc1, "asset": "TRX", "amount": "1.000000"},
		},
	}
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	require.Equal(t, "invalid_entry", decodeError(t, resp).Error.Code)
}

func TestErrorIdempotencyConflict(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, ctxPool)
	key := idemKey(t)

	first := doRequest(t, http.MethodPost, baseURL+"/v1/entries", key, simpleEntryBody(t, acc1, acc2))
	require.Equal(t, http.StatusCreated, first.StatusCode)

	changed := simpleEntryBody(t, acc1, acc2)
	changed["lines"].([]map[string]any)[0]["amount"] = "2.000000"
	changed["lines"].([]map[string]any)[1]["amount"] = "-2.000000"
	second := doRequest(t, http.MethodPost, baseURL+"/v1/entries", key, changed)
	require.Equal(t, http.StatusConflict, second.StatusCode)
	require.Equal(t, "idempotency_conflict", decodeError(t, second).Error.Code)
}

func TestErrorAccountNotFound(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/accounts/does:not:exist/balance", "", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "account_not_found", decodeError(t, resp).Error.Code)
}

func TestErrorOrderNotFound(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/orders/does-not-exist", "", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "order_not_found", decodeError(t, resp).Error.Code)
}

func createOrderViaHTTP(t *testing.T, baseURL string) map[string]any {
	t.Helper()
	body := map[string]any{
		"external_id":       "httpapi-order-" + uniqueSuffix(t),
		"customer_id":       "acme",
		"tier":              "STANDARD",
		"amount_in":         "3000.000000",
		"amount_out":        "2990.700000",
		"fee_units":         "7.500000",
		"network_fee_units": "1.800000",
		"recipient_address": "T-recipient",
		"quoted_at":         time.Now().Format(time.RFC3339),
		"quote_expires_at":  time.Now().Add(90 * time.Second).Format(time.RFC3339),
	}
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/orders", idemKey(t), body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var order map[string]any
	decodeInto(t, resp, &order)
	return order
}

func TestErrorIllegalTransition(t *testing.T) {
	baseURL, _ := testServer(t)
	order := createOrderViaHTTP(t, baseURL) // state: quoted

	body := map[string]any{
		"to_state":         "settled", // quoted -> settled is not a legal pair
		"expected_version": order["version"],
		"reason":           "illegal attempt",
		"occurred_at":      time.Now().Format(time.RFC3339),
	}
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/orders/"+order["external_id"].(string)+"/transitions", idemKey(t), body)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, "illegal_transition", decodeError(t, resp).Error.Code)
}

func TestErrorVersionConflict(t *testing.T) {
	baseURL, _ := testServer(t)
	order := createOrderViaHTTP(t, baseURL)

	body := map[string]any{
		"to_state":         "expired",
		"expected_version": 999, // wrong on purpose
		"reason":           "wrong version",
		"occurred_at":      time.Now().Format(time.RFC3339),
	}
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/orders/"+order["external_id"].(string)+"/transitions", idemKey(t), body)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Equal(t, "version_conflict", decodeError(t, resp).Error.Code)
}

func TestErrorInvalidEntryTransitionRequiresOne(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	order := createOrderViaHTTP(t, baseURL)

	depositAcc := fmt.Sprintf("asset:bsc:deposit:%s", uniqueSuffix(t))
	_, err := accounts.Create(ctx, ctxPool, depositAcc, accounts.Asset, money.USDT_BEP20)
	require.NoError(t, err)

	body := map[string]any{
		"to_state":         "funded", // requires an entry
		"expected_version": order["version"],
		"reason":           "no entry given",
		"occurred_at":      time.Now().Format(time.RFC3339),
	}
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/orders/"+order["external_id"].(string)+"/transitions", idemKey(t), body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	require.Equal(t, "invalid_entry", decodeError(t, resp).Error.Code)
}

func TestErrorSystemHalted(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()

	// screened -> dispatching is HaltBlocked. Walk an order there via the
	// real HTTP API, then halt, then attempt the halt-blocked transition.
	order := createOrderViaHTTP(t, baseURL)
	custBEP := "liability:customer:" + uniqueSuffix(t) + ":USDT_BEP20"
	_, err := accounts.Create(ctx, ctxPool, custBEP, accounts.Liability, money.USDT_BEP20)
	require.NoError(t, err)
	depositAcc := "asset:bsc:deposit:" + uniqueSuffix(t)
	_, err = accounts.Create(ctx, ctxPool, depositAcc, accounts.Asset, money.USDT_BEP20)
	require.NoError(t, err)

	fundedBody := map[string]any{
		"to_state": "funded", "expected_version": order["version"],
		"reason": "deposit", "occurred_at": time.Now().Format(time.RFC3339),
		"entry": map[string]any{
			"entry_type": "deposit_final", "occurred_at": time.Now().Format(time.RFC3339),
			"lines": []map[string]any{
				{"account_code": depositAcc, "asset": "USDT_BEP20", "amount": order["amount_in"]},
				{"account_code": custBEP, "asset": "USDT_BEP20", "amount": "-" + order["amount_in"].(string)},
			},
		},
	}
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/orders/"+order["external_id"].(string)+"/transitions", idemKey(t), fundedBody)
	require.Equal(t, http.StatusOK, resp.StatusCode, "funding must succeed")
	var funded map[string]any
	decodeInto(t, resp, &funded)

	screenedBody := map[string]any{
		"to_state": "screened", "expected_version": funded["version"],
		"reason": "pass", "occurred_at": time.Now().Format(time.RFC3339),
	}
	resp = doRequest(t, http.MethodPost, baseURL+"/v1/orders/"+order["external_id"].(string)+"/transitions", idemKey(t), screenedBody)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var screened map[string]any
	decodeInto(t, resp, &screened)

	require.NoError(t, halt.Set(ctx, ctxPool, halt.SetParams{Reason: "TEST_HALT", Actor: "test"}))
	t.Cleanup(func() { _, _ = ctxPool.Exec(ctx, `UPDATE system_state SET halted=false WHERE id=1`) })

	custTRC := "liability:customer:" + uniqueSuffix(t) + ":USDT_TRC20"
	_, err = accounts.Create(ctx, ctxPool, custTRC, accounts.Liability, money.USDT_TRC20)
	require.NoError(t, err)

	dispatchBody := map[string]any{
		"to_state": "dispatching", "expected_version": screened["version"],
		"reason": "should be blocked", "occurred_at": time.Now().Format(time.RFC3339),
		"entry": map[string]any{
			"entry_type": "conversion", "occurred_at": time.Now().Format(time.RFC3339),
			"lines": []map[string]any{
				{"account_code": custBEP, "asset": "USDT_BEP20", "amount": order["amount_in"]},
				{"account_code": "position:corridor:USDT_BEP20", "asset": "USDT_BEP20", "amount": "-" + order["amount_in"].(string)},
				{"account_code": "position:corridor:USDT_TRC20", "asset": "USDT_TRC20", "amount": order["amount_in"]},
				{"account_code": custTRC, "asset": "USDT_TRC20", "amount": "-" + order["amount_out"].(string)},
				{"account_code": "revenue:fee", "asset": "USDT_TRC20", "amount": "-" + order["fee_units"].(string)},
				{"account_code": "revenue:network_fee", "asset": "USDT_TRC20", "amount": "-" + order["network_fee_units"].(string)},
			},
		},
	}
	resp = doRequest(t, http.MethodPost, baseURL+"/v1/orders/"+order["external_id"].(string)+"/transitions", idemKey(t), dispatchBody)
	require.Equal(t, http.StatusLocked, resp.StatusCode)
	require.Equal(t, "system_halted", decodeError(t, resp).Error.Code)
}

// ---------------------------------------------------------------------
// Behavioral acceptance criteria, named explicitly in the build spec.
// ---------------------------------------------------------------------

func TestReplayReturns200WithSameEntryIDAndReplayedOutcome(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, ctxPool)
	key := idemKey(t)
	body := simpleEntryBody(t, acc1, acc2)

	first := doRequest(t, http.MethodPost, baseURL+"/v1/entries", key, body)
	require.Equal(t, http.StatusCreated, first.StatusCode)
	var firstEntry map[string]any
	decodeInto(t, first, &firstEntry)
	require.Equal(t, "created", firstEntry["outcome"])

	second := doRequest(t, http.MethodPost, baseURL+"/v1/entries", key, body)
	require.Equal(t, http.StatusOK, second.StatusCode, "replay must be 200, not 201")
	var secondEntry map[string]any
	decodeInto(t, second, &secondEntry)
	require.Equal(t, "replayed", secondEntry["outcome"])
	require.Equal(t, firstEntry["id"], secondEntry["id"], "replay must return the same entry id")
}

// TestMetricsExposesRequiredNames checks every metric the build spec
// names is present after real activity has touched each one. It
// deliberately triggers both a success and a failure first:
// entries_total and entry_errors_total are CounterVecs/Counters that
// Prometheus's client library only emits output lines for once
// something has actually incremented them (a vector metric with zero
// observations produces zero lines, by design, not a bug) -- so scraping
// immediately after server startup with no traffic would not actually
// prove these metrics work, only that they don't crash the endpoint.
func TestMetricsExposesRequiredNames(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, ctxPool)

	okResp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), simpleEntryBody(t, acc1, acc2))
	require.Equal(t, http.StatusCreated, okResp.StatusCode)

	failBody := simpleEntryBody(t, acc1, acc2)
	failBody["lines"].([]map[string]any)[1]["amount"] = "-0.999999" // unbalanced, on purpose
	failResp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), failBody)
	require.Equal(t, http.StatusUnprocessableEntity, failResp.StatusCode)

	resp := doRequest(t, http.MethodGet, baseURL+"/metrics", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	body := buf.String()

	for _, name := range []string{
		"ledger_entries_total",
		"ledger_entry_errors_total",
		"ledger_halt_state",
		"ledger_orders_by_state",
		"ledger_recon_drift_units",
		"ledger_recon_lag_seconds",
	} {
		require.Containsf(t, body, name, "metric %s must be exposed", name)
	}
	require.Contains(t, body, `ledger_entry_errors_total{code="unbalanced_entry"}`, "the failed request must be attributed to its actual error code")
}

func TestHealthzReadyzUnauthenticated(t *testing.T) {
	baseURL, _ := testServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equalf(t, http.StatusOK, resp.StatusCode, "%s must not require auth", path)
	}
}

func TestPostHaltSetAndClear(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()

	setResp := doRequest(t, http.MethodPost, baseURL+"/v1/system/halt", idemKey(t), map[string]any{
		"action": "set", "reason": "MANUAL_TEST_HALT",
	})
	require.Equal(t, http.StatusOK, setResp.StatusCode)
	var setState map[string]any
	decodeInto(t, setResp, &setState)
	require.Equal(t, true, setState["halted"])
	require.Equal(t, "MANUAL_TEST_HALT", setState["reason"])

	clearResp := doRequest(t, http.MethodPost, baseURL+"/v1/system/halt", idemKey(t), map[string]any{
		"action": "clear", "note": "confirmed safe",
	})
	require.Equal(t, http.StatusOK, clearResp.StatusCode)
	var clearState map[string]any
	decodeInto(t, clearResp, &clearState)
	require.Equal(t, false, clearState["halted"])

	halted, err := halt.IsHalted(ctx, ctxPool)
	require.NoError(t, err)
	require.False(t, halted)
}

func TestPostHaltClearWithoutNoteRejected(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	require.NoError(t, halt.Set(ctx, ctxPool, halt.SetParams{Reason: "TEST", Actor: "test"}))
	t.Cleanup(func() { _, _ = ctxPool.Exec(ctx, `UPDATE system_state SET halted=false WHERE id=1`) })

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/system/halt", idemKey(t), map[string]any{
		"action": "clear", "note": "",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

func TestGetInvariantsAndTrialBalance(t *testing.T) {
	baseURL, _ := testServer(t)

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/system/invariants", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var invariants map[string]any
	decodeInto(t, resp, &invariants)
	require.Contains(t, invariants, "trial_balance")
	require.Contains(t, invariants, "corridor_position")
	require.Contains(t, invariants, "recon_lag_seconds")
	// testServer configures no CorridorCeilings and never ticks the
	// reconciler, so both are present but empty/zero -- covered
	// separately by TestInvariantsCorridorCeiling below.
	assert.Empty(t, invariants["corridor_position"])
	assert.Zero(t, invariants["recon_lag_seconds"])

	resp2 := doRequest(t, http.MethodGet, baseURL+"/v1/trial-balance", "", nil)
	require.Equal(t, http.StatusOK, resp2.StatusCode)
}

// TestInvariantsCorridorCeiling exercises the field C1.10 adds to
// GET /v1/system/invariants: position:corridor reported against its
// configured ceiling. Pushes the corridor position past a deliberately
// low ceiling and confirms the endpoint reports it as over -- this is
// meant to become the public status page's data source (S3, later), so
// the shape and correctness of this specific field matters beyond just
// "the endpoint returns 200."
func TestInvariantsCorridorCeiling(t *testing.T) {
	ceiling := int64(500_000000) // 500.000000 USDT_BEP20
	// Isolated: this test asserts an exact balance for
	// position:corridor:USDT_BEP20, a globally shared account. Against
	// the shared ledger_test database, another package's concurrently
	// running test can write to that same account between this test's
	// own "before" and "after" reads -- see testServerIsolated's doc
	// comment. That's exactly what happened the first time this ran as
	// part of the full suite rather than in isolation: off by exactly
	// one other, concurrently running test's 1.000000 USDT_BEP20 write
	// landing in that window.
	baseURL, pool := testServerIsolated(t, func(c *recon.Config) {
		c.CorridorCeilings = map[money.Asset]int64{money.USDT_BEP20: ceiling}
	})

	ctx := context.Background()
	before, err := journal.Balance(ctx, pool, "position:corridor:USDT_BEP20")
	require.NoError(t, err)

	liability := "liability:customer:" + uniqueSuffix(t)
	_, err = accounts.Create(ctx, pool, liability, accounts.Liability, money.USDT_BEP20)
	require.NoError(t, err)

	// ledger_test is a shared database across this whole suite, so
	// position:corridor:USDT_BEP20 may already carry balance left by
	// other tests. Push further in whatever direction it's already
	// leaning, by an amount large enough that the final magnitude clears
	// ceiling regardless of where it started.
	delta := int64(50_000_000000) // 50,000.000000 USDT_BEP20
	if before.Units < 0 {
		delta = -delta
	}
	deltaStr, err := money.Format(money.Amount{Asset: money.USDT_BEP20, Units: delta})
	require.NoError(t, err)
	negDeltaStr, err := money.Format(money.Amount{Asset: money.USDT_BEP20, Units: -delta})
	require.NoError(t, err)

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), map[string]any{
		"entry_type":  "http_test_corridor",
		"occurred_at": time.Now().Format(time.RFC3339),
		"lines": []map[string]any{
			{"account_code": "position:corridor:USDT_BEP20", "asset": "USDT_BEP20", "amount": deltaStr},
			{"account_code": liability, "asset": "USDT_BEP20", "amount": negDeltaStr},
		},
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	want := before.Units + delta

	resp2 := doRequest(t, http.MethodGet, baseURL+"/v1/system/invariants", "", nil)
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	var invariants struct {
		CorridorPosition []struct {
			Asset         string `json:"asset"`
			AccountCode   string `json:"account_code"`
			PositionUnits int64  `json:"position_units"`
			CeilingUnits  int64  `json:"ceiling_units"`
			OverCeiling   bool   `json:"over_ceiling"`
		} `json:"corridor_position"`
	}
	decodeInto(t, resp2, &invariants)

	require.Len(t, invariants.CorridorPosition, 1)
	got := invariants.CorridorPosition[0]
	assert.Equal(t, "USDT_BEP20", got.Asset)
	assert.Equal(t, "position:corridor:USDT_BEP20", got.AccountCode)
	assert.Equal(t, want, got.PositionUnits)
	assert.Equal(t, ceiling, got.CeilingUnits)
	assert.True(t, got.OverCeiling)
}

// TestGetAccountBalancePercentEncodedCode is a regression test for a real
// bug: chi's router matches against the request's raw, still-percent-
// encoded path whenever the request line contains any escaping at all
// (it prefers r.URL.RawPath over the already-decoded r.URL.Path), so
// chi.URLParam returns the RAW segment, not the decoded one. Every
// account code in this system uses colons as a separator, and colon is a
// character url.PathEscape and JavaScript's encodeURIComponent both
// percent-encode by default -- so any client that encodes its path
// segments properly (this system's own future callers very possibly
// included) got a 404 for an account that plainly exists. Found by
// actually driving a real browser-based client against this exact
// endpoint, not by unit tests alone, since every existing test built its
// URL by direct string concatenation and never exercised an encoded path.
func TestGetAccountBalancePercentEncodedCode(t *testing.T) {
	baseURL, _ := testServer(t)

	const code = "position:corridor:USDT_BEP20" // seeded, always exists
	// url.PathEscape deliberately leaves colons alone -- RFC 3986 allows
	// them unencoded in a path segment -- so it can't reproduce this bug.
	// JavaScript's encodeURIComponent (what the console.html client and
	// this system's real future callers might well use) encodes them
	// anyway, since it's a general-purpose component encoder, not a
	// path-segment-aware one. Replicate that exactly, rather than reaching
	// for a stdlib encoder whose specific choices might not match.
	encoded := strings.ReplaceAll(code, ":", "%3A")

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/accounts/"+encoded+"/balance", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "a percent-encoded account code must resolve exactly like the unencoded one")
	var result map[string]any
	decodeInto(t, resp, &result)
	assert.Equal(t, code, result["account_code"])
}

func TestGetBalancesPrefix(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/balances?prefix=asset:tron:", "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result map[string]any
	decodeInto(t, resp, &result)
	require.Contains(t, result, "balances")
}

func TestPostReconciliationSnapshot(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	acc1, acc2 := twoTRXAccounts(t, ctx, ctxPool)

	entryResp := doRequest(t, http.MethodPost, baseURL+"/v1/entries", idemKey(t), simpleEntryBody(t, acc1, acc2))
	require.Equal(t, http.StatusCreated, entryResp.StatusCode)
	var entry map[string]any
	decodeInto(t, entryResp, &entry)

	body := map[string]any{
		"account_code":    acc1,
		"observed_amount": "1.000000", // matches -- no drift
		"chain_ref":       "test-block",
		"watermark":       entry["id"],
		"observed_at":     time.Now().Format(time.RFC3339),
	}
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/reconciliation/snapshots", idemKey(t), body)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var result map[string]any
	decodeInto(t, resp, &result)
	require.Equal(t, false, result["halted"])
}

// ---------------------------------------------------------------------
// sender_address and GET /v1/orders?state= (added for C3/screening --
// see docs/03-build/c3-screening-build-prompts.md's "Read this first").
// ---------------------------------------------------------------------

// fundOrderViaHTTP transitions order to funded over the real HTTP API,
// with the real deposit_final entry shape C2's ledgerclient actually
// sends (see depositwatcher/internal/ledgerclient/ledgerclient.go), plus
// sender_address if non-empty.
func fundOrderViaHTTP(t *testing.T, baseURL string, ctx context.Context, pool *pgxpool.Pool, order map[string]any, senderAddress string) map[string]any {
	t.Helper()
	custBEP := "liability:customer:" + uniqueSuffix(t) + ":USDT_BEP20"
	_, err := accounts.Create(ctx, pool, custBEP, accounts.Liability, money.USDT_BEP20)
	require.NoError(t, err)
	depositAcc := "asset:bsc:deposit:" + uniqueSuffix(t)
	_, err = accounts.Create(ctx, pool, depositAcc, accounts.Asset, money.USDT_BEP20)
	require.NoError(t, err)

	body := map[string]any{
		"to_state": "funded", "expected_version": order["version"],
		"reason": "deposit", "occurred_at": time.Now().Format(time.RFC3339),
		"entry": map[string]any{
			"entry_type": "deposit_final", "occurred_at": time.Now().Format(time.RFC3339),
			"lines": []map[string]any{
				{"account_code": depositAcc, "asset": "USDT_BEP20", "amount": order["amount_in"]},
				{"account_code": custBEP, "asset": "USDT_BEP20", "amount": "-" + order["amount_in"].(string)},
			},
		},
	}
	if senderAddress != "" {
		body["sender_address"] = senderAddress
	}

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/orders/"+order["external_id"].(string)+"/transitions", idemKey(t), body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var updated map[string]any
	decodeInto(t, resp, &updated)
	return updated
}

func TestSenderAddressRoundTripsThroughFundingTransition(t *testing.T) {
	baseURL, ctxPool := testServer(t)
	ctx := context.Background()
	order := createOrderViaHTTP(t, baseURL)
	require.Nil(t, order["sender_address"], "a freshly quoted order must have a null sender_address")

	const sender = "0xSenderAddressHTTP00000000000000000001"
	funded := fundOrderViaHTTP(t, baseURL, ctx, ctxPool, order, sender)
	require.Equal(t, sender, funded["sender_address"])

	// A fresh GET sees it too -- not just the transition response.
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/orders/"+order["external_id"].(string), "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var reloaded map[string]any
	decodeInto(t, resp, &reloaded)
	require.Equal(t, sender, reloaded["sender_address"])
}

func TestSenderAddressRejectedOnNonFundedTransition(t *testing.T) {
	baseURL, _ := testServer(t)
	order := createOrderViaHTTP(t, baseURL)

	body := map[string]any{
		"to_state": "expired", "expected_version": order["version"],
		"reason": "not a funding transition", "occurred_at": time.Now().Format(time.RFC3339),
		"sender_address": "0xShouldNotBeAccepted00000000000000001",
	}
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/orders/"+order["external_id"].(string)+"/transitions", idemKey(t), body)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

func TestGetOrders_RequiresState(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/orders", "", nil)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

func TestGetOrders_UnknownStateRejected(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/orders?state=bogus", "", nil)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

func TestGetOrders_InvalidCursorRejected(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodGet, baseURL+"/v1/orders?state=funded&updated_after=not-a-real-cursor", "", nil)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, "invalid_request", decodeError(t, resp).Error.Code)
}

func TestGetOrders_FiltersByStateAndPaginatesWithCursor(t *testing.T) {
	// Isolated database: this test asserts an EXACT set of orders for a
	// state, which the shared ledger_test database (used by every other
	// test in this file, running concurrently across packages) cannot
	// guarantee.
	baseURL, ctxPool := testServerIsolated(t)
	ctx := context.Background()

	var fundedExternalIDs []string
	for i := 0; i < 3; i++ {
		order := createOrderViaHTTP(t, baseURL)
		funded := fundOrderViaHTTP(t, baseURL, ctx, ctxPool, order, "")
		fundedExternalIDs = append(fundedExternalIDs, funded["external_id"].(string))
		time.Sleep(2 * time.Millisecond) // force distinct updated_at for a stable page order
	}
	quotedOrder := createOrderViaHTTP(t, baseURL) // must never appear in state=funded

	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 20; pages++ {
		url := baseURL + "/v1/orders?state=funded&limit=2"
		if cursor != "" {
			url += "&updated_after=" + cursor
		}
		resp := doRequest(t, http.MethodGet, url, "", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var page struct {
			Orders     []map[string]any `json:"orders"`
			NextCursor string           `json:"next_cursor"`
		}
		decodeInto(t, resp, &page)

		if len(page.Orders) == 0 {
			require.Equal(t, cursor, page.NextCursor, "an empty page must echo the caller's own cursor back")
			break
		}
		for _, o := range page.Orders {
			extID := o["external_id"].(string)
			require.False(t, seen[extID], "order %s returned twice across pages", extID)
			seen[extID] = true
			require.NotEqual(t, quotedOrder["external_id"], extID, "a quoted order must never appear in a state=funded listing")
		}
		require.NotEmpty(t, page.NextCursor)
		cursor = page.NextCursor
	}

	for _, extID := range fundedExternalIDs {
		require.True(t, seen[extID], "funded order %s was never returned by any page", extID)
	}
}
