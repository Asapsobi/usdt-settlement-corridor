package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"energybroker/internal/money"
	"energybroker/internal/provider"
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

var validReservationStatuses = map[string]bool{"PENDING": true, "CONFIRMED": true, "FAILED": true}

// getReservations is GET /v1/reservations?status=&limit= (ops-console-
// build-prompts.md's OC.5) -- the first route to list reservations by
// status; GET /v1/reservations/{id} is, and remains, get-by-id-only.
// Closes the gap this session's own second proof run hit directly: a
// stuck FAILED reservation had to be found with a raw SELECT against
// broker_dev, because nothing else could find it.
func (s *Server) getReservations(w http.ResponseWriter, r *http.Request) {
	statuses := r.URL.Query()["status"]
	if len(statuses) == 0 {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "at least one status query parameter is required"))
		return
	}
	for _, st := range statuses {
		if !validReservationStatuses[st] {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
				fmt.Sprintf("unrecognized status %q, want PENDING, CONFIRMED, or FAILED", st)))
			return
		}
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, ok := parsePositiveInt(w, raw)
		if !ok {
			return
		}
		limit = parsed
	}

	rows, err := s.Reservations.ListByStatus(r.Context(), statuses, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]reservationResponse, len(rows))
	for i, row := range rows {
		out[i] = toReservationResponse(row)
	}
	respondJSON(w, http.StatusOK, map[string]any{"reservations": out})
}

func parsePositiveInt(w http.ResponseWriter, raw string) (int, bool) {
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "limit must be a positive integer"))
		return 0, false
	}
	return v, true
}

type postReservationReconcileRequest struct {
	OrderID       int64  `json:"order_id"`
	Provider      string `json:"provider"`
	DelegationID  string `json:"delegation_id"`
	TargetAddress string `json:"target_address"`
	EnergyUnits   int64  `json:"energy_units"`
	CostTRX       string `json:"cost_trx"`
}

// postReservationReconcile is POST /v1/reservations/{id}/reconcile --
// the HTTP form of cmd/reconcile-reservation's own ManualConfirm call,
// added because that operation had no HTTP route at all before now, only
// a CLI needing shell access to whichever machine runs brokerd. Kept
// alongside the CLI (docs/03-build/ops-console-build-prompts.md's OC.5:
// "an additional way to reach the same operation, not a replacement").
// Exactly cmd/reconcile-reservation/main.go's own doc comment applies
// here too: the caller is vouching for a delegation independently
// verified real (e.g. read directly on-chain) -- this is never called
// from Create's own request path, and nothing here re-derives or
// double-checks that verification.
func (s *Server) postReservationReconcile(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	var req postReservationReconcileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.OrderID == 0 || req.Provider == "" || req.DelegationID == "" || req.TargetAddress == "" || req.EnergyUnits <= 0 || req.CostTRX == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
			"order_id, provider, delegation_id, target_address, energy_units (>0), and cost_trx are all required"))
		return
	}
	cost, err := money.ParseDecimal(req.CostTRX)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "cost_trx: "+err.Error()))
		return
	}

	now := time.Now().UTC()
	delegation := provider.Delegation{
		ID: req.DelegationID, ProviderName: req.Provider, TargetAddress: req.TargetAddress,
		EnergyUnits: req.EnergyUnits, CostTRX: cost, RequestedAt: now, ExpiresAt: now.Add(time.Hour), ConfirmedAt: &now,
	}

	res, err := s.Reservations.ManualConfirm(r.Context(), id, req.OrderID, delegation)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toReservationResponse(res))
}
