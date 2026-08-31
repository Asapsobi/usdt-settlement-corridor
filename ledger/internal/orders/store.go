package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/accounts"
	"ledger/internal/journal"
	"ledger/internal/money"
)

// CreateParams is everything Create needs. The new order always starts in
// Quoted with Version 0 -- callers never choose the initial state.
type CreateParams struct {
	ExternalID       string
	CustomerID       string
	Tier             Tier
	AmountIn         money.Amount // must be USDT_BEP20
	AmountOut        money.Amount // must be USDT_TRC20
	FeeUnits         money.Amount // must be USDT_TRC20
	NetworkFeeUnits  money.Amount // must be USDT_TRC20
	RecipientAddress string
	QuotedAt         time.Time
	QuoteExpiresAt   time.Time
}

func (p CreateParams) validate() error {
	if p.ExternalID == "" {
		return fmt.Errorf("%w: empty external_id", ErrInvalidParams)
	}
	if p.CustomerID == "" {
		return fmt.Errorf("%w: empty customer_id", ErrInvalidParams)
	}
	if !p.Tier.Valid() {
		return fmt.Errorf("%w: unknown tier %q", ErrInvalidParams, p.Tier)
	}
	if p.AmountIn.Asset != money.USDT_BEP20 {
		return fmt.Errorf("%w: amount_in must be USDT_BEP20, got %q", ErrInvalidParams, p.AmountIn.Asset)
	}
	if p.AmountOut.Asset != money.USDT_TRC20 {
		return fmt.Errorf("%w: amount_out must be USDT_TRC20, got %q", ErrInvalidParams, p.AmountOut.Asset)
	}
	if p.FeeUnits.Asset != money.USDT_TRC20 {
		return fmt.Errorf("%w: fee_units must be USDT_TRC20, got %q", ErrInvalidParams, p.FeeUnits.Asset)
	}
	if p.NetworkFeeUnits.Asset != money.USDT_TRC20 {
		return fmt.Errorf("%w: network_fee_units must be USDT_TRC20, got %q", ErrInvalidParams, p.NetworkFeeUnits.Asset)
	}
	if p.RecipientAddress == "" {
		return fmt.Errorf("%w: empty recipient_address", ErrInvalidParams)
	}
	if p.QuotedAt.IsZero() {
		return fmt.Errorf("%w: zero quoted_at", ErrInvalidParams)
	}
	if !p.QuoteExpiresAt.After(p.QuotedAt) {
		return fmt.Errorf("%w: quote_expires_at must be after quoted_at", ErrInvalidParams)
	}
	return nil
}

// Create inserts a new order in state Quoted, version 0.
func Create(ctx context.Context, q accounts.Queryer, p CreateParams) (Order, error) {
	if err := p.validate(); err != nil {
		return Order{}, err
	}

	row := q.QueryRow(ctx, `
		INSERT INTO orders
			(external_id, customer_id, tier, state, amount_in, amount_out,
			 fee_units, network_fee_units, recipient_address, quoted_at, quote_expires_at,
			 version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 0)
		RETURNING id, external_id, customer_id, tier, state, amount_in, amount_out,
			fee_units, network_fee_units, recipient_address, quoted_at, quote_expires_at,
			version, created_at, updated_at
	`, p.ExternalID, p.CustomerID, string(p.Tier), string(Quoted),
		p.AmountIn.Units, p.AmountOut.Units, p.FeeUnits.Units, p.NetworkFeeUnits.Units,
		p.RecipientAddress, p.QuotedAt, p.QuoteExpiresAt)

	order, err := scanOrder(row)
	if err != nil {
		return Order{}, fmt.Errorf("orders: create %q: %w", p.ExternalID, err)
	}
	return order, nil
}

// Get looks up an order by its internal id.
func Get(ctx context.Context, q accounts.Queryer, orderID int64) (Order, error) {
	row := q.QueryRow(ctx, orderSelectSQL+" WHERE id = $1", orderID)
	order, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("%w: id %d", ErrOrderNotFound, orderID)
	}
	if err != nil {
		return Order{}, fmt.Errorf("orders: get %d: %w", orderID, err)
	}
	return order, nil
}

// GetByExternalID looks up an order by the id C6 gave the customer.
func GetByExternalID(ctx context.Context, q accounts.Queryer, externalID string) (Order, error) {
	row := q.QueryRow(ctx, orderSelectSQL+" WHERE external_id = $1", externalID)
	order, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("%w: external_id %q", ErrOrderNotFound, externalID)
	}
	if err != nil {
		return Order{}, fmt.Errorf("orders: get by external id %q: %w", externalID, err)
	}
	return order, nil
}

// ListByState returns every order currently in state, ordered by id.
func ListByState(ctx context.Context, q accounts.Queryer, state State) ([]Order, error) {
	rows, err := q.Query(ctx, orderSelectSQL+" WHERE state = $1 ORDER BY id", string(state))
	if err != nil {
		return nil, fmt.Errorf("orders: list by state %s: %w", state, err)
	}
	defer rows.Close()

	var out []Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("orders: list by state %s: %w", state, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orders: list by state %s: %w", state, err)
	}
	return out, nil
}

