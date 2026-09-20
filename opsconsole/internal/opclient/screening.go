package opclient

import (
	"context"
	"net/http"
	"net/url"
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

// ScreeningResult is C3's own GET /v1/screening-results row shape.
type ScreeningResult struct {
	ID            int64     `json:"id"`
	ProviderName  string    `json:"provider_name"`
	SenderAddress string    `json:"sender_address"`
	RiskScore     float64   `json:"risk_score"`
	Flagged       bool      `json:"flagged"`
	ReasonCodes   []string  `json:"reason_codes"`
	CheckedAt     time.Time `json:"checked_at"`
}

// GetScreeningResults calls C3's own GET /v1/screening-results --
// senderAddress is REQUIRED on the real route (an audit lookup for one
// already-known address, not a general browse-everything listing; there
// is no route to discover flagged addresses other than by already
// having one, e.g. from a hold's own funding order).
func (c *ScreeningClient) GetScreeningResults(ctx context.Context, senderAddress string) ([]ScreeningResult, error) {
	var out struct {
		ScreeningResults []ScreeningResult `json:"screening_results"`
	}
	err := do(ctx, c.http, "screening", c.token, http.MethodGet, c.baseURL+"/v1/screening-results?sender_address="+url.QueryEscape(senderAddress), nil, &out)
	return out.ScreeningResults, err
}

// InvalidateScreeningResult calls C3's own POST
// /v1/screening-results/{id}/invalidate. Per that route's own doc
// comment (screening/internal/httpapi/screening_results_handlers.go):
// the path names one row, but the real effect (internal/cache.Invalidate)
// operates on the whole (provider, sender_address) PAIR that row
// belongs to -- every cached result for that pair stops being trusted,
// not just this one row. Nothing is deleted, and this does not itself
// force a re-screen or touch any order's already-recorded state; it
// only means the next lookup for that pair will not be served from
// cache. reason and actor are both required by C3 itself.
func (c *ScreeningClient) InvalidateScreeningResult(ctx context.Context, id int64, reason, actor string) error {
	body := map[string]string{"reason": reason, "actor": actor}
	return do(ctx, c.http, "screening", c.token, http.MethodPost, c.baseURL+"/v1/screening-results/"+strconv.FormatInt(id, 10)+"/invalidate", body, nil)
}

// RescreenFlag is C3's own GET /v1/rescreen-flags row shape
// (screening/internal/httpapi/rescreen_flags_handlers.go's own
// rescreenFlagResponse) -- recorded when a later screening verdict for
// an address diverges from the verdict an order was originally screened
// under.
type RescreenFlag struct {
	ID                    int64      `json:"id"`
	OrderID               int64      `json:"order_id"`
	ExternalID            string     `json:"external_id"`
	OrderStateAtDetection string     `json:"order_state_at_detection"`
	PreviousVerdictID     int64      `json:"previous_verdict_id"`
	NewVerdictID          int64      `json:"new_verdict_id"`
	DetectedAt            time.Time  `json:"detected_at"`
	Resolution            *string    `json:"resolution"`
	ResolvedAt            *time.Time `json:"resolved_at"`
	ResolvedBy            *string    `json:"resolved_by"`
}

// ListRescreenFlags calls C3's own GET /v1/rescreen-flags, optionally
// filtered by resolved (nil lists every flag).
func (c *ScreeningClient) ListRescreenFlags(ctx context.Context, resolved *bool) ([]RescreenFlag, error) {
	u := c.baseURL + "/v1/rescreen-flags"
	if resolved != nil {
		u += "?resolved=" + strconv.FormatBool(*resolved)
	}
	var out struct {
		RescreenFlags []RescreenFlag `json:"rescreen_flags"`
	}
	err := do(ctx, c.http, "screening", c.token, http.MethodGet, u, nil, &out)
	return out.RescreenFlags, err
}

// ResolveRescreenFlag calls C3's own POST /v1/rescreen-flags/{id}/resolve
// -- purely a record of a human decision (that route's own doc comment:
// "this package never acts on it," mechanism not policy), so unlike
// reversal/reorg/transition this carries no further side effect to warn
// about. actor is a genuine request body field on this route (unlike
// C2's orphaned-deposit resolve, which takes actor from the bearer
// token only) -- each service's real contract is followed as-is, not
// assumed uniform.
func (c *ScreeningClient) ResolveRescreenFlag(ctx context.Context, id int64, resolution, actor string) (RescreenFlag, error) {
	body := map[string]string{"resolution": resolution, "actor": actor}
	var out RescreenFlag
	err := do(ctx, c.http, "screening", c.token, http.MethodPost, c.baseURL+"/v1/rescreen-flags/"+strconv.FormatInt(id, 10)+"/resolve", body, &out)
	return out, err
}
