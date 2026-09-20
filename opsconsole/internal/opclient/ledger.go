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
	"net/url"
	"strconv"
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

// Order is C1's own Order schema (ledger/docs/openapi.yaml).
type Order struct {
	ID               int64  `json:"id"`
	ExternalID       string `json:"external_id"`
	CustomerID       string `json:"customer_id"`
	Tier             string `json:"tier"`
	State            string `json:"state"`
	AmountIn         string `json:"amount_in"`
	AmountOut        string `json:"amount_out"`
	FeeUnits         string `json:"fee_units"`
	NetworkFeeUnits  string `json:"network_fee_units"`
	RecipientAddress string `json:"recipient_address"`
	SenderAddress    string `json:"sender_address,omitempty"`
	QuotedAt         string `json:"quoted_at"`
	QuoteExpiresAt   string `json:"quote_expires_at"`
	Version          int    `json:"version"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

// OrderList is C1's own GET /v1/orders response shape.
type OrderList struct {
	Orders     []Order `json:"orders"`
	NextCursor string  `json:"next_cursor"`
}

// GetOrder fetches one order by its external id (GET /v1/orders/{external_id}).
func (c *LedgerClient) GetOrder(ctx context.Context, externalID string) (Order, error) {
	var out Order
	err := do(ctx, c.http, "ledger", c.token, http.MethodGet, c.baseURL+"/v1/orders/"+url.PathEscape(externalID), nil, &out)
	return out, err
}

// ListOrders lists orders in state, paginated by cursor (GET /v1/orders --
// state is required on C1's own real route, there is no unfiltered
// "list everything" mode).
func (c *LedgerClient) ListOrders(ctx context.Context, state, updatedAfter string, limit int) (OrderList, error) {
	q := url.Values{"state": {state}}
	if updatedAfter != "" {
		q.Set("updated_after", updatedAfter)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out OrderList
	err := do(ctx, c.http, "ledger", c.token, http.MethodGet, c.baseURL+"/v1/orders?"+q.Encode(), nil, &out)
	return out, err
}

// BalanceEntry is one row of C1's own GET /v1/balances response
// (ledger/internal/httpapi/balances.go's own balanceResponse).
type BalanceEntry struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Balance     string `json:"balance"`
}

// GetBalances calls C1's own GET /v1/balances?prefix=... -- an empty
// prefix matches every account (accounts.List with an unfiltered
// CodePrefix), which is how this also covers "list every account": C1
// has no separate GET /v1/accounts route.
func (c *LedgerClient) GetBalances(ctx context.Context, prefix string) ([]BalanceEntry, error) {
	u := c.baseURL + "/v1/balances"
	if prefix != "" {
		u += "?prefix=" + url.QueryEscape(prefix)
	}
	var out struct {
		Balances []BalanceEntry `json:"balances"`
	}
	err := do(ctx, c.http, "ledger", c.token, http.MethodGet, u, nil, &out)
	return out.Balances, err
}

// GetTrialBalance calls C1's own GET /v1/trial-balance -- asset -> total
// across every account (ledger/internal/httpapi/balances.go's own
// getTrialBalance, a plain map with no wrapping envelope).
func (c *LedgerClient) GetTrialBalance(ctx context.Context) (map[string]string, error) {
	var out map[string]string
	err := do(ctx, c.http, "ledger", c.token, http.MethodGet, c.baseURL+"/v1/trial-balance", nil, &out)
	return out, err
}

// PostedLine is one line of an EntryDetail's own Lines (C1's own
// postedLineResponse shape).
type PostedLine struct {
	Seq         int    `json:"seq"`
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}

// EntryDetail is C1's own entryResponse shape
// (ledger/internal/httpapi/entries.go).
type EntryDetail struct {
	ID             int64        `json:"id"`
	IdempotencyKey string       `json:"idempotency_key"`
	EntryType      string       `json:"entry_type"`
	OrderID        *int64       `json:"order_id,omitempty"`
	Actor          string       `json:"actor"`
	OccurredAt     time.Time    `json:"occurred_at"`
	RecordedAt     time.Time    `json:"recorded_at"`
	ReversalOf     *int64       `json:"reversal_of,omitempty"`
	Lines          []PostedLine `json:"lines"`
	Outcome        string       `json:"outcome"`
}

// GetEntry calls C1's own GET /v1/entries/{id}.
func (c *LedgerClient) GetEntry(ctx context.Context, id int64) (EntryDetail, error) {
	var out EntryDetail
	err := do(ctx, c.http, "ledger", c.token, http.MethodGet, c.baseURL+"/v1/entries/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}

// GetEntryByKey calls C1's own GET /v1/entries?idempotency_key=... --
// there is no route to browse every entry; a caller needs either this
// key or the numeric id already. See ledger_admin.go's own doc comment
// for why.
func (c *LedgerClient) GetEntryByKey(ctx context.Context, idempotencyKey string) (EntryDetail, error) {
	var out EntryDetail
	err := do(ctx, c.http, "ledger", c.token, http.MethodGet, c.baseURL+"/v1/entries?idempotency_key="+url.QueryEscape(idempotencyKey), nil, &out)
	return out, err
}

// PostReversal calls C1's own POST /v1/entries/{id}/reversal. C1 derives
// the reversing entry's own idempotency key from the original's, never
// from this call's Idempotency-Key header (that header only satisfies
// the blanket no-header-no-write rule) -- do() attaches one
// automatically, same as every other write this client makes. A 409
// against an already-reversed entry comes back as C1's own real
// already_reversed APIError, unchanged.
func (c *LedgerClient) PostReversal(ctx context.Context, id int64, reason string, occurredAt time.Time) (EntryDetail, error) {
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	body := map[string]any{"reason": reason, "occurred_at": occurredAt}
	var out EntryDetail
	err := do(ctx, c.http, "ledger", c.token, http.MethodPost, c.baseURL+"/v1/entries/"+strconv.FormatInt(id, 10)+"/reversal", body, &out)
	return out, err
}

// PostReorg calls C1's own POST /v1/orders/{external_id}/reorg --
// originalEntryKey is the idempotency key of the deposit_final entry
// that funded this order (not its numeric id), the one fact a caller
// detecting the reorg actually has. C1 decides what a reorg means from
// the order's own current state; this call names no scenario or target
// state.
func (c *LedgerClient) PostReorg(ctx context.Context, externalID, originalEntryKey string) (Order, error) {
	body := map[string]string{"original_entry_key": originalEntryKey}
	var out Order
	err := do(ctx, c.http, "ledger", c.token, http.MethodPost, c.baseURL+"/v1/orders/"+url.PathEscape(externalID)+"/reorg", body, &out)
	return out, err
}

// TransitionRequest is the body OC.16's forced-transition form submits --
// deliberately narrower than C1's own real postTransitionRequest, which
// also accepts an `entry` field carrying arbitrary freeform journal
// lines to post alongside the transition. This console exposes only the
// two well-scoped ways to drive a transition C1's own route supports:
// no linked entry at all, or EntryID naming an entry ALREADY posted
// through a real, validated route (POST /v1/entries/{id}/reversal, in
// practice) -- never freeform line construction, which is a
// qualitatively larger and separately-dangerous capability (fabricating
// arbitrary accounting entries from scratch) this document's own build
// text for this chunk does not ask for and this console does not add.
type TransitionRequest struct {
	ToState         string    `json:"to_state"`
	ExpectedVersion int32     `json:"expected_version"`
	Reason          string    `json:"reason"`
	OccurredAt      time.Time `json:"occurred_at"`
	EntryID         *int64    `json:"entry_id,omitempty"`
}

// PostTransition calls C1's own POST /v1/orders/{external_id}/transitions.
// C1's own transition table already rejects illegal (from, to) pairs;
// this call does not replicate that check client-side -- C1's real
// error (illegal_transition, version_conflict, ...) is surfaced
// unchanged.
func (c *LedgerClient) PostTransition(ctx context.Context, externalID string, req TransitionRequest) (Order, error) {
	if req.OccurredAt.IsZero() {
		req.OccurredAt = time.Now().UTC()
	}
	var out Order
	err := do(ctx, c.http, "ledger", c.token, http.MethodPost, c.baseURL+"/v1/orders/"+url.PathEscape(externalID)+"/transitions", req, &out)
	return out, err
}
