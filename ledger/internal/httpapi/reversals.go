package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/db"
	"ledger/internal/journal"
)

// C1.11 -- the reversal surface.
//
// C1.6 built Reverse and HandleDepositReorg, and C1.8 exposed everything
// else over HTTP, but not these two: their only callers were the replay
// harness and the local demo script, both of which run in-process and
// call the Go functions directly. C2 (the deposit watcher) cannot -- the
// architecture is explicit that the other five components are HTTP
// clients of C1 and import none of its internal packages. Reacting to a
// reorg is the single most important thing C2 does, so without these
// routes C2 cannot be built as designed. Adding them now, before C2
// exists, is also the last cheap moment: once a second service is
// written against this API, changing it is a multi-component migration.

type postReversalRequest struct {
	Reason     string    `json:"reason"`
	OccurredAt time.Time `json:"occurred_at"`
}

// alreadyReversedResponse is the 409 body when the entry has already been
// reversed. It carries the standard error envelope AND the reversal that
// actually exists.
//
// Reverse is deliberately not idempotent -- "at most one reversal per
// entry" is a structural invariant, not a request that happens to repeat,
// so a second call must be loud. But over HTTP that leaves a caller whose
// first request timed out unable to tell "I already did this" from
// "someone else did this," since both surface identically. Returning the
// existing reversal answers that without weakening the invariant: the
// second call still writes nothing and still reports a conflict.
type alreadyReversedResponse struct {
	Error            errorBody      `json:"error"`
	ExistingReversal *entryResponse `json:"existing_reversal,omitempty"`
}

// entryIDParam parses the {id} path parameter as a journal entry id.
func entryIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw, ok := urlParam(w, r, "id")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "entry id must be an integer"))
		return 0, false
	}
	return id, true
}

// getEntry is GET /v1/entries/{id}.
func (s *Server) getEntry(w http.ResponseWriter, r *http.Request) {
	id, ok := entryIDParam(w, r)
	if !ok {
		return
	}
	entry, err := journal.GetEntry(r.Context(), s.Pool, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toEntryResponse(entry))
}

// getEntryByKey is GET /v1/entries?idempotency_key=... -- the lookup a
// remote caller actually needs. Every producer knows the key it used
// (the convention requires it to be reconstructible from facts about the
// outside world, precisely so a retry regenerates it), but not the
// numeric id, which only ever appears in a response it may have missed.
func (s *Server) getEntryByKey(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("idempotency_key")
	if key == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "idempotency_key query parameter is required"))
		return
	}
	entry, err := journal.GetEntryByKey(r.Context(), s.Pool, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toEntryResponse(entry))
}

// postReversal is POST /v1/entries/{id}/reversal: the HTTP face of
// journal.Reverse. The reversing entry's own idempotency key is derived
// from the original's ("ledger:reverse:" + original key), never from this
// request's Idempotency-Key header -- that header is required by the
// blanket no-header-no-write rule, but the reversal's identity has to
// come from the entry being reversed, so that two callers racing to
// reverse the same entry collide on the UNIQUE constraint rather than
// creating two reversals under two different caller-chosen keys.
func (s *Server) postReversal(w http.ResponseWriter, r *http.Request) {
	id, ok := entryIDParam(w, r)
	if !ok {
		return
	}

	var req postReversalRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	var entry journal.Entry
	err := db.Tx(r.Context(), s.Pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entry, err = journal.Reverse(ctx, tx, id, actorFromContext(ctx), req.Reason, req.OccurredAt)
		return err
	})
	if errors.Is(err, journal.ErrAlreadyReversed) {
		s.writeAlreadyReversed(w, r, id)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, toEntryResponse(entry))
}

// writeAlreadyReversed writes the 409 for an entry that already has a
// reversal, including that reversal when it can be read back. If the
// read itself fails the conflict is still reported -- the status and
// code are what a caller keys off, and degrading to "conflict without
// the extra detail" is better than turning a correctly-detected
// conflict into a 500.
func (s *Server) writeAlreadyReversed(w http.ResponseWriter, r *http.Request, originalEntryID int64) {
	body := alreadyReversedResponse{
		Error: errorBody{Code: errAlreadyReversed.Code, Message: errAlreadyReversed.Message},
	}
	if existing, found, err := journal.ReversalOf(r.Context(), s.Pool, originalEntryID); err == nil && found {
		resp := toEntryResponse(existing)
		body.ExistingReversal = &resp
	}
	respondJSON(w, errAlreadyReversed.Status, body)
}
