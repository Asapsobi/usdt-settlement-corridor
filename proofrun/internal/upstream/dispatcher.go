package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Dispatcher calls C5 -- GET /v1/dispatch/{order_id} only, the one call
// this driver needs (dispatcher/internal/httpapi/dispatch_handlers.go).
type Dispatcher struct{ c client }

// NewDispatcher returns a Dispatcher for baseURL, authenticating with
// token.
func NewDispatcher(baseURL, token string) *Dispatcher {
	return &Dispatcher{c: newClient("C5 (payout dispatcher)", baseURL, token)}
}

// ErrNotDispatchingYet means this order has never entered `dispatching`
// -- not an error condition for this driver's own status endpoint, just
// "nothing to report from C5 yet."
var ErrNotDispatchingYet = errors.New("upstream: order has not entered dispatching yet")

// DispatchStatus mirrors C5's own dispatchResponse shape.
type DispatchStatus struct {
	OrderID              int64      `json:"order_id"`
	SlotID               int        `json:"slot_id"`
	Status               string     `json:"status"`
	EnteredDispatchingAt time.Time  `json:"entered_dispatching_at"`
	AttemptNumber        *int       `json:"latest_attempt_number,omitempty"`
	AttemptStatus        *string    `json:"latest_attempt_status,omitempty"`
	TronTxID             *string    `json:"tron_txid,omitempty"`
	BroadcastAt          *time.Time `json:"broadcast_at,omitempty"`
}

// GetDispatch calls GET /v1/dispatch/{orderID}. Returns ErrNotDispatchingYet
// (never an *APIError) for C5's own dispatch_not_found -- the normal
// state for any order still `screened`.
func (d *Dispatcher) GetDispatch(ctx context.Context, orderID int64) (DispatchStatus, error) {
	status, body, err := d.c.do(ctx, http.MethodGet, "/v1/dispatch/"+strconv.FormatInt(orderID, 10), "", nil)
	if err != nil {
		return DispatchStatus{}, err
	}
	if status == http.StatusNotFound {
		return DispatchStatus{}, ErrNotDispatchingYet
	}
	if status != http.StatusOK {
		return DispatchStatus{}, decodeAPIError(d.c.component, status, body)
	}
	var ds DispatchStatus
	if err := json.Unmarshal(body, &ds); err != nil {
		return DispatchStatus{}, fmt.Errorf("upstream: decoding GetDispatch response: %w", err)
	}
	return ds, nil
}
