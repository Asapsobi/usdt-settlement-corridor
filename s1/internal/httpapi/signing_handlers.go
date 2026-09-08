package httpapi

import (
	"encoding/hex"
	"net/http"
	"time"

	"s1/internal/requests"
)

type signingRequestResponse struct {
	ID        int64     `json:"id"`
	Status    string    `json:"status"`
	SignedTx  *string   `json:"signed_tx"`
	CreatedAt time.Time `json:"created_at"`
}

func toSigningRequestResponse(req requests.SigningRequest) signingRequestResponse {
	resp := signingRequestResponse{ID: req.ID, Status: string(req.Status), CreatedAt: req.CreatedAt}
	if req.Status == requests.StatusSigned {
		hexTx := hex.EncodeToString(req.SignedTx[:])
		resp.SignedTx = &hexTx
	}
	return resp
}

type postSigningRequestRequest struct {
	SlotID         int     `json:"slot_id"`
	Digest         string  `json:"digest"`
	EstimatedUSD   float64 `json:"estimated_usd"`
	IdempotencyKey string  `json:"idempotency_key"`
}

// postSigningRequest is POST /v1/signing-requests -- C5's own entry
// point into SigningService.RequestSignature.
func (s *Server) postSigningRequest(w http.ResponseWriter, r *http.Request) {
	var req postSigningRequestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.IdempotencyKey == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "idempotency_key is required"))
		return
	}

	digestBytes, err := hex.DecodeString(req.Digest)
	if err != nil || len(digestBytes) != 32 {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "digest must be 32 bytes, hex-encoded"))
		return
	}
	var digest [32]byte
	copy(digest[:], digestBytes)

	result, err := s.Signing.RequestSignature(r.Context(), req.SlotID, digest, req.EstimatedUSD, req.IdempotencyKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.recordSigningRequest(result.Status)
	respondJSON(w, http.StatusCreated, toSigningRequestResponse(result))
}

// getSigningRequest is GET /v1/signing-requests/{id}.
func (s *Server) getSigningRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	result, err := s.Signing.GetSignature(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toSigningRequestResponse(result))
}

// getSlotAddress is GET /v1/slots/{id}/address -- SigningService's own
// SlotAddress, over HTTP.
func (s *Server) getSlotAddress(w http.ResponseWriter, r *http.Request) {
	slotID, ok := urlParamInt(w, r, "id")
	if !ok {
		return
	}
	addr, err := s.Signing.SlotAddress(r.Context(), slotID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"tron_address": addr})
}

// postApprove is POST /v1/signing-requests/{id}/approve -- approver-only
// (see auth.go). actor is resolved from the bearer token, never taken
// from the request body, same posture every prior component's own
// reviewer-facing endpoint takes toward actor identity.
func (s *Server) postApprove(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	result, err := s.Signing.Approve(r.Context(), id, actorFromContext(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.recordApproval("APPROVE")
	respondJSON(w, http.StatusOK, toSigningRequestResponse(result))
}

// postReject is POST /v1/signing-requests/{id}/reject -- approver-only.
func (s *Server) postReject(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	result, err := s.Signing.Reject(r.Context(), id, actorFromContext(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.recordApproval("REJECT")
	respondJSON(w, http.StatusOK, toSigningRequestResponse(result))
}
