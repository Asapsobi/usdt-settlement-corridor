// Package ledgerclient is C3's only path to C1: every HTTP call this
// service makes to the ledger core goes through here, built against the
// exact contract in docs/03-build/c3-screening-build-prompts.md's §A --
// mirrors C2's own internal/ledgerclient, never shared with it (these are
// two separate Go modules with no common internal package between them).
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
	"net/url"
	"strconv"
	"strings"
	"time"

	"screening/internal/verdict"
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
// reading is written exactly once. body is marshaled as JSON when
// non-nil; idempotencyKey is sent as the Idempotency-Key header when
// non-empty (C1.8 requires it on every write).
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
	status, body, err := c.do(ctx, http.MethodGet, "/v1/orders/"+externalID, "", nil)
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

// OrderRef is one row of an orders list page: just enough to enqueue it
// (internal/discovery.enqueueIfNew) or re-screen its sender
// (internal/rescreen) without carrying the whole Order. SenderAddress is
// nil if C1 hasn't recorded one yet (never the case for `screened` or
// `dispatching` orders, since sender_address is set atomically with
// `funded`, which both states are always downstream of).
type OrderRef struct {
	OrderID       int64
	ExternalID    string
	UpdatedAt     time.Time
	SenderAddress *string
}

type listOrdersResp struct {
	Orders     []orderResp `json:"orders"`
	NextCursor string      `json:"next_cursor"`
}

// ListOrdersByState calls GET /v1/orders?state=<state>&updated_after=<cursor>
// (C1's own §A addition). cursor == "" requests the first page (no
// updated_after). The returned newCursor is always usable as the next
// call's cursor -- C1's own contract guarantees it's returned even for
// an empty page (echoing the caller's cursor back), so a poller never
// has to special-case "nothing new" versus "here's where you were".
func (c *Client) ListOrdersByState(ctx context.Context, state, cursor string) (refs []OrderRef, newCursor string, err error) {
	path := "/v1/orders?state=" + url.QueryEscape(state) + "&limit=" + strconv.Itoa(DefaultPollLimit)
	if cursor != "" {
		path += "&updated_after=" + url.QueryEscape(cursor)
	}

	status, body, err := c.do(ctx, http.MethodGet, path, "", nil)
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
		refs[i] = OrderRef{OrderID: o.ID, ExternalID: o.ExternalID, UpdatedAt: o.UpdatedAt, SenderAddress: o.SenderAddress}
	}
	return refs, resp.NextCursor, nil
}

// PollFundedOrders is ListOrdersByState("funded", cursor) -- C3.3's own
// discovery mechanism, kept as its own named method since "polling for
// newly funded orders" is a distinct enough concept from the generic
// list-by-state call C3.7's re-screen job also needs.
func (c *Client) PollFundedOrders(ctx context.Context, cursor string) ([]OrderRef, string, error) {
	return c.ListOrdersByState(ctx, "funded", cursor)
}

// DefaultPollLimit caps how many orders PollFundedOrders asks for per
// call. component-map.md's own volume figure (~4 deposits/hour peak) is
// nowhere close to this; it exists so a page is never unbounded, not
// because the system is expected to approach it.
const DefaultPollLimit = 100

// ErrIllegalTransition means the order left `funded` before this report
// reached C1. Two distinct real causes land here, both handled the same
// safe way (never retried; the pipeline marks the queue row DONE and
// moves on -- neither is an error in C3):
//   - A legitimate race, e.g. a customer cancel landed first (the C3.4
//     build spec's own named case).
//   - A replay of a report that already succeeded: since funded->screened
//     and funded->held post no journal entry, C1 has no idempotent replay
//     path for them (see ReportVerdict's own doc comment) -- a repeated
//     call after the order already moved on to screened/held surfaces
//     identically to the race case above, and is absorbed the same way.
var ErrIllegalTransition = errors.New("ledgerclient: order left funded before the verdict could be reported")

// ErrUnexpectedHalt means C1 returned system_halted for a verdict
// report. funded->screened and funded->held are NOT halt-blocked per
// C1.5's transition table, so this should never happen -- unlike C2.7's
// own deposit report, where system_halted IS expected for some calls and
// handled with routine backoff, this is surfaced as a distinct sentinel
// so the caller can alert loudly (a P1 signal) instead of silently
// treating it as routine, matching the C3.4 build spec's own explicit
// instruction not to copy C2.7's handler verbatim here.
var ErrUnexpectedHalt = errors.New("ledgerclient: unexpected system_halted reporting a verdict (funded->screened/held is not halt-blocked)")

