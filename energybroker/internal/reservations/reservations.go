// Package reservations is C4.4: §A's "With C5" contract made real --
// the chunk C5 will actually be built against once it exists.
//
// This chunk also resolves C4.4's own explicit OPEN QUESTION (energy
// delegated to internal/buffer's own staging address is not
// automatically usable by the specific TRON slot address a reservation
// names, since TRON resource delegation always targets one address).
// This package picks design (b) from that question: one shared buffer,
// with Create performing a fast RE-delegation from the buffer's holding
// address to the requested slot at reservation time, rather than design
// (a) (six parallel buffers, one pre-delegated per known payout slot).
//
// Why (b), concretely: internal/buffer (C4.3) already ships as a single
// pool against one configured StagingAddress -- (a) would mean reworking
// already-built, already-tested replenishment/reservation logic into six
// independently-sized, independently-replenished pools, a much larger
// change than this chunk's own scope, for a payoff ((a)'s "zero
// re-delegation latency") that is only worth that cost if a real
// re-delegation call turns out to be too slow in practice. (b) is also
// the natural, incremental fit for what C4.3 already built: Reserve's
// own claim never changes, only what happens to a claimed allocation
// immediately afterward.
//
// The chunk's own text is explicit that real re-delegation latency
// should be "confirmed on testnet before committing" -- the same
// discipline C2.4 applied to its own BSC contract address. This
// environment has no reachable TRON node (testnet or otherwise) to make
// that call against; TestFastPath_ConfirmsWellWithinDeadline (this
// package's own test suite) proves the FAST PATH'S OWN LOGIC completes
// well within a tight deadline against a fake vendor with a configurable
// synthetic delay, which is the testable assertion this chunk's own
// acceptance criteria ask for -- it is NOT a substitute for the real,
// unclosed action item: confirming actual on-chain re-delegation latency
// against a real TRON testnet before this ships to production. Whoever
// owns that verification should treat design (a) as the documented
// fallback if real latency turns out to be too high.
package reservations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"energybroker/internal/buffer"
	"energybroker/internal/db"
	"energybroker/internal/ledgerclient"
	"energybroker/internal/money"
	"energybroker/internal/provider"
	"energybroker/internal/routing"
)

// Status is one reservation's own lifecycle state.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusConfirmed Status = "CONFIRMED"
	StatusFailed    Status = "FAILED"
)

// Request is §A's own POST /v1/reservations request body.
type Request struct {
	IdempotencyKey string
	ExternalID     string
	TargetAddress  string
	EnergyUnits    int64
	Tier           string // "DIRECT" | "STANDARD" | "SWEEP"
	Deadline       time.Time
}

// ErrInvalidRequest guards Create against a malformed request reaching
// either path below.
var ErrInvalidRequest = errors.New("reservations: invalid request")

func (req Request) validate() error {
	if req.IdempotencyKey == "" {
		return fmt.Errorf("%w: idempotency key is required", ErrInvalidRequest)
	}
	if req.ExternalID == "" {
		return fmt.Errorf("%w: external_id is required", ErrInvalidRequest)
	}
	if req.TargetAddress == "" {
		return fmt.Errorf("%w: target_address is required", ErrInvalidRequest)
	}
	if req.EnergyUnits <= 0 {
		return fmt.Errorf("%w: energy_units must be positive", ErrInvalidRequest)
	}
	switch req.Tier {
	case "DIRECT", "STANDARD", "SWEEP":
	default:
		return fmt.Errorf("%w: tier %q is not one of DIRECT/STANDARD/SWEEP", ErrInvalidRequest, req.Tier)
	}
	if req.Deadline.IsZero() {
		return fmt.Errorf("%w: deadline is required", ErrInvalidRequest)
	}
	return nil
}

