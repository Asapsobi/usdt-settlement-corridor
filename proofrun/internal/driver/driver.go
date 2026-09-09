// Package driver is the MVP proof run's own minimal order-origination
// driver (docs/03-build/mvp-proof-run-plan.md, "(b) A minimal
// order-origination driver"): create an order against C1, get a deposit
// address from C2, poll status across C1/C2/C5. Deliberately NOT
// c6-api-gateway-build-prompts.md's own full spec -- no customer auth,
// no rate limiting, no persisted quotes, no tiered pricing engine, no
// HMAC webhooks, no sandbox. This is scaffolding to prove the pipeline
// works end to end, not a step toward C6; C6 still needs to be built in
// full, separately, against a now-proven backend.
package driver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"proofrun/internal/money"
	"proofrun/internal/upstream"
)

// Config is this driver's own required, no-hardcoded-default tuning.
type Config struct {
	// FeeBasisPoints is the fee this driver charges, in basis points of
	// amount_in -- 25 (0.25%) is the "25 bp fee" decision
	// docs/01-strategy's own pricing analysis is built around
	// (product-operations-architecture.md), not a fabricated number.
	FeeBasisPoints int64
	// NetworkFeeUnits is a flat placeholder, NOT a real network-fee
	// pricing decision -- see this package's own doc comment: the full
	// tiered pricing model belongs to C6, not this scaffold.
	NetworkFeeUnits money.Amount
	// QuoteValidity is how long this driver tells C1/C2 the quote is
	// good for. Generous on purpose (unlike C6's own eventual 90s lock,
	// product-operations-architecture.md decision 5): this proof run
	// expects a human to actually send a real BSC deposit in the time
	// this window allows.
	QuoteValidity time.Duration
}

// Driver bundles the three real upstream clients this proof run needs.
type Driver struct {
	Ledger     *upstream.Ledger
	Watcher    *upstream.Watcher
	Dispatcher *upstream.Dispatcher
	Cfg        Config
}

// ErrAmountTooSmall means amount_in, after this driver's own fee and
// network_fee, would leave a non-positive amount_out -- never a valid
// payout, checked here rather than left for C1's own validation to
// reject less legibly.
var ErrAmountTooSmall = errors.New("driver: amount_in is too small to cover this driver's fee and network_fee")

// CreatePayoutRequest is this driver's own minimal quote-then-order
// input -- the plan's own {amount, recipient_tron_address, external_id},
// plus customer_id (C1's own required field; there is no real customer
// auth here to derive it from, see this package's own doc comment).
type CreatePayoutRequest struct {
	ExternalID           string
	CustomerID           string
	RecipientTronAddress string
	AmountIn             string // decimal string, USDT_BEP20
}

// CreatePayoutResult is what a caller needs to actually fund the
// order: where to send USDT, and the frozen amounts C1 now owns.
type CreatePayoutResult struct {
	ExternalID      string
	OrderID         int64
	DepositAddress  string
	AmountIn        string
	AmountOut       string
	FeeUnits        string
	NetworkFeeUnits string
	QuoteExpiresAt  time.Time
}

// CreatePayout computes a straightforward fee, creates the order
// against C1, and assigns a deposit address against C2 -- exactly the
// plan's own §2(b) scope, nothing more. Both C1 and C2 calls use
// external_id-derived idempotency keys; C2's own POST /v1/addresses is
// genuinely idempotent on order_id, but C1's POST /v1/orders is not
// idempotent on a repeated external_id (it has never needed to be: in
// the real system C6's own quote-lock is what prevents a duplicate call
// from ever reaching it) -- so a CreateOrder failure here is followed by
// one GetOrder attempt, on the theory that a failure a caller retried
// might really mean "this exact order already exists," not "something
// is wrong." A order that genuinely doesn't exist yet still surfaces the
// original CreateOrder error.
func (d *Driver) CreatePayout(ctx context.Context, req CreatePayoutRequest) (CreatePayoutResult, error) {
	amountIn, err := money.ParseDecimal(req.AmountIn)
	if err != nil {
		return CreatePayoutResult{}, fmt.Errorf("driver: amount_in: %w", err)
	}
	if amountIn <= 0 {
		return CreatePayoutResult{}, fmt.Errorf("driver: amount_in must be positive")
	}

	feeUnits := money.Amount(int64(amountIn) * d.Cfg.FeeBasisPoints / 10_000)
	networkFeeUnits := d.Cfg.NetworkFeeUnits
	amountOut := amountIn - feeUnits - networkFeeUnits
	if amountOut <= 0 {
		return CreatePayoutResult{}, ErrAmountTooSmall
	}

	quotedAt := time.Now().UTC()
	quoteExpiresAt := quotedAt.Add(d.Cfg.QuoteValidity)

	order, err := d.Ledger.CreateOrder(ctx, upstream.PostOrderRequest{
		ExternalID: req.ExternalID, CustomerID: req.CustomerID, Tier: "DIRECT",
		AmountIn: amountIn.Format(), AmountOut: amountOut.Format(),
		FeeUnits: feeUnits.Format(), NetworkFeeUnits: networkFeeUnits.Format(),
		RecipientAddress: req.RecipientTronAddress,
		QuotedAt:         quotedAt, QuoteExpiresAt: quoteExpiresAt,
	}, "proofrun:create-order:"+req.ExternalID)
	if err != nil {
		if existing, getErr := d.Ledger.GetOrder(ctx, req.ExternalID); getErr == nil {
			order = existing
		} else {
			return CreatePayoutResult{}, fmt.Errorf("driver: creating order in C1: %w", err)
		}
	}

	addr, err := d.Watcher.AssignAddress(ctx, order.ID, order.ExternalID, req.CustomerID,
		order.QuotedAt, order.QuoteExpiresAt, "proofrun:assign-address:"+req.ExternalID)
	if err != nil {
		return CreatePayoutResult{}, fmt.Errorf("driver: order %d created in C1 (external_id %s) but assigning a deposit address in C2 failed -- retry this call, C1's own side is idempotent-safe to repeat: %w",
			order.ID, order.ExternalID, err)
	}

	return CreatePayoutResult{
		ExternalID: order.ExternalID, OrderID: order.ID, DepositAddress: addr.Address,
		AmountIn: order.AmountIn, AmountOut: order.AmountOut,
		FeeUnits: order.FeeUnits, NetworkFeeUnits: order.NetworkFeeUnits,
		QuoteExpiresAt: order.QuoteExpiresAt,
	}, nil
}