// ReportVerdict posts C3's automatic pass/hold decision to C1: Pass
// transitions funded->screened, Hold transitions funded->held. Neither
// requires a journal entry (C1.5's table marks both RequiresEntry:
// false) -- this never sends one.
//
// No actor field is sent: C1.8's transitions endpoint has none in its
// request DTO and rejects unknown fields outright (DisallowUnknownFields)
// -- actor is derived entirely from this client's own bearer token
// identity on C1's side (confirmed against C1's actual handler, per
// the C3 build spec's own instruction to verify this before C3.6 ships
// rather than trust the spec's assumed reading). Deploying this service
// with a token C1 maps to actor "screening-svc" is an ops/config
// concern, not something this call can express in its request body.
//
// The Idempotency-Key follows invariant 3's exact format,
// "screening:<verdict>:<order_id>:<screening_result_id>" -- tying the
// key to WHICH screening result produced this decision
// (decision.ScreeningResultID), not just which order, so a genuine
// re-screen after a cache invalidation (a new screening_results row, a
// new id) reports as a distinct event rather than colliding with a
// stale prior report for the same order.
//
// That header is sent because C1.8 requires one on every write, NOT
// because C1 actually deduplicates on it here: a real C1 was found,
// while building this chunk, to only replay-detect a transition that
// posts a journal entry (orders.Transition's own replayIfAlreadyPosted,
// keyed by the entry's own idempotency key) -- funded->screened and
// funded->held are both RequiresEntry: false, so there is nothing for
// C1 to recognize a repeat by. A second call after the first already
// succeeded gets illegal_transition (ErrIllegalTransition), not a
// replayed 200 -- see ErrIllegalTransition's own doc comment for why
// that is still safe in practice. This is a real gap between invariant
// 3's stated guarantee and what C1 currently does for entry-less
// transitions, worth raising with whoever owns C1, not something this
// client can paper over on its own.
//
// reason is decision.ReasonCode verbatim (e.g. "screening_hold_flagged"),
// not the "screening_hold:<reason_code>" template §A's worked example
// shows: that template predates C3.2 fixing the actual reason code
// strings, which are already self-namespaced with a "screening_hold_"
// prefix -- concatenating both would just double the string for no
// added information.
func (c *Client) ReportVerdict(ctx context.Context, externalID string, decision verdict.Decision) error {
	return c.reportVerdict(ctx, externalID, decision, true)
}

func (c *Client) reportVerdict(ctx context.Context, externalID string, decision verdict.Decision, allowVersionRetry bool) error {
	order, err := c.GetOrder(ctx, externalID)
	if err != nil {
		return fmt.Errorf("ledgerclient: report_verdict for %s: fetching current version: %w", externalID, err)
	}

	var toState string
	switch decision.Classification {
	case verdict.Pass:
		toState = "screened"
	case verdict.Hold:
		toState = "held"
	default:
		return fmt.Errorf("ledgerclient: report_verdict for %s: unrecognized classification %v", externalID, decision.Classification)
	}

	idempotencyKey := fmt.Sprintf("screening:%s:%d:%d", decision.Classification, order.ID, decision.ScreeningResultID)
	reqBody := map[string]any{
		"to_state":         toState,
		"expected_version": order.Version,
		"reason":           decision.ReasonCode,
		"occurred_at":      time.Now().UTC().Format(time.RFC3339),
	}

	status, body, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", externalID), idempotencyKey, reqBody)
	if err != nil {
		return fmt.Errorf("ledgerclient: report_verdict for %s: %w", externalID, err)
	}
	if status == http.StatusOK {
		return nil
	}

	apiErr := decodeAPIError(status, body)
	switch apiErr.Code {
	case "version_conflict":
		if !allowVersionRetry {
			return fmt.Errorf("ledgerclient: report_verdict for %s: version_conflict persisted after one retry: %w",
				externalID, apiErr)
		}
		slog.Warn("ledgerclient: report_verdict hit version_conflict, retrying once with a fresh version",
			"external_id", externalID)
		return c.reportVerdict(ctx, externalID, decision, false)
	case "illegal_transition":
		return fmt.Errorf("%w: %s: %w", ErrIllegalTransition, externalID, apiErr)
	case "system_halted":
		slog.Error("ledgerclient: P1 ALERT -- unexpected system_halted reporting a verdict; funded->screened/held is not halt-blocked per C1.5's table",
			"external_id", externalID)
		return fmt.Errorf("%w: %w", ErrUnexpectedHalt, apiErr)
	default:
		return apiErr
	}
}

// ReleaseHold posts held->screened for a hold a human reviewer has
// decided to release (C3.6). Idempotency-Key is
// "screening:release:<order_id>:<hold_id>", §A's own format. Like
// ReportVerdict, no actor field is sent -- the reviewer's identity is
// recorded in internal/holds' own resolved_by column, C1's side never
// sees it (see ReportVerdict's own doc comment for why: no actor field
// exists in C1's real request DTO, confirmed while building C3.4).
//
// held->screened is RequiresEntry: false and HaltBlocked: false per
// C1.5's table -- same posture as ReportVerdict's funded->screened, so
// the same three error cases apply the same way: version_conflict
// retried once, illegal_transition wrapped in ErrIllegalTransition
// (the hold's own order left held some other way first -- also covers
// a replay of an already-succeeded release, for the identical
// entry-less-transition reason ReportVerdict's doc comment explains),
// and system_halted surfaced as ErrUnexpectedHalt (a P1 signal, since
// this transition should never actually be halt-blocked).
func (c *Client) ReleaseHold(ctx context.Context, externalID string, orderID, holdID int64) error {
	return c.releaseHold(ctx, externalID, orderID, holdID, true)
}

