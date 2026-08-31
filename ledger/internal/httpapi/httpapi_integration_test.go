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
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/db"
	"ledger/internal/halt"
	"ledger/internal/httpapi"
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
func testServer(t *testing.T) (baseURL string, pool *pgxpool.Pool) {
	t.Helper()
	dbURL := testDatabaseURL(t)
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

	orders.SetHaltCache(halt.NewCache(rawPool))

	reconCfg := recon.Config{Interval: time.Hour}
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

	resp2 := doRequest(t, http.MethodGet, baseURL+"/v1/trial-balance", "", nil)
	require.Equal(t, http.StatusOK, resp2.StatusCode)
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
