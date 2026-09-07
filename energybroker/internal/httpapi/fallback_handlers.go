package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"energybroker/internal/routing"
)

type fallbackEventResponse struct {
	ID          int64      `json:"id"`
	TriggeredAt time.Time  `json:"triggered_at"`
	Reason      string     `json:"reason"`
	OrderID     *int64     `json:"order_id"`
	ResolvedAt  *time.Time `json:"resolved_at"`
	Resolution  *string    `json:"resolution"`
	ResolvedBy  *string    `json:"resolved_by"`
}

func toFallbackEventResponse(e routing.FallbackEvent) fallbackEventResponse {
	return fallbackEventResponse{
		ID: e.ID, TriggeredAt: e.TriggeredAt, Reason: e.Reason, OrderID: e.OrderID,
		ResolvedAt: e.ResolvedAt, Resolution: e.Resolution, ResolvedBy: e.ResolvedBy,
	}
}

// getManualFallbackEvents is GET /v1/manual-fallback-events?resolved=false
// -- resolved omitted lists every event (newest first); resolved=true or
// resolved=false filters to exactly that state.
func (s *Server) getManualFallbackEvents(w http.ResponseWriter, r *http.Request) {
	var resolved *bool
	if raw := r.URL.Query().Get("resolved"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "resolved must be true or false"))
			return
		}
		resolved = &parsed
	}

	list, err := routing.ListFallbackEvents(r.Context(), s.Pool, resolved)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]fallbackEventResponse, len(list))
	for i, e := range list {
		out[i] = toFallbackEventResponse(e)
	}
	respondJSON(w, http.StatusOK, map[string]any{"events": out})
}

type resolveFallbackEventRequest struct {
	Resolution string `json:"resolution"`
	Actor      string `json:"actor"`
}

// postResolveManualFallbackEvent is
// POST /v1/manual-fallback-events/{id}/resolve, body: resolution, actor
// -- per this chunk's own build spec. actor comes from the request
// body, not this API's own bearer auth, the same posture screening's
// own hold-resolution endpoints take: the ops tool calling this
// establishes who the human operator is.
func (s *Server) postResolveManualFallbackEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	var req resolveFallbackEventRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if err := s.Router.Resolve(r.Context(), id, req.Resolution, req.Actor); err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.ManualFallbackEventResolved()

	event, err := s.Router.GetFallbackEvent(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toFallbackEventResponse(event))
}
