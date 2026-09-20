package opclient

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GatewayClient calls one C6 (API gateway) instance, over two
// completely separate credentials for two completely separate route
// families: sandboxToken (a sandbox customer's own sk_test_ key, never
// a production sk_live_ one) for every /v1/sandbox/* route, exactly as
// before OC.19; adminToken (OC.19's own GATEWAY_ADMIN_TOKENS-style
// service token) for every /v1/admin/* route this client's admin
// methods call. Neither is ever substitutable for the other -- gateway
// itself enforces this at the route level (admin_auth.go's own
// requireAdminAuth never consults customers.Store at all), this client
// just mirrors that split rather than blurring it behind one token
// field.
type GatewayClient struct {
	baseURL      string
	sandboxToken string
	adminToken   string
	http         *http.Client
}

// NewGatewayClient returns a GatewayClient for baseURL. Either token
// may be empty if that half of gateway's surface isn't configured for
// this deployment -- a call needing the missing one fails with
// gateway's own real 401, not a client-side guess.
func NewGatewayClient(baseURL, sandboxAPIKey, adminToken string) *GatewayClient {
	return &GatewayClient{baseURL: strings.TrimRight(baseURL, "/"), sandboxToken: sandboxAPIKey, adminToken: adminToken, http: &http.Client{Timeout: 5 * time.Second}}
}

// Healthz reports whether the gateway's own /healthz responds 200.
func (c *GatewayClient) Healthz(ctx context.Context) error {
	return checkHealthz(ctx, c.http, "gateway", c.baseURL)
}

// SandboxOrder is C6's own sandboxOrderResponse shape
// (gateway/internal/httpapi/sandbox_handlers.go).
type SandboxOrder struct {
	ExternalID       string  `json:"external_id"`
	Trigger          string  `json:"trigger"`
	Tier             string  `json:"tier"`
	AmountIn         string  `json:"amount_in"`
	AmountOut        string  `json:"amount_out"`
	FeeUnits         string  `json:"fee_units"`
	NetworkFeeUnits  string  `json:"network_fee_units"`
	RecipientAddress string  `json:"recipient_address"`
	DepositAddress   string  `json:"deposit_address"`
	State            string  `json:"state"`
	HoldReason       *string `json:"hold_reason"`
	WebhookAttempts  int     `json:"webhook_attempts"`
	WebhookExhausted bool    `json:"webhook_exhausted"`
}

// ListSandboxOrders calls C6's own GET /v1/sandbox/orders, added by
// this same build (OC.12) -- the authenticated sandbox customer's own
// orders, newest first.
func (c *GatewayClient) ListSandboxOrders(ctx context.Context) ([]SandboxOrder, error) {
	var out struct {
		Orders []SandboxOrder `json:"orders"`
	}
	err := do(ctx, c.http, "gateway", c.sandboxToken, http.MethodGet, c.baseURL+"/v1/sandbox/orders", nil, &out)
	return out.Orders, err
}

