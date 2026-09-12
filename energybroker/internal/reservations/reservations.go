// Package reservations is C4.4: §A's "With C5" contract made real --
// the chunk C5 will actually be built against once it exists.
//
// This chunk also resolves C4.4's own explicit OPEN QUESTION (energy
// delegated to internal/buffer's own holding address is not
// automatically usable by the specific TRON slot address a reservation
// names, since TRON resource delegation always targets one address).
// This package originally picked design (b) from that question: one
// shared buffer against a single staging address, with Create performing
// a fast RE-delegation to the requested slot at reservation time. That
// design assumed a real vendor could retarget an already-issued
// delegation to a new address on request, at no further cost -- an
// assumption that turned out to be false against all three real
// vendors' actual APIs (Tronsell, Netts, CatFee each only expose "buy a
// new, fixed-receiver order," never "retarget an existing one" -- see
// internal/provider.EnergyProvider's own doc comment). A same-shaped
// "Redelegate" against any of them could only mean a second live
// purchase, which would put vendor latency back in the fast path's own
// critical path (exactly what internal/buffer's staging design exists
// to avoid) and pay for the same energy twice.
//
// This package now uses design (a) instead: internal/buffer keeps one
// independently-sized, independently-replenished pool per known payout
// slot address (buffer.Config.SlotAddresses), each pre-acquired already
// pointed at its own final destination. Reserve's own claim query filters
// on the requested target address directly, so a fast-path confirmation
// (confirmFastPath, below) is a pure database transition against
// already-verified rows -- zero vendor calls, zero re-verification,
// genuinely "zero re-delegation latency" because there is no
// re-delegation step left at all.
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
	"energybroker/internal/pricing"
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
// unchanged) -- claiming an already-provisioned row is itself free (a
// pure database transition), but the energy it holds never was, and
// this field is "what did securing this order's energy cost." Getting
// this right mattered enough to fix here in C4.5: an
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
	// FastPath is nil until CONFIRMED (a PENDING or FAILED reservation
	// never resolved to either path), true for a fast-path (buffer
	// Reserve against an already-provisioned slot) confirmation, false
	// for a slow-path (live Delegate) one. C4.8's own GET
	// /v1/system/invariants and the
	// reservations_total{fast_path,slow_path,failed} metric both read
	// this.
	FastPath *bool
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
// Postgres-backed buffer behind it. Only the slow path still calls
// VerifyOnChain: a row Reserve returns for the fast path was already
// independently verified on-chain once, when Replenish first acquired
// it (invariant 1) -- claiming it is a pure bookkeeping transition that
// changes nothing on-chain, so re-verifying it again at reservation
// time would be redundant, not safer. The slow path's fresh Delegate
// result has no such prior verification, so it still needs one.
type BufferReserver interface {
	Reserve(ctx context.Context, orderID int64, targetAddress string, units int64) (*buffer.Allocation, error)
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
	// RecordVendorOvercharge is C4.7's own reconciliation-flagging call --
	// see routing.Router's own doc comment for why this returns
	// routing.ErrChargedAboveCeiling rather than a distinct bool.
	RecordVendorOvercharge(ctx context.Context, recon pricing.ChargeReconciliation, delegationID string, energyUnits int64, orderID *int64) error
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
	metrics      MetricsRecorder
	providers    map[string]provider.EnergyProvider
	cfg          Config
}

// MetricsRecorder is how Create reports the reservations_total{fast_path,
// slow_path,failed} metric C4.8's own build spec names -- optional, nil
// means no metrics are recorded, never a panic, the same convention
// every other pluggable dependency in this project's own sibling
// modules uses.
type MetricsRecorder interface {
	// ReservationConfirmed fires once per CONFIRMED reservation, tagged
	// by which path confirmed it.
	ReservationConfirmed(viaFastPath bool)
	// ReservationFailed fires once per FAILED reservation, regardless of
	// which path was attempted -- "failed" is its own bucket, not
	// fast_path_failed/slow_path_failed, per this chunk's own metric name.
	ReservationFailed()
}

