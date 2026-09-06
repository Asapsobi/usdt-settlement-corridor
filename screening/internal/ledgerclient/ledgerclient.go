// Package ledgerclient is C3's only path to C1: every HTTP call this
// service makes to the ledger core goes through here, built against the
// exact contract in docs/03-build/c3-screening-build-prompts.md's §A --
// mirrors C2's own internal/ledgerclient, never shared with it (these are
// two separate Go modules with no common internal package between them).
package ledgerclient

import (
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

// Client calls one C1 (ledger) instance, authenticating with a single
// bearer token -- service-to-service auth, per C1's own AuthConfig.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client for baseURL (e.g. "http://localhost:8080"),
// authenticating every call with token.
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

// APIError is a structured error response from C1, preserved so a caller
// can inspect Code without string-matching Error()'s text.
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
	_ = json.Unmarshal(body, &envelope) // best-effort; a malformed body just yields empty Code/Message
	return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// do sends one request and returns its status and raw body -- every
// method on Client goes through this, so request construction and error
// reading is written exactly once.
func (c *Client) do(ctx context.Context, method, path string) (status int, respBody []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("ledgerclient: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

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

// orderResp is the subset of C1's order resource this client reads.
type orderResp struct {
	ID            int64     `json:"id"`
	ExternalID    string    `json:"external_id"`
	State         string    `json:"state"`
	SenderAddress *string   `json:"sender_address"`
	UpdatedAt     time.Time `json:"updated_at"`
	Version       int32     `json:"version"`
}

// GetOrder fetches GET /v1/orders/{externalID}.
func (c *Client) GetOrder(ctx context.Context, externalID string) (Order, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/orders/"+externalID)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, decodeAPIError(status, body)
	}
	var o orderResp
	if err := json.Unmarshal(body, &o); err != nil {
		return Order{}, fmt.Errorf("ledgerclient: decoding order response for %s: %w", externalID, err)
	}
	return Order{
		ID: o.ID, ExternalID: o.ExternalID, State: o.State,
		SenderAddress: o.SenderAddress, UpdatedAt: o.UpdatedAt, Version: o.Version,
	}, nil
}

// Order is the subset of C1's order resource this client exposes.
type Order struct {
	ID            int64
	ExternalID    string
	State         string
	SenderAddress *string
	UpdatedAt     time.Time
	Version       int32
}

// ErrSenderAddressNotYetKnown means C1 has the order but sender_address
// is still null on it -- C2 hasn't reported the deposit's sender yet, or
// (should not happen once C1's transition sets it atomically with
// funded) the write is still in flight. Distinguished from every other
// error so internal/discovery's retry loop can tell "ask again later"
// from "something is actually broken".
var ErrSenderAddressNotYetKnown = errors.New("ledgerclient: order has no sender_address yet")

// GetSenderAddress implements provider.SenderAddressLookup (C3.0) against
// a real C1 -- the swap-in for internal/provider.FakeSenderAddressLookup
// now that C1 ships sender_address (see
// docs/03-build/c3-screening-build-prompts.md's "Read this first").
func (c *Client) GetSenderAddress(ctx context.Context, externalID string) (string, error) {
	order, err := c.GetOrder(ctx, externalID)
	if err != nil {
		return "", fmt.Errorf("ledgerclient: sender address for %s: %w", externalID, err)
	}
	if order.SenderAddress == nil || *order.SenderAddress == "" {
		return "", fmt.Errorf("%w: external_id %s", ErrSenderAddressNotYetKnown, externalID)
	}
	return *order.SenderAddress, nil
}

// OrderRef is one row of a PollFundedOrders page: just enough to enqueue
// it (internal/discovery.enqueueIfNew) without carrying the whole Order.
type OrderRef struct {
	OrderID    int64
	ExternalID string
	UpdatedAt  time.Time
}

type listOrdersResp struct {
	Orders     []orderResp `json:"orders"`
	NextCursor string      `json:"next_cursor"`
}

// PollFundedOrders calls GET /v1/orders?state=funded&updated_after=<cursor>
// (C1's own §A addition, C3.3's discovery mechanism). cursor == ""
// requests the first page (no updated_after). The returned newCursor is
// always usable as the next call's cursor -- C1's own contract guarantees
// it's returned even for an empty page (echoing the caller's cursor back),
// so a poller never has to special-case "nothing new" versus "here's
// where you were".
func (c *Client) PollFundedOrders(ctx context.Context, cursor string) (refs []OrderRef, newCursor string, err error) {
	path := "/v1/orders?state=funded&limit=" + strconv.Itoa(DefaultPollLimit)
	if cursor != "" {
		path += "&updated_after=" + url.QueryEscape(cursor)
	}

	status, body, err := c.do(ctx, http.MethodGet, path)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		return nil, "", decodeAPIError(status, body)
	}

	var resp listOrdersResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", fmt.Errorf("ledgerclient: decoding orders list response: %w", err)
	}

	refs = make([]OrderRef, len(resp.Orders))
	for i, o := range resp.Orders {
		refs[i] = OrderRef{OrderID: o.ID, ExternalID: o.ExternalID, UpdatedAt: o.UpdatedAt}
	}
	return refs, resp.NextCursor, nil
}

// DefaultPollLimit caps how many orders PollFundedOrders asks for per
// call. component-map.md's own volume figure (~4 deposits/hour peak) is
// nowhere close to this; it exists so a page is never unbounded, not
// because the system is expected to approach it.
const DefaultPollLimit = 100