// TransitionParams is what a caller supplies to Transition beyond the
// (orderID, toState, expectedVersion) triple.
type TransitionParams struct {
	Actor      string
	Reason     string
	OccurredAt time.Time
	// Entry is posted via journal.Post in the same transaction as the
	// state change, atomically, when the transition rule requires one.
	// It is an error to supply it when the rule does not require one, and
	// an error to omit it when the rule does.
	Entry *journal.EntryRequest
}

func (p TransitionParams) validate() error {
	if p.Actor == "" {
		return fmt.Errorf("%w: empty actor", ErrInvalidParams)
	}
	if p.Reason == "" {
		return fmt.Errorf("%w: empty reason", ErrInvalidParams)
	}
	if p.OccurredAt.IsZero() {
		return fmt.Errorf("%w: zero occurred_at", ErrInvalidParams)
	}
	return nil
}

// Transition moves an order from its current state to toState, per
// transitionTable, the only source of truth for which moves are legal.
//
// Concurrency is optimistic: expectedVersion must match the order's
// current version, checked twice -- once as an early snapshot read (a
// fast, clear rejection in the common uncontended case) and once, the
// guarantee that actually matters, as the WHERE clause of the UPDATE that
// performs the transition. A CAS miss there is ErrVersionConflict; this
// function never retries on the caller's behalf.
//
// tx is a transaction the caller already opened. If Transition returns a
// non-nil error, the caller must roll back rather than commit -- that is
// what makes a losing attempt in a concurrent-transition race (which may
// have already called journal.Post before losing the final CAS) leave no
// trace: the whole attempt, including any entry it posted, is discarded
// with the transaction.
func Transition(ctx context.Context, tx pgx.Tx, orderID int64, toState State, expectedVersion int32, p TransitionParams) (Order, error) {
	if !toState.Valid() {
		return Order{}, fmt.Errorf("%w: unknown target state %q", ErrInvalidParams, toState)
	}
	if err := p.validate(); err != nil {
		return Order{}, err
	}

	current, err := Get(ctx, tx, orderID)
	if err != nil {
		return Order{}, err
	}
	if current.Version != expectedVersion {
		return Order{}, fmt.Errorf("%w: order %d has version %d, expected %d",
			ErrVersionConflict, orderID, current.Version, expectedVersion)
	}

	r, legal := transitionTable[pair{From: current.State, To: toState}]
	if !legal {
		return Order{}, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, current.State, toState)
	}

	switch {
	case r.RequiresEntry && p.Entry == nil:
		return Order{}, fmt.Errorf("%w: %s -> %s", ErrEntryRequired, current.State, toState)
	case !r.RequiresEntry && p.Entry != nil:
		return Order{}, fmt.Errorf("%w: %s -> %s", ErrEntryNotAllowed, current.State, toState)
	}

	var entryID *int64
	if r.RequiresEntry {
		entry, err := journal.Post(ctx, tx, *p.Entry)
		if err != nil {
			return Order{}, fmt.Errorf("orders: posting entry for %s -> %s: %w", current.State, toState, err)
		}
		entryID = &entry.ID
	}

	row := tx.QueryRow(ctx, `
		UPDATE orders
		SET state = $1, version = version + 1, updated_at = now()
		WHERE id = $2 AND version = $3
		RETURNING id, external_id, customer_id, tier, state, amount_in, amount_out,
			fee_units, network_fee_units, recipient_address, quoted_at, quote_expires_at,
			version, created_at, updated_at
	`, string(toState), orderID, expectedVersion)

	updated, err := scanOrder(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("%w: order %d version %d (lost the race)", ErrVersionConflict, orderID, expectedVersion)
	}
	if err != nil {
		return Order{}, fmt.Errorf("orders: updating order %d: %w", orderID, err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO order_transitions (order_id, from_state, to_state, entry_id, actor, reason, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, orderID, string(current.State), string(toState), entryID, p.Actor, p.Reason, p.OccurredAt)
	if err != nil {
		return Order{}, fmt.Errorf("orders: logging transition for order %d: %w", orderID, err)
	}

	return updated, nil
}

const orderSelectSQL = `
	SELECT id, external_id, customer_id, tier, state, amount_in, amount_out,
		fee_units, network_fee_units, recipient_address, quoted_at, quote_expires_at,
		version, created_at, updated_at
	FROM orders`

type scanRow interface {
	Scan(dest ...any) error
}

func scanOrder(row scanRow) (Order, error) {
	var o Order
	var tier, state string
	var amountIn, amountOut, feeUnits, networkFeeUnits int64
	err := row.Scan(
		&o.ID, &o.ExternalID, &o.CustomerID, &tier, &state, &amountIn, &amountOut,
		&feeUnits, &networkFeeUnits, &o.RecipientAddress, &o.QuotedAt, &o.QuoteExpiresAt,
		&o.Version, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		return Order{}, err
	}
	o.Tier = Tier(tier)
	o.State = State(state)
	o.AmountIn = money.Amount{Asset: money.USDT_BEP20, Units: amountIn}
	o.AmountOut = money.Amount{Asset: money.USDT_TRC20, Units: amountOut}
	o.FeeUnits = money.Amount{Asset: money.USDT_TRC20, Units: feeUnits}
	o.NetworkFeeUnits = money.Amount{Asset: money.USDT_TRC20, Units: networkFeeUnits}
	return o, nil
}
