//go:build integration

// Requires two real, reachable Postgres 16 instances (DISPATCHER_TEST_DATABASE_URL,
// LEDGER_TEST_DATABASE_URL) and a sibling ledger module checkout; run via
// `make test-integration`.
package httpapi_test

import (
	"bytes"
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

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"dispatcher/internal/db"
	"dispatcher/internal/dispatch"
	"dispatcher/internal/httpapi"
	"dispatcher/internal/ledgerclient"
	"dispatcher/internal/money"
	"dispatcher/internal/signing"
	"dispatcher/internal/slots"
	"dispatcher/internal/testledger"
)

const testToken = "test-token"

var portSeq int64 = 19538

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DISPATCHER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("DISPATCHER_TEST_DATABASE_URL not set; skipping integration test")
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

// testServer spins up the real router (httptest.NewServer) against real
// Postgres, a real ledgerd subprocess, and a fake SigningService --
// mirroring every prior component's own testServer shape.
func testServer(t *testing.T) (baseURL string, pool *db.Pool, ledger *testledger.Ledger, ledgerClient *ledgerclient.Client) {
	t.Helper()
	dbURL := testDatabaseURL(t)
	applyMigrations(t, dbURL)

	rawPool, err := db.Open(t.Context(), db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(rawPool.Close)
	if _, err := rawPool.Exec(t.Context(), `TRUNCATE dispatch_state, dispatch_attempts, batch_queue, batches, slots`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}

	ledgerDBURL := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if ledgerDBURL == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	port := atomic.AddInt64(&portSeq, 1)
	l := testledger.Start(t, ledgerDBURL, fmt.Sprintf(":%d", port), testToken, "dispatcher")
	lc := ledgerclient.New(l.BaseURL(), l.Token())

	slotsStore := slots.NewStore(rawPool)
	dispatcher := dispatch.NewDispatcher(lc, dispatch.NewStore(rawPool), dispatch.NewAttemptStore(rawPool), signing.NewFakeSigningService())
	dispatcher.Batches = dispatch.NewBatchStore(rawPool)

	server := &httpapi.Server{
		Pool: rawPool, Auth: httpapi.AuthConfig{Tokens: map[string]string{testToken: "test-actor"}},
		Dispatcher: dispatcher, Slots: slotsStore, Ledger: lc,
		SlotCaps:  slots.Caps{BalanceCeiling: mustAmount(t, "50000.000000"), TxCountCeiling: 5000},
		BuildInfo: func() (string, string) { return "test", "test" },
	}
	router := httpapi.NewRouter(server)

	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)
	return ts.URL, rawPool, l, lc
}

// createSlot registers a slot locally AND ensures its C1 ledger account
// exists -- real slot provisioning would do both; a GET /v1/slots or
// POST /v1/dispatch call reads asset:tron:slot:<id>'s real balance, which
// 404s against C1 if nothing ever created that account.
func createSlot(t *testing.T, slotsStore *slots.Store, lc *ledgerclient.Client, id int, address string) {
	t.Helper()
	if _, err := slotsStore.Create(t.Context(), id, address, time.Now().UTC()); err != nil {
		t.Fatalf("slots.Create(%d): %v", id, err)
	}
	code := fmt.Sprintf("asset:tron:slot:%d", id)
	if err := lc.EnsureAccount(t.Context(), code, ledgerclient.AccountAsset, "USDT_TRC20", "test:ensure:"+code); err != nil {
		t.Fatalf("EnsureAccount(%s): %v", code, err)
	}
}

func mustAmount(t *testing.T, s string) money.Amount {
	t.Helper()
	a, err := money.ParseDecimal(s)
	if err != nil {
		t.Fatalf("ParseDecimal(%q): %v", s, err)
	}
	return a
}

func doRequest(t *testing.T, method, url string, body any) *http.Response {
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
}

