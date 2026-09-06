//go:build integration

// Requires a real, reachable Postgres 16 instance (LEDGER_TEST_DATABASE_URL)
// AND a sibling checkout of the ledger module at ../../../ledger -- the
// same usdt-settlement-corridor layout this whole project already uses.
// This builds and runs the REAL ledgerd binary as a subprocess and drives
// it purely over HTTP -- the C3.3 build spec's own final acceptance
// criterion: "a live integration test against a real C1 instance
// confirms a genuinely funded order appears within one poll interval and
// its sender_address round-trips correctly from what C2 originally
// observed." Mirrors depositwatcher's own
// internal/ledgerclient/ledgerclient_integration_test.go harness exactly
// -- duplicated rather than shared, since these are two separate Go
// modules with no common internal package between them. Run via
// `make test-integration` (build tag "integration").
package ledgerclient_test

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

	"screening/internal/ledgerclient"
)

const (
	ledgerListenAddr = ":18436"
	ledgerBaseURL    = "http://localhost:18436"
	ledgerAPIToken   = "c33-integration-test-token"
	ledgerActor      = "screening"
)

// liveLedger is a real ledgerd process, built and started fresh for this
// test file, plus a raw SQL connection to the same database used ONLY
// for account-creation fixture setup (see this file's own createAccount).
type liveLedger struct {
	t    *testing.T
	pool *pgxpool.Pool
}

func startLiveLedger(t *testing.T) *liveLedger {
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
		"LEDGER_API_TOKENS="+ledgerAPIToken+":"+ledgerActor,
		"LEDGER_LISTEN_ADDR="+ledgerListenAddr,
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

	ll := &liveLedger{t: t, pool: pool}
	ll.waitHealthy()
	return ll
}

