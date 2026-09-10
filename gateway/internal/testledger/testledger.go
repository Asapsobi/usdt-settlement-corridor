// Package testledger builds and runs a REAL ledgerd binary as a
// subprocess and drives it purely over HTTP -- C6.9's own ship gate
// needs a real running C1, not a fake standing in for one, per
// c6-api-gateway-build-prompts.md's own C6.9. Deliberately duplicated
// from screening's/dispatcher's/energybroker's own identical
// internal/testledger rather than shared, matching this project's own
// established convention (each module owns its own copy since they're
// separate Go modules with no common internal package between them).
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
// account-creation fixture setup (there is no public HTTP endpoint for
// account creation with an arbitrary normal_side).
type Ledger struct {
	t       *testing.T
	baseURL string
	token   string
	pool    *pgxpool.Pool
}

// Start builds ledger/cmd/migrate and ledger/cmd/ledgerd from the
// sibling ledger module (../../../ledger, the usdt-settlement-corridor
// layout this whole project uses), runs migrations against
// LEDGER_TEST_DATABASE_URL, and starts ledgerd listening on listenAddr,
// authenticating token to actor. Skips the calling test if
// LEDGER_TEST_DATABASE_URL is unset or no sibling ledger checkout is
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
	migrate.Dir = ledgerRoot
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

func (l *Ledger) BaseURL() string { return l.baseURL }
func (l *Ledger) Token() string   { return l.token }

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

// CreateAccount mirrors accounts.Create's own INSERT exactly against
// the ledger database -- there is no public HTTP endpoint for account
// creation with an arbitrary normal_side, so this is test scaffolding
// only, never a claim that C6 itself can or should write to C1's
// database.
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

// Do sends one authenticated request to this ledgerd instance and
// returns its raw response and body.
func (l *Ledger) Do(method, path string, idempotencyKey string, body any) (int, []byte) {
	l.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			l.t.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(b)
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
	return resp.StatusCode, respBody
}

// Halt sets ledgerd's own halt state via POST /v1/system/halt
// (action=set) -- C1's real operator control surface, used here to
// drive C6.9's own "system halted mid-quote and mid-order-creation"
// scenario.
func (l *Ledger) Halt(reason string) {
	l.t.Helper()
	status, body := l.Do(http.MethodPost, "/v1/system/halt", "replay:halt", map[string]any{
		"action": "set", "reason": reason,
	})
	if status != http.StatusOK {
		l.t.Fatalf("POST /v1/system/halt (set): status %d: %s", status, body)
	}
}

// Clear un-halts ledgerd via POST /v1/system/halt (action=clear).
func (l *Ledger) Clear(note string) {
	l.t.Helper()
	status, body := l.Do(http.MethodPost, "/v1/system/halt", "replay:clear", map[string]any{
		"action": "clear", "note": note,
	})
	if status != http.StatusOK {
		l.t.Fatalf("POST /v1/system/halt (clear): status %d: %s", status, body)
	}
}
