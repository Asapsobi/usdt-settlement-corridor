package httpapi

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"ledger/internal/db"
	"ledger/internal/orders"
)

// postReorgRequest carries the idempotency key of the deposit_final entry
// that funded this order -- not its numeric id. That is what the watcher
// reporting the reorg actually has: it is the same key it minted from the
// tx hash and log index when it first reported the deposit, so a retried
// report reconstructs it identically.
type postReorgRequest struct {
	OriginalEntryKey string `json:"original_entry_key"`
}

// postReorg is POST /v1/orders/{external_id}/reorg: the HTTP face of
// orders.HandleDepositReorg.
//
// This endpoint deliberately does NOT take a scenario, a target state, or
// anything else describing what to do about the reorg. The caller reports
// one fact -- "the deposit that funded this order no longer exists on
// chain" -- and C1 decides what that means from the order's own state,
// because the two cases are genuinely different problems and only the
// ledger knows which applies:
//
//	funded                 -> scenario A: reverse the deposit, return to
//	                          quoted, no loss, no halt.
//	dispatching | settled  -> scenario B: reverse the deposit, book the
//	                          full amount_out to expense:loss:reorg, and
//	                          HALT. The payout already left.
//
// Any other state is ErrUnexpectedState, never a best guess. Letting a
// caller nominate the scenario would make it possible for a buggy watcher
// to book a routine reorg as a loss, or -- far worse -- a post-settlement
// reorg as routine, which is exactly the silent-loss case the halt in
// scenario B exists to make impossible.
func (s *Server) postReorg(w http.ResponseWriter, r *http.Request) {
	externalID, ok := urlParam(w, r, "external_id")
	if !ok {
		return
	}

	var req postReorgRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.OriginalEntryKey == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "original_entry_key is required"))
		return
	}

	order, err := orders.GetByExternalID(r.Context(), s.Pool, externalID)
	if err != nil {
		writeErr(w, err)
		return
	}

	var updated orders.Order
	err = db.Tx(r.Context(), s.Pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = orders.HandleDepositReorg(ctx, tx, order.ID, req.OriginalEntryKey, actorFromContext(ctx))
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toOrderResponse(updated))
}
