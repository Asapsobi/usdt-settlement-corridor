// Package journal is the append-only, multi-asset, balanced journal --
// the heart of C1. It records what happened; it does not decide pricing,
// touch a blockchain, or hold keys.
package journal

import (
	"time"

	"ledger/internal/accounts"
	"ledger/internal/money"
)

// Line is one side of an entry: a signed amount against an account.
// Amount.Units follows the ledger-wide sign convention -- positive is a
// debit, negative is a credit -- never the other way around, and it is
// never reinterpreted per account type. Whether a debit is "good" or "bad"
// news for that account depends on the account's own NormalSide, not on
// anything Line encodes.
type Line struct {
	AccountCode string
	Amount      money.Amount
}

// EntryRequest is everything a caller supplies to Post.
//
// IdempotencyKey convention: "<producer>:<domain>:<natural-id>", e.g.
// "watcher:deposit_final:0xabc...:12" (tx hash + log index) or
// "dispatcher:payout_settled:<tron_txid>". The natural id must come from
// the external world the caller is reacting to, never a UUID minted at
// call time -- a retried call has to generate the exact same key, and only
// a fact about the outside world is guaranteed to reproduce identically.
// Max 255 bytes, enforced both here (validateIdempotencyKey) and by a
// CHECK constraint in migrations/0004_idempotency_key_length.sql.
type EntryRequest struct {
	IdempotencyKey string
	EntryType      string
	Actor          string
	OccurredAt     time.Time
	OrderID        *int64
	Lines          []Line
	Metadata       map[string]any
}

// PostedLine is a Line as it was actually recorded: resolved to a real
// account and assigned its storage position.
type PostedLine struct {
	Seq         int16
	AccountID   int64
	AccountCode string
	Amount      money.Amount
}

// Outcome distinguishes a fresh write from a replay of one that already
// happened, so a caller retrying after a timeout can tell whether it just
// caused the side effect or merely rediscovered it.
type Outcome int

const (
	// Created means this call's INSERT is the one that won: no entry
	// existed for this idempotency key before this call.
	Created Outcome = iota
	// Replayed means an entry with this idempotency key and an identical
	// payload already existed; nothing new was written, and Entry is the
	// original row.
	Replayed
)

func (o Outcome) String() string {
	switch o {
	case Created:
		return "created"
	case Replayed:
		return "replayed"
	default:
		return "unknown"
	}
}

// Entry is a journal_entries row together with the lines that were posted
// with it.
type Entry struct {
	ID             int64
	IdempotencyKey string
	PayloadHash    []byte
	EntryType      string
	OrderID        *int64
	Actor          string
	OccurredAt     time.Time
	RecordedAt     time.Time
	ReversalOf     *int64
	Metadata       map[string]any
	Lines          []PostedLine
	Outcome        Outcome
}

// resolvedLine is a Line after account lookup, immediately before insert.
type resolvedLine struct {
	seq     int16
	account accounts.Account
	amount  money.Amount
}
