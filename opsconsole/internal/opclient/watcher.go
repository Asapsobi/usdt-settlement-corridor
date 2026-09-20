package opclient

import (
	"context"
	"net/http"
	"strconv"
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

// WatchedAddress is C2's own GET /v1/addresses row shape
// (depositwatcher/internal/httpapi/addresses_handlers.go's own
// addressResponse, extended with derivation_index and the new list
// route -- see OC.11).
type WatchedAddress struct {
	Address         string     `json:"address"`
	DerivationIndex uint32     `json:"derivation_index"`
	OrderID         int64      `json:"order_id"`
	ExternalID      string     `json:"external_id"`
	CustomerID      string     `json:"customer_id"`
	Status          string     `json:"status"`
	AssignedAt      time.Time  `json:"assigned_at"`
	RetiredAt       *time.Time `json:"retired_at,omitempty"`
	RetiredReason   *string    `json:"retired_reason,omitempty"`
}

// ListAddresses calls C2's own GET /v1/addresses, added by this same
// build (OC.11) -- includes RETIRED addresses, since a settled order's
// deposit address still holds its real on-chain balance until swept.
func (c *WatcherClient) ListAddresses(ctx context.Context, limit int) ([]WatchedAddress, error) {
	var out struct {
		Addresses []WatchedAddress `json:"addresses"`
	}
	u := c.baseURL + "/v1/addresses"
	if limit > 0 {
		u += "?limit=" + strconv.Itoa(limit)
	}
	err := do(ctx, c.http, "watcher", c.token, http.MethodGet, u, nil, &out)
	return out.Addresses, err
}

// GetAddress calls C2's own GET /v1/addresses/{order_id} -- one order's
// own deposit-address status (WATCHING/FUNDED/RETIRED), the sub-stage
// detail within C1's "quoted"/"funded" states that OC.10's order detail
// view needs.
func (c *WatcherClient) GetAddress(ctx context.Context, orderID int64) (WatchedAddress, error) {
	var out WatchedAddress
	err := do(ctx, c.http, "watcher", c.token, http.MethodGet, c.baseURL+"/v1/addresses/"+strconv.FormatInt(orderID, 10), nil, &out)
	return out, err
}

// ConfirmDepositResult is C2's own postConfirmDeposit response shape.
type ConfirmDepositResult struct {
	OrderID     int64  `json:"order_id"`
	TxHash      string `json:"tx_hash"`
	BlockNumber uint64 `json:"block_number"`
	Accepted    bool   `json:"accepted"`
}

// ConfirmDeposit calls C2's own POST /v1/addresses/{order_id}/confirm-deposit
// (OC.12) -- an operator-supplied txid, verified on-chain before it's fed
// through the exact same crediting path a normally-detected deposit
// uses; still respects finality, never a manual override that skips it.
func (c *WatcherClient) ConfirmDeposit(ctx context.Context, orderID int64, txHash string) (ConfirmDepositResult, error) {
	body := map[string]any{"tx_hash": txHash}
	var out ConfirmDepositResult
	err := do(ctx, c.http, "watcher", c.token, http.MethodPost, c.baseURL+"/v1/addresses/"+strconv.FormatInt(orderID, 10)+"/confirm-deposit", body, &out)
	return out, err
}

// GetAddressBalance calls C2's own GET /v1/addresses/{order_id}/balance.
func (c *WatcherClient) GetAddressBalance(ctx context.Context, orderID int64) (string, error) {
	var out struct {
		BalanceRaw string `json:"balance_raw"`
	}
	err := do(ctx, c.http, "watcher", c.token, http.MethodGet, c.baseURL+"/v1/addresses/"+strconv.FormatInt(orderID, 10)+"/balance", nil, &out)
	return out.BalanceRaw, err
}

// RetireAddress calls C2's own POST /v1/addresses/{order_id}/retire.
// Per addresses.Retire's own doc comment (depositwatcher/internal/addresses/store.go):
// retirement is explicit and permanent -- RETIRED has no legal outgoing
// transition, so a retired address can never be reassigned or
// re-watched again (invariant 6: never reuse a derivation index or an
// address). reason is free text but by convention one of settled,
// refunded, expired, or superseded -- not enforced by C2 itself.
func (c *WatcherClient) RetireAddress(ctx context.Context, orderID int64, reason string) (WatchedAddress, error) {
	body := map[string]string{"reason": reason}
	var out WatchedAddress
	err := do(ctx, c.http, "watcher", c.token, http.MethodPost, c.baseURL+"/v1/addresses/"+strconv.FormatInt(orderID, 10)+"/retire", body, &out)
	return out, err
}

// OrphanedDeposit is C2's own GET /v1/orphaned-deposits row shape
// (depositwatcher/internal/httpapi/orphaned_handlers.go's own
// orphanedDepositResponse).
type OrphanedDeposit struct {
	ID                    int64      `json:"id"`
	OrderID               int64      `json:"order_id"`
	ExternalID            string     `json:"external_id"`
	TxHash                string     `json:"tx_hash"`
	LogIndex              int        `json:"log_index"`
	Amount                string     `json:"amount"`
	DetectedAt            time.Time  `json:"detected_at"`
	OrderStateAtDetection string     `json:"order_state_at_detection"`
	Resolution            *string    `json:"resolution,omitempty"`
	ResolvedAt            *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy            *string    `json:"resolved_by,omitempty"`
}

// ListOrphanedDeposits calls C2's own GET /v1/orphaned-deposits,
// optionally filtered by resolved (nil lists every one).
func (c *WatcherClient) ListOrphanedDeposits(ctx context.Context, resolved *bool) ([]OrphanedDeposit, error) {
	u := c.baseURL + "/v1/orphaned-deposits"
	if resolved != nil {
		u += "?resolved=" + strconv.FormatBool(*resolved)
	}
	var out struct {
		Deposits []OrphanedDeposit `json:"deposits"`
	}
	err := do(ctx, c.http, "watcher", c.token, http.MethodGet, u, nil, &out)
	return out.Deposits, err
}

// ResolveOrphanedDeposit calls C2's own POST
// /v1/orphaned-deposits/{id}/resolve. resolution is genuine free text --
// orphaned.Resolve (depositwatcher/internal/orphaned/orphaned.go) enforces
// no enum, and this system's actor convention means resolved_by always
// comes from the caller's own bearer token, never a body field. A
// second resolve on an already-resolved deposit fails with C2's real
// ErrAlreadyResolved, surfaced unchanged.
func (c *WatcherClient) ResolveOrphanedDeposit(ctx context.Context, id int64, resolution string) (OrphanedDeposit, error) {
	body := map[string]string{"resolution": resolution}
	var out OrphanedDeposit
	err := do(ctx, c.http, "watcher", c.token, http.MethodPost, c.baseURL+"/v1/orphaned-deposits/"+strconv.FormatInt(id, 10)+"/resolve", body, &out)
	return out, err
}

// ProviderHealth is C2's own GET /v1/system/providers row shape
// (depositwatcher/internal/httpapi/system_handlers.go's own
// providerHealthResponse).
type ProviderHealth struct {
	Name                string `json:"name"`
	Healthy             bool   `json:"healthy"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	TotalRounds         int    `json:"total_rounds"`
	TotalFailures       int    `json:"total_failures"`
	LastError           string `json:"last_error,omitempty"`
}

// GetProviders calls C2's own GET /v1/system/providers -- per-provider
// RPC health from C2's own provider pool.
func (c *WatcherClient) GetProviders(ctx context.Context) ([]ProviderHealth, error) {
	var out struct {
		Providers []ProviderHealth `json:"providers"`
	}
	err := do(ctx, c.http, "watcher", c.token, http.MethodGet, c.baseURL+"/v1/system/providers", nil, &out)
	return out.Providers, err
}
