package httpapi

import (
	"net/http"

	"energybroker/internal/provider"
	"energybroker/internal/providercreds"
)

type providerCredentialResponse struct {
	ProviderName string  `json:"provider_name"`
	BaseURL      *string `json:"base_url,omitempty"`
	APIKeyMasked string  `json:"api_key_masked"`
	HasSecret    bool    `json:"has_secret"`
	RealIP       *string `json:"real_ip,omitempty"`
	Enabled      bool    `json:"enabled"`
	UpdatedAt    string  `json:"updated_at"`
	UpdatedBy    string  `json:"updated_by"`
}

// maskAPIKey shows just enough of a key to recognize it in a UI without
// ever fully disclosing it back over HTTP after it's been stored --
// every prior read-facing route in this project that touches a secret
// (none did, until now -- provider credentials are the first) gets this
// posture: a value entered once is never echoed back in full.
func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "••••••••"
	}
	return key[:4] + "…" + key[len(key)-4:]
}

func toProviderCredentialResponse(c providercreds.Credential) providerCredentialResponse {
	return providerCredentialResponse{
		ProviderName: c.ProviderName, BaseURL: c.BaseURL, APIKeyMasked: maskAPIKey(c.APIKey),
		HasSecret: c.APISecret != nil && *c.APISecret != "", RealIP: c.RealIP,
		Enabled: c.Enabled, UpdatedAt: c.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"), UpdatedBy: c.UpdatedBy,
	}
}

// getProviderCredentials is GET /v1/system/provider-credentials --
// ops-console-build-prompts.md's OC.10. Never returns a raw api_key or
// api_secret, only enough to recognize which credential is configured.
func (s *Server) getProviderCredentials(w http.ResponseWriter, r *http.Request) {
	creds, err := providercreds.List(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]providerCredentialResponse, len(creds))
	for i, c := range creds {
		out[i] = toProviderCredentialResponse(c)
	}
	respondJSON(w, http.StatusOK, map[string]any{"providers": out})
}

type postProviderCredentialRequest struct {
	ProviderName string `json:"provider_name"`
	BaseURL      string `json:"base_url,omitempty"`
	APIKey       string `json:"api_key"`
	APISecret    string `json:"api_secret,omitempty"`
	RealIP       string `json:"real_ip,omitempty"`
	Enabled      bool   `json:"enabled"`
	UpdatedBy    string `json:"updated_by"`
}

var validProviderNames = map[string]bool{provider.Tronsell: true, provider.Netts: true, provider.Catfee: true}

// postProviderCredentials is POST /v1/system/provider-credentials --
// add or rotate one vendor's own credentials. Takes effect the next
// time brokerd starts (providersFromDB reads this table at startup) --
// this route does not attempt to hot-swap the live, already-running
// provider set, deliberately: pricing.Poller/internal/buffer/
// internal/reservations all hold real-money vendor clients built once
// at construction, and reaching into that live state from an HTTP
// handler is a correctness risk this build doesn't take on for the sake
// of avoiding a restart.
func (s *Server) postProviderCredentials(w http.ResponseWriter, r *http.Request) {
	var req postProviderCredentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validProviderNames[req.ProviderName] {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, `provider_name must be one of "tronsell", "netts", or "catfee"`))
		return
	}
	if req.APIKey == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "api_key is required"))
		return
	}
	if req.ProviderName == provider.Tronsell && req.BaseURL == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "base_url is required for tronsell"))
		return
	}
	if req.ProviderName == provider.Netts && req.RealIP == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "real_ip is required for netts"))
		return
	}
	if req.UpdatedBy == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "updated_by is required"))
		return
	}

	cred := providercreds.Credential{ProviderName: req.ProviderName, APIKey: req.APIKey, Enabled: req.Enabled, UpdatedBy: req.UpdatedBy}
	if req.BaseURL != "" {
		cred.BaseURL = &req.BaseURL
	}
	if req.APISecret != "" {
		cred.APISecret = &req.APISecret
	}
	if req.RealIP != "" {
		cred.RealIP = &req.RealIP
	}

	if err := providercreds.Upsert(r.Context(), s.Pool, cred); err != nil {
		writeErr(w, err)
		return
	}
	saved, err := providercreds.Get(r.Context(), s.Pool, req.ProviderName)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toProviderCredentialResponse(saved))
}
