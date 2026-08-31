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

// EntryRequest is everything a caller supplies to Post. IdempotencyKey
// uniqueness is enforced by the database (journal_entries.idempotency_key
// is UNIQUE); full replay/conflict semantics on top of that arrive in
// C1.3, not here.
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
}

// resolvedLine is a Line after account lookup, immediately before insert.
type resolvedLine struct {
	seq     int16
	account accounts.Account
	amount  money.Amount
}
