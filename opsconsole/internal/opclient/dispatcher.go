package opclient

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DispatcherClient calls one C5 (payout dispatcher) instance.
type DispatcherClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewDispatcherClient returns a DispatcherClient for baseURL,
// authenticating every call with token.
func NewDispatcherClient(baseURL, token string) *DispatcherClient {
	return &DispatcherClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

// Healthz reports whether the dispatcher's own /healthz responds 200.
func (c *DispatcherClient) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "dispatcher", c.baseURL)
}

// DispatcherInvariants is C5's own GET /v1/system/invariants response
// shape (dispatcher/internal/httpapi/system_handlers.go's own
// invariantsResponse), only the fields the console's home page (OC.2)
// renders.
type DispatcherInvariants struct {
	OpenDispatchingOrders      int `json:"open_dispatching_orders"`
	StuckPendingReconciliation int `json:"stuck_pending_reconciliation"`
	BatchQueueDepth            int `json:"batch_queue_depth"`
}

// GetInvariants reads C5's system invariants.
func (c *DispatcherClient) GetInvariants(ctx context.Context) (DispatcherInvariants, error) {
	var out DispatcherInvariants
	err := do(ctx, c.http, "dispatcher", c.token, http.MethodGet, c.baseURL+"/v1/system/invariants", nil, &out)
	return out, err
}

// Slot is C5's own GET /v1/slots row shape
// (dispatcher/internal/httpapi/slots_handlers.go's own slotResponse).
type Slot struct {
	ID             int        `json:"id"`
	TronAddress    string     `json:"tron_address"`
	Status         string     `json:"status"`
	Balance        string     `json:"balance"`
	TxCount        int64      `json:"tx_count"`
	ActivatedAt    time.Time  `json:"activated_at"`
	RetiredAt      *time.Time `json:"retired_at,omitempty"`
	LastDispatchAt *time.Time `json:"last_dispatch_at,omitempty"`
}

// ListSlots calls C5's own GET /v1/slots.
func (c *DispatcherClient) ListSlots(ctx context.Context) ([]Slot, error) {
	var out struct {
		Slots []Slot `json:"slots"`
	}
	err := do(ctx, c.http, "dispatcher", c.token, http.MethodGet, c.baseURL+"/v1/slots", nil, &out)
	return out.Slots, err
}

// RetireSlot calls C5's own POST /v1/slots/{id}/retire -- "a manual
// override of the normal cap-triggered rotation" per C5's own doc
// comment on that route.
func (c *DispatcherClient) RetireSlot(ctx context.Context, id int, immediate bool) error {
	body := map[string]any{"immediate": immediate}
	return do(ctx, c.http, "dispatcher", c.token, http.MethodPost, c.baseURL+"/v1/slots/"+strconv.Itoa(id)+"/retire", body, nil)
}
