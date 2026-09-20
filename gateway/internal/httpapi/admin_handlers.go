package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"gateway/internal/customers"
	"gateway/internal/webhooks"
)

// This file is OC.19's own backend: gateway had no operator-facing
// route at all before it, for anything (docs/03-build/
// admin-panel-build-prompts.md's own "Read this fourth" -- the largest
// real gap that review found). Every route here is service-token
// authenticated (admin_auth.go), never a customer's own sk_live_/
// sk_test_ key.

type apiKeyResponse struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	IsSandbox   bool      `json:"is_sandbox"`
	Status      string    `json:"status"`
	APIKeyLast4 *string   `json:"api_key_last4,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toAPIKeyResponse(c customers.Customer) apiKeyResponse {
	return apiKeyResponse{ID: c.ID, Name: c.Name, IsSandbox: c.IsSandbox, Status: string(c.Status), APIKeyLast4: c.APIKeyLast4, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// apiKeyIssuedResponse is the ONE response shape in this entire service
// that carries a raw secret on purpose. This does not conflict with
// this system's "no secret ever rendered" rule the way a private-key
// export button would (see docs/03-build/admin-panel-build-prompts.md's
// own "Read this second"): a customer API key is this system's OWN
// credential, minted for the explicit purpose of being handed to that
// customer, and gateway already stores only its hash -- this route is
// the existing issuance moment (Store.Create/CreateSandbox/RotateAPIKey
// already return the raw key today, just with no HTTP route reaching
// them) finally getting one, not a new exposure of anything. A signing
// system's KMS key is the opposite case: its entire design promises the
// raw material never leaves KMS, ever, for anyone -- that promise is
// exactly what makes this route safe to have and that one impossible to
// build.
type apiKeyIssuedResponse struct {
	apiKeyResponse
	APIKey string `json:"api_key"`
}

// getAPIKeys is GET /v1/admin/api-keys?customer_id=<id> -- customer_id
// omitted lists every customer's own key metadata; given, returns just
// that one. Never the hash, never the raw key -- only APIKeyLast4 (see
// that field's own doc comment) and creation/rotation metadata.
func (s *Server) getAPIKeys(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("customer_id")
	if raw == "" {
		list, err := s.Customers.List(r.Context())
		if err != nil {
			writeAPIError(w, errInternal)
			return
		}
		out := make([]apiKeyResponse, len(list))
		for i, c := range list {
			out[i] = toAPIKeyResponse(c)
		}
		respondJSON(w, http.StatusOK, map[string]any{"api_keys": out})
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "customer_id must be an integer"))
		return
	}
	c, err := s.Customers.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, customers.ErrNotFound) {
			writeAPIError(w, errNotFound)
			return
		}
		writeAPIError(w, errInternal)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"api_keys": []apiKeyResponse{toAPIKeyResponse(c)}})
}

type postAPIKeyRequest struct {
	CustomerID int64  `json:"customer_id"`
	Mode       string `json:"mode"`
}

// postAPIKey is POST /v1/admin/api-keys. Real customers rows are ONE
// key per row (api_key_hash, singular), not a one-to-many table the
// build doc's own {customer_id, mode} shape implies -- confirmed by
// reading internal/customers/customers.go directly rather than
// assuming. "Issue" for an existing customer_id therefore means
// RotateAPIKey: the customer's prior key stops authenticating
// immediately, and mode is not a free choice -- it must match the
// target customer's own IsSandbox (fixed forever at creation, invariant
// 4: never converted between sandbox and production) or this rejects
// with 400 rather than silently ignoring the mismatch. This route
// creates no new customer -- customer onboarding itself (Store.Create/
// CreateSandbox) has no admin route of its own yet, a real gap this
// chunk's own explicit route list does not cover either.
func (s *Server) postAPIKey(w http.ResponseWriter, r *http.Request) {
	var req postAPIKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CustomerID == 0 {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "customer_id is required"))
		return
	}
	if req.Mode != "live" && req.Mode != "test" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "mode must be \"live\" or \"test\""))
		return
	}

	existing, err := s.Customers.Get(r.Context(), req.CustomerID)
	if err != nil {
		if errors.Is(err, customers.ErrNotFound) {
			writeAPIError(w, errNotFound)
			return
		}
		writeAPIError(w, errInternal)
		return
	}
	wantSandbox := req.Mode == "test"
	if existing.IsSandbox != wantSandbox {
		writeAPIError(w, newAPIError(http.StatusConflict, "mode_mismatch", "customer "+strconv.FormatInt(req.CustomerID, 10)+" is permanently "+modeName(existing.IsSandbox)+"; mode cannot be changed by issuing a key"))
		return
	}

	c, rawKey, err := s.Customers.RotateAPIKey(r.Context(), req.CustomerID)
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}
	slog.Info("gateway: admin rotated api key", "customer_id", c.ID, "actor", adminActorFromContext(r.Context()))
	respondJSON(w, http.StatusCreated, apiKeyIssuedResponse{apiKeyResponse: toAPIKeyResponse(c), APIKey: rawKey})
}

func modeName(isSandbox bool) string {
	if isSandbox {
		return "sandbox"
	}
	return "production"
}

// postRevokeAPIKey is POST /v1/admin/api-keys/{id}/revoke. There is no
// separate "key" identity apart from the customer row it belongs to
// (see postAPIKey's own doc comment), so revoking id's key is
// suspending customer id -- reusing customers.Store.Suspend exactly as
// it already exists, not a new mechanism. authMiddleware's own
// GetByAPIKey lookup then fails for that hash on the very next real
// call (it isn't a status flag a caller could ignore -- see auth.go).
func (s *Server) postRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	if err := s.Customers.Suspend(r.Context(), id); err != nil {
		if errors.Is(err, customers.ErrNotFound) {
			writeAPIError(w, errNotFound)
			return
		}
		writeAPIError(w, errInternal)
		return
	}
	c, err := s.Customers.Get(r.Context(), id)
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}
	slog.Info("gateway: admin revoked api key (suspended customer)", "customer_id", id, "actor", adminActorFromContext(r.Context()))
	respondJSON(w, http.StatusOK, toAPIKeyResponse(c))
}

type webhookDeliveryResponse struct {
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

func toWebhookDeliveryResponse(d webhooks.Delivery) webhookDeliveryResponse {
	return webhookDeliveryResponse{
		ID: d.ID, CustomerID: d.CustomerID, ExternalID: d.ExternalID, EventType: d.EventType,
		CreatedAt: d.CreatedAt, DeliveredAt: d.DeliveredAt, AttemptCount: d.AttemptCount,
		NextAttemptAt: d.NextAttemptAt, LastError: d.LastError,
	}
}

// getWebhookDeliveries is GET /v1/admin/webhooks/deliveries?status=failed
// (status also accepts pending, delivered, or omitted for every row).
func (s *Server) getWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	list, err := s.Webhooks.List(r.Context(), status, 200)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, err.Error()))
		return
	}
	out := make([]webhookDeliveryResponse, len(list))
	for i, d := range list {
		out[i] = toWebhookDeliveryResponse(d)
	}
	respondJSON(w, http.StatusOK, map[string]any{"deliveries": out})
}

// postRedriveWebhookDelivery is POST
// /v1/admin/webhooks/deliveries/{id}/redrive -- manually re-triggers
// one delivery right now via the exact same attempt logic the
// background delivery loop uses (webhooks.Deliverer.Redeliver), outside
// its normal backoff schedule and regardless of whether it has already
// exhausted every automatic attempt.
func (s *Server) postRedriveWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	d, err := s.Deliverer.Redeliver(r.Context(), id)
	if err != nil {
		if errors.Is(err, webhooks.ErrNotFound) {
			writeAPIError(w, errNotFound)
			return
		}
		if errors.Is(err, webhooks.ErrAlreadyDelivered) {
			writeAPIError(w, newAPIError(http.StatusConflict, "already_delivered", "this delivery already succeeded"))
			return
		}
		writeAPIError(w, errInternal)
		return
	}
	slog.Info("gateway: admin redrove webhook delivery", "delivery_id", id, "attempt_count", d.AttemptCount, "actor", adminActorFromContext(r.Context()))
	respondJSON(w, http.StatusOK, toWebhookDeliveryResponse(d))
}

type adminGatewayOrderResponse struct {
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

// getAdminOrders is GET /v1/admin/orders -- cross-customer, unlike the
// customer-facing GET /v1/orders/{external_id} which C6's own
// sandbox-isolation discipline deliberately scopes to the calling API
// key. Lists gateway_orders (this service's own choreography-tracking
// rows, customer_id included) rather than re-fetching each order's full
// C1 state -- the existing ops-console Orders view already covers that
// lens; this route's own job is specifically "which customer does this
// order belong to," across every customer in one call.
func (s *Server) getAdminOrders(w http.ResponseWriter, r *http.Request) {
	list, err := s.Orders.ListAll(r.Context(), 200)
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}
	out := make([]adminGatewayOrderResponse, len(list))
	for i, o := range list {
		out[i] = adminGatewayOrderResponse{
			ExternalID: o.ExternalID, CustomerID: o.CustomerID, QuoteID: o.QuoteID, C1OrderID: o.C1OrderID,
			C1OrderCreated: o.C1OrderCreated, C2AddressAssigned: o.C2AddressAssigned, DepositAddress: o.DepositAddress,
			CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"orders": out})
}

// getRateLimits is GET /v1/admin/rate-limits. C6.3's own limiter is a
// classic in-memory token bucket (internal/ratelimit's own doc comment:
// "no distributed rate-limit store is needed"), so this is real,
// current, per-instance state -- not a fake aggregate, but also not a
// historical or cross-instance view; note explains the scope plainly
// rather than letting the shape alone imply more than it is.
func (s *Server) getRateLimits(w http.ResponseWriter, r *http.Request) {
	snapshot := s.RateLimiter.Snapshot()
	type bucketResponse struct {
		CustomerID      int64   `json:"customer_id"`
		TokensRemaining float64 `json:"tokens_remaining"`
		PerMinute       int     `json:"per_minute"`
	}
	out := make([]bucketResponse, 0, len(snapshot))
	for id, b := range snapshot {
		out = append(out, bucketResponse{CustomerID: id, TokensRemaining: b.TokensRemaining, PerMinute: b.PerMinute})
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"buckets": out,
		"note":    "in-memory, per-instance state only: a customer with no entry has made no request to THIS running gateway process since it last started, not necessarily none at all; a multi-instance deployment needs this queried per instance to see the full picture.",
	})
}
