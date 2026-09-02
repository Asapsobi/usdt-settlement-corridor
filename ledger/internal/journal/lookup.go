package journal

import (
	"context"
	"fmt"

	"ledger/internal/accounts"
)

// GetEntry returns the entry with this id, including its posted lines.
//
// C1.6's Reverse takes a numeric entry id, but a remote caller only ever
// knows the idempotency KEY it used -- the key is reconstructible from
// facts about the outside world, which is the whole point of the
// convention in journal.go, while an id is assigned by the database and
// only ever comes back in a response the caller may have missed. So the
// HTTP boundary needs both directions: this by id, GetEntryByKey by key.
//
// Both take an accounts.Queryer rather than a pgx.Tx: these are reads,
// and a read handler has no transaction to join -- it queries the pool
// directly, exactly as journal.Balance and accounts.GetByCode already do.
func GetEntry(ctx context.Context, q accounts.Queryer, id int64) (Entry, error) {
	entry, err := getEntryByID(ctx, q, id)
	if err != nil {
		return Entry{}, err
	}
	return withLines(ctx, q, entry)
}

// GetEntryByKey returns the entry posted under this idempotency key,
// including its posted lines.
func GetEntryByKey(ctx context.Context, q accounts.Queryer, key string) (Entry, error) {
	entry, err := GetEntryByIdempotencyKey(ctx, q, key)
	if err != nil {
		return Entry{}, err
	}
	return withLines(ctx, q, entry)
}

// ReversalOf returns the entry that reverses originalEntryID, if one
// exists. found=false means the entry has not been reversed -- which is
// not an error, just the ordinary case.
//
// This exists for the one operationally awkward case in C1.6's
// deliberately non-idempotent Reverse: a caller whose request timed out
// cannot tell whether its own call created the reversal or whether it
// lost a race, because both look like ErrAlreadyReversed on retry. Being
// able to fetch the reversal that actually exists is what closes that
// loop without weakening the at-most-one-reversal invariant itself.
func ReversalOf(ctx context.Context, q accounts.Queryer, originalEntryID int64) (entry Entry, found bool, err error) {
	row := q.QueryRow(ctx, entrySelectSQL+` WHERE reversal_of = $1`, originalEntryID)
	e, err := scanEntry(row)
	if err != nil {
		if isNoRows(err) {
			return Entry{}, false, nil
		}
		return Entry{}, false, fmt.Errorf("journal: looking up reversal of entry %d: %w", originalEntryID, err)
	}
	withLoaded, err := withLines(ctx, q, e)
	if err != nil {
		return Entry{}, false, err
	}
	return withLoaded, true, nil
}

func withLines(ctx context.Context, q accounts.Queryer, entry Entry) (Entry, error) {
	lines, err := loadLines(ctx, q, entry.ID)
	if err != nil {
		return Entry{}, fmt.Errorf("journal: loading lines for entry %d: %w", entry.ID, err)
	}
	entry.Lines = lines
	return entry, nil
}