func (c *Client) releaseHold(ctx context.Context, externalID string, orderID, holdID int64, allowVersionRetry bool) error {
	order, err := c.GetOrder(ctx, externalID)
	if err != nil {
		return fmt.Errorf("ledgerclient: release_hold for %s: fetching current version: %w", externalID, err)
	}

	idempotencyKey := fmt.Sprintf("screening:release:%d:%d", orderID, holdID)
	reqBody := map[string]any{
		"to_state":         "screened",
		"expected_version": order.Version,
		"reason":           "manual_release",
		"occurred_at":      time.Now().UTC().Format(time.RFC3339),
	}

	status, body, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", externalID), idempotencyKey, reqBody)
	if err != nil {
		return fmt.Errorf("ledgerclient: release_hold for %s: %w", externalID, err)
	}
	if status == http.StatusOK {
		return nil
	}

	apiErr := decodeAPIError(status, body)
	switch apiErr.Code {
	case "version_conflict":
		if !allowVersionRetry {
			return fmt.Errorf("ledgerclient: release_hold for %s: version_conflict persisted after one retry: %w", externalID, apiErr)
		}
		slog.Warn("ledgerclient: release_hold hit version_conflict, retrying once with a fresh version", "external_id", externalID)
		return c.releaseHold(ctx, externalID, orderID, holdID, false)
	case "illegal_transition":
		return fmt.Errorf("%w: %s: %w", ErrIllegalTransition, externalID, apiErr)
	case "system_halted":
		slog.Error("ledgerclient: P1 ALERT -- unexpected system_halted releasing a hold; held->screened is not halt-blocked per C1.5's table",
			"external_id", externalID)
		return fmt.Errorf("%w: %w", ErrUnexpectedHalt, apiErr)
	default:
		return apiErr
	}
}

// RejectHold posts held->refunded for a hold a human reviewer has
// decided to reject (C3.6). Idempotency-Key is
// "screening:reject:<order_id>:<hold_id>", §A's own format. Unlike
// ReleaseHold, held->refunded IS RequiresEntry: true and HaltBlocked:
// true per C1.5's table -- entry is required (internal/holds.Reject
// builds it via a RefundEntryBuilder, stubbed until a real refund-entry
// owner exists -- see §A's own "out of scope" note), and system_halted
// here is EXPECTED and routine, exactly like C2.7's own halt-blocked
// case: returned as a plain, retryable APIError, never wrapped in
// ErrUnexpectedHalt.
func (c *Client) RejectHold(ctx context.Context, externalID string, orderID, holdID int64, entry map[string]any) error {
	return c.rejectHold(ctx, externalID, orderID, holdID, entry, true)
}

func (c *Client) rejectHold(ctx context.Context, externalID string, orderID, holdID int64, entry map[string]any, allowVersionRetry bool) error {
	order, err := c.GetOrder(ctx, externalID)
	if err != nil {
		return fmt.Errorf("ledgerclient: reject_hold for %s: fetching current version: %w", externalID, err)
	}

	idempotencyKey := fmt.Sprintf("screening:reject:%d:%d", orderID, holdID)
	reqBody := map[string]any{
		"to_state":         "refunded",
		"expected_version": order.Version,
		"reason":           "manual_reject",
		"occurred_at":      time.Now().UTC().Format(time.RFC3339),
		"entry":            entry,
	}

	status, body, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/orders/%s/transitions", externalID), idempotencyKey, reqBody)
	if err != nil {
		return fmt.Errorf("ledgerclient: reject_hold for %s: %w", externalID, err)
	}
	if status == http.StatusOK {
		return nil
	}

	apiErr := decodeAPIError(status, body)
	switch apiErr.Code {
	case "version_conflict":
		if !allowVersionRetry {
			return fmt.Errorf("ledgerclient: reject_hold for %s: version_conflict persisted after one retry: %w", externalID, apiErr)
		}
		slog.Warn("ledgerclient: reject_hold hit version_conflict, retrying once with a fresh version", "external_id", externalID)
		return c.rejectHold(ctx, externalID, orderID, holdID, entry, false)
	case "illegal_transition":
		return fmt.Errorf("%w: %s: %w", ErrIllegalTransition, externalID, apiErr)
	case "system_halted":
		slog.Info("ledgerclient: reject_hold deferred -- the ledger is halted (held->refunded is halt-blocked, this is expected), will retry",
			"external_id", externalID)
		return apiErr
	default:
		return apiErr
	}
}
