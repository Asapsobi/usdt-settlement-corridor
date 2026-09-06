// Package testledger is shared test infrastructure: it builds and runs
// a REAL ledgerd binary as a subprocess and drives it purely over HTTP,
// for any integration test in this module that needs a genuine C1
// rather than a fake standing in for one (per this project's own
// established discipline -- see depositwatcher's identical, deliberately
// duplicated harness in its own internal/ledgerclient). Shared here,
// rather than duplicated a second time, because both users
// (internal/ledgerclient and internal/pipeline) live in this SAME
// module, unlike the depositwatcher/ledger split across two modules.
//
// Not a _test.go file: Go doesn't let one package's _test.go helpers be
// imported by another package's tests, so this has to be an ordinary
// package, imported only from _test.go files in practice.
package testledger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Ledger is a real ledgerd process, built and started fresh for one
// test, plus a raw SQL connection to the same database used ONLY for
// account-creation fixture setup (see CreateAccount).
type Ledger struct {
	t       *testing.T
	baseURL string
	token   string
	pool    *pgxpool.Pool
}

// Start builds ledger/cmd/migrate and ledger/cmd/ledgerd from the
// sibling ledger module (../../../ledger, the usdt-settlement-corridor
// layout this whole project uses), runs migrations against
// LEDGER_TEST_DATABASE_URL, and starts ledgerd listening on listenAddr
// (e.g. ":18436"), authenticating token to actor. Skips the calling test
// if LEDGER_TEST_DATABASE_URL is unset or no sibling ledger checkout is
// found.
func Start(t *testing.T, listenAddr, token, actor string) *Ledger {
	t.Helper()
	dbURL := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}

	ledgerRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "ledger"))
	if err != nil {
		t.Fatalf("resolving ledger module path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ledgerRoot, "go.mod")); err != nil {
		t.Skipf("no sibling ledger module found at %s; skipping (expects the usdt-settlement-corridor layout)", ledgerRoot)
	}

	tmpDir := t.TempDir()
	migrateBin := filepath.Join(tmpDir, "migrate_bin")
	ledgerdBin := filepath.Join(tmpDir, "ledgerd_bin")

	build := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = ledgerRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", pkg, err, output)
		}
	}
	build(migrateBin, "./cmd/migrate")
	build(ledgerdBin, "./cmd/ledgerd")

	migrate := exec.Command(migrateBin, "up")
	migrate.Dir = ledgerRoot // goose.Up uses a relative "migrations" path, resolved against the process's cwd
	migrate.Env = append(os.Environ(), "LEDGER_DATABASE_URL="+dbURL)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("running ledger migrations: %v\n%s", err, output)
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connecting to ledger test database: %v", err)
	}
	t.Cleanup(pool.Close)

	ledgerd := exec.Command(ledgerdBin)
	ledgerd.Dir = ledgerRoot
	ledgerd.Env = append(os.Environ(),
		"LEDGER_DATABASE_URL="+dbURL,
		"LEDGER_API_TOKENS="+token+":"+actor,
		"LEDGER_LISTEN_ADDR="+listenAddr,
	)
	var logs bytes.Buffer
	ledgerd.Stdout = &logs
	ledgerd.Stderr = &logs
	if err := ledgerd.Start(); err != nil {
		t.Fatalf("starting ledgerd: %v", err)
	}
	t.Cleanup(func() {
		_ = ledgerd.Process.Kill()
		_ = ledgerd.Wait()
		if t.Failed() {
			t.Logf("ledgerd output:\n%s", logs.String())
		}
	})

	baseURL := "http://localhost" + listenAddr
	l := &Ledger{t: t, baseURL: baseURL, token: token, pool: pool}
	l.waitHealthy()
	return l
}

// BaseURL is this ledgerd instance's own base URL -- e.g. to pass to
// ledgerclient.New.
func (l *Ledger) BaseURL() string { return l.baseURL }

// Token is the bearer token this ledgerd instance was started with.
func (l *Ledger) Token() string { return l.token }

