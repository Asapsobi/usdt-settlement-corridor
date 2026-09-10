// Package sandbox is C6.7: a fully separate code path exercising
// decision 5's own four deterministic failure triggers (reorg,
// screening_hold, energy_exhaustion, retry_storm), never touching real
// C1/C2/C4/C5 state or a real vendor call -- see
// c6-api-gateway-build-prompts.md's own "Read this third": "No
// production sign-off without exercising all four."
//
// Every sandbox order lives in its own table (sandbox_orders), never
// gateway_orders, and carries an external_id from its own "sbx_"
// namespace -- a production GET /v1/orders/{external_id} only ever
// reads gateway_orders, so a sandbox id is structurally invisible to
// it, not just conventionally hidden (invariant 4). No production
// package may import this one -- enforced mechanically by
// no_import_test.go, mirroring every sibling module's own
// internal/provider-boundary discipline.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gateway/internal/db"
	"gateway/internal/money"
	"gateway/internal/pricing"
)

// Trigger is one of the four documented sandbox-only scenarios,
// selected by postOrderRequest's own "trigger" field -- honored only
// for a sandbox customer (customer.IsSandbox), rejected outright for a
// production one, so it stays impossible to hit by accident on a real
// order.
type Trigger string

const (
	TriggerReorg            Trigger = "reorg"
	TriggerScreeningHold    Trigger = "screening_hold"
	TriggerEnergyExhaustion Trigger = "energy_exhaustion"
	TriggerRetryStorm       Trigger = "retry_storm"
)

// ErrUnknownTrigger means trigger wasn't one of the four documented
// values.
var ErrUnknownTrigger = errors.New("sandbox: unknown trigger")

// ValidTrigger reports whether trigger is one of the four documented
// values.
func ValidTrigger(trigger string) bool {
	switch Trigger(trigger) {
	case TriggerReorg, TriggerScreeningHold, TriggerEnergyExhaustion, TriggerRetryStorm:
		return true
	}
	return false
}

// screeningHoldReasonCode is the realistic C3 reason code
// TriggerScreeningHold attaches -- "screening_hold_flagged" verbatim,
// the same code screening/internal/ledgerclient.go's own real
// transition call uses, per C6.3 scenario A's shape.
const screeningHoldReasonCode = "screening_hold_flagged"

// Order is one row of sandbox_orders.
type Order struct {
	ExternalID       string
	CustomerID       int64
	Trigger          Trigger
	Tier             pricing.Tier
	AmountIn         money.Amount
	AmountOut        money.Amount
	FeeUnits         money.Amount
	NetworkFeeUnits  money.Amount
	RecipientAddress string
	DepositAddress   string
	State            string
	HoldReason       *string
	WebhookAttempts  int
	WebhookExhausted bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ErrNotFound means no sandbox_orders row exists for the given
// external_id.
var ErrNotFound = errors.New("sandbox: no such order")

// Store is the sandbox_orders table's own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// externalIDPrefix guarantees zero overlap with any production
// external_id by construction, not just by living in a separate table
// -- "impossible to hit by accident," per C6.7's own acceptance
// criterion.
const externalIDPrefix = "sbx_"

func generateExternalID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("sandbox: generating external_id: %w", err)
	}
	return externalIDPrefix + hex.EncodeToString(raw), nil
}