// NewService wires a Service. providers must contain an entry for every
// provider name cfg.Weights names, and for every provider name any
// energy_buffer row's own provider_name could hold -- Create returns an
// error the first time either path needs a name with no matching entry.
// costReporter and metrics may both be nil (see their own doc comments).
//
// Returns an error if cfg.Ceiling is zero or negative -- C4.7's own
// adversarial scenario, same reasoning as buffer.NewBuffer's own
// identical check: refuse to start rather than silently permanently
// fallback-ladder every reservation's own slow path.
func NewService(pool *db.Pool, orders OrderResolver, buf BufferReserver, router ProviderSelector, costReporter EnergyCostReporter, metrics MetricsRecorder, providers map[string]provider.EnergyProvider, cfg Config) (*Service, error) {
	if cfg.Ceiling <= 0 {
		return nil, fmt.Errorf("reservations: %w: Config.Ceiling must be positive, got %v", routing.ErrInvalidCeiling, cfg.Ceiling)
	}
	return &Service{pool: pool, orders: orders, buf: buf, router: router, costReporter: costReporter, metrics: metrics, providers: providers, cfg: cfg}, nil
}

// RecentReservedUnits implements buffer.DemandObserver (C4.3): the
// interface that chunk was built against before this package -- the
// thing that actually creates reservations -- existed to answer it.
// Scoped to targetAddress, matching each slot's own independent target
// level (design (a) -- see this package's own doc comment).
func (s *Service) RecentReservedUnits(ctx context.Context, window time.Duration, targetAddress string) (int64, error) {
	return recentReservedUnits(ctx, s.pool, window, targetAddress)
}

// DemandObserver implements buffer.DemandObserver directly off a *db.Pool,
// with no dependency on a constructed *Service. This exists to break a
// real circular constructor dependency in production wiring:
// buffer.NewBuffer needs a DemandObserver, but the only thing that
// naturally answers "how much was recently reserved" is this package's
// own reservations table -- which NewService also needs a *buffer.Buffer
// to construct. Every query this type runs is byte-for-byte the same one
// Service.RecentReservedUnits runs; this is not a second implementation
// to keep in sync, only a second entry point to the same one.
type DemandObserver struct {
	Pool *db.Pool
}

// RecentReservedUnits implements buffer.DemandObserver.
func (o DemandObserver) RecentReservedUnits(ctx context.Context, window time.Duration, targetAddress string) (int64, error) {
	return recentReservedUnits(ctx, o.Pool, window, targetAddress)
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

// ListByStatus lists reservations matching any of statuses, newest
// first, bounded by limit.
func (s *Service) ListByStatus(ctx context.Context, statuses []string, limit int) ([]Reservation, error) {
	return listByStatus(ctx, s.pool, statuses, limit)
}

// Create implements §A's own POST /v1/reservations: idempotent on
// req.IdempotencyKey, resolving req.ExternalID to C1's own internal
// order id, then the fast path (buffer.Reserve against req.TargetAddress's
// own pre-provisioned slot) or, on ErrBufferExhausted, the slow path
// (routing.SelectProvider + provider.Delegate, bounded by req.Deadline)
// -- see this package's own doc comment for why a database claim, not a
// cold vendor call, is the fast path's own confirmation step.
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

	alloc, err := s.buf.Reserve(ctx, order.ID, req.TargetAddress, req.EnergyUnits)
	if err == nil {
		vendor, cost, delegations, fastErr := s.confirmFastPath(ctx, alloc)
		if fastErr == nil {
			return s.confirmAndReport(ctx, reservation.ID, order.ID, vendor, cost, delegations, true)
		}
		// The buffer already committed this allocation (Reserve's own
		// transaction succeeded) before confirmFastPath failed -- those
		// energy_buffer rows are RESERVED against an allocation this
		// reservation will never actually use. Flagged loudly, not
		// silently retried or auto-released: same reconciliation posture
		// screening's own holds.Release takes when its local write fails
		// after a successful remote call, just mirrored (the remote call
		// failed here, not the local write). In practice this should only
		// ever be a configuration error (a buffer row references a
		// provider name with no wired EnergyProvider) -- there is no
		// vendor call left in this path to fail transiently.
		slog.Error("reservations: fast-path confirmation failed after the buffer already reserved capacity -- the underlying allocation needs manual reconciliation",
			"reservation_id", reservation.ID, "allocation_id", alloc.ID, "order_id", order.ID, "error", fastErr)
		return s.fail(ctx, reservation.ID)
	}
	if !errors.Is(err, buffer.ErrBufferExhausted) {
		return Reservation{}, fmt.Errorf("reservations: reserving from the buffer: %w", err)
	}

	return s.slowPath(ctx, *reservation, req, order.ID)
}

