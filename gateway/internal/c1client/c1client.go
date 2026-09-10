// Package c1client is C6's only path to C1: every HTTP call this
// service makes to the ledger core goes through here, built against
// ledger/internal/httpapi's REAL shipped request/response shapes
// (ledger/docs/openapi.yaml), the same "the code, not the spec, is the
// source of truth" discipline every prior component here applies to C1.
package c1client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gateway/internal/money"
)

// Client calls one C1 (ledger) instance, authenticating with a single
// bearer token -- service-to-service, per C1's own AuthConfig; not the
// customer-facing auth C6.1 owns.
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
	return fmt.Sprintf("c1client: C1 returned %d %s: %s", e.Status, e.Code, e.Message)
}

func decodeAPIError(status int, body []byte) *APIError {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(body, &envelope)
	return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// Sentinel errors matched against APIError.Code -- C1's own real,
// stable error codes.
var (
	ErrOrderNotFound       = errors.New("c1client: no such order")
	ErrIllegalTransition   = errors.New("c1client: no such transition is legal from the order's current state")
	ErrVersionConflict     = errors.New("c1client: expected_version did not match the order's current version")
	ErrSystemHalted        = errors.New("c1client: the ledger is halted")
	ErrIdempotencyConflict = errors.New("c1client: a different request already used that idempotency key")
)

var codeToSentinel = map[string]error{
	"order_not_found":      ErrOrderNotFound,
	"illegal_transition":   ErrIllegalTransition,
	"version_conflict":     ErrVersionConflict,
	"system_halted":        ErrSystemHalted,
	"idempotency_conflict": ErrIdempotencyConflict,
}

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
			return 0, nil, fmt.Errorf("c1client: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("c1client: building request: %w", err)
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
		return 0, nil, fmt.Errorf("c1client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("c1client: reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

// HaltState mirrors C1's own GET/POST /v1/system/halt response shape.
type HaltState struct {
	Halted bool   `json:"halted"`
	Reason string `json:"reason"`
}

// GetHaltState calls GET /v1/system/halt -- invariant 6's own check,
// made before issuing a quote and again before creating an order.
func (c *Client) GetHaltState(ctx context.Context) (HaltState, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/system/halt", "", nil)
	if err != nil {
		return HaltState{}, err
	}
	if status != http.StatusOK {
		return HaltState{}, classify(decodeAPIError(status, body))
	}
	var hs HaltState
	if err := json.Unmarshal(body, &hs); err != nil {
		return HaltState{}, fmt.Errorf("c1client: decoding halt state: %w", err)
	}
	return hs, nil
}

// Order mirrors C1's own Order response shape (ledger/docs/openapi.yaml).
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
	QuotedAt         time.Time
	QuoteExpiresAt   time.Time
	Version          int32
}

type orderResponse struct {
	ID               int64     `json:"id"`
	ExternalID       string    `json:"external_id"`
	CustomerID       string    `json:"customer_id"`
	Tier             string    `json:"tier"`
	State            string    `json:"state"`
	AmountIn         string    `json:"amount_in"`
	AmountOut        string    `json:"amount_out"`
	FeeUnits         string    `json:"fee_units"`
	NetworkFeeUnits  string    `json:"network_fee_units"`
	RecipientAddress string    `json:"recipient_address"`
	QuotedAt         time.Time `json:"quoted_at"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
	Version          int32     `json:"version"`
}

func (r orderResponse) toOrder() (Order, error) {
	amountIn, err := money.ParseDecimal(r.AmountIn)
	if err != nil {
		return Order{}, fmt.Errorf("c1client: parsing amount_in: %w", err)
	}
	amountOut, err := money.ParseDecimal(r.AmountOut)
	if err != nil {
		return Order{}, fmt.Errorf("c1client: parsing amount_out: %w", err)
	}
	feeUnits, err := money.ParseDecimal(r.FeeUnits)
	if err != nil {
		return Order{}, fmt.Errorf("c1client: parsing fee_units: %w", err)
	}
	networkFeeUnits, err := money.ParseDecimal(r.NetworkFeeUnits)
	if err != nil {
		return Order{}, fmt.Errorf("c1client: parsing network_fee_units: %w", err)
	}
	return Order{
		ID: r.ID, ExternalID: r.ExternalID, CustomerID: r.CustomerID, Tier: r.Tier, State: r.State,
		AmountIn: amountIn, AmountOut: amountOut, FeeUnits: feeUnits, NetworkFeeUnits: networkFeeUnits,
		RecipientAddress: r.RecipientAddress, QuotedAt: r.QuotedAt, QuoteExpiresAt: r.QuoteExpiresAt,
		Version: r.Version,
	}, nil
}

// PostOrderRequest mirrors C1's own PostOrderRequest body exactly.
type PostOrderRequest struct {
	ExternalID       string
	CustomerID       string
	Tier             string
	AmountIn         money.Amount
	AmountOut        money.Amount
	FeeUnits         money.Amount
	NetworkFeeUnits  money.Amount
	RecipientAddress string
	QuotedAt         time.Time
	QuoteExpiresAt   time.Time
}

type postOrderBody struct {
	ExternalID       string    `json:"external_id"`
	CustomerID       string    `json:"customer_id"`
	Tier             string    `json:"tier"`
	AmountIn         string    `json:"amount_in"`
	AmountOut        string    `json:"amount_out"`
	FeeUnits         string    `json:"fee_units"`
	NetworkFeeUnits  string    `json:"network_fee_units"`
	RecipientAddress string    `json:"recipient_address"`
	QuotedAt         time.Time `json:"quoted_at"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
}

// CreateOrder calls POST /v1/orders. idempotencyKey is required by C1's
// own blanket "no header, no write" rule.
func (c *Client) CreateOrder(ctx context.Context, req PostOrderRequest, idempotencyKey string) (Order, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/orders", idempotencyKey, postOrderBody{
		ExternalID: req.ExternalID, CustomerID: req.CustomerID, Tier: req.Tier,
		AmountIn: req.AmountIn.Format(), AmountOut: req.AmountOut.Format(),
		FeeUnits: req.FeeUnits.Format(), NetworkFeeUnits: req.NetworkFeeUnits.Format(),
		RecipientAddress: req.RecipientAddress, QuotedAt: req.QuotedAt, QuoteExpiresAt: req.QuoteExpiresAt,
	})
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusCreated {
		return Order{}, classify(decodeAPIError(status, body))
	}
	var resp orderResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Order{}, fmt.Errorf("c1client: decoding CreateOrder response: %w", err)
	}
	return resp.toOrder()
}

