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

// ledgerFixture drives C1 directly through funded -> screened ->
// dispatching -> settled, the way real C2/C3/C4/C5 each would over an
// order's own lifetime -- none of which gateway itself has any
// business doing for real (WHAT C6 IS NOT, §0). Harness-only, mirroring
// screening's, dispatcher's, and energybroker's own identical
// internal/replay/ledgerfixture.go pattern; the exact entry shapes
// below (account codes, entry_type, lines) are copied verbatim from
// screening/internal/ledgerclient.go's ReportVerdict and
// dispatcher/internal/dispatch/{dispatch,finality}.go's own
// buildConversionLines/buildSettlementLines -- this fixture is not
// guessing C1's own accounting rules, it is replaying the real ones.
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

// fixtureOrder is the subset of C1's order resource this fixture needs.
type fixtureOrder struct {
	ID              int64  `json:"id"`
	ExternalID      string `json:"external_id"`
	CustomerID      string `json:"customer_id"`
	State           string `json:"state"`
	Version         int32  `json:"version"`
	AmountIn        string `json:"amount_in"`
	AmountOut       string `json:"amount_out"`
	FeeUnits        string `json:"fee_units"`
	NetworkFeeUnits string `json:"network_fee_units"`
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

func (f *ledgerFixture) createAccount(ctx context.Context, pool *pgxpool.Pool, code, accountType, asset string, normalSide int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
	`, code, accountType, asset, normalSide)
	return err
}

func customerAccountCode(customerID, asset string) string {
	return fmt.Sprintf("liability:customer:%s:%s", customerID, asset)
}

// fundOrder transitions o (quoted) to funded -- ReportDepositFinal's own
// shape: the deposit_final entry crediting a per-order deposit account
// and debiting the customer's own BEP20 liability.
func (f *ledgerFixture) fundOrder(ctx context.Context, pool *pgxpool.Pool, o fixtureOrder) (fixtureOrder, error) {
	depositAcc := fmt.Sprintf("asset:bsc:deposit:%d", o.ID)
	bepLiability := customerAccountCode(o.CustomerID, "USDT_BEP20")
	if err := f.createAccount(ctx, pool, depositAcc, "ASSET", "USDT_BEP20", 1); err != nil {
		return fixtureOrder{}, fmt.Errorf("creating deposit account %s: %w", depositAcc, err)
	}
	if err := f.createAccount(ctx, pool, bepLiability, "LIABILITY", "USDT_BEP20", -1); err != nil {
		return fixtureOrder{}, fmt.Errorf("creating customer BEP20 liability %s: %w", bepLiability, err)
	}

	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", o.ExternalID),
		"replay:fund:"+o.ExternalID, map[string]any{
			"to_state": "funded", "expected_version": o.Version, "reason": "bep20_deposit_final", "occurred_at": now,
			"sender_address": "0xreplay-sender-" + o.ExternalID,
			"entry": map[string]any{
				"entry_type": "deposit_final", "occurred_at": now,
				"lines": []map[string]any{
					{"account_code": depositAcc, "asset": "USDT_BEP20", "amount": o.AmountIn},
					{"account_code": bepLiability, "asset": "USDT_BEP20", "amount": "-" + o.AmountIn},
				},
			},
		})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("funding order %s: status %d: %s", o.ExternalID, status, body)
	}
	return f.decodeOrder(body, o)
}

// screenOrder transitions o (funded) to screened -- RequiresEntry:
// false per C1.5's own table, matching ReportVerdict's real shape
// exactly.
func (f *ledgerFixture) screenOrder(ctx context.Context, o fixtureOrder) (fixtureOrder, error) {
	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", o.ExternalID),
		"replay:screen:"+o.ExternalID, map[string]any{
			"to_state": "screened", "expected_version": o.Version, "reason": "screening_pass", "occurred_at": now,
		})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("screening order %s: status %d: %s", o.ExternalID, status, body)
	}
	return f.decodeOrder(body, o)
}

// dispatchOrder transitions o (screened) to dispatching -- the E2
// conversion entry, buildConversionLines' own six lines verbatim.
func (f *ledgerFixture) dispatchOrder(ctx context.Context, pool *pgxpool.Pool, o fixtureOrder) (fixtureOrder, error) {
	trcLiability := customerAccountCode(o.CustomerID, "USDT_TRC20")
	if err := f.createAccount(ctx, pool, trcLiability, "LIABILITY", "USDT_TRC20", -1); err != nil {
		return fixtureOrder{}, fmt.Errorf("creating customer TRC20 liability %s: %w", trcLiability, err)
	}

	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", o.ExternalID),
		"replay:dispatch:"+o.ExternalID, map[string]any{
			"to_state": "dispatching", "expected_version": o.Version, "reason": "dispatch_start", "occurred_at": now,
			"entry": map[string]any{
				"entry_type": "conversion", "occurred_at": now,
				"lines": []map[string]any{
					{"account_code": customerAccountCode(o.CustomerID, "USDT_BEP20"), "asset": "USDT_BEP20", "amount": o.AmountIn},
					{"account_code": "position:corridor:USDT_BEP20", "asset": "USDT_BEP20", "amount": "-" + o.AmountIn},
					{"account_code": "position:corridor:USDT_TRC20", "asset": "USDT_TRC20", "amount": o.AmountIn},
					{"account_code": trcLiability, "asset": "USDT_TRC20", "amount": "-" + o.AmountOut},
					{"account_code": "revenue:fee", "asset": "USDT_TRC20", "amount": "-" + o.FeeUnits},
					{"account_code": "revenue:network_fee", "asset": "USDT_TRC20", "amount": "-" + o.NetworkFeeUnits},
				},
			},
		})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("dispatching order %s: status %d: %s", o.ExternalID, status, body)
	}
	return f.decodeOrder(body, o)
}

// settleOrder transitions o (dispatching) to settled -- the E3
// settlement entry, buildSettlementLines' own two lines verbatim,
// against a fixture slot account this run owns exclusively.
func (f *ledgerFixture) settleOrder(ctx context.Context, pool *pgxpool.Pool, o fixtureOrder, slotID int) (fixtureOrder, error) {
	slotAcc := fmt.Sprintf("asset:tron:slot:%d", slotID)
	if err := f.createAccount(ctx, pool, slotAcc, "ASSET", "USDT_TRC20", 1); err != nil {
		return fixtureOrder{}, fmt.Errorf("creating slot account %s: %w", slotAcc, err)
	}

	now := time.Now().UTC()
	status, body, err := f.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", o.ExternalID),
		"replay:settle:"+o.ExternalID, map[string]any{
			"to_state": "settled", "expected_version": o.Version, "reason": "payout_sr_final", "occurred_at": now,
			"entry": map[string]any{
				"entry_type": "payout_settled", "occurred_at": now,
				"lines": []map[string]any{
					{"account_code": customerAccountCode(o.CustomerID, "USDT_TRC20"), "asset": "USDT_TRC20", "amount": o.AmountOut},
					{"account_code": slotAcc, "asset": "USDT_TRC20", "amount": "-" + o.AmountOut},
				},
			},
		})
	if err != nil {
		return fixtureOrder{}, err
	}
	if status != http.StatusOK {
		return fixtureOrder{}, fmt.Errorf("settling order %s: status %d: %s", o.ExternalID, status, body)
	}
	return f.decodeOrder(body, o)
}

func (f *ledgerFixture) decodeOrder(body []byte, prev fixtureOrder) (fixtureOrder, error) {
	var o fixtureOrder
	if err := json.Unmarshal(body, &o); err != nil {
		return fixtureOrder{}, fmt.Errorf("decoding transition response: %w: %s", err, body)
	}
	o.AmountIn, o.AmountOut, o.FeeUnits, o.NetworkFeeUnits = prev.AmountIn, prev.AmountOut, prev.FeeUnits, prev.NetworkFeeUnits
	return o, nil
}