// confirmFastPath turns alloc's claimed energy_buffer rows directly into
// the Delegations confirmAndReport needs to post to the ledger. Zero
// vendor calls of any kind -- every row Reserve claimed was pre-acquired
// by Replenish already pointed at the reservation's own target address
// (buffer.Buffer keeps one independently-sized pool per known payout
// slot, see internal/provider.EnergyProvider's own doc comment for why),
// and was already independently verified on-chain once, at Replenish
// time (invariant 1) -- claiming it here is a pure database transition
// that changes nothing on-chain, so there is nothing left to call or
// re-verify. Returns one provider.Delegation per underlying row, for
// confirmAndReport to post an E4 entry against each: a fast-path
// allocation spanning rows from more than one original acquisition
// genuinely has more than one delegation, and each needs its own,
// separately idempotency-keyed entry (invariant 4).
func (s *Service) confirmFastPath(ctx context.Context, alloc *buffer.Allocation) (vendor string, totalCost money.Amount, delegations []provider.Delegation, err error) {
	rows, err := s.buf.RowsByIDs(ctx, alloc.RowIDs)
	if err != nil {
		return "", 0, nil, err
	}

	vendorsSeen := map[string]bool{}
	for _, row := range rows {
		if _, ok := s.providers[row.ProviderName]; !ok {
			return "", 0, nil, fmt.Errorf("reservations: buffer row %d references provider %q with no wired EnergyProvider", row.ID, row.ProviderName)
		}

		vendorsSeen[row.ProviderName] = true
		totalCost += row.CostTRX
		delegations = append(delegations, provider.Delegation{
			ID: row.DelegationID, ProviderName: row.ProviderName, TargetAddress: row.SlotAddress,
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

	// A fresh quote immediately before Delegate, deliberately not
	// whatever cached price SelectProvider itself used to make its
	// routing decision moments ago (C4.7's own "concurrent replenishment
	// and reservation racing on a price update" scenario: the two must
	// each reconcile against the price in effect right at their OWN
	// Delegate call, not a shared, possibly-already-stale routing-time
	// read).
	quote, err := prov.Quote(slowCtx)
	if err != nil {
		slog.Error("reservations: slow path: re-quoting the just-selected provider failed", "reservation_id", reservation.ID, "provider", sel.Provider, "error", err)
		return s.fail(ctx, reservation.ID)
	}

	delegation, err := prov.Delegate(slowCtx, req.TargetAddress, req.EnergyUnits, s.cfg.delegationDuration())
	if err != nil {
		slog.Error("reservations: slow path: Delegate failed", "reservation_id", reservation.ID, "provider", sel.Provider, "error", err)
		return s.fail(ctx, reservation.ID)
	}

	// C4.7: reconcile what this provider actually charged against what
	// it just quoted and against ceiling -- see buffer.Replenish's own
	// identical check for why this can't be skipped just because the
	// vendor's own API call already "succeeded".
	recon := pricing.ReconcileCharge(quote.PricePerUnitSun, delegation, s.cfg.Ceiling)
	oid := orderID
	if err := s.router.RecordVendorOvercharge(ctx, recon, delegation.ID, delegation.EnergyUnits, &oid); err != nil {
		if errors.Is(err, routing.ErrChargedAboveCeiling) {
			slog.Error("reservations: slow path: refusing to confirm on an over-ceiling-charged delegation", "reservation_id", reservation.ID, "provider", sel.Provider, "error", err)
			return s.fail(ctx, reservation.ID)
		}
		slog.Error("reservations: slow path: recording the vendor overcharge event failed", "reservation_id", reservation.ID, "provider", sel.Provider, "error", err)
	}

	// A vendor's own 200 response is never trusted alone (invariant 1) --
	// but the on-chain state it's claiming can genuinely take a few
	// seconds to propagate after that response returns (TRON's own block
	// time, plus whatever the vendor's own broadcast pipeline adds).
	// Found live running a real order against CatFee (10 Sep 2026): the
	// very first VerifyOnChain call after Delegate returned saw 0
	// available energy and failed the reservation outright, discarding a
	// delegation that (confirmed independently, seconds later) landed
	// correctly at 64999/65000 units -- a real purchase, wasted by
	// checking before it had a chance to land, not a real shortfall. Three
	// attempts, 3s apart, bounded by slowCtx's own deadline, gives real
	// propagation time without weakening what "confirmed" means -- a
	// delegation that's genuinely absent or partial after 9s still fails
	// exactly as before.
	var confirmed bool
	var verifyErr error
verifyRetry:
	for attempt := 1; attempt <= 3; attempt++ {
		confirmed, verifyErr = s.buf.VerifyOnChain(slowCtx, delegation)
		if verifyErr != nil || confirmed {
			break
		}
		if attempt < 3 {
			select {
			case <-slowCtx.Done():
				break verifyRetry
			case <-time.After(3 * time.Second):
			}
		}
	}
	if verifyErr != nil {
		slog.Error("reservations: slow path: on-chain verification errored", "reservation_id", reservation.ID, "provider", sel.Provider, "error", verifyErr)
		return s.fail(ctx, reservation.ID)
	}
	if !confirmed {
		slog.Error("reservations: slow path: delegation did not verify on-chain after 3 attempts over 9s -- a vendor's own 200 response is never trusted alone (invariant 1)",
			"reservation_id", reservation.ID, "provider", sel.Provider)
		return s.fail(ctx, reservation.ID)
	}

	return s.confirmAndReport(ctx, reservation.ID, orderID, delegation.ProviderName, delegation.CostTRX, []provider.Delegation{delegation}, false)
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
func (s *Service) confirmAndReport(ctx context.Context, reservationID, orderID int64, vendor string, cost money.Amount, delegations []provider.Delegation, viaFastPath bool) (Reservation, error) {
	r, err := s.confirm(ctx, reservationID, vendor, cost, viaFastPath)
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

// ManualConfirm is the "needs manual reconciliation" gap confirmAndReport's
// own doc comment names, given an actual tool: a reservation the slow
// path marked FAILED (a verification timeout, a transient error) whose
// underlying delegation is independently confirmed real -- e.g. via a
// direct on-chain read against the vendor's target address -- gets
// confirmed here using that already-real delegation, instead of either
// staying permanently stuck (Create's own idempotency-key replay means
// the normal path never re-attempts a terminal FAILED row) or paying for
// a second vendor order that duplicates the first. Ops-tool only, never
// called from Create's own request path -- the caller is responsible for
// having independently verified the delegation before calling this, the
// same trust boundary cmd/seed-slot and cmd/seed-slot-key already
// established for their own "no HTTP route for this operator action"
// gaps elsewhere in this project.
func (s *Service) ManualConfirm(ctx context.Context, reservationID, orderID int64, delegation provider.Delegation) (Reservation, error) {
	return s.confirmAndReport(ctx, reservationID, orderID, delegation.ProviderName, delegation.CostTRX, []provider.Delegation{delegation}, false)
}

func (s *Service) confirm(ctx context.Context, id int64, vendor string, cost money.Amount, viaFastPath bool) (Reservation, error) {
	r, err := markConfirmed(ctx, s.pool, id, vendor, cost, viaFastPath)
	if err != nil {
		return Reservation{}, err
	}
	if s.metrics != nil {
		s.metrics.ReservationConfirmed(viaFastPath)
	}
	return *r, nil
}

func (s *Service) fail(ctx context.Context, id int64) (Reservation, error) {
	r, err := markFailed(ctx, s.pool, id)
	if err != nil {
		return Reservation{}, err
	}
	if s.metrics != nil {
		s.metrics.ReservationFailed()
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
