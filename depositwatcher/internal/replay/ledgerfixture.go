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

	"depositwatcher/internal/db"
)

// ledgerFixture drives C1 as a test fixture: creating orders and forcing
// them into states a real order lifecycle would only reach through C1's
// own screening/dispatch/settlement chunks, none of which C2 or this
// harness has any business calling for real. This is harness-only --
// unlike ledgerclient.Client, nothing here is a capability C2 itself
// ever legitimately needs in production (C2 never creates a C1 order;
// C6 does), so it stays in this "not itself imported by anything else"
// package rather than being added to ledgerclient.
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
	CustomerID string `json:"customer_id"`
	State      string `json:"state"`
	AmountIn   string `json:"amount_in"`
	Version    int32  `json:"version"`
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

func (f *ledgerFixture) createOrder(ctx context.Context, externalID, customerID, amountIn, amountOut, fee, networkFee string, quoteExpiresIn time.Duration) (fixtureOrder, error) {
	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, "/v1/orders", "replay:create:"+externalID, map[string]any{
		"external_id": externalID, "customer_id": customerID, "tier": "STANDARD",
		"amount_in": amountIn, "amount_out": amountOut, "fee_units": fee, "network_fee_units": networkFee,
		"recipient_address": "T-recipient-" + externalID, "quoted_at": now, "quote_expires_at": now.Add(quoteExpiresIn),
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

func (f *ledgerFixture) getOrder(ctx context.Context, externalID string) (fixtureOrder, error) {
	status, body, err := f.do(ctx, http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("GET /v1/orders/%s: status %d: %s", externalID, status, body)
	}
	var o fixtureOrder
	if err := json.Unmarshal(body, &o); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding order response: %w: %s", err, body)
	}
	return o, nil
}

// expireOrder forces externalID's order from Quoted to Expired -- what a
// real quote-expiry sweep (out of scope for both C1 and C2 as built)
// would eventually do, needed here purely so the QuoteExpiry scenario has
// a real expired order in C1 to race against.
func (f *ledgerFixture) expireOrder(ctx context.Context, o fixtureOrder) error {
	status, body, err := f.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", o.ExternalID),
		"replay:expire:"+o.ExternalID, map[string]any{
			"to_state": "expired", "expected_version": o.Version,
			"reason": "replay harness: quote expiry", "occurred_at": time.Now().UTC(),
		})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("expiring order %s: status %d: %s", o.ExternalID, status, body)
	}
	return nil
}

type fixtureHalt struct {
	Halted bool   `json:"halted"`
	Reason string `json:"reason"`
}

func (f *ledgerFixture) getHalt(ctx context.Context) (fixtureHalt, error) {
	status, body, err := f.do(ctx, http.MethodGet, "/v1/system/halt", "", nil)
	if err != nil {
		return fixtureHalt{}, err
	}
	if status != http.StatusOK {
		return fixtureHalt{}, fmt.Errorf("GET /v1/system/halt: status %d: %s", status, body)
	}
	var h fixtureHalt
	if err := json.Unmarshal(body, &h); err != nil {
		return fixtureHalt{}, fmt.Errorf("decoding halt response: %w: %s", err, body)
	}
	return h, nil
}

func (f *ledgerFixture) clearHalt(ctx context.Context) error {
	status, body, err := f.do(ctx, http.MethodPost, "/v1/system/halt", "replay:clear-halt", map[string]any{
		"action": "clear", "note": "c2.10 replay harness cleanup",
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("clearing halt: status %d: %s", status, body)
	}
	return nil
}

// createAccount mirrors accounts.Create's own INSERT exactly against the
// ledger database (pool -- a *pgxpool.Pool connected to the LEDGER's own
// database, satisfying depositwatcher's own db.Queryer structurally,
// since both modules use the same pgx/v5 types). There is no public HTTP
// endpoint for account creation (C1's own chart of accounts is seeded at
// boot or created via the internal accounts.Create Go function, never
// over HTTP -- C2 never gets to call that), so this fixture does what
// C2.6/C2.7's own tests already do, never as a claim that C2 itself can
// or should write to C1's database.
func (f *ledgerFixture) createAccount(ctx context.Context, pool db.Queryer, code, accountType, asset string, normalSide int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	return err
}
