package journal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"ledger/internal/money"
)

// ErrIdempotencyConflict is returned when IdempotencyKey was already used
// with a payload that hashes differently from this call's. This is always
// a caller bug -- the same key must mean the same request -- so it must be
// loud, never silently swallowed or silently accepted as a new write.
var ErrIdempotencyConflict = errors.New("journal: idempotency key reused with a different payload")

// Post validates req, resolves each line's account, and either inserts a
// new entry or discovers that IdempotencyKey already exists, per this
// SEMANTICS table (C1.3), implemented exactly and without variation:
//
//   - Key not seen before: insert, return the new entry, Outcome=Created.
//   - Key seen, payload hashes identically: return the ORIGINAL entry,
//     Outcome=Replayed. No second row, no error.
//   - Key seen, payload hashes differently: return ErrIdempotencyConflict
//     naming both hashes. Nothing is written.
//
// The race between concurrent callers using the same key is resolved by
// the database's unique index on idempotency_key, not by a read-then-write
// check in Go -- a check-then-act here would itself be a race. INSERT ...
// ON CONFLICT DO NOTHING either wins outright or, if another transaction
// holds the conflicting key, blocks until that transaction resolves and
// then correctly sees whether it committed. Concretely: of N concurrent
// callers with the same key and payload, Postgres serializes them so that
// exactly one INSERT succeeds; every other call's ON CONFLICT DO NOTHING
// returns zero rows only once the winner has actually committed, so the
// SELECT that follows is guaranteed to see it.
//
// A successful Post also applies every line to the account_balances cache
// (C1.4) in the same tx, so the cache can never be observed out of step
// with the journal -- there is no window where the entry exists but the
// balance it implies does not, or vice versa.
//
// tx is a transaction the caller already opened -- Post never begins or
// commits one itself, so an order state transition (C1.5) can be made to
// commit atomically with the entry and its balance updates by running all
// of it against the same tx.
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
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id, idempotency_key, payload_hash, entry_type, order_id, actor,
			occurred_at, recorded_at, reversal_of, metadata
	`, req.IdempotencyKey, hash, req.EntryType, req.OrderID, req.Actor, req.OccurredAt, metadata)

	entry, err := scanEntry(row)
	switch {
	case err == nil:
		return postLines(ctx, tx, entry, resolved)

	case errors.Is(err, pgx.ErrNoRows):
		return resolveExisting(ctx, tx, req, hash)

	default:
		return Entry{}, fmt.Errorf("journal: insert entry: %w", err)
	}
}

// postLines is reached only when this call's own INSERT won the race: it
// owns writing every line, applying each to the account_balances cache,
// and reporting Outcome=Created.
func postLines(ctx context.Context, tx pgx.Tx, entry Entry, resolved []resolvedLine) (Entry, error) {
	entry.Outcome = Created
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

	if err := applyBalances(ctx, tx, entry.ID, resolved); err != nil {
		return Entry{}, err
	}

	return entry, nil
}

// applyBalances updates account_balances for every account this entry
// touched. It processes accounts in ascending account_id order -- not the
// order lines were submitted or stored -- which is what makes concurrent
// entries touching overlapping account sets deadlock-proof: every
// transaction that ever locks two of the same rows locks them in the same
// relative order, so a circular wait can never form.
func applyBalances(ctx context.Context, tx pgx.Tx, entryID int64, resolved []resolvedLine) error {
	sorted := make([]resolvedLine, len(resolved))
	copy(sorted, resolved)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].account.ID < sorted[j].account.ID
	})

	for _, rl := range sorted {
		_, err := tx.Exec(ctx, `
			INSERT INTO account_balances (account_id, asset, balance_units, last_entry_id)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (account_id) DO UPDATE
			SET balance_units = account_balances.balance_units + EXCLUDED.balance_units,
				last_entry_id = EXCLUDED.last_entry_id,
				updated_at = now()
		`, rl.account.ID, string(rl.amount.Asset), rl.amount.Units, entryID)
		if err != nil {
			return fmt.Errorf("journal: apply balance for account %d: %w", rl.account.ID, err)
		}
	}
	return nil
}

// resolveExisting is reached when ON CONFLICT DO NOTHING found IdempotencyKey
// already in use. It decides Replayed vs ErrIdempotencyConflict by comparing
// payload hashes, and writes nothing either way.
func resolveExisting(ctx context.Context, tx pgx.Tx, req EntryRequest, newHash []byte) (Entry, error) {
	existing, err := getEntryByKey(ctx, tx, req.IdempotencyKey)
	if err != nil {
		return Entry{}, fmt.Errorf("journal: idempotency lookup for %q: %w", req.IdempotencyKey, err)
	}

	if !bytes.Equal(existing.PayloadHash, newHash) {
		return Entry{}, fmt.Errorf("%w: key %q, existing hash %x, new hash %x",
			ErrIdempotencyConflict, req.IdempotencyKey, existing.PayloadHash, newHash)
	}

	lines, err := loadLines(ctx, tx, existing.ID)
	if err != nil {
		return Entry{}, fmt.Errorf("journal: loading lines for replay of %q: %w", req.IdempotencyKey, err)
	}
	existing.Lines = lines
	existing.Outcome = Replayed
	return existing, nil
}

func getEntryByKey(ctx context.Context, tx pgx.Tx, key string) (Entry, error) {
	row := tx.QueryRow(ctx, `
		SELECT id, idempotency_key, payload_hash, entry_type, order_id, actor,
			occurred_at, recorded_at, reversal_of, metadata
		FROM journal_entries
		WHERE idempotency_key = $1
	`, key)
	return scanEntry(row)
}

func loadLines(ctx context.Context, tx pgx.Tx, entryID int64) ([]PostedLine, error) {
	rows, err := tx.Query(ctx, `
		SELECT jl.seq, jl.account_id, a.code, jl.asset, jl.amount_units
		FROM journal_lines jl
		JOIN accounts a ON a.id = jl.account_id
		WHERE jl.entry_id = $1
		ORDER BY jl.seq
	`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PostedLine
	for rows.Next() {
		var pl PostedLine
		var asset string
		var units int64
		if err := rows.Scan(&pl.Seq, &pl.AccountID, &pl.AccountCode, &asset, &units); err != nil {
			return nil, err
		}
		pl.Amount = money.Amount{Asset: money.Asset(asset), Units: units}
		out = append(out, pl)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
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
