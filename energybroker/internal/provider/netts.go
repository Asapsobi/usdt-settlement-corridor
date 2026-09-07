package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"energybroker/internal/money"
)

// DefaultNettsBaseURL is Netts's real, documented API host, confirmed
// against their own live OpenAPI reference at doc.netts.io/api/reference
// ("The base URL is https://netts.io/apiv2"), not their marketing copy.
const DefaultNettsBaseURL = "https://netts.io/apiv2"

// nettsEnergyService is the /pricing service key for the 1-hour energy
// product -- the one Delegate always orders (see its own doc comment).
const nettsEnergyService = "energy_1h"

// NettsConfig configures a NettsProvider. APIKey comes from a Netts
// workspace account (netts.io/workspace). RealIP is the caller's own
// outbound IP, which Netts requires to be pre-whitelisted in that same
// workspace's API settings AND sent back as the X-Real-IP header on
// every request -- a request from any other IP is rejected with 401.
// This has no sane default: the egress IP a deployment actually calls
// out from is environment-specific, so it must be configured, never
// detected or guessed here.
type NettsConfig struct {
	BaseURL    string // DefaultNettsBaseURL if empty
	APIKey     string
	RealIP     string
	HTTPClient *http.Client // a default 15s-timeout client if nil
}

// NettsProvider is a real, HTTP-calling EnergyProvider for Netts,
// implementing exactly Quote and Delegate -- Netts's own API has no
// endpoint that retargets an existing order to a new address (only
// POST /energy/reclaim/{orderId}, which reclaims early, a different
// operation), so it never implemented the old Redelegate method either.
// See internal/provider.EnergyProvider's own doc comment for the full
// account.
type NettsProvider struct {
	baseURL string
	apiKey  string
	realIP  string
	http    *http.Client
}

// NewNettsProvider returns a NettsProvider for cfg.
func NewNettsProvider(cfg NettsConfig) *NettsProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultNettsBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &NettsProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  cfg.APIKey,
		realIP:  cfg.RealIP,
		http:    client,
	}
}

func (p *NettsProvider) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("netts: building request: %w", err)
	}
	req.Header.Set("X-API-KEY", p.apiKey)
	req.Header.Set("X-Real-IP", p.realIP)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// nettsPricingResponse is Netts's own GET /pricing response shape,
// confirmed verbatim against their live OpenAPI reference.
type nettsPricingResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Services map[string]struct {
			Unit    string `json:"unit"`
			Periods []struct {
				ID        string  `json:"id"`
				IsCurrent bool    `json:"is_current"`
				Price     float64 `json:"price"`
			} `json:"periods"`
		} `json:"services"`
	} `json:"data"`
}

// Quote implements EnergyProvider, reading the currently-active period's
// own sun rate for the energy_1h service off GET /pricing -- the
// "recommended pricing endpoint" per Netts's own docs, replacing their
// deprecated GET /prices.
func (p *NettsProvider) Quote(ctx context.Context) (Quote, error) {
	req, err := p.newRequest(ctx, http.MethodGet, "/pricing?services="+nettsEnergyService, nil)
	if err != nil {
		return Quote{}, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return Quote{}, fmt.Errorf("netts: GET /pricing: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Quote{}, fmt.Errorf("%w: netts: reading /pricing response: %v", ErrMalformedResponse, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Quote{}, fmt.Errorf("netts: GET /pricing: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var parsed nettsPricingResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Quote{}, fmt.Errorf("%w: netts: decoding /pricing response: %v", ErrMalformedResponse, err)
	}
	if !parsed.Success {
		return Quote{}, fmt.Errorf("%w: netts: /pricing responded success=false", ErrMalformedResponse)
	}
	service, ok := parsed.Data.Services[nettsEnergyService]
	if !ok {
		return Quote{}, fmt.Errorf("%w: netts: /pricing response has no %q service", ErrMalformedResponse, nettsEnergyService)
	}
	for _, period := range service.Periods {
		if period.IsCurrent {
			return Quote{
				ProviderName:    Netts,
				PricePerUnitSun: period.Price,
				QuotedAt:        time.Now().UTC(),
				// Netts documents no per-call unit cap below the
				// 300,000-unit threshold where price drift becomes
				// possible -- 0 means "unknown/uncapped" to every caller
				// (buffer.Buffer.Replenish's own chunking check is `> 0`),
				// never a guessed number.
				MaxUnitsAvailable: 0,
			}, nil
		}
	}
	return Quote{}, fmt.Errorf("%w: netts: /pricing response for %q has no period marked is_current", ErrMalformedResponse, nettsEnergyService)
}

type nettsOrderRequest struct {
	Amount         int64  `json:"amount"`
	ReceiveAddress string `json:"receiveAddress"`
}

// nettsOrderResponse is Netts's own POST /order1h response shape,
// confirmed verbatim against their live OpenAPI reference.
type nettsOrderResponse struct {
	Detail struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OrderID string  `json:"orderId"`
			PaidTRX float64 `json:"paidTRX"`
			Hash    string  `json:"hash"`
			Energy  int64   `json:"energy"`
		} `json:"data"`
	} `json:"detail"`
}

