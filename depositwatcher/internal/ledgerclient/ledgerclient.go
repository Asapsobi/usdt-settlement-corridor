// Package ledgerclient is C2's only path to C1: every HTTP call this
// service makes to the ledger core goes through here, built against the
// exact contract in the C2 build spec's §A -- never constructed ad hoc
// anywhere else in this codebase.
package ledgerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"depositwatcher/internal/finality"
)

// Client calls one C1 (ledger) instance, authenticating with a single
// bearer token -- service-to-service auth, per C1's own AuthConfig; there
// is no per-customer scoping here.
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

// Order is the subset of C1's order resource this client actually reads.
type Order struct {
	ExternalID string `json:"external_id"`
	State      string `json:"state"`
	Version    int32  `json:"version"`
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

// ReportReorg posts to /v1/orders/{externalID}/reorg -- see the C2 build
// spec's §A "Reporting a reorg": no actor field (the bearer token
// supplies it, same as every other C1 write), no occurred_at, no
// scenario flag. C1 alone decides, from the order's own current state,
// whether this is scenario A (order returns to quoted, routine, no loss)
// or scenario B (order state untouched, a real loss booked, the ledger
// now halted) -- this method only reports what happened on-chain and
// relays which scenario C1 says applied, in its own logging, at the
// right severity for each.
//
// originalEntryKey must be exactly the Idempotency-Key C2.7 used when it
// first reported this deposit final -- see DepositFinalIdempotencyKey,
// the single place that convention is defined, so this call and that one
// can never drift apart. A second call with the same originalEntryKey
// (a genuine retry, or the surrounding process restarting mid-report)
// hits C1's own already_reversed path (409): this method treats that as
// success, not an error to retry or alert on -- exactly the point of the
// idempotency key.
func (c *Client) ReportReorg(ctx context.Context, externalID, originalEntryKey string) error {
	reqBody, err := json.Marshal(map[string]string{"original_entry_key": originalEntryKey})
	if err != nil {
		return fmt.Errorf("ledgerclient: encoding reorg report body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/orders/%s/reorg", c.baseURL, externalID), bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("ledgerclient: building reorg report request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	// A DIFFERENT idempotency key from originalEntryKey -- that key
	// identifies the deposit_final entry being reversed; this one
	// identifies THIS report of that reversal, per §A's own two-key
	// convention ("watcher:reorg_report:<tx_hash>:<log_index>" against
	// "watcher:deposit_final:<tx_hash>:<log_index>"). Derived from
	// originalEntryKey's own suffix rather than threading tx_hash/log_index
	// through this method separately, since DepositFinalIdempotencyKey
	// guarantees that suffix's shape.
	req.Header.Set("Idempotency-Key", "watcher:reorg_report:"+strings.TrimPrefix(originalEntryKey, finality.DepositFinalIdempotencyKeyPrefix))

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ledgerclient: reorg report request for %s: %w", externalID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("ledgerclient: reading reorg report response for %s: %w", externalID, err)
	}

	if resp.StatusCode == http.StatusConflict {
		var envelope apiErrorEnvelope
		if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code == "already_reversed" {
			slog.Info("ledgerclient: reorg already reported (idempotent replay, C1's already_reversed path)",
				"external_id", externalID, "original_entry_key", originalEntryKey)
			return nil
		}
	}

	if resp.StatusCode != http.StatusOK {
		var envelope apiErrorEnvelope
		_ = json.Unmarshal(body, &envelope)
		return &APIError{Status: resp.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}

	var order Order
	if err := json.Unmarshal(body, &order); err != nil {
		return fmt.Errorf("ledgerclient: decoding reorg report response for %s: %w", externalID, err)
	}

	if order.State == "quoted" {
		slog.Info("ledgerclient: post-final reorg reported -- scenario A (routine, order returned to quoted, no loss)",
			"external_id", externalID, "original_entry_key", originalEntryKey)
	} else {
		slog.Error("ledgerclient: post-final reorg reported -- SCENARIO B: a real loss was booked and the ledger is now halted",
			"external_id", externalID, "original_entry_key", originalEntryKey, "order_state", order.State)
	}
	return nil
}
