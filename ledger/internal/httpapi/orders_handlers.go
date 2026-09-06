package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/db"
	"ledger/internal/journal"
	"ledger/internal/money"
	"ledger/internal/orders"
)

type postOrderRequest struct {
	ExternalID       string    `json:"external_id"`
	CustomerID       string    `json:"customer_id"`
	Tier             string    `json:"tier"`
	AmountIn         string    `json:"amount_in"`
	AmountOut        string    `json:"amount_out"`
	FeeUnits         string    `json:"fee_units"`
	NetworkFeeUnits  string    `json:"network_fee_units"`
	RecipientAddress string    `json:"recipient_address"`
	QuotedAt         time.Time `json:"quoted_at"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
}

type orderResponse struct {
	ID               int64     `json:"id"`
	ExternalID       string    `json:"external_id"`
	CustomerID       string    `json:"customer_id"`
	Tier             string    `json:"tier"`
	State            string    `json:"state"`
	AmountIn         string    `json:"amount_in"`
	AmountOut        string    `json:"amount_out"`
	FeeUnits         string    `json:"fee_units"`
	NetworkFeeUnits  string    `json:"network_fee_units"`
	RecipientAddress string    `json:"recipient_address"`
	SenderAddress    *string   `json:"sender_address"`
	QuotedAt         time.Time `json:"quoted_at"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
	Version          int32     `json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func toOrderResponse(o orders.Order) orderResponse {
	amountIn, _ := money.Format(o.AmountIn)
	amountOut, _ := money.Format(o.AmountOut)
	feeUnits, _ := money.Format(o.FeeUnits)
	networkFeeUnits, _ := money.Format(o.NetworkFeeUnits)
	return orderResponse{
		ID: o.ID, ExternalID: o.ExternalID, CustomerID: o.CustomerID, Tier: string(o.Tier), State: string(o.State),
		AmountIn: amountIn, AmountOut: amountOut, FeeUnits: feeUnits, NetworkFeeUnits: networkFeeUnits,
		RecipientAddress: o.RecipientAddress, SenderAddress: o.SenderAddress,
		QuotedAt: o.QuotedAt, QuoteExpiresAt: o.QuoteExpiresAt,
		Version: o.Version, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
}

// postOrder is POST /v1/orders, always creating in Quoted. Amount fields
// are parsed against the fixed assets orders.CreateParams itself
// documents (amount_in is always USDT_BEP20, the other three always
// USDT_TRC20) -- the request has no per-field asset to get wrong.
func (s *Server) postOrder(w http.ResponseWriter, r *http.Request) {
	var req postOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	amountIn, err := money.ParseDecimal(req.AmountIn, money.USDT_BEP20)
	if err != nil {
		writeErr(w, err)
		return
	}
	amountOut, err := money.ParseDecimal(req.AmountOut, money.USDT_TRC20)
	if err != nil {
		writeErr(w, err)
		return
	}
	feeUnits, err := money.ParseDecimal(req.FeeUnits, money.USDT_TRC20)
	if err != nil {
		writeErr(w, err)
		return
	}
	networkFeeUnits, err := money.ParseDecimal(req.NetworkFeeUnits, money.USDT_TRC20)
	if err != nil {
		writeErr(w, err)
		return
	}

	order, err := orders.Create(r.Context(), s.Pool, orders.CreateParams{
		ExternalID:       req.ExternalID,
		CustomerID:       req.CustomerID,
		Tier:             orders.Tier(req.Tier),
		AmountIn:         amountIn,
		AmountOut:        amountOut,
		FeeUnits:         feeUnits,
		NetworkFeeUnits:  networkFeeUnits,
		RecipientAddress: req.RecipientAddress,
		QuotedAt:         req.QuotedAt,
		QuoteExpiresAt:   req.QuoteExpiresAt,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, toOrderResponse(order))
}

// getOrder is GET /v1/orders/{external_id}.
func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	externalID, ok := urlParam(w, r, "external_id")
	if !ok {
		return
	}
	order, err := orders.GetByExternalID(r.Context(), s.Pool, externalID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toOrderResponse(order))
}

type listOrdersResponse struct {
	Orders     []orderResponse `json:"orders"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// getOrders is GET /v1/orders?state=<state>&updated_after=<cursor>&limit=<n>
// -- added for C3 (screening) discovery: C1's only way to say "which
// orders just entered a state", since there is no event bus in this
// system by design (see docs/03-build/c3-screening-build-prompts.md's
// "Read this first"). Deliberately generic (list-by-state, not
// "list-funded-for-screening") so it's a reusable primitive for ops
// tooling and C6 too, not a point-to-point coupling to one caller.
//
// next_cursor is always returned when there is a cursor to give,
// including an empty page (echoes the caller's own updated_after back so
// a poller never has to special-case "nothing new yet" versus "here's
// where you were") -- a poller can always feed it straight back in as
// its next updated_after, forever.
func (s *Server) getOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	stateParam := q.Get("state")
	if stateParam == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "state is required"))
		return
	}
	state := orders.State(stateParam)
	if !state.Valid() {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "unknown state "+stateParam))
		return
	}

	limit := orders.DefaultListLimit
	if raw := q.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "limit must be a positive integer"))
			return
		}
		limit = parsed
	}

	var after *orders.Cursor
	if raw := q.Get("updated_after"); raw != "" {
		parsed, err := orders.ParseCursor(raw)
		if err != nil {
			writeErr(w, err)
			return
		}
		after = &parsed
	}

	list, err := orders.ListByStateAfter(r.Context(), s.Pool, state, after, limit)
	if err != nil {
		writeErr(w, err)
		return
	}

	resp := listOrdersResponse{Orders: make([]orderResponse, len(list))}
	for i, o := range list {
		resp.Orders[i] = toOrderResponse(o)
	}
	switch {
	case len(list) > 0:
		last := list[len(list)-1]
		resp.NextCursor = (orders.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}).String()
	case after != nil:
		resp.NextCursor = after.String()
	}
	respondJSON(w, http.StatusOK, resp)
}

