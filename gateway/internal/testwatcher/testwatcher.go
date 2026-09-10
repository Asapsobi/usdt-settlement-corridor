// Package testwatcher builds and runs a REAL watcherd (C2) binary as a
// subprocess and drives it purely over HTTP -- C6.9's own ship gate
// needs a real running C2 too, not a fake, per
// c6-api-gateway-build-prompts.md's own C6.9. Started with
// WATCHER_RPC_PROVIDERS unset, so watcherd serves its HTTP boundary
// only, with no live chain-watching engine -- gateway's own replay
// never needs a real BSC RPC provider, only POST /v1/addresses and
// GET /v1/addresses/{order_id}. Mirrors internal/testledger's own
// shape exactly.
package testwatcher

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
	"github.com/tyler-smith/go-bip32"
)

// Watcher is a real watcherd process, built and started fresh for one
// test.
type Watcher struct {
	t       *testing.T
	baseURL string
	token   string
}

// fixtureXpub is a deterministic, throwaway extended public key --
// never a real key, never used for anything but satisfying watcherd's
// own required-at-startup WATCHER_XPUB. Matches
// depositwatcher/internal/replay/harness.go's own fixture-generation
// approach (a fixed seed string through BIP32), just run here through
// the standard github.com/tyler-smith/go-bip32 (a real, valid xpub
// wire format any correct BIP32 parser accepts, including
// depositwatcher's own hand-rolled internal/addresses/bip32.go).
func fixtureXpub() (string, error) {
	master, err := bip32.NewMasterKey([]byte("c6.9 gateway replay harness fixture -- never use for anything real"))
	if err != nil {
		return "", fmt.Errorf("testwatcher: generating fixture xpub: %w", err)
	}
	return master.PublicKey().B58Serialize(), nil
}

// Start builds depositwatcher/cmd/migrate and depositwatcher/cmd/watcherd
// from the sibling depositwatcher module (../../../depositwatcher, the
// usdt-settlement-corridor layout this whole project uses), runs
// migrations against WATCHER_TEST_DATABASE_URL, and starts watcherd
// listening on listenAddr, authenticating token to actor. Skips the
// calling test if WATCHER_TEST_DATABASE_URL is unset or no sibling
// depositwatcher checkout is found.
func Start(t *testing.T, listenAddr, token, actor string) *Watcher {
	t.Helper()
	dbURL := os.Getenv("WATCHER_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("WATCHER_TEST_DATABASE_URL not set; skipping integration test")
	}

	watcherRoot, err := filepath.Abs(filepath.Join("..", "..", "..", "depositwatcher"))
	if err != nil {
		t.Fatalf("resolving depositwatcher module path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(watcherRoot, "go.mod")); err != nil {
		t.Skipf("no sibling depositwatcher module found at %s; skipping (expects the usdt-settlement-corridor layout)", watcherRoot)
	}

	tmpDir := t.TempDir()
	migrateBin := filepath.Join(tmpDir, "migrate_bin")
	watcherdBin := filepath.Join(tmpDir, "watcherd_bin")

	build := func(out, pkg string) {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = watcherRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("building %s: %v\n%s", pkg, err, output)
		}
	}
	build(migrateBin, "./cmd/migrate")
	build(watcherdBin, "./cmd/watcherd")

	migrate := exec.Command(migrateBin, "up")
	migrate.Dir = watcherRoot
	migrate.Env = append(os.Environ(), "WATCHER_DATABASE_URL="+dbURL)
	if output, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("running watcher migrations: %v\n%s", err, output)
	}

	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("connecting to watcher test database: %v", err)
	}
	pool.Close() // only used to fail fast if the database is unreachable

	xpub, err := fixtureXpub()
	if err != nil {
		t.Fatalf("%v", err)
	}

	watcherd := exec.Command(watcherdBin)
	watcherd.Dir = watcherRoot
	watcherd.Env = append(os.Environ(),
		"WATCHER_DATABASE_URL="+dbURL,
		"WATCHER_API_TOKENS="+token+":"+actor,
		"WATCHER_LISTEN_ADDR="+listenAddr,
		"WATCHER_XPUB="+xpub,
	)
	var logs bytes.Buffer
	watcherd.Stdout = &logs
	watcherd.Stderr = &logs
	if err := watcherd.Start(); err != nil {
		t.Fatalf("starting watcherd: %v", err)
	}
	t.Cleanup(func() {
		_ = watcherd.Process.Kill()
		_ = watcherd.Wait()
		if t.Failed() {
			t.Logf("watcherd output:\n%s", logs.String())
		}
	})

	baseURL := "http://localhost" + listenAddr
	w := &Watcher{t: t, baseURL: baseURL, token: token}
	w.waitHealthy()
	return w
}

func (w *Watcher) BaseURL() string { return w.baseURL }
func (w *Watcher) Token() string   { return w.token }

func (w *Watcher) waitHealthy() {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(w.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.t.Fatalf("watcherd never became healthy at %s within the deadline", w.baseURL)
}

// Do sends one authenticated request to this watcherd instance and
// returns its raw response and body.
func (w *Watcher) Do(method, path string, idempotencyKey string, body any) (int, []byte) {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, w.baseURL+path, reader)
	if err != nil {
		w.t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		w.t.Fatalf("%s %s: reading response body: %v", method, path, err)
	}
	return resp.StatusCode, respBody
}
