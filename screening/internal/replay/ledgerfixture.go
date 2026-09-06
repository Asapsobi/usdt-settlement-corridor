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

	"screening/internal/db"
)

// ledgerFixture drives C1 as a test fixture: creating and funding orders
// directly, the way a real C2 would over its own lifetime, none of which
// C3 has any business doing for real. This is harness-only -- unlike
// ledgerclient.Client, nothing here is a capability C3 itself ever
// legitimately needs in production (C3 never creates or funds an order;
// C1/C2 do), so it stays in this "not itself imported by anything else"
// package rather than being added to ledgerclient. Mirrors both
// depositwatcher's own internal/replay/ledgerfixture.go and this
// module's own internal/testledger.FundOrder (the *testing.T-bound
// helper this harness can't use from cmd/replay/main.go, but whose exact
// wire shape -- a transitions POST with an embedded deposit_final entry
// and a sibling sender_address field -- this mirrors precisely).
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
	ID            int64   `json:"id"`
	ExternalID    string  `json:"external_id"`
	CustomerID    string  `json:"customer_id"`
	State         string  `json:"state"`
	Version       int32   `json:"version"`
	SenderAddress *string `json:"sender_address"`
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
	o.CustomerID = customerID
	return o, nil
}

// fundOrder transitions order to funded exactly the way a real C2 does
// (ledgerclient.ReportDepositFinal's own shape, matching
// internal/testledger.FundOrder's own wire format byte for byte): the
// deposit_final entry plus a sibling sender_address field, against
// per-order accounts this fixture creates first since C1 has no public
// endpoint for account creation.
func (f *ledgerFixture) fundOrder(ctx context.Context, pool db.Queryer, o fixtureOrder, senderAddress string) (fixtureOrder, error) {
	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", o.ID)
	custAcc := "liability:customer:" + o.CustomerID
	if err := f.createAccount(ctx, pool, depositAcc, "ASSET", "USDT_BEP20", 1); err != nil {
		return fixtureOrder{}, fmt.Errorf("creating deposit account %s: %w", depositAcc, err)
	}
	if err := f.createAccount(ctx, pool, custAcc, "LIABILITY", "USDT_BEP20", -1); err != nil {
		return fixtureOrder{}, fmt.Errorf("creating customer account %s: %w", custAcc, err)
	}

	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", o.ExternalID),
		"replay:fund:"+o.ExternalID, map[string]any{
			"to_state":         "funded",
			"expected_version": o.Version,
			"reason":           "bep20_deposit_final",
			"occurred_at":      now,
			"sender_address":   senderAddress,
			"entry": map[string]any{
				"entry_type":  "deposit_final",
				"occurred_at": now,
				"lines": []map[string]any{
					{"account_code": depositAcc, "asset": "USDT_BEP20", "amount": "3000.000000"},
					{"account_code": custAcc, "asset": "USDT_BEP20", "amount": "-3000.000000"},
				},
			},
		})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("funding order %s: status %d: %s", o.ExternalID, status, body)
	}
	var funded fixtureOrder
	if err := json.Unmarshal(body, &funded); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding transition response: %w: %s", err, body)
	}
	funded.CustomerID = o.CustomerID
	return funded, nil
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

// cancelFundedOrder forces o (currently `funded`) to `refunded` --
// {Funded, Refunded} in C1's own transition table, "customer cancel
// pre-conversion" -- purely to give scenarioIllegalTransitionRace a
// real order that has genuinely left `funded` by the time C3's own call
// arrives, the same race C1.9/C2.10's own scenario catalog names.
// RequiresEntry: true, so this posts the exact reversal of fundOrder's
// own deposit_final lines (each account nets back to zero) -- C3 itself
// never constructs an entry like this; only this fixture does, to
// simulate what a real customer-cancel path (out of scope for every
// component built so far) would eventually produce.
func (f *ledgerFixture) cancelFundedOrder(ctx context.Context, pool db.Queryer, o fixtureOrder) error {
	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", o.ID)
	custAcc := "liability:customer:" + o.CustomerID

	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", o.ExternalID),
		"replay:cancel:"+o.ExternalID, map[string]any{
			"to_state":         "refunded",
			"expected_version": o.Version,
			"reason":           "customer_cancel_pre_conversion",
			"occurred_at":      now,
			"entry": map[string]any{
				"entry_type":  "reversal",
				"occurred_at": now,
				"lines": []map[string]any{
					{"account_code": depositAcc, "asset": "USDT_BEP20", "amount": "-3000.000000"},
					{"account_code": custAcc, "asset": "USDT_BEP20", "amount": "3000.000000"},
				},
			},
		})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("cancelling order %s: status %d: %s", o.ExternalID, status, body)
	}
	return nil
}

// latestTransitionReason returns the `reason` column of the most recent
// order_transitions row for orderID -- there is no public HTTP endpoint
// for this, so this is fixture scaffolding only, used to confirm the
// exact reason string a transition was recorded with (e.g.
// distinguishing screening_pass_vendor_unavailable from a plain
// screening_pass, or screening_hold_unavailable from
// screening_hold_flagged).
func (f *ledgerFixture) latestTransitionReason(ctx context.Context, pool db.Queryer, orderID int64) (string, error) {
	var reason string
	err := pool.QueryRow(ctx, `
		SELECT reason FROM order_transitions WHERE order_id = $1 ORDER BY id DESC LIMIT 1
	`, orderID).Scan(&reason)
	if err != nil {
		return "", fmt.Errorf("fetching latest transition reason for order %d: %w", orderID, err)
	}
	return reason, nil
}

// createAccount mirrors accounts.Create's own INSERT exactly against the
// ledger database (pool -- a db.Queryer connected to the LEDGER's own
// database). There is no public HTTP endpoint for account creation, so
// this fixture does what every other integration test in this project
// already does, never as a claim that C3 itself can or should write to
// C1's database.
func (f *ledgerFixture) createAccount(ctx context.Context, pool db.Queryer, code, accountType, asset string, normalSide int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	return err
}
