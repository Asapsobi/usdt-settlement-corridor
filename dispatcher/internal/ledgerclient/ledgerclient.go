// Package ledgerclient is C5's only path to C1: every HTTP call this
// service makes to the ledger core goes through here, built against
// ledger/internal/httpapi's REAL shipped request/response shapes and
// error codes (errors.go), not re-derived from c5-payout-dispatcher-
// build-prompts.md's own §A sketch -- confirmed by reading the real code
// directly, the same "the code, not the spec, is the source of truth"
// discipline every prior component in this project has applied to C1.
package ledgerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dispatcher/internal/money"
)

// Client calls one C1 (ledger) instance, authenticating with a single
// bearer token.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for baseURL, authenticating every call with
// token.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

// APIError is a structured error response from C1, preserved so a
// caller can inspect Code without string-matching Error()'s text.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ledgerclient: C1 returned %d %s: %s", e.Status, e.Code, e.Message)
}

func decodeAPIError(status int, body []byte) *APIError {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(body, &envelope)
	return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// Sentinel errors matched against APIError.Code -- C1's own real,
// stable error codes (ledger/internal/httpapi/errors.go), not guessed.
var (
	ErrOrderNotFound     = errors.New("ledgerclient: no such order")
	ErrIllegalTransition = errors.New("ledgerclient: no such transition is legal from the order's current state")
	ErrVersionConflict   = errors.New("ledgerclient: expected_version did not match the order's current version")
	ErrSystemHalted      = errors.New("ledgerclient: the ledger is halted")
	ErrAlreadyReversed   = errors.New("ledgerclient: that entry has already been reversed")
	ErrEntryNotFound     = errors.New("ledgerclient: no such journal entry")
)

var codeToSentinel = map[string]error{
	"order_not_found":    ErrOrderNotFound,
	"illegal_transition": ErrIllegalTransition,
	"version_conflict":   ErrVersionConflict,
	"system_halted":      ErrSystemHalted,
	"already_reversed":   ErrAlreadyReversed,
	"entry_not_found":    ErrEntryNotFound,
}

// classify wraps apiErr with whichever sentinel its Code matches, so
// callers can errors.Is against a stable Go error instead of comparing
// Code strings themselves.
func classify(apiErr *APIError) error {
	if sentinel, ok := codeToSentinel[apiErr.Code]; ok {
		return fmt.Errorf("%w: %w", sentinel, apiErr)
	}
	return apiErr
}

func (c *Client) do(ctx context.Context, method, path, idempotencyKey string, body any) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("ledgerclient: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("ledgerclient: building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("ledgerclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("ledgerclient: reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

// Order is the subset of C1's order resource this client reads.
type Order struct {
	ID               int64
	ExternalID       string
	CustomerID       string
	Tier             string
	State            string
	AmountIn         money.Amount
	AmountOut        money.Amount
	FeeUnits         money.Amount
	NetworkFeeUnits  money.Amount
	RecipientAddress string
	Version          int32
}

type orderResponse struct {
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
	Version          int32  `json:"version"`
}

func (r orderResponse) toOrder() (Order, error) {
	amountIn, err := money.ParseDecimal(r.AmountIn)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: parsing amount_in: %w", err)
	}
	amountOut, err := money.ParseDecimal(r.AmountOut)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: parsing amount_out: %w", err)
	}
	feeUnits, err := money.ParseDecimal(r.FeeUnits)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: parsing fee_units: %w", err)
	}
	networkFeeUnits, err := money.ParseDecimal(r.NetworkFeeUnits)
	if err != nil {
		return Order{}, fmt.Errorf("ledgerclient: parsing network_fee_units: %w", err)
	}
	return Order{
		ID: r.ID, ExternalID: r.ExternalID, CustomerID: r.CustomerID, Tier: r.Tier, State: r.State,
		AmountIn: amountIn, AmountOut: amountOut, FeeUnits: feeUnits, NetworkFeeUnits: networkFeeUnits,
		RecipientAddress: r.RecipientAddress, Version: r.Version,
	}, nil
}

// GetOrder fetches GET /v1/orders/{externalID}.
func (c *Client) GetOrder(ctx context.Context, externalID string) (Order, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, classify(decodeAPIError(status, body))
	}
	var resp orderResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Order{}, fmt.Errorf("ledgerclient: decoding order response for %s: %w", externalID, err)
	}
	return resp.toOrder()
}

type balanceResponse struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Balance     string `json:"balance"`
}

// GetAccountBalance calls GET /v1/accounts/{code}/balance -- implements
// slots.BalanceReader (structurally; C5's internal/slots declares its
// own narrow consumer interface rather than this package exporting one,
// matching this project's established convention).
func (c *Client) GetAccountBalance(ctx context.Context, accountCode string) (money.Amount, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/accounts/"+accountCode+"/balance", "", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, classify(decodeAPIError(status, body))
	}
	var resp balanceResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("ledgerclient: decoding balance response for %s: %w", accountCode, err)
	}
	return money.ParseDecimal(resp.Balance)
}

// AccountType is C1's closed set of account types, mirrored here rather
// than imported (C5 has no import path into the ledger module -- HTTP is
// the only boundary, per ledger/internal/httpapi's own package doc).
type AccountType string

const (
	AccountAsset     AccountType = "ASSET"
	AccountLiability AccountType = "LIABILITY"
)

type postAccountRequest struct {
	Code  string `json:"code"`
	Type  string `json:"type"`
	Asset string `json:"asset"`
}

type accountResponse struct {
	ID    int64  `json:"id"`
	Code  string `json:"code"`
	Type  string `json:"type"`
	Asset string `json:"asset"`
}

// EnsureAccount calls POST /v1/accounts, C1's idempotent-on-code account
// creation endpoint. Safe to call on every dispatch attempt, not just the
// first one for a given customer or slot: a second call with the same
// code is a no-op read of the account as it actually exists (C1.1's own
// accounts.Create semantics, unchanged by the HTTP wrapper C1 added
// around it). idempotencyKey satisfies the blanket "no header, no write"
// rule but is not otherwise load-bearing -- C1's own idempotency here is
// keyed on code, not on this header.
func (c *Client) EnsureAccount(ctx context.Context, code string, accountType AccountType, asset string, idempotencyKey string) error {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/accounts", idempotencyKey, postAccountRequest{
		Code: code, Type: string(accountType), Asset: asset,
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return classify(decodeAPIError(status, body))
	}
	var resp accountResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("ledgerclient: decoding EnsureAccount response for %s: %w", code, err)
	}
	return nil
}
