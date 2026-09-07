package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"energybroker/internal/money"
)

// tronsellSuccessCode is Tronsell's own documented success code
// ("code=99999 indicates success") across every endpoint.
const tronsellSuccessCode = 99999

// ErrTronsellBaseURLNotConfigured is returned by NewTronsellProvider
// when cfg.BaseURL is empty. Unlike CatFee and Netts, Tronsell's own
// public API docs (tronsell.io/docs/) never state a literal base URL --
// their own "Test Key" example leaves it as an unresolved `{{BASE_URL}}`
// template, confirmed by inspecting the docs page's own JS bundles, not
// just the rendered text. There is no safe default to guess for
// financial-infrastructure code that places real orders, so this must
// be supplied explicitly (confirm the real value with Tronsell support
// or your own account dashboard) rather than silently defaulting to a
// guess that might not even be the right host.
var ErrTronsellBaseURLNotConfigured = errors.New("provider: tronsell: BaseURL is not configured -- see ErrTronsellBaseURLNotConfigured's own doc comment")

// ErrTronsellNoPriceObserved is Quote's own result before this
// TronsellProvider has ever completed a real Delegate call. Tronsell's
// public API (confirmed against tronsell.io/docs/) has no free
// price-quote endpoint the way CatFee's GET /v1/estimate or Netts's GET
// /pricing do -- GET /v1/api/resource and GET /v1/api/query report
// account balance and remaining pool inventory, never a price. Quoting
// would otherwise mean placing a real, paid order just to answer "what
// does energy cost right now," which this client refuses to do
// silently: spending real TRX as a side effect of a routine price poll
// (pricing.Poller calls Quote roughly once a minute) is exactly the kind
// of surprising financial behavior this module's own invariants exist
// to prevent elsewhere. Instead, Quote reports the price observed on
// this instance's own most recent successful Delegate call. Until one
// has happened, this provider is correctly reported unhealthy by
// pricing.Poller and routing.SelectProvider will never choose it --
// which is the honest state of affairs, not a bug: nobody, including
// this client, actually knows Tronsell's current price yet.
var ErrTronsellNoPriceObserved = errors.New("provider: tronsell: no price observed yet -- see ErrTronsellNoPriceObserved's own doc comment")

// TronsellConfig configures a TronsellProvider. BaseURL has no default
// -- see ErrTronsellBaseURLNotConfigured. APIKey comes from a Tronsell
// account (tronsell.io).
type TronsellConfig struct {
	BaseURL    string // required -- see ErrTronsellBaseURLNotConfigured
	APIKey     string
	HTTPClient *http.Client // a default 15s-timeout client if nil
}

// TronsellProvider is a real, HTTP-calling EnergyProvider for
// Tronsell.io, implementing exactly Quote and Delegate -- Tronsell's own
// API has no endpoint that retargets an existing order to a new address
// (GET /v1/order/directRecycle is a manual early-reclaim, a different
// operation), so it never implemented the old Redelegate method either.
// See internal/provider.EnergyProvider's own doc comment for the full
// account.
type TronsellProvider struct {
	baseURL string
	apiKey  string
	http    *http.Client

	mu           sync.Mutex
	lastPriceSun *float64
}

