// Package ledgerclient is C4's only path to C1: every HTTP call this
// service makes to the ledger core goes through here, built against the
// exact contract in docs/03-build/c4-energy-broker-build-prompts.md's
// §A -- mirrors C2's and C3's own internal/ledgerclient, never shared
// with either (three separate Go modules, no common internal package
// between them).
//
// Unlike C2 and C3, C4 needs zero additions to C1's already-shipped HTTP
// surface (§A's own "Read this second" -- er, its own §A intro): the
// only calls this client makes are the already-general-purpose
// GET /v1/orders/{external_id} (this chunk, C4.4, to resolve an
// external_id into C1's internal order id) and POST /v1/entries
// (C4.5, cost attribution -- not yet built).
package ledgerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// Order is the subset of C1's order resource this client reads --
// C4.4 only ever needs the internal id, but ExternalID is kept too so a
// caller can log/verify it round-tripped correctly.
type Order struct {
	ID         int64  `json:"id"`
	ExternalID string `json:"external_id"`
}

// GetOrder fetches GET /v1/orders/{externalID}, resolving it to C1's own
// internal order id -- §A's "Resolves external_id -> C1's internal order
// id" requirement.
func (c *Client) GetOrder(ctx context.Context, externalID string) (Order, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/orders/"+externalID)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, decodeAPIError(status, body)
	}
	var o Order
	if err := json.Unmarshal(body, &o); err != nil {
		return Order{}, fmt.Errorf("ledgerclient: decoding order response for %s: %w", externalID, err)
	}
	return o, nil
}
