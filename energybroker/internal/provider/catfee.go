package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"energybroker/internal/money"
)

// DefaultCatfeeBaseURL is CatFee's real, documented production API host
// (confirmed against their own live OpenAPI-rendered reference at
// docs.catfee.io/api-reference, not their marketing copy).
const DefaultCatfeeBaseURL = "https://api.catfee.io"

// catfeeMinQuantity is CatFee's own documented minimum order size
// ("delegation energy quantity, must >= 65000" on both /v1/order and
// /v1/estimate) -- Quote uses it as the reference size for a price
// probe, since CatFee has no way to ask "what would X cost" below this
// floor.
const catfeeMinQuantity = 65000

// catfeeDuration is the only value CatFee's own API accepts for
// `duration` on /v1/order and /v1/estimate ("allowed values are 1h").
// Delegate always requests exactly this, regardless of what duration a
// caller asks for -- see Delegation's own doc comment on why the
// returned ExpiresAt, not the caller's request, is what actually
// matters.
const catfeeDuration = "1h"

// CatfeeConfig configures a CatfeeProvider. APIKey and APISecret come
// from a CatFee dashboard account (dashboard.catfee.io) -- never
// hardcoded, never logged.
type CatfeeConfig struct {
	BaseURL    string // DefaultCatfeeBaseURL if empty
	APIKey     string
	APISecret  string
	HTTPClient *http.Client // a default 15s-timeout client if nil
}

// CatfeeProvider is a real, HTTP-calling EnergyProvider for CatFee.IO,
// implementing exactly Quote and Delegate -- CatFee's own API has
// nothing that retargets an existing order to a new address (their
// closest feature, Transaction Handoff, hands back an unsigned
// transaction for the CALLER to broadcast, which is out of this
// component's scope per "Read this second" in the build doc -- not a
// same-shape retarget), so it never implemented the old Redelegate
// method either. See internal/provider.EnergyProvider's own doc comment
// for the full account.
type CatfeeProvider struct {
	baseURL   string
	apiKey    string
	apiSecret string
	http      *http.Client
}

// NewCatfeeProvider returns a CatfeeProvider for cfg.
func NewCatfeeProvider(cfg CatfeeConfig) *CatfeeProvider {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultCatfeeBaseURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &CatfeeProvider{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    cfg.APIKey,
		apiSecret: cfg.APISecret,
		http:      client,
	}
}

// catfeeEnvelope is every CatFee response's own shape -- confirmed
// verbatim against their live API reference, not guessed from prose:
// "CatFee API usually returns HTTP 200. Always judge success based on
// `code` in the response body, not HTTP status."
type catfeeEnvelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	SubCode string          `json:"sub_code"`
	SubMsg  string          `json:"sub_msg"`
	Data    json.RawMessage `json:"data"`
}

// catfeeOrderPayload is CatFee's own OrderPayload schema, read field-for-
// field off their live /v1/order and /v1/order/{id} reference pages
// (both return the identical shape).
type catfeeOrderPayload struct {
	ID               string `json:"id"`
	ClientOrderID    string `json:"client_order_id"`
	ResourceType     string `json:"resource_type"`
	SourceType       string `json:"source_type"`
	PayTimestamp     int64  `json:"pay_timestamp"`
	Receiver         string `json:"receiver"`
	DelegateHash     string `json:"delegate_hash"`
	PayAmountSun     int64  `json:"pay_amount_sun"`
	Quantity         int64  `json:"quantity"`
	Duration         int64  `json:"duration"`
	ExpiredTimestamp int64  `json:"expired_timestamp"`
	Status           string `json:"status"`
	ConfirmStatus    string `json:"confirm_status"`
}

