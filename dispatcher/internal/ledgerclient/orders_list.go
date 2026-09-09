package ledgerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DefaultPollLimit is how many orders internal/orchestrate asks C1 for
// per ListOrdersByState call -- matches screening's own discovery poll
// size, the only other place this project polls C1's list-by-state
// route today.
const DefaultPollLimit = 100

// OrderRef is one row of an orders list page: just enough to decide
// whether to dispatch it (internal/orchestrate) without carrying the
// whole Order, mirroring screening's own internal/ledgerclient.OrderRef.
type OrderRef struct {
	OrderID    int64
	ExternalID string
	UpdatedAt  time.Time
}

type listOrderResp struct {
	ID         int64     `json:"id"`
	ExternalID string    `json:"external_id"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type listOrdersResp struct {
	Orders     []listOrderResp `json:"orders"`
	NextCursor string          `json:"next_cursor"`
}

// ListOrdersByState calls GET /v1/orders?state=<state>&updated_after=<cursor>
// (C1's own §A addition, the same route screening's discovery loop
// polls for state=funded). cursor == "" requests the first page. The
// returned newCursor is always usable as the next call's cursor, per
// C1's own contract -- see screening/internal/ledgerclient's identical
// method for the full reasoning; duplicated here rather than shared,
// matching this project's per-module primitive convention.
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
		return nil, "", classify(decodeAPIError(status, body))
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
