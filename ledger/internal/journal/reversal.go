package journal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"ledger/internal/money"
)

var (
	ErrEntryNotFound          = errors.New("journal: entry not found")
	ErrAlreadyReversed        = errors.New("journal: entry already reversed")
	ErrCannotReverseAReversal = errors.New("journal: cannot reverse a reversal")
	ErrInvalidReverseParams   = errors.New("journal: invalid reverse parameters")
)

// negatedLine is one line of the original entry, negated, ready to
// become either a Line (for hashing) or a lineToApply (for writing).
type negatedLine struct {
	seq         int16
	accountID   int64
	accountCode string
	amount      money.Amount
}

// Reverse posts a new entry that negates every line of the entry
// identified by originalEntryID -- same accounts, same assets, opposite
// sign -- and records reversal_of = originalEntryID. Its idempotency key
// is deterministic: "ledger:reverse:" + the original entry's own key, so
// a retried caller reconstructs the exact same key rather than minting a
// fresh one.
//
// Unlike Post, a second Reverse of the same original entry is a hard
// error (ErrAlreadyReversed), never a silent replay: "no more than one
// reversal per entry" is a structural invariant, not a request that
// happens to repeat. Reverse therefore does not delegate to Post -- Post's
// ON CONFLICT DO NOTHING + replay-on-matching-hash semantics would
// otherwise turn a second Reverse call into a quiet no-op success instead
// of the loud rejection this needs. The UNIQUE constraint on
// journal_entries.reversal_of (migrations/0007) is what makes this safe
// under concurrency; the preliminary existence check below is only a
// fast, clear rejection in the common uncontended case.
//
// A reversal can never itself be reversed (ErrCannotReverseAReversal) --
// checked here in Go only, not by a database constraint, since expressing
// "the row this row's reversal_of points to must not itself have a
// non-null reversal_of" is a cross-row check a CHECK constraint cannot
// make; it would need its own trigger, which nothing in this chunk's
// acceptance criteria calls for.
func Reverse(ctx context.Context, tx pgx.Tx, originalEntryID int64, actor, reason string, occurredAt time.Time) (Entry, error) {
	if actor == "" {
		return Entry{}, fmt.Errorf("%w: empty actor", ErrInvalidReverseParams)
	}
	if reason == "" {
		return Entry{}, fmt.Errorf("%w: empty reason", ErrInvalidReverseParams)
	}
	if occurredAt.IsZero() {
		return Entry{}, fmt.Errorf("%w: zero occurred_at", ErrInvalidReverseParams)
	}

	original, err := getEntryByID(ctx, tx, originalEntryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w: id %d", ErrEntryNotFound, originalEntryID)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("journal: reverse: loading entry %d: %w", originalEntryID, err)
	}
	if original.ReversalOf != nil {
		return Entry{}, fmt.Errorf("%w: entry %d is itself a reversal of entry %d",
			ErrCannotReverseAReversal, originalEntryID, *original.ReversalOf)
	}

	var alreadyReversed bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal_entries WHERE reversal_of = $1)`, originalEntryID).
		Scan(&alreadyReversed)
	if err != nil {
		return Entry{}, fmt.Errorf("journal: reverse: checking existing reversal of %d: %w", originalEntryID, err)
	}
	if alreadyReversed {
		return Entry{}, fmt.Errorf("%w: entry %d", ErrAlreadyReversed, originalEntryID)
	}

	originalLines, err := loadLines(ctx, tx, originalEntryID)
	if err != nil {
		return Entry{}, fmt.Errorf("journal: reverse: loading lines of %d: %w", originalEntryID, err)
	}

	negated := make([]negatedLine, len(originalLines))
	for i, pl := range originalLines {
		neg, err := pl.Amount.Neg()
		if err != nil {
			return Entry{}, fmt.Errorf("journal: reverse: negating line %d of entry %d: %w", i, originalEntryID, err)
		}
		negated[i] = negatedLine{seq: pl.Seq, accountID: pl.AccountID, accountCode: pl.AccountCode, amount: neg}
	}

	idempotencyKey := "ledger:reverse:" + original.IdempotencyKey
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return Entry{}, err
	}

	hashLines := make([]Line, len(negated))
	for i, n := range negated {
		hashLines[i] = Line{AccountCode: n.accountCode, Amount: n.amount}
	}
	hash, err := canonicalHash(EntryRequest{
		IdempotencyKey: idempotencyKey,
		EntryType:      "reversal",
		Actor:          actor,
		OccurredAt:     occurredAt,
		OrderID:        original.OrderID,
		Lines:          hashLines,
	})
	if err != nil {
		return Entry{}, err
	}

	metadata := map[string]any{
		"reversal_of_idempotency_key": original.IdempotencyKey,
		"reason":                      reason,
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO journal_entries
			(idempotency_key, payload_hash, entry_type, order_id, actor, occurred_at, reversal_of, metadata)
		VALUES ($1, $2, 'reversal', $3, $4, $5, $6, $7)
		RETURNING id, idempotency_key, payload_hash, entry_type, order_id, actor,
			occurred_at, recorded_at, reversal_of, metadata
	`, idempotencyKey, hash, original.OrderID, actor, occurredAt, originalEntryID, metadata)

	entry, err := scanEntry(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Entry{}, fmt.Errorf("%w: entry %d (lost a concurrent race to reverse it)", ErrAlreadyReversed, originalEntryID)
		}
		return Entry{}, fmt.Errorf("journal: reverse: inserting reversal entry: %w", err)
	}

	toApply := make([]lineToApply, len(negated))
	for i, n := range negated {
		toApply[i] = lineToApply{
			seq:         n.seq,
			accountID:   n.accountID,
			accountCode: n.accountCode,
			asset:       n.amount.Asset,
			units:       n.amount.Units,
		}
	}
	posted, err := insertLinesAndApplyBalances(ctx, tx, entry.ID, toApply)
	if err != nil {
		return Entry{}, err
	}

	entry.Outcome = Created
	entry.Lines = posted
	return entry, nil
}

func getEntryByID(ctx context.Context, tx pgx.Tx, id int64) (Entry, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, idempotency_key, payload_hash, entry_type, order_id, actor,
			occurred_at, recorded_at, reversal_of, metadata
		FROM journal_entries
		WHERE id = $1
	`, id)
	return scanEntry(row)
}
