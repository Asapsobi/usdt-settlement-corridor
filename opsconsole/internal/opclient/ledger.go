// Package opclient holds the ops console's six typed HTTP clients, one
// per downstream service (C1-C5/S1) -- narrow, matching exactly what the
// console's own chunks need, not a generic REST client. Each file mirrors
// gateway/internal/c1client's own shape (baseURL/token/http.Client,
// APIError, a small do helper) rather than inventing a new convention.
package opclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// APIError is a structured error response from a downstream service,
// preserved so a caller can render Code/Message without string-matching.
type APIError struct {
	Service string
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("opclient: %s returned %d %s: %s", e.Service, e.Status, e.Code, e.Message)
}

type apiErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeAPIError(service string, status int, body []byte) *APIError {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(body, &envelope)
	return &APIError{Service: service, Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// LedgerClient calls one C1 (ledger) instance.
type LedgerClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewLedgerClient returns a LedgerClient for baseURL, authenticating
// every call with token.
func NewLedgerClient(baseURL, token string) *LedgerClient {
	return &LedgerClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

func do(ctx context.Context, client *http.Client, service, token, method, url string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("opclient: %s: encoding request: %w", service, err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return fmt.Errorf("opclient: %s: building request: %w", service, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idempotencyKey())
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("opclient: %s: %s %s: %w", service, method, url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("opclient: %s: reading response: %w", service, err)
	}
	if resp.StatusCode >= 300 {
		return decodeAPIError(service, resp.StatusCode, respBody)
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("opclient: %s: decoding response: %w", service, err)
		}
	}
	return nil
}

// Healthz reports whether the ledger's own /healthz responds 200.
func (c *LedgerClient) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "ledger", c.baseURL)
}

func checkHealthz(ctx context.Context, client *http.Client, service, baseURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("opclient: %s: healthz: %w", service, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opclient: %s: healthz returned %d", service, resp.StatusCode)
	}
	return nil
}

// HaltState is C1's own GET /v1/system/halt response shape
// (ledger/internal/httpapi/system.go's own haltStateResponse).
type HaltState struct {
	Halted bool   `json:"halted"`
	Reason string `json:"reason,omitempty"`
}

// GetHaltState reads C1's current halt state.
func (c *LedgerClient) GetHaltState(ctx context.Context) (HaltState, error) {
	var out HaltState
	err := do(ctx, c.http, "ledger", c.token, http.MethodGet, c.baseURL+"/v1/system/halt", nil, &out)
	return out, err
}

// SetHalt sets or clears C1's halt state. action is "set" or "clear".
// note is threaded in as the operator's own display name (see OC.3's own
// doc comment: C1 has no per-human actor concept beyond the bearer
// token's own identity, so the operator's name goes in note instead).
func (c *LedgerClient) SetHalt(ctx context.Context, action, reason, note string) error {
	body := map[string]any{"action": action, "reason": reason, "note": note}
	return do(ctx, c.http, "ledger", c.token, http.MethodPost, c.baseURL+"/v1/system/halt", body, nil)
}

// LedgerInvariants is C1's own GET /v1/system/invariants response shape
// (ledger/internal/httpapi/system.go's own invariantsResponse), only the
// fields the console's home page (OC.2) renders.
type LedgerInvariants struct {
	Halted          bool    `json:"halted"`
	HaltReason      string  `json:"halt_reason,omitempty"`
	TrialBalanceOK  bool    `json:"trial_balance_ok"`
	CacheOK         bool    `json:"cache_ok"`
	ReconLagSeconds float64 `json:"recon_lag_seconds"`
}

// GetInvariants reads C1's system invariants.
func (c *LedgerClient) GetInvariants(ctx context.Context) (LedgerInvariants, error) {
	var out LedgerInvariants
	err := do(ctx, c.http, "ledger", c.token, http.MethodGet, c.baseURL+"/v1/system/invariants", nil, &out)
	return out, err
}
