package opclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ProofrunClient calls the MVP proof-run driver
// (proofrun/internal/httpapi/server.go's own doc comment: "no auth, no
// rate limiting... a supervised proof-run tool, not a customer-facing
// service"). This client, and the console page that uses it, is exactly
// that supervision: every call still only runs from behind the
// console's own operator-session auth, and every create-order call is
// written to the audit log before it goes out, same as every other
// write in this app. Unlike every other client in this package there
// is no bearer token -- proofrun's own server genuinely checks none.
type ProofrunClient struct {
	baseURL string
	http    *http.Client
}

// NewProofrunClient returns a ProofrunClient for baseURL.
func NewProofrunClient(baseURL string) *ProofrunClient {
	return &ProofrunClient{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

// Healthz reports whether proofrun's own /healthz responds 200.
func (c *ProofrunClient) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "proofrun", c.baseURL)
}

// ProofrunAPIError is proofrun's own {"message": "..."} error shape
// (proofrun/internal/httpapi/server.go's apiError) -- distinct from
// every other service's {"error":{"code","message"}} envelope, so it
// gets its own decoding rather than reusing opclient's shared do().
type ProofrunAPIError struct {
	Status  int
	Message string
}

func (e *ProofrunAPIError) Error() string {
	return fmt.Sprintf("opclient: proofrun returned %d: %s", e.Status, e.Message)
}

func (c *ProofrunClient) call(ctx context.Context, method, url string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opclient: proofrun: encoding request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return fmt.Errorf("opclient: proofrun: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opclient: proofrun: %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("opclient: proofrun: reading response: %w", err)
	}
	if resp.StatusCode >= 300 {
		var apiErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(respBody, &apiErr)
		return &ProofrunAPIError{Status: resp.StatusCode, Message: apiErr.Message}
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("opclient: proofrun: decoding response: %w", err)
		}
	}
	return nil
}

// CreatePayoutRequest mirrors proofrun's own POST /v1/payouts request
// shape (proofrun/internal/httpapi/payouts_handlers.go's postPayoutRequest).
type CreatePayoutRequest struct {
	ExternalID           string `json:"external_id"`
	CustomerID           string `json:"customer_id"`
	RecipientTronAddress string `json:"recipient_tron_address"`
	AmountIn             string `json:"amount_in"`
}

// CreatePayoutResult mirrors proofrun's own postPayoutResponse.
type CreatePayoutResult struct {
	ExternalID      string `json:"external_id"`
	OrderID         int64  `json:"order_id"`
	DepositAddress  string `json:"deposit_address"`
	AmountIn        string `json:"amount_in"`
	AmountOut       string `json:"amount_out"`
	FeeUnits        string `json:"fee_units"`
	NetworkFeeUnits string `json:"network_fee_units"`
	QuoteExpiresAt  string `json:"quote_expires_at"`
}

// CreatePayout calls proofrun's own POST /v1/payouts: create a real C1
// order and assign it a real C2 deposit address in one call.
func (c *ProofrunClient) CreatePayout(ctx context.Context, req CreatePayoutRequest) (CreatePayoutResult, error) {
	var out CreatePayoutResult
	err := c.call(ctx, http.MethodPost, c.baseURL+"/v1/payouts", req, &out)
	return out, err
}

// PayoutStatus mirrors proofrun's own getPayoutResponse
// (proofrun/internal/httpapi/payouts_handlers.go) -- a nil pointer
// field means that component hasn't been reached yet, not an error.
type PayoutStatus struct {
	ExternalID       string  `json:"external_id"`
	OrderID          int64   `json:"order_id"`
	State            string  `json:"state"`
	AmountIn         string  `json:"amount_in"`
	AmountOut        string  `json:"amount_out"`
	RecipientAddress string  `json:"recipient_address"`
	DepositAddress   *string `json:"deposit_address,omitempty"`
	AddressStatus    *string `json:"deposit_address_status,omitempty"`
	AddressError     string  `json:"deposit_address_error,omitempty"`
	DispatchStatus   *string `json:"dispatch_status,omitempty"`
	SlotID           *int    `json:"slot_id,omitempty"`
	DispatchError    string  `json:"dispatch_error,omitempty"`
	PayoutTxHash     *string `json:"payout_tx_hash,omitempty"`
	Note             string  `json:"note"`
}

// GetPayout calls proofrun's own GET /v1/payouts/{external_id}.
func (c *ProofrunClient) GetPayout(ctx context.Context, externalID string) (PayoutStatus, error) {
	var out PayoutStatus
	err := c.call(ctx, http.MethodGet, c.baseURL+"/v1/payouts/"+url.PathEscape(externalID), nil, &out)
	return out, err
}
