// Package testledger is shared test infrastructure: it builds and runs
// a REAL ledgerd binary as a subprocess and drives it purely over HTTP,
// for any integration test in this module that needs a genuine C1
// rather than a fake standing in for one -- the same discipline
// screening's own internal/testledger established (mirrored, not
// shared: separate modules, no common internal package between them).
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

// Ledger is a real ledgerd process, built and started fresh for one
// test, plus a raw SQL connection to the same database used ONLY for
// account-creation fixture setup and direct balance/journal assertions
// (see CreateAccount and JournalLineAmount).
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
// (e.g. ":18438"), authenticating token to actor. Skips the calling test
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

// OrderResp is the subset of C1's order resource this test helper decodes.
type OrderResp struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
	Version    int32  `json:"version"`
}

// CreateOrder posts POST /v1/orders with a fixed, valid set of amounts --
// C4.5's own tests just need a real, legitimate order id to attribute an
// energy_cost entry against, not any particular order content.
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

// CreateAccount mirrors accounts.Create's own INSERT exactly -- there is
// no public HTTP endpoint for creating an account, so this is test
// scaffolding only, never a claim that C4 itself can or should write to
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

// AccountBalance sums journal_lines.amount_units for the account named
// code -- there is no public HTTP endpoint scoped to a single account by
// code in a way this test needs, and GET /v1/balances' own shape isn't
// worth decoding here just to check one account moved by the exact
// expected amount; a direct query is simpler and exactly as trustworthy
// (this IS the ledger's own database).
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
// not be toggled by a caller), so this is test scaffolding only, mirror
// of what C1's own halt-related integration tests already do against
// their own database.
func (l *Ledger) SetHalted(halted bool, reason string) {
	l.t.Helper()
	_, err := l.pool.Exec(context.Background(), `
		UPDATE system_state SET halted = $1, halt_reason = $2 WHERE id = 1
	`, halted, reason)
	if err != nil {
		l.t.Fatalf("setting halted=%v: %v", halted, err)
	}
}