func TestHealthzReadyzUnauthenticated(t *testing.T) {
	baseURL, _, _, _ := testServer(t)

	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}

	resp, err = http.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want 200", resp.StatusCode)
	}
}

func TestPostDispatch_HappyPath(t *testing.T) {
	baseURL, pool, ledger, lc := testServer(t)

	slotsStore := slots.NewStore(pool)
	createSlot(t, slotsStore, lc, 1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")

	created := ledger.CreateOrder("order-http-1", "cust-http-1")
	screened := ledger.AdvanceToScreened(created)

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/dispatch", map[string]any{"external_id": screened.ExternalID})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/dispatch status = %d, want 202", resp.StatusCode)
	}
	var got struct {
		OrderID int64  `json:"order_id"`
		SlotID  int    `json:"slot_id"`
		Status  string `json:"status"`
	}
	decodeInto(t, resp, &got)
	if got.SlotID != 1 || got.Status != "DISPATCHING" {
		t.Fatalf("dispatch response = %+v, want SlotID=1 Status=DISPATCHING", got)
	}

	getResp := doRequest(t, http.MethodGet, fmt.Sprintf("%s/v1/dispatch/%d", baseURL, got.OrderID), nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/dispatch/{id} status = %d, want 200", getResp.StatusCode)
	}
}

func TestPostDispatch_NotScreenedRejected(t *testing.T) {
	baseURL, pool, ledger, lc := testServer(t)
	slotsStore := slots.NewStore(pool)
	createSlot(t, slotsStore, lc, 1, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")

	created := ledger.CreateOrder("order-http-notscreened", "cust-http-notscreened")

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/dispatch", map[string]any{"external_id": created.ExternalID})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /v1/dispatch status = %d, want 409 (order is still quoted, not screened)", resp.StatusCode)
	}
}

func TestGetSlotsAndRetire(t *testing.T) {
	baseURL, pool, _, lc := testServer(t)
	slotsStore := slots.NewStore(pool)
	createSlot(t, slotsStore, lc, 2, "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj")

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/slots", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/slots status = %d, want 200", resp.StatusCode)
	}
	var listBody struct {
		Slots []struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
		} `json:"slots"`
	}
	decodeInto(t, resp, &listBody)
	found := false
	for _, s := range listBody.Slots {
		if s.ID == 2 {
			found = true
			if s.Status != "ACTIVE" {
				t.Fatalf("slot 2 status = %q, want ACTIVE", s.Status)
			}
		}
	}
	if !found {
		t.Fatal("slot 2 not present in GET /v1/slots response")
	}

	retireResp := doRequest(t, http.MethodPost, baseURL+"/v1/slots/2/retire", map[string]any{"immediate": true})
	if retireResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/slots/2/retire status = %d, want 200", retireResp.StatusCode)
	}
	var retired struct {
		Status string `json:"status"`
	}
	decodeInto(t, retireResp, &retired)
	if retired.Status != "RETIRED" {
		t.Fatalf("retired slot status = %q, want RETIRED", retired.Status)
	}
}

func TestGetSystemInvariants(t *testing.T) {
	baseURL, pool, _, lc := testServer(t)
	slotsStore := slots.NewStore(pool)
	createSlot(t, slotsStore, lc, 3, "TLyqzVGLV1srkB7dToTAEqgDSfPtXRJZYH")

	resp := doRequest(t, http.MethodGet, baseURL+"/v1/system/invariants", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/system/invariants status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		SlotHeadroom []struct {
			SlotID int `json:"slot_id"`
		} `json:"slot_headroom"`
	}
	decodeInto(t, resp, &got)
	found := false
	for _, h := range got.SlotHeadroom {
		if h.SlotID == 3 {
			found = true
		}
	}
	if !found {
		t.Fatal("slot 3 not present in GET /v1/system/invariants slot_headroom")
	}
}

func TestErrorUnauthorized(t *testing.T) {
	baseURL, _, _, _ := testServer(t)
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/slots", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/slots (no auth): %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
