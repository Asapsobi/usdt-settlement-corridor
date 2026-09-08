package ledgerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"dispatcher/internal/money"
)

// EntryLine is one line of an entry this client posts inline with a
// transition -- mirrors C1's own entryLineRequest exactly.
type EntryLine struct {
	AccountCode string
	Asset       string
	Amount      money.Amount
}

// TransitionWithEntry calls POST /v1/orders/{externalID}/transitions
// with an inline entry -- C5's own E2 (screened -> dispatching) and E3
// (dispatching -> settled) both go through this one call shape, each
// with its own entry_type and lines (c1-ledger-build-prompts.md §B).
// idempotencyKey becomes both the request's own Idempotency-Key header
// AND, per C1's real behavior, the posted entry's own idempotency key --
// a retried call with the same key replays the identical entry, never
// posts a second one.
func (c *Client) TransitionWithEntry(ctx context.Context, externalID, toState string, expectedVersion int32, reason, entryType string, occurredAt time.Time, lines []EntryLine, idempotencyKey string) (Order, error) {
	reqLines := make([]entryLineRequest, len(lines))
	for i, l := range lines {
		reqLines[i] = entryLineRequest{AccountCode: l.AccountCode, Asset: l.Asset, Amount: l.Amount.Format()}
	}
	body := postTransitionRequest{
		ToState: toState, ExpectedVersion: expectedVersion, Reason: reason, OccurredAt: occurredAt,
		Entry: &transitionEntryRequest{EntryType: entryType, OccurredAt: occurredAt, Lines: reqLines},
	}
	return c.postTransition(ctx, externalID, idempotencyKey, body)
}

// TransitionWithEntryID calls the same endpoint but names an
// ALREADY-POSTED entry (in practice, a reversal from PostReversal) as
// this transition's own cause, rather than posting a new one -- the
// dispatching -> held path (C5.7).
func (c *Client) TransitionWithEntryID(ctx context.Context, externalID, toState string, expectedVersion int32, reason string, entryID int64, occurredAt time.Time, idempotencyKey string) (Order, error) {
	body := postTransitionRequest{
		ToState: toState, ExpectedVersion: expectedVersion, Reason: reason, OccurredAt: occurredAt, EntryID: &entryID,
	}
	return c.postTransition(ctx, externalID, idempotencyKey, body)
}

type entryLineRequest struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}

type transitionEntryRequest struct {
	EntryType  string             `json:"entry_type"`
	OccurredAt time.Time          `json:"occurred_at"`
	Lines      []entryLineRequest `json:"lines"`
}

type postTransitionRequest struct {
	ToState         string                  `json:"to_state"`
	ExpectedVersion int32                   `json:"expected_version"`
	Reason          string                  `json:"reason"`
	OccurredAt      time.Time               `json:"occurred_at"`
	Entry           *transitionEntryRequest `json:"entry,omitempty"`
	EntryID         *int64                  `json:"entry_id,omitempty"`
}

func (c *Client) postTransition(ctx context.Context, externalID, idempotencyKey string, body postTransitionRequest) (Order, error) {
	status, respBody, err := c.do(ctx, http.MethodPost, "/v1/orders/"+externalID+"/transitions", idempotencyKey, body)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, classify(decodeAPIError(status, respBody))
	}
	var resp orderResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return Order{}, fmt.Errorf("ledgerclient: decoding transition response for %s: %w", externalID, err)
	}
	return resp.toOrder()
}

// Entry is the subset of C1's journal entry resource this client needs.
type Entry struct {
	ID             int64
	IdempotencyKey string
	ReversalOf     *int64
}

type postedLineResponse struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}
type entryResponse struct {
	ID             int64                `json:"id"`
	IdempotencyKey string               `json:"idempotency_key"`
	ReversalOf     *int64               `json:"reversal_of,omitempty"`
	Lines          []postedLineResponse `json:"lines"`
	Outcome        string               `json:"outcome"`
}

func (r entryResponse) toEntry() Entry {
	return Entry{ID: r.ID, IdempotencyKey: r.IdempotencyKey, ReversalOf: r.ReversalOf}
}

// GetEntryByIdempotencyKey calls GET /v1/entries?idempotency_key=... --
// C5's own way to resolve the E2 conversion entry's real numeric id
// (the response to entering `dispatching` returns the ORDER, not the
// entry it posted) before it can be reversed.
func (c *Client) GetEntryByIdempotencyKey(ctx context.Context, idempotencyKey string) (Entry, error) {
	status, body, err := c.do(ctx, http.MethodGet, "/v1/entries?idempotency_key="+idempotencyKey, "", nil)
	if err != nil {
		return Entry{}, err
	}
	if status != http.StatusOK {
		return Entry{}, classify(decodeAPIError(status, body))
	}
	var resp entryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Entry{}, fmt.Errorf("ledgerclient: decoding entry response: %w", err)
	}
	return resp.toEntry(), nil
}

type postReversalBody struct {
	Reason     string    `json:"reason"`
	OccurredAt time.Time `json:"occurred_at"`
}

type alreadyReversedResponse struct {
	ExistingReversal *entryResponse `json:"existing_reversal,omitempty"`
}

// PostReversal calls POST /v1/entries/{entryID}/reversal. This endpoint
// is deliberately NOT idempotent on C1's own side (per its real doc
// comment: "at most one reversal per entry is a structural invariant,
// not a request that happens to repeat"), but a 409 response embeds the
// existing reversal's own id -- so a retried call safely discovers
// "already done" without a separate pre-check GET, and this method
// returns that existing reversal (alreadyExisted=true) rather than
// erroring, letting a caller treat both outcomes uniformly.
func (c *Client) PostReversal(ctx context.Context, entryID int64, reason string, occurredAt time.Time) (reversal Entry, alreadyExisted bool, err error) {
	// The Idempotency-Key header is required by C1's blanket "no header,
	// no write" rule (every write route, per requireIdempotencyKey's own
	// doc comment) even though this specific endpoint's own idempotency
	// is derived server-side from the entry being reversed, not from
	// this header's value -- confirmed the hard way: omitting it
	// entirely 400s against a real C1, invisible against a fake server
	// that doesn't enforce the middleware. Any non-empty value satisfies
	// the rule; this one is at least deterministic for a given entryID.
	idempotencyKey := fmt.Sprintf("dispatcher:reverse:%d", entryID)
	status, body, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/v1/entries/%d/reversal", entryID), idempotencyKey, postReversalBody{
		Reason: reason, OccurredAt: occurredAt,
	})
	if err != nil {
		return Entry{}, false, err
	}
	if status == http.StatusCreated {
		var resp entryResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return Entry{}, false, fmt.Errorf("ledgerclient: decoding reversal response: %w", err)
		}
		return resp.toEntry(), false, nil
	}
	if status == http.StatusConflict {
		var already alreadyReversedResponse
		if err := json.Unmarshal(body, &already); err == nil && already.ExistingReversal != nil {
			return already.ExistingReversal.toEntry(), true, nil
		}
		return Entry{}, false, classify(decodeAPIError(status, body))
	}
	return Entry{}, false, classify(decodeAPIError(status, body))
}