// Reservation is §A's own reservation resource.
//
// CostTRX is the real, non-zero cost of the energy THIS order actually
// consumed, populated once CONFIRMED, never nil after that: for the slow
// path, the fresh Delegate call's own cost; for the fast path, the SUM
// of whatever the underlying energy_buffer row(s) originally cost when
// Replenish first bought them (buffer.Row.CostTRX, carried through
// unchanged) -- retargeting them via Redelegate is itself free, but the
// energy was never free, and this field is "what did securing this
// order's energy cost," not "what did the redelegation call itself
// cost." Getting this right mattered enough to fix here in C4.5: an
// earlier version of this package (C4.4) hardcoded the fast path's own
// CostTRX to 0, which would have meant no cost ever attributed to C1 for
// energy consumed via the fast path at all. See ledgerclient's own
// ReportEnergyCost, which posts one E4 entry per underlying delegation,
// not one combined entry per reservation -- each entry is idempotency-
// keyed on ITS OWN delegation id (invariant 4), and a fast-path
// reservation spanning buffer rows from more than one original
// acquisition genuinely has more than one delegation behind it.
type Reservation struct {
	ID             int64
	IdempotencyKey string
	ExternalID     string
	OrderID        int64
	TargetAddress  string
	EnergyUnits    int64
	Tier           string
	Status         Status
	Vendor         *string
	CostTRX        *money.Amount
	ConfirmedAt    *time.Time
	Deadline       time.Time
	CreatedAt      time.Time
}

// ErrNotFound means no reservation exists with the given id.
var ErrNotFound = errors.New("reservations: no such reservation")

// OrderResolver is the one call this package needs from C1 --
// ledgerclient.Client's real implementation, or a fake for testing.
type OrderResolver interface {
	GetOrder(ctx context.Context, externalID string) (ledgerclient.Order, error)
}

// BufferReserver is the three calls this package needs from C4.3's own
// Buffer -- an interface, not a concrete *buffer.Buffer, so this
// package's own tests can exercise the fast path without a real
// Postgres-backed buffer behind it.
type BufferReserver interface {
	Reserve(ctx context.Context, orderID, units int64) (*buffer.Allocation, error)
	RowsByIDs(ctx context.Context, ids []int64) ([]buffer.Row, error)
	VerifyOnChain(ctx context.Context, d provider.Delegation) (bool, error)
}

// ProviderSelector is the two calls the slow path needs from C4.2's own
// Router: SelectProvider itself, and (C4.6) OnFallbackTriggered when it
// can't select anything -- recorded/de-duplicated/alerted on, per
// docs/runbook-energy-fallback.md, never an automated JustLendDAO call.
type ProviderSelector interface {
	SelectProvider(ctx context.Context, weights routing.RoutingWeights, ceiling float64) (routing.Selection, error)
	OnFallbackTriggered(ctx context.Context, selectionReason string, orderID *int64) (routing.FallbackEvent, error)
}

// EnergyCostReporter is the one call this package needs from C1 for cost
// attribution (C4.5) -- ledgerclient.Client's real implementation, or a
// fake for testing. Optional: nil means no cost is ever reported, never
// a panic, the same convention every other pluggable dependency in this
// project's own sibling modules uses (e.g. screening's own
// pipeline.MetricsRecorder) -- useful for tests that only care about the
// reservation/buffer mechanics, not C1 write-through.
type EnergyCostReporter interface {
	ReportEnergyCost(ctx context.Context, delegation provider.Delegation, orderID int64) error
}

// Config scopes the slow path's own routing/pricing behavior -- the
// same Weights/Ceiling internal/buffer's own Replenish uses, since both
// paths are choosing among the same three primary vendors under the
// same invariant-3 ceiling.
type Config struct {
	Weights            routing.RoutingWeights
	Ceiling            float64
	DelegationDuration time.Duration // how long the slow path's own fresh Delegate call leases capacity for
	// FallbackPollInterval is how often the slow path re-checks
	// SelectProvider while blocked on the manual fallback ladder
	// (DefaultFallbackPollInterval if unset) -- an outage is often
	// transient (a vendor recovers, a price spike passes), so a single
	// bad SelectProvider result must not fail the reservation outright;
	// only running out req.Deadline does (C4.6's own acceptance
	// criterion: "the reservation still resolves to failed once its
	// deadline elapses", not on the first fallback signal).
	FallbackPollInterval time.Duration
}

// DefaultDelegationDuration matches internal/buffer's own default, when
// Config.DelegationDuration is unset.
const DefaultDelegationDuration = 24 * time.Hour

func (cfg Config) delegationDuration() time.Duration {
	if cfg.DelegationDuration <= 0 {
		return DefaultDelegationDuration
	}
	return cfg.DelegationDuration
}

// DefaultFallbackPollInterval is how often the slow path re-checks
// SelectProvider while blocked on the fallback ladder, when
// Config.FallbackPollInterval is unset -- short enough to notice a
// recovery well within a realistic reservation deadline (tens of
// seconds to a few minutes), not so short it hammers routing/pricing.
const DefaultFallbackPollInterval = 2 * time.Second

