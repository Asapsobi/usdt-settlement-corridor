package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/accounts"
	"ledger/internal/halt"
	"ledger/internal/journal"
	"ledger/internal/money"
)

// haltCache is package-level rather than a Transition parameter because
// Transition's signature is fixed by C1.5 and every existing caller
// already matches it. SetHaltCache must be called once, at process
// startup (cmd/ledgerd) or in a test's setup, before any halt-blocked
// transition runs -- see the ErrHaltCacheNotConfigured check in
// Transition for what happens if it hasn't been.
var haltCache *halt.Cache

// SetHaltCache wires up the in-memory halt cache Transition consults for
// halt-blocked transitions (see transitionTable's HaltBlocked field).
// Call once per process.
func SetHaltCache(c *halt.Cache) {
	haltCache = c
}

// ErrHaltCacheNotConfigured means SetHaltCache was never called. Treated
// as a hard error for any halt-blocked transition rather than silently
// treating "no cache configured" as "not halted": failing closed here is
// deliberate -- letting money-moving transitions through because of a
// missing setup call is a worse failure mode than refusing them.
var ErrHaltCacheNotConfigured = errors.New("orders: halt cache not configured; call SetHaltCache at startup")

// ErrSystemHalted is returned by Transition for a halt-blocked pair while
// the ledger is halted.
var ErrSystemHalted = errors.New("orders: system is halted")

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
	// Use this when the transition itself is what should cause the entry
	// to exist.
	Entry *journal.EntryRequest
	// EntryID names an entry ALREADY posted earlier in the same
	// transaction -- for example by journal.Reverse, called directly by
	// C1.6's reorg handling before it calls Transition -- to be recorded
	// as this transition's cause without posting anything new. At most
	// one of Entry / EntryID may be set, and exactly one must be set
	// when the transition rule requires an entry.
	EntryID *int64
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
	if p.Entry != nil && p.EntryID != nil {
		return fmt.Errorf("%w: both Entry and EntryID set", ErrInvalidParams)
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
// For a HaltBlocked pair (see transitionTable), halt state is checked
// twice: first the in-memory Cache (haltCache), for fast rejection with
// up to 1 second of staleness; then, only if that says "not halted," the
// authoritative transactional read against system_state using this same
// tx -- because a fast-but-stale "not halted" is not good enough to
// actually let money leave. A pair that is not HaltBlocked never checks
// either: quoted->funded (deposit recording) must keep working while
// halted, and neither read-only calls nor journal.Reverse ever reach
// this function at all.
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

	if r.HaltBlocked {
		if haltCache == nil {
			return Order{}, ErrHaltCacheNotConfigured
		}
		fastHalted, err := haltCache.IsHalted(ctx)
		if err != nil {
			return Order{}, fmt.Errorf("orders: checking cached halt state: %w", err)
		}
		if fastHalted {
			return Order{}, fmt.Errorf("%w: %s -> %s", ErrSystemHalted, current.State, toState)
		}

		authHalted, err := halt.IsHalted(ctx, tx)
		if err != nil {
			return Order{}, fmt.Errorf("orders: checking authoritative halt state: %w", err)
		}
		if authHalted {
			return Order{}, fmt.Errorf("%w: %s -> %s", ErrSystemHalted, current.State, toState)
		}
	}

	switch {
	case r.RequiresEntry && p.Entry == nil && p.EntryID == nil:
		return Order{}, fmt.Errorf("%w: %s -> %s", ErrEntryRequired, current.State, toState)
	case !r.RequiresEntry && (p.Entry != nil || p.EntryID != nil):
		return Order{}, fmt.Errorf("%w: %s -> %s", ErrEntryNotAllowed, current.State, toState)
	}

	entryID := p.EntryID
	if p.Entry != nil {
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
