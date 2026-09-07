package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ledgerFixture drives C1 as a test fixture: creating the orders and
// accounts this harness's own scenarios need, none of which C4 has any
// business doing for real (C4 only ever resolves an existing order via
// OrderResolver.GetOrder and posts E4 entries against it -- it never
// creates orders or accounts on C1). This is harness-only, mirroring
// screening's and depositwatcher's own internal/replay/ledgerfixture.go,
// and this module's own internal/testledger (the *testing.T-bound
// helper this harness can't use from cmd/replay/main.go, which is an
// ordinary binary, not a go test).
//
// Unlike screening's own fixture, this one never needs to FUND an order
// (drive it to `funded`, `held`, `screened`, etc.) -- C4's own
// OrderResolver.GetOrder resolves external_id -> internal order id
// regardless of the order's own state (confirmed against
// ledgerclient.GetOrder's real implementation: a bare
// GET /v1/orders/{external_id}), so a freshly created order is already
// everything a reservation needs to attach to.
type ledgerFixture struct {
	baseURL string
	token   string
	http    *http.Client
}

func newLedgerFixture(baseURL, token string) *ledgerFixture {
	return &ledgerFixture{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second}}
}

func (f *ledgerFixture) do(ctx context.Context, method, path, idempotencyKey string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, err
}

type fixtureOrder struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
}

func (f *ledgerFixture) waitHealthy(ctx context.Context, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/healthz", nil)
		resp, err := f.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("ledger fixture: %s never became healthy within %s", f.baseURL, deadline)
}

// createOrder posts a real, valid POST /v1/orders -- C4's own scenarios
// just need a real, resolvable order id to attach reservations and E4
// entries to, not any particular order content, the same posture C4.5's
// own ledgerclient_integration_test.go already takes.
func (f *ledgerFixture) createOrder(ctx context.Context, externalID, customerID string) (fixtureOrder, error) {
	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, "/v1/orders", "replay:create:"+externalID, map[string]any{
		"external_id": externalID, "customer_id": customerID, "tier": "STANDARD",
		"amount_in": "3000.000000", "amount_out": "2990.700000", "fee_units": "7.500000", "network_fee_units": "1.800000",
		"recipient_address": "T-recipient-" + externalID, "quoted_at": now, "quote_expires_at": now.Add(10 * time.Minute),
	})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusCreated {
		return fixtureOrder{}, fmt.Errorf("POST /v1/orders for %s: status %d: %s", externalID, status, body)
	}
	var o fixtureOrder
	if err := json.Unmarshal(body, &o); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding order response: %w: %s", err, body)
	}
	return o, nil
}

// createAccount mirrors accounts.Create's own INSERT exactly against the
// ledger database (pool -- a raw connection to C1's own database). There
// is no public HTTP endpoint for account creation, so this fixture does
// what every other integration test in this project already does, never
// as a claim that C4 itself can or should write to C1's database.
func (f *ledgerFixture) createAccount(ctx context.Context, pool *pgxpool.Pool, code, accountType, asset string, normalSide int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	return err
}

// journalEntryCount returns how many journal_entries rows exist with the
// given idempotency_key -- 0 or 1, ever; more than 1 would mean C1's own
// idempotency guarantee broke. There is no public endpoint scoped this
// way, so this queries C1's own database directly, exactly as
// internal/testledger.JournalEntryCount does for this module's other
// live tests.
func journalEntryCount(ctx context.Context, pool *pgxpool.Pool, idempotencyKey string) (int, error) {
	var count int
	err := pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE idempotency_key = $1`, idempotencyKey).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting journal_entries for key %q: %w", idempotencyKey, err)
	}
	return count, nil
}

// journalLineAmount returns the amount_units posted against accountCode
// by the journal_entries row with the given idempotency_key -- used to
// confirm an E4 entry posted the CORRECT actual cost, not merely that
// some entry exists.
func journalLineAmount(ctx context.Context, pool *pgxpool.Pool, idempotencyKey, accountCode string) (int64, error) {
	var amount int64
	err := pool.QueryRow(ctx, `
		SELECT jl.amount_units
		FROM journal_lines jl
		JOIN journal_entries je ON je.id = jl.entry_id
		JOIN accounts a ON a.id = jl.account_id
		WHERE je.idempotency_key = $1 AND a.code = $2
	`, idempotencyKey, accountCode).Scan(&amount)
	if err != nil {
		return 0, fmt.Errorf("fetching journal line amount for key %q, account %q: %w", idempotencyKey, accountCode, err)
	}
	return amount, nil
}