func (ll *liveLedger) waitHealthy() {
	ll.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(ledgerBaseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	ll.t.Fatalf("ledgerd never became healthy at %s within the deadline", ledgerBaseURL)
}

// createAccount mirrors accounts.Create's own INSERT exactly -- there is
// no public HTTP endpoint for creating an account, so this is test
// scaffolding only, never a claim that C3 itself can or should write to
// C1's database directly.
func (ll *liveLedger) createAccount(code, accountType, asset string, normalSide int) {
	ll.t.Helper()
	_, err := ll.pool.Exec(context.Background(), `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	if err != nil {
		ll.t.Fatalf("creating fixture account %q: %v", code, err)
	}
}

type orderResp struct {
	ID            int64   `json:"id"`
	ExternalID    string  `json:"external_id"`
	State         string  `json:"state"`
	Version       int32   `json:"version"`
	SenderAddress *string `json:"sender_address"`
}

func (ll *liveLedger) do(method, path string, idempotencyKey string, body any) (*http.Response, []byte) {
	ll.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			ll.t.Fatalf("encoding request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ledgerBaseURL+path, reader)
	if err != nil {
		ll.t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ledgerAPIToken)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ll.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		ll.t.Fatalf("%s %s: reading response body: %v", method, path, err)
	}
	return resp, respBody
}

func (ll *liveLedger) createOrder(externalID, customerID string) orderResp {
	ll.t.Helper()
	now := time.Now().UTC()
	resp, body := ll.do(http.MethodPost, "/v1/orders", "create:"+externalID, map[string]any{
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
		ll.t.Fatalf("POST /v1/orders for %s: status %d: %s", externalID, resp.StatusCode, body)
	}
	var o orderResp
	if err := json.Unmarshal(body, &o); err != nil {
		ll.t.Fatalf("decoding order response: %v: %s", err, body)
	}
	return o
}

// fundOrder transitions order to funded exactly the way C2's own
// ledgerclient.ReportDepositFinal does -- the real deposit_final entry
// shape, plus sender_address sibling to entry.
func (ll *liveLedger) fundOrder(order orderResp, customerID, senderAddress string) orderResp {
	ll.t.Helper()
	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", order.ID)
	custBEP := "liability:customer:" + customerID
	ll.createAccount(depositAcc, "ASSET", "USDT_BEP20", 1)
	ll.createAccount(custBEP, "LIABILITY", "USDT_BEP20", -1)

	resp, body := ll.do(http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", order.ExternalID),
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
		ll.t.Fatalf("POST /v1/orders/%s/transitions to funded: status %d: %s", order.ExternalID, resp.StatusCode, body)
	}
	var o orderResp
	if err := json.Unmarshal(body, &o); err != nil {
		ll.t.Fatalf("decoding transition response: %v: %s", err, body)
	}
	return o
}

// TestPollFundedOrdersAndGetSenderAddress_AgainstRealC1 is C3.3's own
// final acceptance criterion: a genuinely funded order appears via
// PollFundedOrders, and GetSenderAddress returns exactly the address
// that was reported at funding time -- against a real, running C1, not
// a fake standing in for one.
func TestPollFundedOrdersAndGetSenderAddress_AgainstRealC1(t *testing.T) {
	ll := startLiveLedger(t)
	externalID := "c33-discovery-" + fmt.Sprint(time.Now().UnixNano())
	customerID := "c33-discovery-cust-" + fmt.Sprint(time.Now().UnixNano())
	const sender = "0xAE2166bd7901Ea67c1E2Bc4179418fC228108F0"

	order := ll.createOrder(externalID, customerID)
	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)

	// Before funding: not visible under state=funded at all.
	refsBefore, _, err := client.PollFundedOrders(context.Background(), "")
	if err != nil {
		t.Fatalf("PollFundedOrders (before funding): %v", err)
	}
	for _, ref := range refsBefore {
		if ref.ExternalID == externalID {
			t.Fatalf("order %s appeared in PollFundedOrders before it was ever funded", externalID)
		}
	}

	ll.fundOrder(order, customerID, sender)

	// Within one poll (no interval to wait on -- PollFundedOrders is a
	// single synchronous call, not the loop): the order must now appear.
	refs, _, err := client.PollFundedOrders(context.Background(), "")
	if err != nil {
		t.Fatalf("PollFundedOrders (after funding): %v", err)
	}
	var found bool
	for _, ref := range refs {
		if ref.ExternalID == externalID {
			found = true
			if ref.OrderID != order.ID {
				t.Fatalf("PollFundedOrders returned order_id %d for %s, want %d", ref.OrderID, externalID, order.ID)
			}
		}
	}
	if !found {
		t.Fatalf("order %s did not appear in PollFundedOrders after being funded", externalID)
	}

	got, err := client.GetSenderAddress(context.Background(), externalID)
	if err != nil {
		t.Fatalf("GetSenderAddress: %v", err)
	}
	if got != sender {
		t.Fatalf("GetSenderAddress = %q, want %q (the address reported at funding time)", got, sender)
	}
}

// TestPollFundedOrders_CursorNeverReturnsAnAlreadySeenOrder proves the
// cursor this client threads through is genuinely forward-only against a
// real C1: paging with limit=1 across several funded orders never
// repeats one.
func TestPollFundedOrders_CursorNeverReturnsAnAlreadySeenOrder(t *testing.T) {
	ll := startLiveLedger(t)
	customerID := "c33-cursor-cust-" + fmt.Sprint(time.Now().UnixNano())
	const sender = "0xAE2166bd7901Ea67c1E2Bc4179418fC228108F1"

	var externalIDs []string
	for i := 0; i < 3; i++ {
		externalID := fmt.Sprintf("c33-cursor-%d-%d", time.Now().UnixNano(), i)
		order := ll.createOrder(externalID, customerID)
		ll.fundOrder(order, customerID, sender)
		externalIDs = append(externalIDs, externalID)
		time.Sleep(2 * time.Millisecond)
	}

	client := ledgerclient.New(ledgerBaseURL, ledgerAPIToken)
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 50; pages++ {
		refs, newCursor, err := client.PollFundedOrders(context.Background(), cursor)
		if err != nil {
			t.Fatalf("PollFundedOrders: %v", err)
		}
		if len(refs) == 0 {
			break
		}
		for _, ref := range refs {
			if seen[ref.ExternalID] {
				t.Fatalf("order %s returned twice across pages", ref.ExternalID)
			}
			seen[ref.ExternalID] = true
		}
		cursor = newCursor
	}

	for _, externalID := range externalIDs {
		if !seen[externalID] {
			t.Fatalf("order %s (funded) was never returned by any page", externalID)
		}
	}
}
