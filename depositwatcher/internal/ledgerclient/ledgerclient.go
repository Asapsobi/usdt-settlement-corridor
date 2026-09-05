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

func decodeAPIError(status int, body []byte) *APIError {
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(body, &envelope) // best-effort; a malformed body just yields empty Code/Message
	return &APIError{Status: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// do sends one request and returns its status and raw body -- every
// method on Client goes through this, so request construction and error
// reading is written exactly once.
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

// GetOrder fetches GET /v1/orders/{externalID} -- used both to learn an
// order's current version before a write (ReportDepositFinal) and by
// callers that just need to read state.
func (c *Client) GetOrder(ctx context.Context, externalID string) (Order, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, decodeAPIError(status, body)
	}
	var order Order
	if err := json.Unmarshal(body, &order); err != nil {
		return Order{}, fmt.Errorf("ledgerclient: decoding order response for %s: %w", externalID, err)
	}
	return order, nil
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
	// A DIFFERENT idempotency key from originalEntryKey -- that key
	// identifies the deposit_final entry being reversed; this one
	// identifies THIS report of that reversal, per §A's own two-key
	// convention ("watcher:reorg_report:<tx_hash>:<log_index>" against
	// "watcher:deposit_final:<tx_hash>:<log_index>"). Derived from
	// originalEntryKey's own suffix rather than threading tx_hash/log_index
	// through this method separately, since DepositFinalIdempotencyKey
	// guarantees that suffix's shape.
	idempotencyKey := "watcher:reorg_report:" + strings.TrimPrefix(originalEntryKey, finality.DepositFinalIdempotencyKeyPrefix)

	status, body, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/reorg", externalID), idempotencyKey,
		map[string]string{"original_entry_key": originalEntryKey})
	if err != nil {
		return fmt.Errorf("ledgerclient: reorg report for %s: %w", externalID, err)
	}

	if status == http.StatusConflict {
		if apiErr := decodeAPIError(status, body); apiErr.Code == "already_reversed" {
			slog.Info("ledgerclient: reorg already reported (idempotent replay, C1's already_reversed path)",
				"external_id", externalID, "original_entry_key", originalEntryKey)
			return nil
		}
	}
	if status != http.StatusOK {
		return decodeAPIError(status, body)
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

// ReportDepositFinal posts the one write C2 ever makes to credit a
// deposit -- POST /v1/orders/{external_id}/transitions to funded, with
// the E1 entry inline, exactly the shape §A specifies. The
// Idempotency-Key header is set to the same key as the entry's own
// idempotency_key -- belt and suspenders, matching C1.8's rule that
// every write handler requires the header regardless of whether the
// operation underneath has its own idempotency semantics.
//
// expected_version is fetched fresh via GetOrder rather than threaded in
// by candidate: the version at the moment this method actually calls C1
// is the only one that matters, and a value carried on candidate since
// whenever it finalized could already be stale.
//
// Error handling, per this chunk's own build spec:
//   - 409 version_conflict: a legitimate race (something else touched the
//     order between the GetOrder above and this POST) -- refetched and
//     retried exactly once with the fresh version, not treated as a
//     failure at all unless the retry ALSO conflicts.
//   - 423 system_halted: C1 is deliberately refusing money-moving writes
//     right now, not a problem with this specific report. Returned as a
//     plain (retryable) error -- finality.Tracker's own tick cadence
//     (C2.5) already is the "retry on an interval" mechanism this needs,
//     so no separate backoff timer is built here.
//   - 409 illegal_transition: the order left quoted before this call
//     reached C1 (e.g. it expired first). Wrapped in both
//     finality.ErrPermanentFailure and finality.ErrOrphanedDeposit --
//     retrying this exact call can never succeed, and finality.Tracker
//     routes the more specific sentinel to HandleUnreportable (C2.8),
//     which records it for manual reconciliation rather than just
//     logging and dropping it.
//   - 409 idempotency_conflict: structurally impossible if this method's
//     own key construction is correct (the same tx_hash:log_index always
//     produces the same payload). Also wrapped in
//     finality.ErrPermanentFailure and logged as a P1 alert -- retrying
//     an actual bug in a loop is strictly worse than surfacing it once,
//     loudly.
func (c *Client) ReportDepositFinal(ctx context.Context, candidate finality.Candidate) error {
	return c.reportDepositFinal(ctx, candidate, true)
}

func (c *Client) reportDepositFinal(ctx context.Context, candidate finality.Candidate, allowVersionRetry bool) error {
	order, err := c.GetOrder(ctx, candidate.ExternalID)
	if err != nil {
		return fmt.Errorf("ledgerclient: deposit_final for %s: fetching current version: %w", candidate.ExternalID, err)
	}

	idempotencyKey := finality.DepositFinalIdempotencyKey(candidate.TxHash, candidate.LogIndex)
	occurredAt := candidate.BlockTime.UTC().Format(time.RFC3339)
	depositAccount := fmt.Sprintf("asset:bsc:deposit:%d", candidate.OrderID)
	customerAccount := "liability:customer:" + candidate.CustomerID

	reqBody := map[string]any{
		"to_state":         "funded",
		"expected_version": order.Version,
		"reason":           "bep20_deposit_final",
		"occurred_at":      occurredAt,
		"entry": map[string]any{
			"entry_type":  "deposit_final",
			"occurred_at": occurredAt,
			"lines": []map[string]any{
				{"account_code": depositAccount, "asset": "USDT_BEP20", "amount": candidate.Amount.Format()},
				{"account_code": customerAccount, "asset": "USDT_BEP20", "amount": (-candidate.Amount).Format()},
			},
		},
	}

	status, body, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/v1/orders/%s/transitions", candidate.ExternalID), idempotencyKey, reqBody)
	if err != nil {
		return fmt.Errorf("ledgerclient: deposit_final for %s: %w", candidate.ExternalID, err)
	}
	if status == http.StatusOK {
		return nil
	}

	apiErr := decodeAPIError(status, body)
	switch apiErr.Code {
	case "version_conflict":
		if !allowVersionRetry {
			return fmt.Errorf("ledgerclient: deposit_final for %s: version_conflict persisted after one retry: %w",
				candidate.ExternalID, apiErr)
		}
		slog.Warn("ledgerclient: deposit_final hit version_conflict, retrying once with a fresh version",
			"external_id", candidate.ExternalID)
		return c.reportDepositFinal(ctx, candidate, false)
	case "system_halted":
		slog.Info("ledgerclient: deposit_final deferred -- the ledger is halted, will retry",
			"external_id", candidate.ExternalID)
		return apiErr
	case "illegal_transition":
		return fmt.Errorf("ledgerclient: deposit_final for %s: order no longer in quoted -- hand off to C2.8: %w: %w: %w",
			candidate.ExternalID, finality.ErrPermanentFailure, finality.ErrOrphanedDeposit, apiErr)
	case "idempotency_conflict":
		slog.Error("ledgerclient: P1 BUG ALERT -- deposit_final idempotency_conflict: this service's own key construction produced the same key for two different payloads",
			"external_id", candidate.ExternalID, "idempotency_key", idempotencyKey)
		return fmt.Errorf("%w: %w", finality.ErrPermanentFailure, apiErr)
	default:
		return apiErr
	}
}
