// Package ledgerclient is C4's only path to C1: every HTTP call this
// service makes to the ledger core goes through here, built against the
// exact contract in docs/03-build/c4-energy-broker-build-prompts.md's
// §A -- mirrors C2's and C3's own internal/ledgerclient, never shared
// with either (three separate Go modules, no common internal package
// between them).
//
// Unlike C2 and C3, C4 needs zero additions to C1's already-shipped HTTP
// surface: the only calls this client makes are the already-general-
// purpose GET /v1/orders/{external_id} (C4.4, to resolve an external_id
// into C1's internal order id) and POST /v1/entries (C4.5, cost
// attribution).
package ledgerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"energybroker/internal/provider"
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
	status, body, err := c.do(ctx, http.MethodGet, "/v1/orders/"+externalID, "", nil)
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

// entryLineRequest and postEntryRequest match C1's own decoding structs
// exactly (ledger/internal/httpapi/entries.go's postEntryRequest) --
// confirmed against the real implementation, not guessed from §A's own
// pseudo-JSON, which shows idempotency_key and actor as body fields.
// Neither actually is one: idempotency_key is the Idempotency-Key HTTP
// header only, and actor is derived server-side from the bearer token's
// own LEDGER_API_TOKENS mapping -- C1's decoder rejects unknown fields
// outright (400), so sending either in the body would break every call.
type entryLineRequest struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}

type postEntryRequest struct {
	EntryType  string             `json:"entry_type"`
	OrderID    *int64             `json:"order_id,omitempty"`
	OccurredAt time.Time          `json:"occurred_at"`
	Lines      []entryLineRequest `json:"lines"`
}

type entryResponse struct {
	ID      int64  `json:"id"`
	Outcome string `json:"outcome"` // "created" | "replayed"
}

// ErrUnexpectedHalt and ErrIdempotencyConflictBug are both P1 signals,
// same posture C2.7 and C3.4 take toward their own "this should be
// structurally impossible" error paths -- confirmed against C1's real
// implementation (ledger/internal/httpapi/entries.go and
// internal/journal/post.go) while building this chunk: a bare
// POST /v1/entries is NEVER halt-gated for ANY entry_type -- halt.IsHalted
// is only ever consulted inside orders.Transition, for the 4 pairs
// C1.5's own transitions table marks HaltBlocked, and a standalone entry
// post never goes through that function at all. So ErrUnexpectedHalt
// should be unreachable in practice; if C1 ever returns it here, that is
// a real, surprising change to its own halt semantics, not routine
// backoff-and-retry territory -- retrying blindly would risk masking
// exactly that regression. See ledgerclient_integration_test.go's own
// live test against a halted C1 instance, which proves the current,
// real behavior empirically rather than assuming either way (this
// chunk's own acceptance criterion).
var (
	ErrUnexpectedHalt         = errors.New("ledgerclient: P1 ALERT -- unexpected system_halted reporting an energy cost entry; POST /v1/entries is not halt-gated for any entry_type")
	ErrIdempotencyConflictBug = errors.New("ledgerclient: P1 ALERT -- idempotency_conflict reporting an energy cost entry; this should be structurally impossible if delegation ids are unique")
)

// ReportEnergyCost posts entry E4 (docs/03-build/c1-ledger-build-prompts.md
// §B): the TRX cost of one specific delegation, attributed to orderID.
// Idempotency-Key is "broker:energy_cost:<delegation.ID>", per invariant
// 4 -- a replay of the same delegation's report (a C4 restart between
// Delegate/Redelegate succeeding and this call landing) hits C1's own
// idempotency path and returns success without a double-charge; this
// function makes no attempt to detect that case itself; C1 already does.
//
// OccurredAt is derived from delegation.RequestedAt, deliberately never
// time.Now() at call time: C1's own idempotency check hashes occurred_at
// as part of the payload (confirmed against its real implementation --
// ledger/internal/journal/hash.go), so two calls reporting the exact
// same delegation must produce a byte-for-byte identical request or C1
// correctly (and loudly) treats them as a genuine conflict, not a safe
// replay -- discovered by this package's own live-C1 replay test.
// delegation.RequestedAt is fixed for the life of one Delegation value,
// so this makes replay-safety automatic rather than relying on every
// caller to remember to reuse the same clock reading.
func (c *Client) ReportEnergyCost(ctx context.Context, delegation provider.Delegation, orderID int64) error {
	idempotencyKey := "broker:energy_cost:" + delegation.ID

	reqBody := postEntryRequest{
		EntryType:  "energy_cost",
		OrderID:    &orderID,
		OccurredAt: delegation.RequestedAt,
		Lines: []entryLineRequest{
			{AccountCode: "expense:energy", Asset: "TRX", Amount: delegation.CostTRX.Format()},
			{AccountCode: "asset:tron:energy_wallet", Asset: "TRX", Amount: (-delegation.CostTRX).Format()},
		},
	}

	status, body, err := c.do(ctx, http.MethodPost, "/v1/entries", idempotencyKey, reqBody)
	if err != nil {
		return fmt.Errorf("ledgerclient: report_energy_cost for delegation %s: %w", delegation.ID, err)
	}
	if status == http.StatusOK || status == http.StatusCreated {
		return nil
	}

	apiErr := decodeAPIError(status, body)
	switch apiErr.Code {
	case "system_halted":
		slog.Error("ledgerclient: P1 ALERT -- unexpected system_halted reporting an energy cost entry",
			"delegation_id", delegation.ID, "order_id", orderID)
		return fmt.Errorf("%w: %w", ErrUnexpectedHalt, apiErr)
	case "idempotency_conflict":
		slog.Error("ledgerclient: P1 ALERT -- idempotency_conflict reporting an energy cost entry",
			"delegation_id", delegation.ID, "order_id", orderID)
		return fmt.Errorf("%w: %w", ErrIdempotencyConflictBug, apiErr)
	default:
		return apiErr
	}
}
