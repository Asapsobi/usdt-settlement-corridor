package opclient

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// GatewayClient calls one C6 (API gateway) instance. Unlike every other
// client in this package, it authenticates with a sandbox customer's
// own sk_test_ API key, not a service-to-service bearer token -- C6
// has no operator-auth concept of its own (see docs/03-build/
// ops-console-build-prompts.md's OC.12), so the console holds one
// sandbox customer's key the same way it holds every other service's
// own token. This client only ever reaches sandbox routes; it must
// never be given a production sk_live_ key.
type GatewayClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewGatewayClient returns a GatewayClient for baseURL, authenticating
// every call with sandboxAPIKey.
func NewGatewayClient(baseURL, sandboxAPIKey string) *GatewayClient {
	return &GatewayClient{baseURL: strings.TrimRight(baseURL, "/"), token: sandboxAPIKey, http: &http.Client{Timeout: 5 * time.Second}}
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
	err := do(ctx, c.http, "gateway", c.token, http.MethodGet, c.baseURL+"/v1/sandbox/orders", nil, &out)
	return out.Orders, err
}
