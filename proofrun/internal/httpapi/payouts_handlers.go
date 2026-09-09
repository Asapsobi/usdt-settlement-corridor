package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"proofrun/internal/driver"
)

type postPayoutRequest struct {
	ExternalID           string `json:"external_id"`
	CustomerID           string `json:"customer_id"`
	RecipientTronAddress string `json:"recipient_tron_address"`
	AmountIn             string `json:"amount_in"`
}

type postPayoutResponse struct {
	ExternalID      string `json:"external_id"`
	OrderID         int64  `json:"order_id"`
	DepositAddress  string `json:"deposit_address"`
	AmountIn        string `json:"amount_in"`
	AmountOut       string `json:"amount_out"`
	FeeUnits        string `json:"fee_units"`
	NetworkFeeUnits string `json:"network_fee_units"`
	QuoteExpiresAt  string `json:"quote_expires_at"`
}

// postPayout is POST /v1/payouts -- docs/03-build/mvp-proof-run-plan.md's
// own "one entrypoint that takes {amount, recipient_tron_address,
// external_id}... and returns the deposit address."
func (s *Server) postPayout(w http.ResponseWriter, r *http.Request) {
	var req postPayoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ExternalID == "" || req.CustomerID == "" || req.RecipientTronAddress == "" || req.AmountIn == "" {
		writeError(w, http.StatusBadRequest, errors.New("external_id, customer_id, recipient_tron_address, and amount_in are all required"))
		return
	}

	result, err := s.Driver.CreatePayout(r.Context(), driver.CreatePayoutRequest{
		ExternalID: req.ExternalID, CustomerID: req.CustomerID,
		RecipientTronAddress: req.RecipientTronAddress, AmountIn: req.AmountIn,
	})
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, driver.ErrAmountTooSmall) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err)
		return
	}

	respondJSON(w, http.StatusCreated, postPayoutResponse{
		ExternalID: result.ExternalID, OrderID: result.OrderID, DepositAddress: result.DepositAddress,
		AmountIn: result.AmountIn, AmountOut: result.AmountOut,
		FeeUnits: result.FeeUnits, NetworkFeeUnits: result.NetworkFeeUnits,
		QuoteExpiresAt: result.QuoteExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

type getPayoutResponse struct {
	ExternalID       string  `json:"external_id"`
	OrderID          int64   `json:"order_id"`
	State            string  `json:"state"`
	AmountIn         string  `json:"amount_in"`
	AmountOut        string  `json:"amount_out"`
	RecipientAddress string  `json:"recipient_address"`
	DepositAddress   *string `json:"deposit_address,omitempty"`
	AddressStatus    *string `json:"deposit_address_status,omitempty"`
	AddressError     string  `json:"deposit_address_error,omitempty"`
	DispatchStatus   *string `json:"dispatch_status,omitempty"`
	SlotID           *int    `json:"slot_id,omitempty"`
	DispatchError    string  `json:"dispatch_error,omitempty"`
	PayoutTxHash     *string `json:"payout_tx_hash,omitempty"`
	Note             string  `json:"note"`
}

// getPayout is GET /v1/payouts/{external_id} -- the plan's own "one
// status entrypoint." deposit_tx_hash and energy_reservation_id are
// deliberately never in this response -- see driver.PayoutStatus's own
// doc comment for why, echoed in this response's own Note field so a
// caller reading only the JSON (not this repo's source) still sees it.
func (s *Server) getPayout(w http.ResponseWriter, r *http.Request) {
	externalID := chi.URLParam(r, "external_id")
	status, err := s.Driver.GetStatus(r.Context(), externalID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	resp := getPayoutResponse{
		ExternalID: status.ExternalID, OrderID: status.Order.ID, State: status.Order.State,
		AmountIn: status.Order.AmountIn, AmountOut: status.Order.AmountOut,
		RecipientAddress: status.Order.RecipientAddress,
		PayoutTxHash:      status.PayoutTxHash,
		AddressError:      status.AddressError,
		DispatchError:     status.DispatchError,
		Note: "deposit_tx_hash and energy_reservation_id are not exposed by C2/C4's own HTTP APIs today " +
			"(see proofrun/internal/driver's own doc comment) -- not fabricated, genuinely unavailable from this driver",
	}
	if status.Address != nil {
		resp.DepositAddress = &status.Address.Address
		resp.AddressStatus = &status.Address.Status
	}
	if status.Dispatch != nil {
		resp.DispatchStatus = &status.Dispatch.Status
		resp.SlotID = &status.Dispatch.SlotID
	}

	respondJSON(w, http.StatusOK, resp)
}