// PayoutStatus is this driver's own reconstructed lifecycle view -- the
// plan's own definition of done: "a single external_id/order_id must
// let you reconstruct the whole lifecycle." DepositTxHash and
// EnergyReservationID are deliberately absent: neither C2 nor C4
// exposes either one over its real, shipped HTTP surface today (C2 has
// no deposit-by-order endpoint; C4 has no reservation-by-order endpoint,
// only by-reservation-id, and this driver never sees a reservation id --
// C5's own orchestrate loop calls C4 directly). Reported here as an
// honest, flagged gap rather than reached around via direct
// cross-service database access, which every component in this project
// has otherwise treated as a hard boundary.
type PayoutStatus struct {
	ExternalID string
	Order      upstream.Order
	Address    *upstream.WatchedAddress
	Dispatch   *upstream.DispatchStatus
	// PayoutTxHash is C5's own tron_txid once broadcast -- copied out of
	// Dispatch for convenience, nil until it exists.
	PayoutTxHash *string
	// AddressError/DispatchError carry a non-nil Address/Dispatch fetch
	// failure that ISN'T "this order hasn't reached that component yet"
	// -- a real connectivity problem, say. Reported alongside whatever
	// C1 (and any other component) DID successfully answer, rather than
	// failing this whole call: an operator watching a proof run's
	// progress gets more value from partial status than from a single
	// component's outage hiding everything else.
	AddressError  string
	DispatchError string
}

// GetStatus reconstructs everything this driver can see about
// externalID across C1, C2, and C5. A component this order hasn't
// reached yet (no address assigned, order never entered dispatching)
// is reported as a nil field, never an error -- "not there yet" is the
// normal, expected shape of most of this order's own lifetime. Only C1
// -- the source of truth for whether this order exists at all -- is a
// hard failure; C2/C5 unavailability degrades into AddressError/
// DispatchError instead (see PayoutStatus's own doc comment).
func (d *Driver) GetStatus(ctx context.Context, externalID string) (PayoutStatus, error) {
	order, err := d.Ledger.GetOrder(ctx, externalID)
	if err != nil {
		return PayoutStatus{}, fmt.Errorf("driver: fetching order from C1: %w", err)
	}

	status := PayoutStatus{ExternalID: externalID, Order: order}

	addr, err := d.Watcher.GetAddress(ctx, order.ID)
	switch {
	case err == nil:
		status.Address = &addr
	case isNotFound(err):
		// normal -- C2 hasn't recorded anything for this order (shouldn't
		// happen once CreatePayout has run, but GetStatus makes no
		// assumption about how an order came to exist in C1)
	default:
		status.AddressError = err.Error()
	}

	dispatch, err := d.Dispatcher.GetDispatch(ctx, order.ID)
	switch {
	case err == nil:
		status.Dispatch = &dispatch
		status.PayoutTxHash = dispatch.TronTxID
	case errors.Is(err, upstream.ErrNotDispatchingYet):
		// normal -- order hasn't entered dispatching yet
	default:
		status.DispatchError = err.Error()
	}

	return status, nil
}

func isNotFound(err error) bool {
	var apiErr *upstream.APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404
}