func (cfg Config) fallbackPollInterval() time.Duration {
	if cfg.FallbackPollInterval <= 0 {
		return DefaultFallbackPollInterval
	}
	return cfg.FallbackPollInterval
}

// Service is this package's own entry point -- §A's "With C5" contract,
// implemented.
type Service struct {
	pool         *db.Pool
	orders       OrderResolver
	buf          BufferReserver
	router       ProviderSelector
	costReporter EnergyCostReporter
	providers    map[string]provider.EnergyProvider
	cfg          Config
}

// NewService wires a Service. providers must contain an entry for every
// provider name cfg.Weights names, and for every provider name any
// energy_buffer row's own provider_name could hold -- Create returns an
// error the first time either path needs a name with no matching entry.
// costReporter may be nil (see EnergyCostReporter's own doc comment).
func NewService(pool *db.Pool, orders OrderResolver, buf BufferReserver, router ProviderSelector, costReporter EnergyCostReporter, providers map[string]provider.EnergyProvider, cfg Config) *Service {
	return &Service{pool: pool, orders: orders, buf: buf, router: router, costReporter: costReporter, providers: providers, cfg: cfg}
}

// RecentReservedUnits implements buffer.DemandObserver (C4.3): the
// interface that chunk was built against before this package -- the
// thing that actually creates reservations -- existed to answer it.
func (s *Service) RecentReservedUnits(ctx context.Context, window time.Duration) (int64, error) {
	return recentReservedUnits(ctx, s.pool, window)
}

// Get fetches a reservation by id.
func (s *Service) Get(ctx context.Context, id int64) (Reservation, error) {
	r, err := getByID(ctx, s.pool, id)
	if err != nil {
		return Reservation{}, err
	}
	if r == nil {
		return Reservation{}, ErrNotFound
	}
	return *r, nil
}

// Create implements §A's own POST /v1/reservations: idempotent on
// req.IdempotencyKey, resolving req.ExternalID to C1's own internal
// order id, then the fast path (buffer.Reserve + Redelegate) or, on
// ErrBufferExhausted, the slow path (routing.SelectProvider +
// provider.Delegate, bounded by req.Deadline) -- see this package's own
// doc comment for why re-delegation, not a cold vendor call, is the fast
// path's own confirmation step.
func (s *Service) Create(ctx context.Context, req Request) (Reservation, error) {
	if err := req.validate(); err != nil {
		return Reservation{}, err
	}

	if existing, err := getByIdempotencyKey(ctx, s.pool, req.IdempotencyKey); err != nil {
		return Reservation{}, err
	} else if existing != nil {
		return *existing, nil
	}

	order, err := s.orders.GetOrder(ctx, req.ExternalID)
	if err != nil {
		return Reservation{}, fmt.Errorf("reservations: resolving external_id %s: %w", req.ExternalID, err)
	}

	reservation, err := insertPending(ctx, s.pool, req, order.ID)
	if err != nil {
		if errors.Is(err, errDuplicateIdempotencyKey) {
			// Lost a concurrent race to create this exact reservation --
			// the winner's row is the correct answer, not an error.
			existing, getErr := getByIdempotencyKey(ctx, s.pool, req.IdempotencyKey)
			if getErr != nil {
				return Reservation{}, getErr
			}
			if existing != nil {
				return *existing, nil
			}
		}
		return Reservation{}, err
	}

	alloc, err := s.buf.Reserve(ctx, order.ID, req.EnergyUnits)
	if err == nil {
		vendor, cost, delegations, fastErr := s.confirmFastPath(ctx, alloc, req.TargetAddress)
		if fastErr == nil {
			return s.confirmAndReport(ctx, reservation.ID, order.ID, vendor, cost, delegations)
		}
		// The buffer already committed this allocation (Reserve's own
		// transaction succeeded) before re-delegation failed -- those
		// energy_buffer rows are RESERVED against an allocation this
		// reservation will never actually use. Flagged loudly, not
		// silently retried or auto-released: same reconciliation posture
		// screening's own holds.Release takes when its local write fails
		// after a successful remote call, just mirrored (the remote call
		// failed here, not the local write).
		slog.Error("reservations: fast-path re-delegation failed after the buffer already reserved capacity -- the underlying allocation needs manual reconciliation",
			"reservation_id", reservation.ID, "allocation_id", alloc.ID, "order_id", order.ID, "error", fastErr)
		return s.fail(ctx, reservation.ID)
	}
	if !errors.Is(err, buffer.ErrBufferExhausted) {
		return Reservation{}, fmt.Errorf("reservations: reserving from the buffer: %w", err)
	}

	return s.slowPath(ctx, *reservation, req, order.ID)
}

