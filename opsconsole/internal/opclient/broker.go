package opclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// BrokerClient calls one C4 (energy broker) instance.
type BrokerClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewBrokerClient returns a BrokerClient for baseURL, authenticating
// every call with token.
func NewBrokerClient(baseURL, token string) *BrokerClient {
	return &BrokerClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

// Healthz reports whether the broker's own /healthz responds 200.
func (c *BrokerClient) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "broker", c.baseURL)
}

// BrokerInvariants is C4's own GET /v1/system/invariants response shape
// (energybroker/internal/httpapi/system_handlers.go's own
// invariantsResponse).
type BrokerInvariants struct {
	BufferAvailable          int64 `json:"buffer_available"`
	BufferTarget             int64 `json:"buffer_target"`
	FastPathConfirmedCount   int64 `json:"fast_path_confirmed_count"`
	SlowPathConfirmedCount   int64 `json:"slow_path_confirmed_count"`
	OpenManualFallbackEvents int64 `json:"open_manual_fallback_events"`
}

// GetInvariants reads C4's system invariants.
func (c *BrokerClient) GetInvariants(ctx context.Context) (BrokerInvariants, error) {
	var out BrokerInvariants
	err := do(ctx, c.http, "broker", c.token, http.MethodGet, c.baseURL+"/v1/system/invariants", nil, &out)
	return out, err
}

// Reservation is C4's own reservationResponse shape
// (energybroker/internal/httpapi/reservations_handlers.go).
type Reservation struct {
	ID            int64      `json:"id"`
	ExternalID    string     `json:"external_id"`
	OrderID       int64      `json:"order_id"`
	TargetAddress string     `json:"target_address"`
	EnergyUnits   int64      `json:"energy_units"`
	Tier          string     `json:"tier"`
	Status        string     `json:"status"`
	Vendor        *string    `json:"vendor"`
	CostTRX       *string    `json:"cost_trx"`
	ConfirmedAt   *time.Time `json:"confirmed_at"`
	Deadline      time.Time  `json:"deadline"`
	CreatedAt     time.Time  `json:"created_at"`
}

// ListReservations calls C4's own GET /v1/reservations?status=..., added
// by this same build (OC.5) -- the first route to list reservations at
// all.
func (c *BrokerClient) ListReservations(ctx context.Context, statuses []string, limit int) ([]Reservation, error) {
	q := url.Values{}
	for _, s := range statuses {
		q.Add("status", s)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Reservations []Reservation `json:"reservations"`
	}
	err := do(ctx, c.http, "broker", c.token, http.MethodGet, c.baseURL+"/v1/reservations?"+q.Encode(), nil, &out)
	return out.Reservations, err
}

// ReconcileRequest is the body OC.5's reconcile form submits, matching
// cmd/reconcile-reservation's own CLI flags one-for-one.
type ReconcileRequest struct {
	OrderID       int64  `json:"order_id"`
	Provider      string `json:"provider"`
	DelegationID  string `json:"delegation_id"`
	TargetAddress string `json:"target_address"`
	EnergyUnits   int64  `json:"energy_units"`
	CostTRX       string `json:"cost_trx"`
}

// ReconcileReservation calls C4's own POST /v1/reservations/{id}/reconcile.
func (c *BrokerClient) ReconcileReservation(ctx context.Context, id int64, req ReconcileRequest) (Reservation, error) {
	var out Reservation
	err := do(ctx, c.http, "broker", c.token, http.MethodPost, c.baseURL+"/v1/reservations/"+strconv.FormatInt(id, 10)+"/reconcile", req, &out)
	return out, err
}

// FallbackEvent is C4's own manual-fallback-event shape
// (energybroker/internal/httpapi/fallback_handlers.go's own
// fallbackEventResponse).
type FallbackEvent struct {
	ID          int64      `json:"id"`
	TriggeredAt time.Time  `json:"triggered_at"`
	Reason      string     `json:"reason"`
	OrderID     *int64     `json:"order_id"`
	ResolvedAt  *time.Time `json:"resolved_at"`
	Resolution  *string    `json:"resolution"`
	ResolvedBy  *string    `json:"resolved_by"`
}

// ListFallbackEvents calls C4's own GET /v1/manual-fallback-events.
func (c *BrokerClient) ListFallbackEvents(ctx context.Context, resolved *bool) ([]FallbackEvent, error) {
	u := c.baseURL + "/v1/manual-fallback-events"
	if resolved != nil {
		u += "?resolved=" + strconv.FormatBool(*resolved)
	}
	var out struct {
		Events []FallbackEvent `json:"events"`
	}
	err := do(ctx, c.http, "broker", c.token, http.MethodGet, u, nil, &out)
	return out.Events, err
}

// ResolveFallbackEvent calls C4's own POST
// /v1/manual-fallback-events/{id}/resolve.
func (c *BrokerClient) ResolveFallbackEvent(ctx context.Context, id int64, resolution, actor string) error {
	body := map[string]string{"resolution": resolution, "actor": actor}
	return do(ctx, c.http, "broker", c.token, http.MethodPost, c.baseURL+"/v1/manual-fallback-events/"+strconv.FormatInt(id, 10)+"/resolve", body, nil)
}
