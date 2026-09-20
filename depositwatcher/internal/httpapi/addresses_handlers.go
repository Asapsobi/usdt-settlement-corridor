package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"depositwatcher/internal/addresses"
)

type addressResponse struct {
	Address         string     `json:"address"`
	DerivationIndex uint32     `json:"derivation_index"`
	OrderID         int64      `json:"order_id"`
	ExternalID      string     `json:"external_id"`
	CustomerID      string     `json:"customer_id"`
	Status          string     `json:"status"`
	QuotedAt        time.Time  `json:"quoted_at"`
	QuoteExpiresAt  time.Time  `json:"quote_expires_at"`
	AssignedAt      time.Time  `json:"assigned_at"`
	RetiredAt       *time.Time `json:"retired_at,omitempty"`
	RetiredReason   *string    `json:"retired_reason,omitempty"`
}

func toAddressResponse(wa addresses.WatchedAddress) addressResponse {
	return addressResponse{
		Address: string(wa.Address), DerivationIndex: wa.DerivationIndex, OrderID: wa.OrderID,
		ExternalID: wa.ExternalID, CustomerID: wa.CustomerID,
		Status: string(wa.Status), QuotedAt: wa.QuotedAt, QuoteExpiresAt: wa.QuoteExpiresAt,
		AssignedAt: wa.AssignedAt, RetiredAt: wa.RetiredAt, RetiredReason: wa.RetiredReason,
	}
}

// orderIDParam parses the {order_id} path parameter, common to all three
// address routes.
func orderIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw, ok := urlParam(w, r, "order_id")
	if !ok {
		return 0, false
	}
	orderID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "order_id must be an integer"))
		return 0, false
	}
	return orderID, true
}

type postAddressRequest struct {
	OrderID        int64     `json:"order_id"`
	ExternalID     string    `json:"external_id"`
	CustomerID     string    `json:"customer_id"`
	QuotedAt       time.Time `json:"quoted_at"`
	QuoteExpiresAt time.Time `json:"quote_expires_at"`
}

// postAddress is POST /v1/addresses. Always 200, never 201-then-200:
// idempotent on order_id (internal/addresses.Assign's own guarantee),
// and the build spec is explicit that a first-time assignment and a
// replay of one are not meant to be distinguishable by status code here,
// unlike C1's Created-vs-Replayed journal entries.
func (s *Server) postAddress(w http.ResponseWriter, r *http.Request) {
	var req postAddressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.OrderID == 0 || req.ExternalID == "" || req.CustomerID == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
			"order_id, external_id, and customer_id are required"))
		return
	}

	addr, err := addresses.Assign(r.Context(), s.Pool, req.OrderID, req.ExternalID, req.CustomerID, req.QuotedAt, req.QuoteExpiresAt)
	if err != nil {
		writeErr(w, err)
		return
	}

	wa, err := addresses.GetByOrderID(r.Context(), s.Pool, req.OrderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	_ = addr // already reflected in wa.Address; kept for clarity that Assign's own return value is the source of truth for what was assigned
	respondJSON(w, http.StatusOK, toAddressResponse(wa))
}

// getAddresses is GET /v1/addresses -- lists every watched address
// regardless of status, newest first (ops-console-build-prompts.md's
// OC.11: the console's own Sweep page needs RETIRED addresses too,
// since a settled order's deposit address still holds its real on-chain
// balance until an operator manually sweeps it).
func (s *Server) getAddresses(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "limit must be a positive integer"))
			return
		}
		limit = parsed
	}
	list, err := addresses.ListAll(r.Context(), s.Pool, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]addressResponse, len(list))
	for i, wa := range list {
		out[i] = toAddressResponse(wa)
	}
	respondJSON(w, http.StatusOK, map[string]any{"addresses": out})
}

// getAddressBalance is GET /v1/addresses/{order_id}/balance -- an
// on-demand, single-provider read (see chain.Pool.ERC20BalanceOf's own
// doc comment for why this one doesn't need 2-provider agreement) of
// the watched token's current balance at that address. 503
// system_component_not_ready if this instance has no ChainPool
// configured, same posture as getProviders.
func (s *Server) getAddressBalance(w http.ResponseWriter, r *http.Request) {
	orderID, ok := orderIDParam(w, r)
	if !ok {
		return
	}
	if s.ChainPool == nil {
		writeAPIError(w, errSystemComponentNotReady)
		return
	}
	wa, err := addresses.GetByOrderID(r.Context(), s.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	balance, err := s.ChainPool.ERC20BalanceOf(r.Context(), s.ContractAddress, common.HexToAddress(string(wa.Address)))
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadGateway, "upstream_error", "reading on-chain balance: "+err.Error()))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"address": string(wa.Address), "balance_raw": balance.String()})
}

// getAddress is GET /v1/addresses/{order_id}.
func (s *Server) getAddress(w http.ResponseWriter, r *http.Request) {
	orderID, ok := orderIDParam(w, r)
	if !ok {
		return
	}
	wa, err := addresses.GetByOrderID(r.Context(), s.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toAddressResponse(wa))
}

type postRetireAddressRequest struct {
	Reason string `json:"reason"`
}

// postRetireAddress is POST /v1/addresses/{order_id}/retire.
func (s *Server) postRetireAddress(w http.ResponseWriter, r *http.Request) {
	orderID, ok := orderIDParam(w, r)
	if !ok {
		return
	}
	var req postRetireAddressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "reason is required"))
		return
	}

	if err := addresses.Retire(r.Context(), s.Pool, orderID, req.Reason); err != nil {
		writeErr(w, err)
		return
	}
	wa, err := addresses.GetByOrderID(r.Context(), s.Pool, orderID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toAddressResponse(wa))
}