// GetOrder calls GET /v1/orders/{externalID}.
func (c *Client) GetOrder(ctx context.Context, externalID string) (Order, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/orders/"+url.PathEscape(externalID), "", nil)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, classify(decodeAPIError(status, body))
	}
	var resp orderResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Order{}, fmt.Errorf("c1client: decoding GetOrder response: %w", err)
	}
	return resp.toOrder()
}

// DefaultPollLimit is how many orders ListOrdersByState asks C1 for per
// call -- matches every sibling component's own discovery poll size.
const DefaultPollLimit = 100

// ListOrdersByState calls GET /v1/orders?state=<state>&updated_after=<cursor>,
// C1's own discovery primitive (used here by C6.6's webhook trigger).
// cursor == "" requests the first page.
func (c *Client) ListOrdersByState(ctx context.Context, state, cursor string) (orders []Order, newCursor string, err error) {
	path := "/v1/orders?state=" + url.QueryEscape(state) + "&limit=" + strconv.Itoa(DefaultPollLimit)
	if cursor != "" {
		path += "&updated_after=" + url.QueryEscape(cursor)
	}
	status, body, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		return nil, "", classify(decodeAPIError(status, body))
	}

	var resp struct {
		Orders     []orderResponse `json:"orders"`
		NextCursor string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", fmt.Errorf("c1client: decoding orders list response: %w", err)
	}
	orders = make([]Order, len(resp.Orders))
	for i, o := range resp.Orders {
		order, err := o.toOrder()
		if err != nil {
			return nil, "", err
		}
		orders[i] = order
	}
	return orders, resp.NextCursor, nil
}