// confirmFastPath redirects every energy_buffer row alloc claimed to
// targetAddress, verifying each one on-chain before trusting it. Zero
// calls to any EnergyProvider.Delegate -- only Redelegate -- per this
// chunk's own acceptance criterion. Returns one provider.Delegation per
// underlying row (reconstructed from that row's own original
// acquisition -- provider name, delegation id, units, and cost --  with
// TargetAddress updated to where it was just redirected), for
// confirmAndReport to post an E4 entry against each: a fast-path
// allocation spanning rows from more than one original acquisition
// genuinely has more than one delegation, and each needs its own,
// separately idempotency-keyed entry (invariant 4).
func (s *Service) confirmFastPath(ctx context.Context, alloc *buffer.Allocation, targetAddress string) (vendor string, totalCost money.Amount, delegations []provider.Delegation, err error) {
	rows, err := s.buf.RowsByIDs(ctx, alloc.RowIDs)
	if err != nil {
		return "", 0, nil, err
	}

	vendorsSeen := map[string]bool{}
	for _, row := range rows {
		prov, ok := s.providers[row.ProviderName]
		if !ok {
			return "", 0, nil, fmt.Errorf("reservations: buffer row %d references provider %q with no wired EnergyProvider", row.ID, row.ProviderName)
		}

		fresh, err := prov.Redelegate(ctx, row.DelegationID, targetAddress, row.Units)
		if err != nil {
			return "", 0, nil, fmt.Errorf("redelegating row %d (%s, delegation %s): %w", row.ID, row.ProviderName, row.DelegationID, err)
		}

		confirmed, err := s.buf.VerifyOnChain(ctx, fresh)
		if err != nil {
			return "", 0, nil, fmt.Errorf("verifying redelegated row %d on-chain: %w", row.ID, err)
		}
		if !confirmed {
			return "", 0, nil, fmt.Errorf("redelegated row %d did not verify on-chain", row.ID)
		}

		vendorsSeen[row.ProviderName] = true
		totalCost += row.CostTRX
		delegations = append(delegations, provider.Delegation{
			ID: row.DelegationID, ProviderName: row.ProviderName, TargetAddress: targetAddress,
			EnergyUnits: row.Units, CostTRX: row.CostTRX, RequestedAt: row.AcquiredAt,
		})
	}

	return joinVendorNames(vendorsSeen), totalCost, delegations, nil
}

// pollUntilSelectableOrDeadline retries routing.SelectProvider while it
// keeps falling back to the manual ladder, on cfg.fallbackPollInterval(),
// until either a real primary becomes selectable or slowCtx's own
// deadline elapses -- C4.6's own acceptance criterion: an outage or a
// price spike is often transient, so a single bad SelectProvider result
// must not fail the reservation outright; only running out the caller's
// own deadline does. The very first fallback signal is recorded once via
// OnFallbackTriggered (de-duplicated against any already-open event of
// the same reason -- see routing's own doc comment), not once per poll:
// re-triggering on every 2-second tick of a single reservation's own
// wait would defeat that de-duplication's whole purpose.
func (s *Service) pollUntilSelectableOrDeadline(slowCtx context.Context, reservationID, orderID int64) (routing.Selection, error) {
	var triggeredReason string
	for {
		sel, err := s.router.SelectProvider(slowCtx, s.cfg.Weights, s.cfg.Ceiling)
		if err != nil {
			slog.Error("reservations: slow path: selecting a provider failed", "reservation_id", reservationID, "error", err)
			return routing.Selection{}, err
		}
		if sel.Provider != provider.JustLendManual {
			return sel, nil
		}

		if sel.Reason != triggeredReason {
			triggeredReason = sel.Reason
			oid := orderID
			event, err := s.router.OnFallbackTriggered(slowCtx, sel.Reason, &oid)
			if err != nil {
				slog.Error("reservations: slow path: recording the fallback event failed", "reservation_id", reservationID, "error", err)
			}
			slog.Warn("reservations: slow path: routing fell back to the manual ladder -- waiting for a resolution or the reservation's own deadline, see docs/runbook-energy-fallback.md",
				"reservation_id", reservationID, "reason", sel.Reason, "fallback_event_id", event.ID)
		}

		select {
		case <-slowCtx.Done():
			slog.Error("reservations: slow path: deadline elapsed while blocked on the manual fallback ladder", "reservation_id", reservationID, "reason", sel.Reason)
			return routing.Selection{}, slowCtx.Err()
		case <-time.After(s.cfg.fallbackPollInterval()):
		}
	}
}

