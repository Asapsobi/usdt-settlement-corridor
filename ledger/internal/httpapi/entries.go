package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/db"
	"ledger/internal/journal"
	"ledger/internal/money"
)

type entryLineRequest struct {
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}

type postEntryRequest struct {
	EntryType  string             `json:"entry_type"`
	OrderID    *int64             `json:"order_id,omitempty"`
	OccurredAt time.Time          `json:"occurred_at"`
	Lines      []entryLineRequest `json:"lines"`
	Metadata   map[string]any     `json:"metadata,omitempty"`
}

type postedLineResponse struct {
	Seq         int16  `json:"seq"`
	AccountCode string `json:"account_code"`
	Asset       string `json:"asset"`
	Amount      string `json:"amount"`
}

type entryResponse struct {
	ID             int64                `json:"id"`
	IdempotencyKey string               `json:"idempotency_key"`
	EntryType      string               `json:"entry_type"`
	OrderID        *int64               `json:"order_id,omitempty"`
	Actor          string               `json:"actor"`
	OccurredAt     time.Time            `json:"occurred_at"`
	RecordedAt     time.Time            `json:"recorded_at"`
	ReversalOf     *int64               `json:"reversal_of,omitempty"`
	Lines          []postedLineResponse `json:"lines"`
	Outcome        string               `json:"outcome"`
}

// entryLinesToJournalLines parses every line's decimal-string amount
// against its declared asset. Any money.Parse* error here maps, via
// mapError, to 400 invalid_amount -- this is the one place a malformed
// amount for POST /entries is actually caught (decodeJSON already caught
// a JSON *number* in this position; this catches a syntactically valid
// JSON string that isn't a valid decimal, or has too many decimal places
// for its asset).
func entryLinesToJournalLines(reqLines []entryLineRequest) ([]journal.Line, error) {
	lines := make([]journal.Line, len(reqLines))
	for i, l := range reqLines {
		amt, err := money.ParseDecimal(l.Amount, money.Asset(l.Asset))
		if err != nil {
			return nil, err
		}
		lines[i] = journal.Line{AccountCode: l.AccountCode, Amount: amt}
	}
	return lines, nil
}

func toEntryResponse(e journal.Entry) entryResponse {
	lines := make([]postedLineResponse, len(e.Lines))
	for i, l := range e.Lines {
		amt, _ := money.Format(l.Amount)
		lines[i] = postedLineResponse{Seq: l.Seq, AccountCode: l.AccountCode, Asset: string(l.Amount.Asset), Amount: amt}
	}
	outcome := "created"
	if e.Outcome == journal.Replayed {
		outcome = "replayed"
	}
	return entryResponse{
		ID: e.ID, IdempotencyKey: e.IdempotencyKey, EntryType: e.EntryType, OrderID: e.OrderID,
		Actor: e.Actor, OccurredAt: e.OccurredAt, RecordedAt: e.RecordedAt, ReversalOf: e.ReversalOf,
		Lines: lines, Outcome: outcome,
	}
}

// postEntry is POST /v1/entries. 201 for a fresh entry, 200 for a replay
// of an identical prior request -- the response body's outcome field
// says which, but callers that only care about the status code (per the
// build spec's acceptance criterion) get the distinction there too.
func (s *Server) postEntry(w http.ResponseWriter, r *http.Request) {
	var req postEntryRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	lines, err := entryLinesToJournalLines(req.Lines)
	if err != nil {
		writeErr(w, err)
		return
	}

	entryReq := journal.EntryRequest{
		IdempotencyKey: idempotencyKeyFromContext(r.Context()),
		EntryType:      req.EntryType,
		Actor:          actorFromContext(r.Context()),
		OccurredAt:     req.OccurredAt,
		OrderID:        req.OrderID,
		Lines:          lines,
		Metadata:       req.Metadata,
	}

	var entry journal.Entry
	err = db.Tx(r.Context(), s.Pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		entry, err = journal.Post(ctx, tx, entryReq)
		return err
	})
	if err != nil {
		s.Metrics.EntryErrorsTotal.WithLabelValues(mapError(err).Code).Inc()
		writeErr(w, err)
		return
	}
	s.Metrics.EntriesTotal.Inc()

	status := http.StatusCreated
	if entry.Outcome == journal.Replayed {
		status = http.StatusOK
	}
	respondJSON(w, status, toEntryResponse(entry))
}
