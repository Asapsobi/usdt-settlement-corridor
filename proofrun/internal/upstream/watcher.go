package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Watcher calls C2 -- POST /v1/addresses and GET /v1/addresses/{order_id}
// only, the two calls this driver needs
// (depositwatcher/internal/httpapi/addresses_handlers.go).
type Watcher struct{ c client }

// NewWatcher returns a Watcher for baseURL, authenticating with token.
func NewWatcher(baseURL, token string) *Watcher {
	return &Watcher{c: newClient("C2 (deposit watcher)", baseURL, token)}
}

// WatchedAddress mirrors C2's own address response shape. Status is one
// of WATCHING, FUNDED, RETIRED (depositwatcher/internal/addresses.Status)
// -- FUNDED is this driver's own signal that a real BSC deposit finalized.
type WatchedAddress struct {
	Address        string     `json:"address"`
	OrderID        int64      `json:"order_id"`
	ExternalID     string     `json:"external_id"`
	CustomerID     string     `json:"customer_id"`
	Status         string     `json:"status"`
	QuotedAt       time.Time  `json:"quoted_at"`
	QuoteExpiresAt time.Time  `json:"quote_expires_at"`
	AssignedAt     time.Time  `json:"assigned_at"`
	RetiredAt      *time.Time `json:"retired_at,omitempty"`
	RetiredReason  *string    `json:"retired_reason,omitempty"`
}

type postAddressRequest struct {
	OrderID        int64     `json:"order_id"`
	ExternalID     string    `json:"external_id"`
	CustomerID     string    `json:"customer_id"`
	QuotedAt       time.Time `json:"quoted_at"`
	QuoteExpiresAt time.Time `json:"quote_expires_at"`
}

// AssignAddress calls POST /v1/addresses -- idempotent on orderID
// (C2's own contract): safe to call once per order, and safe to retry.
func (w *Watcher) AssignAddress(ctx context.Context, orderID int64, externalID, customerID string, quotedAt, quoteExpiresAt time.Time, idempotencyKey string) (WatchedAddress, error) {
	status, body, err := w.c.do(ctx, http.MethodPost, "/v1/addresses", idempotencyKey, postAddressRequest{
		OrderID: orderID, ExternalID: externalID, CustomerID: customerID,
		QuotedAt: quotedAt, QuoteExpiresAt: quoteExpiresAt,
	})
	if err != nil {
		return WatchedAddress{}, err
	}
	if status != http.StatusOK {
		return WatchedAddress{}, decodeAPIError(w.c.component, status, body)
	}
	var addr WatchedAddress
	if err := json.Unmarshal(body, &addr); err != nil {
		return WatchedAddress{}, fmt.Errorf("upstream: decoding AssignAddress response: %w", err)
	}
	return addr, nil
}

// GetAddress calls GET /v1/addresses/{orderID}.
func (w *Watcher) GetAddress(ctx context.Context, orderID int64) (WatchedAddress, error) {
	status, body, err := w.c.do(ctx, http.MethodGet, "/v1/addresses/"+strconv.FormatInt(orderID, 10), "", nil)
	if err != nil {
		return WatchedAddress{}, err
	}
	if status != http.StatusOK {
		return WatchedAddress{}, decodeAPIError(w.c.component, status, body)
	}
	var addr WatchedAddress
	if err := json.Unmarshal(body, &addr); err != nil {
		return WatchedAddress{}, fmt.Errorf("upstream: decoding GetAddress response: %w", err)
	}
	return addr, nil
}