// slowPath is C4.4's own vendor-latency-carrying path: a live
// routing.SelectProvider + a direct, synchronous Delegate, bounded by
// req.Deadline. Metering how often this path is taken (versus the fast
// path) is C4.8's own job, not this chunk's -- see this chunk's own
// build spec.
func (s *Service) slowPath(ctx context.Context, reservation Reservation, req Request, orderID int64) (Reservation, error) {
	slowCtx, cancel := context.WithDeadline(ctx, req.Deadline)
	defer cancel()

	sel, err := s.pollUntilSelectableOrDeadline(slowCtx, reservation.ID, orderID)
	if err != nil {
		return s.fail(ctx, reservation.ID)
	}

	prov, ok := s.providers[sel.Provider]
	if !ok {
		return Reservation{}, fmt.Errorf("reservations: router selected %q, which has no wired EnergyProvider", sel.Provider)
	}

	delegation, err := prov.Delegate(slowCtx, req.TargetAddress, req.EnergyUnits, s.cfg.delegationDuration())
	if err != nil {
		slog.Error("reservations: slow path: Delegate failed", "reservation_id", reservation.ID, "provider", sel.Provider, "error", err)
		return s.fail(ctx, reservation.ID)
	}

	confirmed, err := s.buf.VerifyOnChain(slowCtx, delegation)
	if err != nil {
		slog.Error("reservations: slow path: on-chain verification errored", "reservation_id", reservation.ID, "provider", sel.Provider, "error", err)
		return s.fail(ctx, reservation.ID)
	}
	if !confirmed {
		slog.Error("reservations: slow path: delegation did not verify on-chain -- a vendor's own 200 response is never trusted alone (invariant 1)",
			"reservation_id", reservation.ID, "provider", sel.Provider)
		return s.fail(ctx, reservation.ID)
	}

	return s.confirmAndReport(ctx, reservation.ID, orderID, delegation.ProviderName, delegation.CostTRX, []provider.Delegation{delegation})
}

// confirmAndReport marks id CONFIRMED, then reports cost to C1 for every
// delegation involved -- best-effort, AFTER confirmation, using ctx (not
// any path-specific deadline context, which may already be past its own
// deadline by now): the energy is already secured on-chain at this
// point, and a cost-reporting failure is a bookkeeping problem, never a
// reason to undo a reservation a customer's payout may already be
// relying on. A failure here is logged loudly (P1-flavored, via
// ledgerclient's own ErrUnexpectedHalt/ErrIdempotencyConflictBug for the
// cases that are genuinely surprising) rather than silently swallowed,
// but never flips the reservation back to FAILED.
func (s *Service) confirmAndReport(ctx context.Context, reservationID, orderID int64, vendor string, cost money.Amount, delegations []provider.Delegation) (Reservation, error) {
	r, err := s.confirm(ctx, reservationID, vendor, cost)
	if err != nil {
		return Reservation{}, err
	}

	if s.costReporter != nil {
		for _, d := range delegations {
			if err := s.costReporter.ReportEnergyCost(ctx, d, orderID); err != nil {
				slog.Error("reservations: reporting energy cost to C1 failed -- needs manual reconciliation",
					"reservation_id", reservationID, "order_id", orderID, "delegation_id", d.ID, "provider", d.ProviderName, "error", err)
			}
		}
	}
	return r, nil
}

func (s *Service) confirm(ctx context.Context, id int64, vendor string, cost money.Amount) (Reservation, error) {
	r, err := markConfirmed(ctx, s.pool, id, vendor, cost)
	if err != nil {
		return Reservation{}, err
	}
	return *r, nil
}

func (s *Service) fail(ctx context.Context, id int64) (Reservation, error) {
	r, err := markFailed(ctx, s.pool, id)
	if err != nil {
		return Reservation{}, err
	}
	return *r, nil
}

// joinVendorNames renders the set of providers involved in one
// allocation as a stable, human-readable string -- almost always exactly
// one name; more than one only when a single reservation's claim spanned
// buffer rows from different providers, which Reserve's own
// oldest-expiry-first claim order makes possible but uncommon.
func joinVendorNames(seen map[string]bool) string {
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic, testable string
	out := ""
	for i, name := range names {
		if i > 0 {
			out += ","
		}
		out += name
	}
	return out
}
