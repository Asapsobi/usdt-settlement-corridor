package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Ledger calls C1 -- POST/GET /v1/orders only, the two calls this
// driver needs (ledger/docs/openapi.yaml).
type Ledger struct{ c client }

// NewLedger returns a Ledger for baseURL, authenticating with token.
func NewLedger(baseURL, token string) *Ledger {
	return &Ledger{c: newClient("C1 (ledger)", baseURL, token)}
}

// Order mirrors C1's own Order response shape.
type Order struct {
	ID               int64     `json:"id"`
	ExternalID       string    `json:"external_id"`
	CustomerID       string    `json:"customer_id"`
	Tier             string    `json:"tier"`
	State            string    `json:"state"`
	AmountIn         string    `json:"amount_in"`
	AmountOut        string    `json:"amount_out"`
	FeeUnits         string    `json:"fee_units"`
	NetworkFeeUnits  string    `json:"network_fee_units"`
	RecipientAddress string    `json:"recipient_address"`
	QuotedAt         time.Time `json:"quoted_at"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
	Version          int32     `json:"version"`
}

// PostOrderRequest mirrors C1's own PostOrderRequest body.
type PostOrderRequest struct {
	ExternalID       string    `json:"external_id"`
	CustomerID       string    `json:"customer_id"`
	Tier             string    `json:"tier"`
	AmountIn         string    `json:"amount_in"`
	AmountOut        string    `json:"amount_out"`
	FeeUnits         string    `json:"fee_units"`
	NetworkFeeUnits  string    `json:"network_fee_units"`
	RecipientAddress string    `json:"recipient_address"`
	QuotedAt         time.Time `json:"quoted_at"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
}

// CreateOrder calls POST /v1/orders. idempotencyKey is required by C1's
// own blanket "no header, no write" rule.
func (l *Ledger) CreateOrder(ctx context.Context, req PostOrderRequest, idempotencyKey string) (Order, error) {
	status, body, err := l.c.do(ctx, http.MethodPost, "/v1/orders", idempotencyKey, req)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusCreated {
		return Order{}, decodeAPIError(l.c.component, status, body)
	}
	var order Order
	if err := json.Unmarshal(body, &order); err != nil {
		return Order{}, fmt.Errorf("upstream: decoding CreateOrder response: %w", err)
	}
	return order, nil
}

// GetOrder calls GET /v1/orders/{externalID}.
func (l *Ledger) GetOrder(ctx context.Context, externalID string) (Order, error) {
	status, body, err := l.c.do(ctx, http.MethodGet, "/v1/orders/"+externalID, "", nil)
	if err != nil {
		return Order{}, err
	}
	if status != http.StatusOK {
		return Order{}, decodeAPIError(l.c.component, status, body)
	}
	var order Order
	if err := json.Unmarshal(body, &order); err != nil {
		return Order{}, fmt.Errorf("upstream: decoding GetOrder response: %w", err)
	}
	return order, nil
}
