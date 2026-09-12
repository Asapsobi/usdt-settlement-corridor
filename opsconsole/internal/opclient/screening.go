package opclient

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ScreeningClient calls one C3 (screening) instance.
type ScreeningClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewScreeningClient returns a ScreeningClient for baseURL,
// authenticating every call with token.
func NewScreeningClient(baseURL, token string) *ScreeningClient {
	return &ScreeningClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

// Healthz reports whether screening's own /healthz responds 200.
func (c *ScreeningClient) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "screening", c.baseURL)
}

// QueueStats is C3's own GET /v1/system/queue response shape
// (screening/internal/httpapi/system_handlers.go's own
// queueStatsResponse).
type QueueStats struct {
	DepthByStatus           map[string]int `json:"depth_by_status"`
	OldestPendingAgeSeconds *float64       `json:"oldest_pending_age_seconds"`
}

// GetQueue reads C3's screening-queue stats.
func (c *ScreeningClient) GetQueue(ctx context.Context) (QueueStats, error) {
	var out QueueStats
	err := do(ctx, c.http, "screening", c.token, http.MethodGet, c.baseURL+"/v1/system/queue", nil, &out)
	return out, err
}

// Hold is C3's own GET /v1/holds row shape
// (screening/internal/httpapi/holds_handlers.go's own holdResponse).
type Hold struct {
	ID                int64      `json:"id"`
	OrderID           int64      `json:"order_id"`
	ExternalID        string     `json:"external_id"`
	ReasonCode        string     `json:"reason_code"`
	ScreeningResultID *int64     `json:"screening_result_id"`
	OpenedAt          time.Time  `json:"opened_at"`
	Status            string     `json:"status"`
	ResolvedBy        *string    `json:"resolved_by"`
	ResolvedAt        *time.Time `json:"resolved_at"`
	ResolutionNote    *string    `json:"resolution_note"`
}

// ListHolds calls C3's own GET /v1/holds, optionally filtered by status
// ("" lists every status).
func (c *ScreeningClient) ListHolds(ctx context.Context, status string) ([]Hold, error) {
	u := c.baseURL + "/v1/holds"
	if status != "" {
		u += "?status=" + status
	}
	var out struct {
		Holds []Hold `json:"holds"`
	}
	err := do(ctx, c.http, "screening", c.token, http.MethodGet, u, nil, &out)
	return out.Holds, err
}

// ReleaseHold calls C3's own POST /v1/holds/{id}/release. reviewer is
// the OPERATOR'S OWN display name -- C3's own route takes reviewer from
// the request body, exactly the shape that lets this attribute
// correctly (unlike C1's halt route, see OC.3's own doc comment).
func (c *ScreeningClient) ReleaseHold(ctx context.Context, id int64, reviewer, note string) error {
	body := map[string]string{"reviewer": reviewer, "note": note}
	return do(ctx, c.http, "screening", c.token, http.MethodPost, c.baseURL+"/v1/holds/"+strconv.FormatInt(id, 10)+"/release", body, nil)
}

// RejectHold calls C3's own POST /v1/holds/{id}/reject.
func (c *ScreeningClient) RejectHold(ctx context.Context, id int64, reviewer, note string) error {
	body := map[string]string{"reviewer": reviewer, "note": note}
	return do(ctx, c.http, "screening", c.token, http.MethodPost, c.baseURL+"/v1/holds/"+strconv.FormatInt(id, 10)+"/reject", body, nil)
}