// AdminAPIKey is C6's own GET/POST /v1/admin/api-keys row shape
// (gateway/internal/httpapi/admin_handlers.go's own apiKeyResponse) --
// never the hash, never the raw key, except APIKey below.
type AdminAPIKey struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	IsSandbox   bool      `json:"is_sandbox"`
	Status      string    `json:"status"`
	APIKeyLast4 *string   `json:"api_key_last4,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AdminAPIKeyIssued is POST /v1/admin/api-keys' own response shape --
// the one call in this whole client that returns a raw secret, shown
// exactly once. See gateway's own admin_handlers.go doc comment on
// apiKeyIssuedResponse for why this doesn't conflict with invariant 5.
type AdminAPIKeyIssued struct {
	AdminAPIKey
	APIKey string `json:"api_key"`
}

// ListAPIKeys calls C6's own GET /v1/admin/api-keys?customer_id=... --
// customerID 0 lists every customer's own key metadata.
func (c *GatewayClient) ListAPIKeys(ctx context.Context, customerID int64) ([]AdminAPIKey, error) {
	u := c.baseURL + "/v1/admin/api-keys"
	if customerID != 0 {
		u += "?customer_id=" + strconv.FormatInt(customerID, 10)
	}
	var out struct {
		APIKeys []AdminAPIKey `json:"api_keys"`
	}
	err := do(ctx, c.http, "gateway", c.adminToken, http.MethodGet, u, nil, &out)
	return out.APIKeys, err
}

// IssueAPIKey calls C6's own POST /v1/admin/api-keys -- rotates
// customerID's own key (see AdminAPIKeyIssued's own doc comment and
// that route's real semantics: one key per customer row, not a
// one-to-many list). mode must be "live" or "test" and must match the
// target customer's own permanently-fixed identity, or C6's real
// mode_mismatch 409 comes back unchanged.
func (c *GatewayClient) IssueAPIKey(ctx context.Context, customerID int64, mode string) (AdminAPIKeyIssued, error) {
	body := map[string]any{"customer_id": customerID, "mode": mode}
	var out AdminAPIKeyIssued
	err := do(ctx, c.http, "gateway", c.adminToken, http.MethodPost, c.baseURL+"/v1/admin/api-keys", body, &out)
	return out, err
}

// RevokeAPIKey calls C6's own POST /v1/admin/api-keys/{id}/revoke --
// suspends the customer that key belongs to (there is no separate key
// identity); the next real gateway call authenticating with that key's
// hash fails immediately. Per gateway's own route table today, this
// only blocks a PRODUCTION customer's two requireActiveCustomer routes
// (POST /quotes, POST /orders) -- no sandbox route checks active status
// at all currently, so revoking a SANDBOX customer's key does not, by
// itself, block their sandbox-route access; opsconsole states this
// plainly rather than implying stronger protection than gateway's real
// route table currently enforces.
func (c *GatewayClient) RevokeAPIKey(ctx context.Context, customerID int64) (AdminAPIKey, error) {
	var out AdminAPIKey
	err := do(ctx, c.http, "gateway", c.adminToken, http.MethodPost, c.baseURL+"/v1/admin/api-keys/"+strconv.FormatInt(customerID, 10)+"/revoke", nil, &out)
	return out, err
}

// AdminWebhookDelivery is C6's own GET /v1/admin/webhooks/deliveries
// row shape (gateway/internal/httpapi/admin_handlers.go's own
// webhookDeliveryResponse).
type AdminWebhookDelivery struct {
	ID            int64      `json:"id"`
	CustomerID    int64      `json:"customer_id"`
	ExternalID    string     `json:"external_id"`
	EventType     string     `json:"event_type"`
	CreatedAt     time.Time  `json:"created_at"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
	AttemptCount  int        `json:"attempt_count"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	LastError     *string    `json:"last_error,omitempty"`
}

// ListWebhookDeliveries calls C6's own GET
// /v1/admin/webhooks/deliveries?status=... ("" lists every row).
func (c *GatewayClient) ListWebhookDeliveries(ctx context.Context, status string) ([]AdminWebhookDelivery, error) {
	u := c.baseURL + "/v1/admin/webhooks/deliveries"
	if status != "" {
		u += "?status=" + status
	}
	var out struct {
		Deliveries []AdminWebhookDelivery `json:"deliveries"`
	}
	err := do(ctx, c.http, "gateway", c.adminToken, http.MethodGet, u, nil, &out)
	return out.Deliveries, err
}

// RedriveWebhookDelivery calls C6's own POST
// /v1/admin/webhooks/deliveries/{id}/redrive -- makes exactly one new
// delivery attempt right now via the same real logic the background
// delivery loop uses, bypassing normal backoff and exhaustion.
func (c *GatewayClient) RedriveWebhookDelivery(ctx context.Context, id int64) (AdminWebhookDelivery, error) {
	var out AdminWebhookDelivery
	err := do(ctx, c.http, "gateway", c.adminToken, http.MethodPost, c.baseURL+"/v1/admin/webhooks/deliveries/"+strconv.FormatInt(id, 10)+"/redrive", nil, &out)
	return out, err
}

// AdminGatewayOrder is C6's own GET /v1/admin/orders row shape
// (gateway/internal/httpapi/admin_handlers.go's own
// adminGatewayOrderResponse) -- gateway's own choreography-tracking
// rows (gateway_orders), across every customer_id, not a re-fetch of
// each order's full C1 state.
type AdminGatewayOrder struct {
	ExternalID        string    `json:"external_id"`
	CustomerID        int64     `json:"customer_id"`
	QuoteID           int64     `json:"quote_id"`
	C1OrderID         int64     `json:"c1_order_id"`
	C1OrderCreated    bool      `json:"c1_order_created"`
	C2AddressAssigned bool      `json:"c2_address_assigned"`
	DepositAddress    *string   `json:"deposit_address,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ListAdminOrders calls C6's own GET /v1/admin/orders.
func (c *GatewayClient) ListAdminOrders(ctx context.Context) ([]AdminGatewayOrder, error) {
	var out struct {
		Orders []AdminGatewayOrder `json:"orders"`
	}
	err := do(ctx, c.http, "gateway", c.adminToken, http.MethodGet, c.baseURL+"/v1/admin/orders", nil, &out)
	return out.Orders, err
}

// RateLimitBucket is one row of C6's own GET /v1/admin/rate-limits
// response.
type RateLimitBucket struct {
	CustomerID      int64   `json:"customer_id"`
	TokensRemaining float64 `json:"tokens_remaining"`
	PerMinute       int     `json:"per_minute"`
}

// RateLimitSnapshot is C6's own GET /v1/admin/rate-limits response --
// Note states plainly that Buckets reflects only this one gateway
// process's own in-memory state since it last started, per
// internal/ratelimit's own real design (no distributed store).
type RateLimitSnapshot struct {
	Buckets []RateLimitBucket `json:"buckets"`
	Note    string            `json:"note"`
}

// GetRateLimits calls C6's own GET /v1/admin/rate-limits.
func (c *GatewayClient) GetRateLimits(ctx context.Context) (RateLimitSnapshot, error) {
	var out RateLimitSnapshot
	err := do(ctx, c.http, "gateway", c.adminToken, http.MethodGet, c.baseURL+"/v1/admin/rate-limits", nil, &out)
	return out, err
}
