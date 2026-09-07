package buffer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTronGridBaseURL is TRON's own public full-node API gateway --
// the same host both CatFee's and Tronsell's own docs point integrators
// at for independent on-chain verification (their worked examples call
// exactly this endpoint against exactly this host).
const DefaultTronGridBaseURL = "https://api.trongrid.io"

// TronGridConfig configures a TronGridReader. APIKey is optional (basic
// getaccountresource queries need no authentication at all -- TronGrid's
// own API reference lists this endpoint's security requirements as
// empty) but strongly recommended in production: an unauthenticated
// caller shares TronGrid's public per-IP rate limit with every other
// anonymous caller on the internet, not a limit scoped to this
// deployment.
type TronGridConfig struct {
	BaseURL    string // DefaultTronGridBaseURL if empty
	APIKey     string // sent as TRON-PRO-API-KEY if set; optional
	HTTPClient *http.Client
}

// TronGridReader implements TronEnergyReader against a real TRON full
// node via TronGrid's public gateway -- read-only, no signing, per this
// interface's own doc comment.
//
// TRON's basic account-resource query reports an ADDRESS's own current
// aggregate energy (EnergyLimit minus EnergyUsed), never a breakdown by
// delegation: on-chain Stake 2.0 delegated resources have no
// vendor-agnostic "delegation ID" a public API can filter by (the
// delegationID this module passes around is each vendor's own opaque
// order identifier, meaningful only to that vendor, never to TRON
// itself). DelegationUnits therefore reports the address's own current
// total available energy, ignoring delegationID beyond using it in
// error messages -- exactly the verification method both CatFee's and
// Tronsell's own docs recommend to their integrators ("query the
// address's energy_limit/energy_used, not a specific delegation").
// VerifyOnChain's own check (`total >= d.EnergyUnits`) already treats
// the return value as a sufficiency check, not a delegation-specific
// fact, so this is a faithful implementation of the interface's actual
// contract, not an approximation of a stronger one nothing here could
// deliver anyway.
type TronGridReader struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewTronGridReader returns a TronGridReader for cfg.
func NewTronGridReader(cfg TronGridConfig) *TronGridReader {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultTronGridBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TronGridReader{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  cfg.APIKey,
		http:    client,
	}
}

type tronGridResourceRequest struct {
	Address string `json:"address"`
	Visible bool   `json:"visible"`
}

// tronGridResourceResponse is TronGrid's own getaccountresource response
// shape -- field names and casing confirmed against TRON's own official
// API reference (developers.tron.network), not guessed from either
// vendor's own looser prose description of the same fields.
type tronGridResourceResponse struct {
	EnergyLimit int64 `json:"EnergyLimit"`
	EnergyUsed  int64 `json:"EnergyUsed"`
}

// DelegationUnits implements buffer.TronEnergyReader -- see this type's
// own doc comment for what it actually measures.
func (r *TronGridReader) DelegationUnits(ctx context.Context, address, delegationID string) (int64, error) {
	reqBody, err := json.Marshal(tronGridResourceRequest{Address: address, Visible: true})
	if err != nil {
		return 0, fmt.Errorf("trongrid: encoding request for %s (delegation %s): %w", address, delegationID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/wallet/getaccountresource", bytes.NewReader(reqBody))
	if err != nil {
		return 0, fmt.Errorf("trongrid: building request for %s (delegation %s): %w", address, delegationID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if r.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", r.apiKey)
	}

	resp, err := r.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("trongrid: getaccountresource for %s (delegation %s): %w", address, delegationID, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("trongrid: reading response for %s (delegation %s): %w", address, delegationID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("trongrid: getaccountresource for %s (delegation %s): HTTP %d: %s", address, delegationID, resp.StatusCode, string(body))
	}

	// A brand-new, never-activated address returns `{}` (every field
	// absent, not zeroed) -- decoding into the zero-value struct handles
	// that correctly: EnergyLimit=0, EnergyUsed=0, so available below is
	// 0, exactly "no energy here yet".
	var parsed tronGridResourceResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("trongrid: decoding response for %s (delegation %s): %w", address, delegationID, err)
	}

	available := parsed.EnergyLimit - parsed.EnergyUsed
	if available < 0 {
		available = 0
	}
	return available, nil
}
