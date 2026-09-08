// Package testledger is shared test infrastructure: it builds and runs a
// REAL ledgerd binary as a subprocess and drives it purely over HTTP, for
// any integration test in this module that needs a genuine C1 rather
// than a fake standing in for one -- the same discipline energybroker's
// and screening's own internal/testledger already established (mirrored,
// not shared: separate modules, no common internal package between them).
//
// Not a _test.go file: Go doesn't let one package's _test.go helpers be
// imported by another package's tests, so this has to be an ordinary
// package, imported only from _test.go files in practice.
package testledger

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Ledger is a real ledgerd process, built and started fresh for one test,
// plus a raw SQL connection to the same database used ONLY for
// account-creation fixture setup and direct assertions.
type Ledger struct {
	t       *testing.T
	baseURL string
	token   string
	pool    *pgxpool.Pool
}

// Start builds ledger/cmd/migrate and ledger/cmd/ledgerd from the sibling
// ledger module (../../../ledger, the usdt-settlement-corridor layout
// this whole project uses), runs migrations against dbURL, and starts
// ledgerd listening on listenAddr (e.g. ":18538"), authenticating token
// to actor. Skips the calling test if dbURL is unset or no sibling ledger
// checkout is found.
func Start(t *testing.T, dbURL, listenAddr, token, actor string) *Ledger {
	t.Helper()
	if dbURL == "" {
		t.Skip("no test database URL set; skipping integration test")
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

// Do sends one authenticated request to this ledgerd instance and returns
// its raw response and body.
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

// OrderResp is the subset of C1's order resource this test helper decodes.
type OrderResp struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
	CustomerID string `json:"customer_id"`
	State      string `json:"state"`
	Version    int32  `json:"version"`
}

// CreateOrder posts POST /v1/orders with a fixed, valid set of amounts
// matching c1-ledger-build-prompts.md §B's own worked example exactly
// ($3,000 in, $2,990.70 out, $7.50 fee, $1.80 network fee) -- so a
// dispatch test's own E2 balance assertions can check against known
// numbers, not re-derive them. recipient_address is a real, valid,
// checksummed TRON address (not a fabricated "T-recipient-..." string):
// txbuild.BuildTransfer/BuildMultisend validate the recipient address's
// own base58check checksum before constructing anything, so a fixture
// address that fails that check breaks any test exercising those paths.
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
		"recipient_address": "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj",
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

// AdvanceToScreened walks a freshly created order (in quoted) through
// funded and into screened -- the state EnterDispatching's own real
// acceptance criterion requires a test order to start from. Creates the
// deposit and BEP20 liability accounts E1 needs directly against the
// database (mirroring what C2 would do over HTTP in a real deployment;
// see cmd/seed-console's own doc comment for why direct account creation
// is the established test-fixture pattern here, not a shortcut specific
// to this helper).
func (l *Ledger) AdvanceToScreened(order OrderResp) OrderResp {
	l.t.Helper()
	depositAccount := "asset:bsc:deposit:" + order.ExternalID
	bepLiability := "liability:customer:" + order.CustomerID + ":USDT_BEP20"
	l.CreateAccount(depositAccount, "ASSET", "USDT_BEP20")
	l.CreateAccount(bepLiability, "LIABILITY", "USDT_BEP20")

	now := time.Now().UTC()
	resp, body := l.Do(http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "fund:"+order.ExternalID, map[string]any{
		"to_state":         "funded",
		"expected_version": order.Version,
		"reason":           "deposit_final",
		"occurred_at":      now,
		"entry": map[string]any{
			"entry_type":  "deposit_final",
			"occurred_at": now,
			"lines": []map[string]any{
				{"account_code": depositAccount, "asset": "USDT_BEP20", "amount": "3000.000000"},
				{"account_code": bepLiability, "asset": "USDT_BEP20", "amount": "-3000.000000"},
			},
		},
	})
	if resp.StatusCode != http.StatusOK {
		l.t.Fatalf("funding %s: status %d: %s", order.ExternalID, resp.StatusCode, body)
	}
	var funded OrderResp
	if err := json.Unmarshal(body, &funded); err != nil {
		l.t.Fatalf("decoding funded order response: %v: %s", err, body)
	}

	resp, body = l.Do(http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "screen:"+order.ExternalID, map[string]any{
		"to_state":         "screened",
		"expected_version": funded.Version,
		"reason":           "screening_pass",
		"occurred_at":      now,
	})
	if resp.StatusCode != http.StatusOK {
		l.t.Fatalf("screening %s: status %d: %s", order.ExternalID, resp.StatusCode, body)
	}
	var screened OrderResp
	if err := json.Unmarshal(body, &screened); err != nil {
		l.t.Fatalf("decoding screened order response: %v: %s", err, body)
	}
	return screened
}

// CreateAccount mirrors accounts.Create's own INSERT exactly -- there is
// no public HTTP endpoint for creating an account with type+asset in one
// call before this test needs one to exist (POST /v1/accounts, added
// alongside this dispatch chunk, takes exactly this shape; this helper
// predates needing to depend on dispatcher's own ledgerclient from a
// package that must stay import-free of it, so it goes straight at the
// database, same as every prior component's own testledger helper).
func (l *Ledger) CreateAccount(code, accountType, asset string) {
	l.t.Helper()
	normalSide := 1
	switch accountType {
	case "LIABILITY", "REVENUE", "EQUITY":
		normalSide = -1
	case "POSITION":
		normalSide = 0
	}
	_, err := l.pool.Exec(context.Background(), `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	if err != nil {
		l.t.Fatalf("creating fixture account %q: %v", code, err)
	}
}

// AccountBalance sums journal_lines.amount_units for the account named
// code -- a direct query, simpler and exactly as trustworthy as decoding
// GET /v1/balances just to check one account moved by the expected
// amount (this IS the ledger's own database).
func (l *Ledger) AccountBalance(code string) int64 {
	l.t.Helper()
	var balance int64
	err := l.pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(jl.amount_units), 0)
		FROM journal_lines jl
		JOIN accounts a ON a.id = jl.account_id
		WHERE a.code = $1
	`, code).Scan(&balance)
	if err != nil {
		l.t.Fatalf("summing balance for account %q: %v", code, err)
	}
	return balance
}

// JournalEntryCount returns how many journal_entries rows exist with the
// given idempotency_key -- 0 or 1, ever; more than 1 would mean C1's own
// idempotency guarantee broke.
func (l *Ledger) JournalEntryCount(idempotencyKey string) int {
	l.t.Helper()
	var count int
	err := l.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM journal_entries WHERE idempotency_key = $1
	`, idempotencyKey).Scan(&count)
	if err != nil {
		l.t.Fatalf("counting journal_entries for key %q: %v", idempotencyKey, err)
	}
	return count
}

// SetHalted sets or clears C1's own halt flag directly against its
// database -- there is no public HTTP endpoint to halt the system for a
// test's own purposes (halt is meant to fire from C1's own reconciler,
// not be toggled by a caller).
func (l *Ledger) SetHalted(halted bool, reason string) {
	l.t.Helper()
	_, err := l.pool.Exec(context.Background(), `
		UPDATE system_state SET halted = $1, halt_reason = $2 WHERE id = 1
	`, halted, reason)
	if err != nil {
		l.t.Fatalf("setting halted=%v: %v", halted, err)
	}
}
