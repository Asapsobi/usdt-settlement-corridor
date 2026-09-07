package httpapi

import "net/http"

type providerTotalResponse struct {
	ProviderName string `json:"provider_name"`
	Available    int64  `json:"available"`
	Reserved     int64  `json:"reserved"`
}

// getBuffer is GET /v1/buffer -- current AVAILABLE/RESERVED totals by
// provider, for ops visibility, per this chunk's own build spec.
func (s *Server) getBuffer(w http.ResponseWriter, r *http.Request) {
	totals, err := s.Buffer.Totals(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]providerTotalResponse, len(totals))
	for i, t := range totals {
		out[i] = providerTotalResponse{ProviderName: t.ProviderName, Available: t.Available, Reserved: t.Reserved}
	}
	respondJSON(w, http.StatusOK, map[string]any{"providers": out})
}
