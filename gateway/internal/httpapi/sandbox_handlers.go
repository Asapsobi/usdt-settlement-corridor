package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gateway/internal/money"
	"gateway/internal/pricing"
	"gateway/internal/sandbox"
)

type postSandboxOrderRequest struct {
	Tier             string `json:"tier"`
	AmountIn         string `json:"amount_in"`
	RecipientAddress string `json:"recipient_address"`
	Trigger          string `json:"trigger"`
}

type sandboxOrderResponse struct {
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

func sandboxOrderResponseFrom(o sandbox.Order) sandboxOrderResponse {
	return sandboxOrderResponse{
		ExternalID: o.ExternalID, Trigger: string(o.Trigger), Tier: string(o.Tier),
		AmountIn: o.AmountIn.Format(), AmountOut: o.AmountOut.Format(),
		FeeUnits: o.FeeUnits.Format(), NetworkFeeUnits: o.NetworkFeeUnits.Format(),
		RecipientAddress: o.RecipientAddress, DepositAddress: o.DepositAddress, State: o.State,
		HoldReason: o.HoldReason, WebhookAttempts: o.WebhookAttempts, WebhookExhausted: o.WebhookExhausted,
	}
}

// postSandboxOrder is POST /v1/sandbox/orders -- C6.7. Unlike
// production's own quote-then-order split, a sandbox order is priced
// and created in one call: the point of this endpoint is a documented,
// deterministic scripted outcome, not a faithful re-creation of every
// production step. trigger selects which of the four documented
// scenarios this order deterministically plays out.
func (s *Server) postSandboxOrder(w http.ResponseWriter, r *http.Request) {
	var req postSandboxOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Tier == "" || req.AmountIn == "" || req.RecipientAddress == "" || req.Trigger == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "tier, amount_in, recipient_address, and trigger are all required"))
		return
	}
	if !sandbox.ValidTrigger(req.Trigger) {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
			"trigger must be one of: reorg, screening_hold, energy_exhaustion, retry_storm"))
		return
	}

	amountIn, err := money.ParseDecimal(req.AmountIn)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "amount_in: "+err.Error()))
		return
	}

	s.Metrics.SandboxRequestsTotal.WithLabelValues(req.Trigger).Inc()

	customer := customerFromContext(r.Context())
	o, err := s.Sandbox.CreateOrder(r.Context(), customer.ID, sandbox.Trigger(req.Trigger), pricing.Tier(req.Tier), amountIn, req.RecipientAddress)
	if err != nil {
		if errors.Is(err, pricing.ErrUnknownTier) || errors.Is(err, pricing.ErrNonPositiveAmount) || errors.Is(err, pricing.ErrAmountTooSmall) {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, err.Error()))
			return
		}
		writeAPIError(w, errInternal)
		return
	}

	respondJSON(w, http.StatusCreated, sandboxOrderResponseFrom(o))
}

// getSandboxOrderStatus is GET /v1/sandbox/orders/{external_id} --
// reads sandbox_orders only, never gateway_orders; a sandbox
// external_id is invisible to the production GET /v1/orders/{id}
// (C6.5) and vice versa, by construction (invariant 4).
func (s *Server) getSandboxOrderStatus(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	customer := customerFromContext(r.Context())

	o, err := s.Sandbox.Get(r.Context(), externalID, customer.ID)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeAPIError(w, errNotFound)
			return
		}
		writeAPIError(w, errInternal)
		return
	}

	respondJSON(w, http.StatusOK, sandboxOrderResponseFrom(o))
}
