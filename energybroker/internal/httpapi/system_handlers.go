package httpapi

import (
	"net/http"
	"time"

	"energybroker/internal/reservations"
	"energybroker/internal/routing"
)

type healthzResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	version, commit := "unknown", "unknown"
	if s.BuildInfo != nil {
		version, commit = s.BuildInfo()
	}
	respondJSON(w, http.StatusOK, healthzResponse{Status: "ok", Version: version, Commit: commit})
}

// readyzHandler additionally checks the database is actually reachable
// -- healthz says the process is up, readyz says it can do its job.
// Same distinction as every prior component's own readyz.
func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.Pool.Ping(r.Context()); err != nil {
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready", "error": err.Error()})
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type priceStatusResponse struct {
	ProviderName    string     `json:"provider_name"`
	PricePerUnitSun *float64   `json:"price_per_unit_sun"`
	ObservedAt      *time.Time `json:"observed_at"`
	Healthy         bool       `json:"healthy"`
	Error           *string    `json:"error"`
}

// getSystemPrices is GET /v1/system/prices -- latest CurrentPrice per
// provider, health flags, and the configured ceiling, per this chunk's
// own build spec.
func (s *Server) getSystemPrices(w http.ResponseWriter, r *http.Request) {
	snapshot := s.Poller.Snapshot(r.Context())
	out := make([]priceStatusResponse, len(snapshot))
	for i, p := range snapshot {
		resp := priceStatusResponse{ProviderName: p.ProviderName, Healthy: p.Healthy}
		if p.Healthy {
			price := p.PricePerUnitSun
			observedAt := p.ObservedAt
			resp.PricePerUnitSun = &price
			resp.ObservedAt = &observedAt
		} else {
			errText := p.Error
			resp.Error = &errText
		}
		out[i] = resp
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"providers":            out,
		"ceiling_sun_per_unit": s.Ceiling,
		"weights":              s.Weights,
	})
}

type invariantsResponse struct {
	BufferAvailable          int64    `json:"buffer_available"`
	BufferTarget             int64    `json:"buffer_target"`
	ReconciliationLagSeconds *float64 `json:"reconciliation_lag_seconds"`
	FastPathConfirmedCount   int64    `json:"fast_path_confirmed_count"`
	SlowPathConfirmedCount   int64    `json:"slow_path_confirmed_count"`
	FastPathRatio            *float64 `json:"fast_path_ratio"`
	OpenManualFallbackEvents int64    `json:"open_manual_fallback_events"`
}

// getSystemInvariants is GET /v1/system/invariants -- buffer level vs
// target, reconciliation lag, fast-path vs slow-path ratio, per this
// chunk's own build spec.
func (s *Server) getSystemInvariants(w http.ResponseWriter, r *http.Request) {
	available, err := s.Buffer.AvailableTotal(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	target, err := s.Buffer.TargetLevel(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}

	var lag *float64
	if last := s.Buffer.LastReconciledAt(); !last.IsZero() {
		seconds := time.Since(last).Seconds()
		lag = &seconds
	}

	fastCount, slowCount, err := reservations.PathCounts(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	var ratio *float64
	if total := fastCount + slowCount; total > 0 {
		r := float64(fastCount) / float64(total)
		ratio = &r
	}

	openEvents, err := routing.ListFallbackEvents(r.Context(), s.Pool, boolPtr(false))
	if err != nil {
		writeErr(w, err)
		return
	}

	respondJSON(w, http.StatusOK, invariantsResponse{
		BufferAvailable:          available,
		BufferTarget:             target,
		ReconciliationLagSeconds: lag,
		FastPathConfirmedCount:   fastCount,
		SlowPathConfirmedCount:   slowCount,
		FastPathRatio:            ratio,
		OpenManualFallbackEvents: int64(len(openEvents)),
	})
}

func boolPtr(b bool) *bool { return &b }