// NewTronsellProvider returns a TronsellProvider for cfg, or
// ErrTronsellBaseURLNotConfigured if cfg.BaseURL is empty.
func NewTronsellProvider(cfg TronsellConfig) (*TronsellProvider, error) {
	if cfg.BaseURL == "" {
		return nil, ErrTronsellBaseURLNotConfigured
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &TronsellProvider{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		http:    client,
	}, nil
}

func (p *TronsellProvider) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("tronsell: building request: %w", err)
	}
	req.Header.Set("key", p.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// tronsellEnvelope is every Tronsell response's own shape, confirmed
// verbatim against their live docs.
type tronsellEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Quote implements EnergyProvider -- see ErrTronsellNoPriceObserved's
// own doc comment for why this never makes a live call of its own, only
// ever reports what the most recent real Delegate call observed.
func (p *TronsellProvider) Quote(ctx context.Context) (Quote, error) {
	p.mu.Lock()
	price := p.lastPriceSun
	p.mu.Unlock()
	if price == nil {
		return Quote{}, ErrTronsellNoPriceObserved
	}
	return Quote{
		ProviderName:      Tronsell,
		PricePerUnitSun:   *price,
		QuotedAt:          time.Now().UTC(),
		MaxUnitsAvailable: 0, // unknown -- see this file's own Quote doc comment; never guessed
	}, nil
}

func (p *TronsellProvider) setLastPriceSun(priceInSun float64) {
	// priceInSun arrives as "thousandths of a sun" per Tronsell's own
	// docs ("资源单价，千分之一sun") -- dividing by 1000 recovers the
	// actual sun rate. Missing this would misprice every quote by 1000x.
	sun := priceInSun / 1000
	p.mu.Lock()
	p.lastPriceSun = &sun
	p.mu.Unlock()
}

type tronsellRentRequest struct {
	ResourceType        int    `json:"resourceType"`
	ReceiveAddress      string `json:"receiveAddress"`
	ResourceValue       int64  `json:"resourceValue"`
	LeaseDurationSecond int64  `json:"leaseDurationSecond"`
}

// tronsellRentData is the data payload POST /v1/order/rent and
// POST /v1/order/useRent both return, confirmed verbatim against
// Tronsell's own live docs.
type tronsellRentData struct {
	OrderNo        string  `json:"orderNo"`
	ResourceType   int     `json:"resourceType"`
	ReceiveAddress string  `json:"receiveAddress"`
	PriceInSun     float64 `json:"priceInSun"` // thousandths of a sun -- see setLastPriceSun
	CreateTime     int64   `json:"createTime"`
	ResourceValue  int64   `json:"resourceValue"`
	RentTimeSecond int64   `json:"rentTimeSecond"`
	PayTime        int64   `json:"payTime"`
	PayAmount      float64 `json:"payAmount"` // real TRX, not sun
	Status         int     `json:"status"`
}

// tronsellResourceTypeEnergy is Tronsell's own documented constant
// ("1=能量，0=带宽" -- 1=Energy, 0=Bandwidth). This client only ever
// deals in energy.
const tronsellResourceTypeEnergy = 1

// Delegate implements EnergyProvider via POST /v1/order/useRent --
// preferred over /v1/order/rent per Tronsell's own FAQ recommendation
// ("rent may reclaim effective energy exactly at the 5-minute mark when
// the next delegation arrives; useRent avoids this"). Unlike CatFee and
// Netts, Tronsell genuinely accepts a custom leaseDurationSecond, so
// duration is honored directly rather than clamped to one fixed value --
// ExpiresAt still reports the response's own rentTimeSecond, never an
// unchecked echo of the request, in case Tronsell ever grants something
// different than asked.
func (p *TronsellProvider) Delegate(ctx context.Context, target string, units int64, duration time.Duration) (Delegation, error) {
	reqBody, err := json.Marshal(tronsellRentRequest{
		ResourceType:        tronsellResourceTypeEnergy,
		ReceiveAddress:      target,
		ResourceValue:       units,
		LeaseDurationSecond: int64(duration.Seconds()),
	})
	if err != nil {
		return Delegation{}, fmt.Errorf("tronsell: encoding /v1/order/useRent request: %w", err)
	}
	req, err := p.newRequest(ctx, http.MethodPost, "/v1/order/useRent", bytes.NewReader(reqBody))
	if err != nil {
		return Delegation{}, err
	}
	requestedAt := time.Now().UTC()

	resp, err := p.http.Do(req)
	if err != nil {
		return Delegation{}, fmt.Errorf("tronsell: POST /v1/order/useRent: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Delegation{}, fmt.Errorf("%w: tronsell: reading /v1/order/useRent response: %v", ErrMalformedResponse, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Delegation{}, fmt.Errorf("tronsell: POST /v1/order/useRent: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var envelope tronsellEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Delegation{}, fmt.Errorf("%w: tronsell: decoding /v1/order/useRent response: %v", ErrMalformedResponse, err)
	}
	if envelope.Code != tronsellSuccessCode {
		return Delegation{}, fmt.Errorf("tronsell: POST /v1/order/useRent: code=%d msg=%q", envelope.Code, envelope.Msg)
	}
	var data tronsellRentData
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return Delegation{}, fmt.Errorf("%w: tronsell: decoding /v1/order/useRent data: %v", ErrMalformedResponse, err)
	}
	if data.OrderNo == "" {
		return Delegation{}, fmt.Errorf("%w: tronsell: /v1/order/useRent returned an empty orderNo", ErrMalformedResponse)
	}

	p.setLastPriceSun(data.PriceInSun)

	cost, err := money.ParseDecimal(fmt.Sprintf("%.6f", data.PayAmount))
	if err != nil {
		return Delegation{}, fmt.Errorf("tronsell: order %s: computing cost: %w", data.OrderNo, err)
	}

	grantedDuration := time.Duration(data.RentTimeSecond) * time.Second
	if data.RentTimeSecond <= 0 {
		// Never trust a non-positive echo -- fall back to the requested
		// duration rather than computing an ExpiresAt in the past or at
		// RequestedAt itself, which insertAvailable's own caller
		// (buffer.Buffer.Replenish) would otherwise treat as capacity
		// that's already expired the instant it's recorded.
		grantedDuration = duration
	}

	return Delegation{
		ID:            data.OrderNo,
		ProviderName:  Tronsell,
		TargetAddress: target,
		EnergyUnits:   units,
		CostTRX:       cost,
		RequestedAt:   requestedAt,
		ExpiresAt:     requestedAt.Add(grantedDuration),
		ConfirmedAt:   nil, // on-chain verification is internal/buffer's job, never this client's
	}, nil
}