// CreateOrder prices amountIn exactly like a real order (C6's own
// pricing library is a pure function, not a call to C1/C2 -- reusing
// it here does not violate invariant 4), fabricates a deposit address,
// and synchronously drives the order through trigger's own fully
// scripted, deterministic outcome -- no background loop, no real
// elapsed time, so "same request, same sequence... every time, no
// timing races" holds by construction, not by luck.
func (s *Store) CreateOrder(ctx context.Context, customerID int64, trigger Trigger, tier pricing.Tier, amountIn money.Amount, recipientAddress string) (Order, error) {
	if !ValidTrigger(string(trigger)) {
		return Order{}, ErrUnknownTrigger
	}

	priced, err := pricing.ComputeQuote(tier, amountIn)
	if err != nil {
		return Order{}, fmt.Errorf("sandbox: pricing order: %w", err)
	}

	externalID, err := generateExternalID()
	if err != nil {
		return Order{}, err
	}
	depositAddress := externalIDPrefix + "deposit_" + externalID[len(externalIDPrefix):]

	state, holdReason, webhookAttempts, webhookExhausted := scriptOutcome(trigger)

	row := s.pool.QueryRow(ctx, `
		INSERT INTO sandbox_orders (external_id, customer_id, trigger, tier, amount_in, amount_out, fee_units, network_fee_units,
			recipient_address, deposit_address, state, hold_reason, webhook_attempts, webhook_exhausted)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING external_id, customer_id, trigger, tier, amount_in, amount_out, fee_units, network_fee_units,
			recipient_address, deposit_address, state, hold_reason, webhook_attempts, webhook_exhausted, created_at, updated_at
	`, externalID, customerID, string(trigger), string(tier), int64(priced.AmountIn), int64(priced.AmountOut), int64(priced.FeeUnits), int64(priced.NetworkFeeUnits),
		recipientAddress, depositAddress, state, holdReason, webhookAttempts, webhookExhausted)
	o, err := scanOrder(row)
	if err != nil {
		return Order{}, fmt.Errorf("sandbox: creating order: %w", err)
	}
	return o, nil
}

// scriptOutcome is the one place all four triggers' documented
// final-state behavior is defined -- deliberately a pure function
// (trigger in, outcome out), so its own determinism is obvious from
// the signature alone.
func scriptOutcome(trigger Trigger) (state string, holdReason *string, webhookAttempts int, webhookExhausted bool) {
	switch trigger {
	case TriggerReorg:
		// funded, then a simulated BSC reorg reverses it back to quoted
		// -- C1.6 scenario A's real shape; the customer-visible end state
		// is "quoted" again, exactly as if funding had never happened.
		return "quoted", nil, 0, false
	case TriggerScreeningHold:
		reason := screeningHoldReasonCode
		return "held", &reason, 0, false
	case TriggerEnergyExhaustion:
		// screened, then dispatching stalls past a simulated deadline --
		// C4's own ceiling/fallback-ladder exhaustion shape.
		return "dispatching", nil, 0, false
	case TriggerRetryStorm:
		// The order itself settles normally -- it is the webhook, not the
		// order, that fails deterministically for exactly
		// webhooks.DefaultMaxAttempts (8) attempts.
		return "settled", nil, 8, true
	default:
		return "quoted", nil, 0, false // unreachable: CreateOrder validates first
	}
}

// Get fetches a sandbox order by external_id, scoped to customerID --
// ownership checked, not just existence, same posture as gateway_orders'
// own Get.
func (s *Store) Get(ctx context.Context, externalID string, customerID int64) (Order, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT external_id, customer_id, trigger, tier, amount_in, amount_out, fee_units, network_fee_units,
			recipient_address, deposit_address, state, hold_reason, webhook_attempts, webhook_exhausted, created_at, updated_at
		FROM sandbox_orders WHERE external_id = $1 AND customer_id = $2
	`, externalID, customerID)
	o, err := scanOrder(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, ErrNotFound
		}
		return Order{}, fmt.Errorf("sandbox: fetching %q: %w", externalID, err)
	}
	return o, nil
}

type scanRowIface interface {
	Scan(dest ...any) error
}

func scanOrder(row scanRowIface) (Order, error) {
	var o Order
	var tier, trigger string
	var amountIn, amountOut, feeUnits, networkFeeUnits int64
	if err := row.Scan(&o.ExternalID, &o.CustomerID, &trigger, &tier, &amountIn, &amountOut, &feeUnits, &networkFeeUnits,
		&o.RecipientAddress, &o.DepositAddress, &o.State, &o.HoldReason, &o.WebhookAttempts, &o.WebhookExhausted, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return Order{}, err
	}
	o.Trigger = Trigger(trigger)
	o.Tier = pricing.Tier(tier)
	o.AmountIn = money.Amount(amountIn)
	o.AmountOut = money.Amount(amountOut)
	o.FeeUnits = money.Amount(feeUnits)
	o.NetworkFeeUnits = money.Amount(networkFeeUnits)
	return o, nil
}