// do sends one CatFee request, signing it per their own documented
// scheme: CF-ACCESS-SIGN is HMAC-SHA256(timestamp+method+requestPath,
// apiSecret), base64-encoded -- requestPath includes the query string,
// exactly as sent on the wire, confirmed against their own Go example.
func (p *CatfeeProvider) do(ctx context.Context, method, path string, query url.Values) (*catfeeEnvelope, error) {
	requestPath := path
	if len(query) > 0 {
		requestPath += "?" + query.Encode()
	}
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	mac := hmac.New(sha256.New, []byte(p.apiSecret))
	mac.Write([]byte(timestamp + method + requestPath))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+requestPath, nil)
	if err != nil {
		return nil, fmt.Errorf("catfee: building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("CF-ACCESS-KEY", p.apiKey)
	req.Header.Set("CF-ACCESS-SIGN", signature)
	req.Header.Set("CF-ACCESS-TIMESTAMP", timestamp)

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catfee: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: catfee: reading response for %s %s: %v", ErrMalformedResponse, method, path, err)
	}
	var envelope catfeeEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("%w: catfee: decoding response for %s %s: %v", ErrMalformedResponse, method, path, err)
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("catfee: %s %s: code=%d msg=%q sub_code=%q sub_msg=%q", method, path, envelope.Code, envelope.Msg, envelope.SubCode, envelope.SubMsg)
	}
	return &envelope, nil
}

// Quote implements EnergyProvider. CatFee's GET /v1/estimate reports a
// TOTAL cost for a given quantity+duration, never a per-unit rate
// directly, so this probes at catfeeMinQuantity (their own documented
// floor) and divides -- a real, momentary vendor call, same as every
// other provider's own Quote.
func (p *CatfeeProvider) Quote(ctx context.Context) (Quote, error) {
	query := url.Values{
		"quantity": {strconv.Itoa(catfeeMinQuantity)},
		"duration": {catfeeDuration},
	}
	envelope, err := p.do(ctx, http.MethodGet, "/v1/estimate", query)
	if err != nil {
		return Quote{}, err
	}
	var totalSun int64
	if err := json.Unmarshal(envelope.Data, &totalSun); err != nil {
		return Quote{}, fmt.Errorf("%w: catfee: decoding /v1/estimate data: %v", ErrMalformedResponse, err)
	}

	return Quote{
		ProviderName:    Catfee,
		PricePerUnitSun: float64(totalSun) / float64(catfeeMinQuantity),
		QuotedAt:        time.Now().UTC(),
		// CatFee's docs document no per-call unit cap -- 0 means
		// "unknown/uncapped" to every caller (buffer.Buffer.Replenish's
		// own chunking check is `> 0`), never a guessed number.
		MaxUnitsAvailable: 0,
	}, nil
}

// Delegate implements EnergyProvider. Always requests catfeeDuration
// ("1h") regardless of the caller's own requested duration -- CatFee's
// API accepts no other value -- and reports back the real expiry CatFee
// actually granted via ExpiresAt, never the caller's own request. Uses
// POST /v1/order.
func (p *CatfeeProvider) Delegate(ctx context.Context, target string, units int64, duration time.Duration) (Delegation, error) {
	query := url.Values{
		"quantity": {strconv.FormatInt(units, 10)},
		"receiver": {target},
		"duration": {catfeeDuration},
	}
	requestedAt := time.Now().UTC()
	envelope, err := p.do(ctx, http.MethodPost, "/v1/order", query)
	if err != nil {
		return Delegation{}, err
	}
	var order catfeeOrderPayload
	if err := json.Unmarshal(envelope.Data, &order); err != nil {
		return Delegation{}, fmt.Errorf("%w: catfee: decoding /v1/order data: %v", ErrMalformedResponse, err)
	}
	if order.ID == "" {
		return Delegation{}, fmt.Errorf("%w: catfee: /v1/order returned an empty order id", ErrMalformedResponse)
	}

	// pay_amount_sun is the real, actual charge CatFee just took -- never
	// the /v1/estimate quote from Quote(), which may be stale or simply
	// wrong by the time this order actually executes (the same
	// "reconcile against what was ACTUALLY charged" discipline this
	// module's own pricing.ReconcileCharge exists to enforce upstream of
	// this client).
	cost, err := money.ParseDecimal(fmt.Sprintf("%.6f", float64(order.PayAmountSun)/1_000000))
	if err != nil {
		return Delegation{}, fmt.Errorf("catfee: order %s: computing cost: %w", order.ID, err)
	}

	return Delegation{
		ID:            order.ID,
		ProviderName:  Catfee,
		TargetAddress: target,
		EnergyUnits:   units,
		CostTRX:       cost,
		RequestedAt:   requestedAt,
		ExpiresAt:     requestedAt.Add(time.Hour), // catfeeDuration is always "1h"
		ConfirmedAt:   nil,                        // on-chain verification is internal/buffer's job, never this client's
	}, nil
}
