package httpapi

import (
	"net/http"
	"time"

	"energybroker/internal/reservations"
)

type reservationResponse struct {
	ID            int64      `json:"id"`
	ExternalID    string     `json:"external_id"`
	OrderID       int64      `json:"order_id"`
	TargetAddress string     `json:"target_address"`
	EnergyUnits   int64      `json:"energy_units"`
	Tier          string     `json:"tier"`
	Status        string     `json:"status"`
	Vendor        *string    `json:"vendor"`
	CostTRX       *string    `json:"cost_trx"`
	ConfirmedAt   *time.Time `json:"confirmed_at"`
	Deadline      time.Time  `json:"deadline"`
	CreatedAt     time.Time  `json:"created_at"`
	FastPath      *bool      `json:"fast_path"`
}

func toReservationResponse(r reservations.Reservation) reservationResponse {
	var cost *string
	if r.CostTRX != nil {
		formatted := r.CostTRX.Format()
		cost = &formatted
	}
	return reservationResponse{
		ID: r.ID, ExternalID: r.ExternalID, OrderID: r.OrderID, TargetAddress: r.TargetAddress,
		EnergyUnits: r.EnergyUnits, Tier: r.Tier, Status: string(r.Status), Vendor: r.Vendor,
		CostTRX: cost, ConfirmedAt: r.ConfirmedAt, Deadline: r.Deadline, CreatedAt: r.CreatedAt,
		FastPath: r.FastPath,
	}
}

type postReservationRequest struct {
	ExternalID    string    `json:"external_id"`
	TargetAddress string    `json:"target_address"`
	EnergyUnits   int64     `json:"energy_units"`
	Tier          string    `json:"tier"`
	Deadline      time.Time `json:"deadline"`
}

// postReservation is POST /v1/reservations, §A's own "With C5" contract.
//
// A deliberate departure from §A's own speculative sketch (written
// before C4.4 existed): that sketch described an async 202-pending
// response the caller would then poll GET /v1/reservations/{id} to
// resolve. C4.4, once actually built, made Create fully synchronous --
// it blocks through the fast path or the slow path's own retry-until-
// deadline loop and returns the FINAL status directly. Re-architecting
// into a background-job-plus-poll model now, in the HTTP-boundary
// chunk, would mean redoing already-built, already-tested C4.4/C4.6
// logic for no functional gain C5 actually needs -- a synchronous
// call that blocks up to the caller's own deadline is a normal, common
// shape for a service-to-service authorization-style call, as long as
// the caller's own HTTP client timeout is set accordingly (documented
// in docs/openapi.yaml). This handler returns 201 Created with the
// reservation's own FINAL status (CONFIRMED or FAILED) rather than a
// 202 with "pending" -- Create() never returns while a reservation is
// still actually pending.
func (s *Server) postReservation(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "Idempotency-Key header is required"))
		return
	}

	var req postReservationRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	res, err := s.Reservations.Create(r.Context(), reservations.Request{
		IdempotencyKey: idempotencyKey,
		ExternalID:     req.ExternalID,
		TargetAddress:  req.TargetAddress,
		EnergyUnits:    req.EnergyUnits,
		Tier:           req.Tier,
		Deadline:       req.Deadline,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, toReservationResponse(res))
}

// getReservation is GET /v1/reservations/{id}.
func (s *Server) getReservation(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	res, err := s.Reservations.Get(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toReservationResponse(res))
}
