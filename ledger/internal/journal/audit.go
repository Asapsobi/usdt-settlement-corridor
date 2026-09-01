package journal

import (
	"context"
	"log/slog"

	"ledger/internal/db"
)

// auditAmount is one line's account/asset/signed-units, the shape the
// build spec's audit log line calls for. Units keeps the ledger-wide
// signed convention (positive debit, negative credit) rather than
// splitting into separate debit/credit fields -- same reasoning as Line
// itself.
type auditAmount struct {
	Account string `json:"account"`
	Asset   string `json:"asset"`
	Units   int64  `json:"units"`
}

func postedLinesToAudit(lines []PostedLine) ([]string, []auditAmount) {
	accounts := make([]string, len(lines))
	amounts := make([]auditAmount, len(lines))
	for i, l := range lines {
		accounts[i] = l.AccountCode
		amounts[i] = auditAmount{Account: l.AccountCode, Asset: string(l.Amount.Asset), Units: l.Amount.Units}
	}
	return accounts, amounts
}

func requestedLinesToAudit(lines []Line) ([]string, []auditAmount) {
	accounts := make([]string, len(lines))
	amounts := make([]auditAmount, len(lines))
	for i, l := range lines {
		accounts[i] = l.AccountCode
		amounts[i] = auditAmount{Account: l.AccountCode, Asset: string(l.Amount.Asset), Units: l.Amount.Units}
	}
	return accounts, amounts
}

// auditSuccess reports a write that will actually persist: it defers via
// db.OnCommit rather than logging immediately, since Post and Reverse can
// both return success only for their caller's enclosing transaction to
// roll back afterward (see db.OnCommit's doc comment). If ctx wasn't
// produced by db.Tx -- internal/replay's own harness manages transactions
// directly rather than through db.Tx, since it predates this package and
// simulated traffic isn't audited -- OnCommit silently drops the
// callback and this call is a no-op.
func auditSuccess(ctx context.Context, entry Entry) {
	accounts, amounts := postedLinesToAudit(entry.Lines)
	db.OnCommit(ctx, func() {
		slog.Info("ledger write",
			"actor", entry.Actor,
			"idempotency_key", entry.IdempotencyKey,
			"entry_type", entry.EntryType,
			"order_id", entry.OrderID,
			"accounts", accounts,
			"amounts", amounts,
			"result", entry.Outcome.String(),
		)
	})
}

// auditFailure reports a write attempt that was rejected. Unlike success,
// this is logged immediately: nothing Post or Reverse itself did needs a
// later commit to become real, since every failure path returns before
// any row exists that a caller's subsequent rollback would need to undo.
func auditFailure(actor, idempotencyKey, entryType string, orderID *int64, accounts []string, amounts []auditAmount, err error) {
	slog.Warn("ledger write rejected",
		"actor", actor,
		"idempotency_key", idempotencyKey,
		"entry_type", entryType,
		"order_id", orderID,
		"accounts", accounts,
		"amounts", amounts,
		"result", "error",
		"error", err.Error(),
	)
}