func (l *Ledger) waitHealthy() {
	l.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(l.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	l.t.Fatalf("ledgerd never became healthy at %s within the deadline", l.baseURL)
}

// CreateAccount mirrors accounts.Create's own INSERT exactly -- there is
// no public HTTP endpoint for creating an account, so this is test
// scaffolding only, never a claim that C3 itself can or should write to
// C1's database directly.
func (l *Ledger) CreateAccount(code, accountType, asset string, normalSide int) {
	l.t.Helper()
	_, err := l.pool.Exec(context.Background(), `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	if err != nil {
		l.t.Fatalf("creating fixture account %q: %v", code, err)
	}
}

// OrderResp is the subset of C1's order resource these test helpers
// decode.
type OrderResp struct {
	ID            int64   `json:"id"`
	ExternalID    string  `json:"external_id"`
	State         string  `json:"state"`
	Version       int32   `json:"version"`
	SenderAddress *string `json:"sender_address"`
}

// Do sends one authenticated request to this ledgerd instance and
// returns its raw response and body.
func (l *Ledger) Do(method, path string, idempotencyKey string, body any) (*http.Response, []byte) {
	l.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			l.t.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, l.baseURL+path, reader)
	if err != nil {
		l.t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+l.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		l.t.Fatalf("%s %s: reading response body: %v", method, path, err)
	}
	return resp, respBody
}

// CreateOrder posts POST /v1/orders with a fixed, valid set of amounts
// (3000.000000 USDT_BEP20 in, matching FundOrder's own hardcoded lines).
func (l *Ledger) CreateOrder(externalID, customerID string) OrderResp {
	l.t.Helper()
	now := time.Now().UTC()
	resp, body := l.Do(http.MethodPost, "/v1/orders", "create:"+externalID, map[string]any{
		"external_id":       externalID,
		"customer_id":       customerID,
		"tier":              "STANDARD",
		"amount_in":         "3000.000000",
		"amount_out":        "2990.700000",
		"fee_units":         "7.500000",
		"network_fee_units": "1.800000",
		"recipient_address": "T-recipient-" + externalID,
		"quoted_at":         now,
		"quote_expires_at":  now.Add(10 * time.Minute),
	})
	if resp.StatusCode != http.StatusCreated {
		l.t.Fatalf("POST /v1/orders for %s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o OrderResp
	if err := json.Unmarshal(body, &o); err != nil {
		l.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

// FundOrder transitions order to funded exactly the way C2's own
// ledgerclient.ReportDepositFinal does -- the real deposit_final entry
// shape, plus sender_address sibling to entry.
func (l *Ledger) FundOrder(order OrderResp, customerID, senderAddress string) OrderResp {
	l.t.Helper()
	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custBEP := "liability:customer:" + customerID
	l.CreateAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	l.CreateAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	resp, body := l.Do(http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", order.ExternalID),
		"fund:"+order.ExternalID, map[string]any{
			"to_state":         "funded",
			"expected_version": order.Version,
			"reason":           "bep20_deposit_final",
			"occurred_at":      time.Now().UTC(),
			"sender_address":   senderAddress,
			"entry": map[string]any{
				"entry_type":  "deposit_final",
				"occurred_at": time.Now().UTC(),
				"lines": []map[string]any{
					{"account_code": depositAcc, "asset": "USDT_BEP20", "amount": "3000.000000"},
					{"account_code": custBEP, "asset": "USDT_BEP20", "amount": "-3000.000000"},
				},
			},
		})
	if resp.StatusCode != http.StatusOK {
		l.t.Fatalf("POST /v1/orders/%s/transitions to funded: status %d: %s", order.ExternalID, resp.StatusCode, body)
	}
	var o OrderResp
	if err := json.Unmarshal(body, &o); err != nil {
		l.t.Fatalf("decoding transition response: %v: %s", err, body)
	}
	return o
}

// GetOrder fetches GET /v1/orders/{externalID}.
func (l *Ledger) GetOrder(externalID string) OrderResp {
	l.t.Helper()
	resp, body := l.Do(http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if resp.StatusCode != http.StatusOK {
		l.t.Fatalf("GET /v1/orders/%s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o OrderResp
	if err := json.Unmarshal(body, &o); err != nil {
		l.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

// SetHalt sets or clears C1's system halt directly against its
// database -- there is no way to do this over HTTP for this test's
// purposes without also authoring a real halt-worthy loss event, and
// the halt/clear HTTP endpoints require their own request shapes this
// package doesn't otherwise need. Mirrors system_state's own columns
// exactly (see ledger/migrations for the authoritative shape).
func (l *Ledger) SetHalt(halted bool, reason string) {
	l.t.Helper()
	var err error
	if halted {
		_, err = l.pool.Exec(context.Background(), `
			UPDATE system_state SET halted = true, halt_reason = $1, halted_at = now(), halted_by = 'test'
			WHERE id = 1
		`, reason)
	} else {
		_, err = l.pool.Exec(context.Background(), `
			UPDATE system_state SET halted = false, halt_reason = NULL, halt_detail = NULL, halted_at = NULL, halted_by = NULL
			WHERE id = 1
		`)
	}
	if err != nil {
		l.t.Fatalf("setting halt=%v: %v", halted, err)
	}
}