type transitionEntryRequest struct {
	EntryType  string             `json:"entry_type"`
	OccurredAt time.Time          `json:"occurred_at"`
	Lines      []entryLineRequest `json:"lines"`
	Metadata   map[string]any     `json:"metadata,omitempty"`
}

type postTransitionRequest struct {
	ToState         string                  `json:"to_state"`
	ExpectedVersion int32                   `json:"expected_version"`
	Reason          string                  `json:"reason"`
	OccurredAt      time.Time               `json:"occurred_at"`
	Entry           *transitionEntryRequest `json:"entry,omitempty"`
	// EntryID names an entry already posted -- in practice a reversal
	// created via POST /v1/entries/{id}/reversal -- to be recorded as
	// this transition's cause without posting anything new.
	//
	// orders.TransitionParams has had this field since C1.5, but until
	// C1.11 nothing exposed it, so the only way to drive the two
	// reversal-shaped transitions (funded->quoted, dispatching->held)
	// over HTTP was to hand-build a negating entry and post it through
	// `entry`. That produces an ordinary entry with reversal_of NULL: it
	// balances, so nothing rejects it, but it is not linked to what it
	// undoes and it is not covered by the UNIQUE constraint that makes a
	// double-reversal impossible. Both halves of that guarantee are the
	// point of C1.6, and this field is what lets a remote caller keep
	// them.
	//
	// At most one of entry / entry_id may be set; TransitionParams
	// enforces that, and exactly one is required when the transition rule
	// requires an entry.
	EntryID *int64 `json:"entry_id,omitempty"`
	// SenderAddress is sibling to entry, not a line or metadata field
	// inside it: it isn't a ledger amount, and C3 needs to query it
	// structurally, not parse a jsonb blob this API makes no shape
	// promise about. Only accepted on a transition into funded -- see
	// orders.TransitionParams.SenderAddress.
	SenderAddress *string `json:"sender_address,omitempty"`
}

// postTransition is POST /v1/orders/{external_id}/transitions. When the
// request carries an entry, it is posted with THIS request's
// Idempotency-Key header as its own idempotency key (not a separately
// generated one) -- retrying the identical HTTP request therefore
// replays the identical journal entry too, the same idempotent-retry
// guarantee POST /entries gives standalone callers.
func (s *Server) postTransition(w http.ResponseWriter, r *http.Request) {
	externalID, ok := urlParam(w, r, "external_id")
	if !ok {
		return
	}

	var req postTransitionRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	order, err := orders.GetByExternalID(r.Context(), s.Pool, externalID)
	if err != nil {
		writeErr(w, err)
		return
	}

	var entryReq *journal.EntryRequest
	if req.Entry != nil {
		lines, err := entryLinesToJournalLines(req.Entry.Lines)
		if err != nil {
			writeErr(w, err)
			return
		}
		orderID := order.ID
		entryReq = &journal.EntryRequest{
			IdempotencyKey: idempotencyKeyFromContext(r.Context()),
			EntryType:      req.Entry.EntryType,
			Actor:          actorFromContext(r.Context()),
			OccurredAt:     req.Entry.OccurredAt,
			OrderID:        &orderID,
			Lines:          lines,
			Metadata:       req.Entry.Metadata,
		}
	}

	var updated orders.Order
	err = db.Tx(r.Context(), s.Pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = orders.Transition(ctx, tx, order.ID, orders.State(req.ToState), req.ExpectedVersion, orders.TransitionParams{
			Actor:         actorFromContext(ctx),
			Reason:        req.Reason,
			OccurredAt:    req.OccurredAt,
			Entry:         entryReq,
			EntryID:       req.EntryID,
			SenderAddress: req.SenderAddress,
		})
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toOrderResponse(updated))
}
