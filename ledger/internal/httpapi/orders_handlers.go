package httpapi

import (
	"context"
	"net/http"
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
		RecipientAddress: o.RecipientAddress, QuotedAt: o.QuotedAt, QuoteExpiresAt: o.QuoteExpiresAt,
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
			Actor:      actorFromContext(ctx),
			Reason:     req.Reason,
			OccurredAt: req.OccurredAt,
			Entry:      entryReq,
		})
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toOrderResponse(updated))
}
