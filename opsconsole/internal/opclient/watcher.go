package opclient

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// WatcherClient calls one C2 (deposit watcher) instance.
type WatcherClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewWatcherClient returns a WatcherClient for baseURL, authenticating
// every call with token.
func NewWatcherClient(baseURL, token string) *WatcherClient {
	return &WatcherClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

// Healthz reports whether the watcher's own /healthz responds 200.
func (c *WatcherClient) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "watcher", c.baseURL)
}

// WatcherInvariants is C2's own GET /v1/system/invariants response shape
// (depositwatcher/internal/httpapi/system_handlers.go's own
// invariantsResponse). Pointer fields are independently omitted by C2
// itself when their own source isn't configured on that instance.
type WatcherInvariants struct {
	CursorLagBlocks                  *int64   `json:"cursor_lag_blocks,omitempty"`
	PendingFinalityCount             *int     `json:"pending_finality_count,omitempty"`
	OldestPendingCandidateAgeSeconds *float64 `json:"oldest_pending_candidate_age_seconds,omitempty"`
}

// GetInvariants reads C2's system invariants.
func (c *WatcherClient) GetInvariants(ctx context.Context) (WatcherInvariants, error) {
	var out WatcherInvariants
	err := do(ctx, c.http, "watcher", c.token, http.MethodGet, c.baseURL+"/v1/system/invariants", nil, &out)
	return out, err
}

// Cursor is C2's own GET /v1/system/cursor response shape
// (depositwatcher/internal/httpapi/cursor_handlers.go's own
// cursorResponse, added by this same build -- see OC.4).
type Cursor struct {
	LastScanned          int64      `json:"last_scanned"`
	LastCandidateScanned *int64     `json:"last_candidate_scanned,omitempty"`
	UpdatedAt            *time.Time `json:"updated_at,omitempty"`
}

// GetCursor reads C2's raw ingestion cursor.
func (c *WatcherClient) GetCursor(ctx context.Context) (Cursor, error) {
	var out Cursor
	err := do(ctx, c.http, "watcher", c.token, http.MethodGet, c.baseURL+"/v1/system/cursor", nil, &out)
	return out, err
}

// SetCursor overrides C2's ingestion cursor -- the write side of OC.4.
func (c *WatcherClient) SetCursor(ctx context.Context, lastScanned, lastCandidateScanned int64, reason string) (Cursor, error) {
	body := map[string]any{"last_scanned": lastScanned, "last_candidate_scanned": lastCandidateScanned, "reason": reason}
	var out Cursor
	err := do(ctx, c.http, "watcher", c.token, http.MethodPost, c.baseURL+"/v1/system/cursor", body, &out)
	return out, err
}