// nettsSuccessCode is Netts's own documented success code on
// POST /order1h ("code": 10000 in their worked example, "Successful, X
// TRX deducted").
const nettsSuccessCode = 10000

// Delegate implements EnergyProvider via POST /order1h -- always a
// 1-hour order, Netts's only automated-failover product (order5m draws
// from Netts's own internal pools only, with no external-provider
// fallback, per their own docs); the caller's own requested duration is
// ignored, and ExpiresAt reports the real 1-hour grant, never whatever
// was asked for (see Delegation's own doc comment on why that
// distinction matters).
func (p *NettsProvider) Delegate(ctx context.Context, target string, units int64, duration time.Duration) (Delegation, error) {
	reqBody, err := json.Marshal(nettsOrderRequest{Amount: units, ReceiveAddress: target})
	if err != nil {
		return Delegation{}, fmt.Errorf("netts: encoding /order1h request: %w", err)
	}
	req, err := p.newRequest(ctx, http.MethodPost, "/order1h", bytes.NewReader(reqBody))
	if err != nil {
		return Delegation{}, err
	}
	requestedAt := time.Now().UTC()

	resp, err := p.http.Do(req)
	if err != nil {
		return Delegation{}, fmt.Errorf("netts: POST /order1h: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Delegation{}, fmt.Errorf("%w: netts: reading /order1h response: %v", ErrMalformedResponse, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Delegation{}, fmt.Errorf("netts: POST /order1h: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var parsed nettsOrderResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Delegation{}, fmt.Errorf("%w: netts: decoding /order1h response: %v", ErrMalformedResponse, err)
	}
	if parsed.Detail.Code != nettsSuccessCode {
		return Delegation{}, fmt.Errorf("netts: POST /order1h: code=%d msg=%q", parsed.Detail.Code, parsed.Detail.Msg)
	}
	if parsed.Detail.Data.OrderID == "" {
		return Delegation{}, fmt.Errorf("%w: netts: /order1h returned an empty orderId", ErrMalformedResponse)
	}

	cost, err := money.ParseDecimal(fmt.Sprintf("%.6f", parsed.Detail.Data.PaidTRX))
	if err != nil {
		return Delegation{}, fmt.Errorf("netts: order %s: computing cost: %w", parsed.Detail.Data.OrderID, err)
	}

	return Delegation{
		ID:            parsed.Detail.Data.OrderID,
		ProviderName:  Netts,
		TargetAddress: target,
		EnergyUnits:   units,
		CostTRX:       cost,
		RequestedAt:   requestedAt,
		ExpiresAt:     requestedAt.Add(time.Hour), // /order1h is always a 1-hour grant
		ConfirmedAt:   nil,                        // on-chain verification is internal/buffer's job, never this client's
	}, nil
}
