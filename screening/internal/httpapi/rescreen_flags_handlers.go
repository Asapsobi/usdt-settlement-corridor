package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"screening/internal/rescreen"
)

type rescreenFlagResponse struct {
	ID                    int64      `json:"id"`
	OrderID               int64      `json:"order_id"`
	ExternalID            string     `json:"external_id"`
	OrderStateAtDetection string     `json:"order_state_at_detection"`
	PreviousVerdictID     int64      `json:"previous_verdict_id"`
	NewVerdictID          int64      `json:"new_verdict_id"`
	DetectedAt            time.Time  `json:"detected_at"`
	Resolution            *string    `json:"resolution"`
	ResolvedAt            *time.Time `json:"resolved_at"`
	ResolvedBy            *string    `json:"resolved_by"`
}

func toRescreenFlagResponse(f rescreen.Flag) rescreenFlagResponse {
	return rescreenFlagResponse{
		ID: f.ID, OrderID: f.OrderID, ExternalID: f.ExternalID, OrderStateAtDetection: f.OrderStateAtDetection,
		PreviousVerdictID: f.PreviousVerdictID, NewVerdictID: f.NewVerdictID, DetectedAt: f.DetectedAt,
		Resolution: f.Resolution, ResolvedAt: f.ResolvedAt, ResolvedBy: f.ResolvedBy,
	}
}

// getRescreenFlags is GET /v1/rescreen-flags?resolved=<bool> -- resolved
// omitted lists every flag (newest first); resolved=true/false filters.
func (s *Server) getRescreenFlags(w http.ResponseWriter, r *http.Request) {
	var resolved *bool
	if raw := r.URL.Query().Get("resolved"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "resolved must be true or false"))
			return
		}
		resolved = &parsed
	}

	list, err := rescreen.List(r.Context(), s.Pool, resolved)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]rescreenFlagResponse, len(list))
	for i, f := range list {
		out[i] = toRescreenFlagResponse(f)
	}
	respondJSON(w, http.StatusOK, map[string]any{"rescreen_flags": out})
}

type resolveRescreenFlagRequest struct {
	Resolution string `json:"resolution"`
	Actor      string `json:"actor"`
}

// postResolveRescreenFlag is POST /v1/rescreen-flags/{id}/resolve --
// purely a record of a human's decision (this package never acts on
// it, see internal/rescreen's own build spec: mechanism, not policy).
func (s *Server) postResolveRescreenFlag(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	var req resolveRescreenFlagRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if err := rescreen.Resolve(r.Context(), s.Pool, id, req.Resolution, req.Actor); err != nil {
		writeErr(w, err)
		return
	}

	f, err := rescreen.Get(r.Context(), s.Pool, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toRescreenFlagResponse(f))
}
