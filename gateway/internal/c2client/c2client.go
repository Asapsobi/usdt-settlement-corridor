// Package c2client is C6's only path to C2: every HTTP call this
// service makes to the deposit watcher goes through here, built against
// depositwatcher/internal/httpapi's REAL shipped shape (C2.9), the
// contract c6-api-gateway-build-prompts.md's own "Read this second"
// names explicitly.
package c2client

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
)

// Client calls one C2 (deposit watcher) instance, authenticating with a
// single bearer token.
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

// APIError is a structured error response from C2.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("c2client: C2 returned %d %s: %s", e.Status, e.Code, e.Message)
}

func decodeAPIError(status int, body []byte) *APIError {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(body, &envelope)
	return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// ErrAddressNotFound means C2 has no watched address for the given
// order id -- used by C6.4's own reconciliation loop to tell "never
// assigned" from a real, unexpected error.
var ErrAddressNotFound = errors.New("c2client: no address assigned for that order")

func classify(apiErr *APIError) error {
	if apiErr.Status == http.StatusNotFound {
		return fmt.Errorf("%w: %w", ErrAddressNotFound, apiErr)
	}
	return apiErr
}

func (c *Client) do(ctx context.Context, method, path, idempotencyKey string, body any) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("c2client: encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("c2client: building request: %w", err)
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
		return 0, nil, fmt.Errorf("c2client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("c2client: reading response for %s %s: %w", method, path, err)
	}
	return resp.StatusCode, respBody, nil
}

// WatchedAddress mirrors C2's own address response shape.
type WatchedAddress struct {
	Address        string     `json:"address"`
	OrderID        int64      `json:"order_id"`
	ExternalID     string     `json:"external_id"`
	CustomerID     string     `json:"customer_id"`
	Status         string     `json:"status"`
	QuotedAt       time.Time  `json:"quoted_at"`
	QuoteExpiresAt time.Time  `json:"quote_expires_at"`
	AssignedAt     time.Time  `json:"assigned_at"`
	RetiredAt      *time.Time `json:"retired_at,omitempty"`
	RetiredReason  *string    `json:"retired_reason,omitempty"`
}

type postAddressRequest struct {
	OrderID        int64     `json:"order_id"`
	ExternalID     string    `json:"external_id"`
	CustomerID     string    `json:"customer_id"`
	QuotedAt       time.Time `json:"quoted_at"`
	QuoteExpiresAt time.Time `json:"quote_expires_at"`
}

// AssignAddress calls POST /v1/addresses -- idempotent on orderID
// (C2's own contract, "Read this second"): calling this twice for the
// same order_id returns the same address both times, 200 either way.
func (c *Client) AssignAddress(ctx context.Context, orderID int64, externalID, customerID string, quotedAt, quoteExpiresAt time.Time, idempotencyKey string) (WatchedAddress, error) {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/addresses", idempotencyKey, postAddressRequest{
		OrderID: orderID, ExternalID: externalID, CustomerID: customerID,
		QuotedAt: quotedAt, QuoteExpiresAt: quoteExpiresAt,
	})
	if err != nil {
		return WatchedAddress{}, err
	}
	if status != http.StatusOK {
		return WatchedAddress{}, classify(decodeAPIError(status, body))
	}
	var addr WatchedAddress
	if err := json.Unmarshal(body, &addr); err != nil {
		return WatchedAddress{}, fmt.Errorf("c2client: decoding AssignAddress response: %w", err)
	}
	return addr, nil
}

// GetAddress calls GET /v1/addresses/{orderID} -- C6.4's own
// reconciliation loop uses this to check whether an address was ever
// actually assigned before retrying.
func (c *Client) GetAddress(ctx context.Context, orderID int64) (WatchedAddress, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/addresses/"+url.PathEscape(strconv.FormatInt(orderID, 10)), "", nil)
	if err != nil {
		return WatchedAddress{}, err
	}
	if status != http.StatusOK {
		return WatchedAddress{}, classify(decodeAPIError(status, body))
	}
	var addr WatchedAddress
	if err := json.Unmarshal(body, &addr); err != nil {
		return WatchedAddress{}, fmt.Errorf("c2client: decoding GetAddress response: %w", err)
	}
	return addr, nil
}

// RetireAddress calls POST /v1/addresses/{orderID}/retire.
func (c *Client) RetireAddress(ctx context.Context, orderID int64, reason, idempotencyKey string) error {
	status, body, err := c.do(ctx, http.MethodPost, "/v1/addresses/"+url.PathEscape(strconv.FormatInt(orderID, 10))+"/retire", idempotencyKey,
		map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return classify(decodeAPIError(status, body))
	}
	return nil
}
