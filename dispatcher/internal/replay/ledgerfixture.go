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

// ledgerFixture drives a real, already-running C1 as a test fixture --
// creating the orders and accounts this harness's own scenarios need,
// error-returning mirror of internal/testledger (the *testing.T-bound
// helper this harness can't use from cmd/replay/main.go, an ordinary
// binary, not a go test), same posture as energybroker's own
// internal/replay/ledgerfixture.go.
type ledgerFixture struct {
	baseURL string
	token   string
	http    *http.Client
	pool    *pgxpool.Pool
}

func newLedgerFixture(baseURL, token string, pool *pgxpool.Pool) *ledgerFixture {
	return &ledgerFixture{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second}, pool: pool}
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

type fixtureOrder struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
	CustomerID string `json:"customer_id"`
	State      string `json:"state"`
	Version    int32  `json:"version"`
}

// createOrder posts a real, valid POST /v1/orders matching
// c1-ledger-build-prompts.md §B's own worked example exactly, and a
// real, valid, checksummed TRON recipient address -- BuildTransfer
// itself validates it, and this harness exercises BuildTransfer for
// real.
func (f *ledgerFixture) createOrder(ctx context.Context, externalID, customerID string) (fixtureOrder, error) {
	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, "/v1/orders", "replay:create:"+externalID, map[string]any{
		"external_id": externalID, "customer_id": customerID, "tier": "STANDARD",
		"amount_in": "3000.000000", "amount_out": "2990.700000", "fee_units": "7.500000", "network_fee_units": "1.800000",
		"recipient_address": "TXLAQ63Xg1NAzckPwKHvzw7CSEmLMEqcdj", "quoted_at": now, "quote_expires_at": now.Add(10 * time.Minute),
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

// advanceToScreened walks a freshly created order through funded and
// into screened -- mirroring internal/testledger.AdvanceToScreened.
func (f *ledgerFixture) advanceToScreened(ctx context.Context, order fixtureOrder) (fixtureOrder, error) {
	depositAccount := "asset:bsc:deposit:" + order.ExternalID
	bepLiability := "liability:customer:" + order.CustomerID + ":USDT_BEP20"
	if err := f.createAccount(ctx, depositAccount, "ASSET", "USDT_BEP20", 1); err != nil {
		return fixtureOrder{}, err
	}
	if err := f.createAccount(ctx, bepLiability, "LIABILITY", "USDT_BEP20", -1); err != nil {
		return fixtureOrder{}, err
	}

	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "replay:fund:"+order.ExternalID, map[string]any{
		"to_state": "funded", "expected_version": order.Version, "reason": "deposit_final", "occurred_at": now,
		"entry": map[string]any{
			"entry_type": "deposit_final", "occurred_at": now,
			"lines": []map[string]any{
				{"account_code": depositAccount, "asset": "USDT_BEP20", "amount": "3000.000000"},
				{"account_code": bepLiability, "asset": "USDT_BEP20", "amount": "-3000.000000"},
			},
		},
	})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("funding %s: status %d: %s", order.ExternalID, status, body)
	}
	var funded fixtureOrder
	if err := json.Unmarshal(body, &funded); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding funded order response: %w: %s", err, body)
	}

	status, body, err = f.do(ctx, http.MethodPost, "/v1/orders/"+order.ExternalID+"/transitions", "replay:screen:"+order.ExternalID, map[string]any{
		"to_state": "screened", "expected_version": funded.Version, "reason": "screening_pass", "occurred_at": now,
	})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("screening %s: status %d: %s", order.ExternalID, status, body)
	}
	var screened fixtureOrder
	if err := json.Unmarshal(body, &screened); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding screened order response: %w: %s", err, body)
	}
	return screened, nil
}

// newScreenedOrder is createOrder + advanceToScreened in one call -- the
// starting point almost every scenario below needs.
func (f *ledgerFixture) newScreenedOrder(ctx context.Context, externalID, customerID string) (fixtureOrder, error) {
	created, err := f.createOrder(ctx, externalID, customerID)
	if err != nil {
		return fixtureOrder{}, err
	}
	return f.advanceToScreened(ctx, created)
}

func (f *ledgerFixture) createAccount(ctx context.Context, code, accountType, asset string, normalSide int) error {
	_, err := f.pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	return err
}

func (f *ledgerFixture) accountBalance(ctx context.Context, code string) (int64, error) {
	var balance int64
	err := f.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(jl.amount_units), 0)
		FROM journal_lines jl
		JOIN accounts a ON a.id = jl.account_id
		WHERE a.code = $1
	`, code).Scan(&balance)
	return balance, err
}

func (f *ledgerFixture) journalEntryCount(ctx context.Context, idempotencyKey string) (int, error) {
	var count int
	err := f.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE idempotency_key = $1`, idempotencyKey).Scan(&count)
	return count, err
}

// journalLineAmount returns the amount_units posted against accountCode
// by the journal_entries row with the given idempotency_key -- used to
// confirm an E3 entry posted the CORRECT amount, not merely that some
// entry exists.
func (f *ledgerFixture) journalLineAmount(ctx context.Context, idempotencyKey, accountCode string) (int64, error) {
	var amount int64
	err := f.pool.QueryRow(ctx, `
		SELECT jl.amount_units
		FROM journal_lines jl
		JOIN journal_entries je ON je.id = jl.entry_id
		JOIN accounts a ON a.id = jl.account_id
		WHERE je.idempotency_key = $1 AND a.code = $2
	`, idempotencyKey, accountCode).Scan(&amount)
	return amount, err
}

// reversalCount returns how many reversal entries exist for the entry
// whose own idempotency key is conversionEntryKey -- FINAL ASSERTION 4's
// own data ("exactly one reversal, never zero, never two").
func (f *ledgerFixture) reversalCount(ctx context.Context, conversionEntryKey string) (int, error) {
	var count int
	err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_entries je
		JOIN journal_entries orig ON orig.id = je.reversal_of
		WHERE orig.idempotency_key = $1
	`, conversionEntryKey).Scan(&count)
	return count, err
}

func (f *ledgerFixture) setHalted(ctx context.Context, halted bool, reason string) error {
	_, err := f.pool.Exec(ctx, `UPDATE system_state SET halted = $1, halt_reason = $2 WHERE id = 1`, halted, reason)
	return err
}

func (f *ledgerFixture) getOrderState(ctx context.Context, externalID string) (string, error) {
	var state string
	err := f.pool.QueryRow(ctx, `SELECT state FROM orders WHERE external_id = $1`, externalID).Scan(&state)
	return state, err
}
