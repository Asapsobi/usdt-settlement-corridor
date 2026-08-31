package journal

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Post validates req, resolves each line's account, and inserts the entry
// and its lines. tx is a transaction the caller already opened -- Post
// never begins or commits one itself, so a balance update (C1.4) or an
// order state transition (C1.5) can be made to commit atomically with the
// entry by running all three against the same tx.
//
// Post enforces every Go-layer rule from C1.2 before issuing any INSERT:
// too few lines, a zero-amount line, a line whose asset doesn't match its
// account, and any per-asset imbalance are all rejected here. Postgres
// enforces the same balance invariant independently (see
// migrations/0003_journal.sql) precisely so that this Go-layer check is
// defense in depth, not the only thing standing between a bug and a broken
// ledger.
func Post(ctx context.Context, tx pgx.Tx, req EntryRequest) (Entry, error) {
	resolved, err := validate(ctx, tx, req)
	if err != nil {
		return Entry{}, err
	}

	hash, err := canonicalHash(req)
	if err != nil {
		return Entry{}, err
	}

	metadata := req.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO journal_entries
			(idempotency_key, payload_hash, entry_type, order_id, actor, occurred_at, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, idempotency_key, payload_hash, entry_type, order_id, actor,
			occurred_at, recorded_at, reversal_of, metadata
	`, req.IdempotencyKey, hash, req.EntryType, req.OrderID, req.Actor, req.OccurredAt, metadata)

	entry, err := scanEntry(row)
	if err != nil {
		return Entry{}, fmt.Errorf("journal: insert entry: %w", err)
	}

	entry.Lines = make([]PostedLine, len(resolved))
	for i, rl := range resolved {
		_, err := tx.Exec(ctx, `
			INSERT INTO journal_lines (entry_id, seq, account_id, asset, amount_units)
			VALUES ($1, $2, $3, $4, $5)
		`, entry.ID, rl.seq, rl.account.ID, string(rl.amount.Asset), rl.amount.Units)
		if err != nil {
			return Entry{}, fmt.Errorf("journal: insert line %d: %w", i, err)
		}
		entry.Lines[i] = PostedLine{
			Seq:         rl.seq,
			AccountID:   rl.account.ID,
			AccountCode: rl.account.Code,
			Amount:      rl.amount,
		}
	}

	return entry, nil
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanEntry(row scanRow) (Entry, error) {
	var e Entry
	err := row.Scan(
		&e.ID, &e.IdempotencyKey, &e.PayloadHash, &e.EntryType, &e.OrderID, &e.Actor,
		&e.OccurredAt, &e.RecordedAt, &e.ReversalOf, &e.Metadata,
	)
	if err != nil {
		return Entry{}, err
	}
	return e, nil
}
