//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. Covers C1.10's audit log requirement: a
// structured line for every write, with the specific correctness property
// that makes deferring-until-commit necessary in the first place -- a
// write whose enclosing transaction is later rolled back by something
// else must never be logged as having succeeded.
package journal_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"ledger/internal/db"
	"ledger/internal/journal"
	"ledger/internal/money"
)

// captureLogs swaps slog's default logger for one writing JSON lines to a
// buffer, restoring the previous logger on cleanup. internal/journal logs
// via the package-level slog default rather than taking a logger as a
// parameter (matching internal/recon's existing style), so this is the
// only way to observe what it actually emits. Safe here because nothing
// in this package's test suite calls t.Parallel.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func balancedEntryReq(key string) journal.EntryRequest {
	return journal.EntryRequest{
		IdempotencyKey: key,
		EntryType:      "test_audit",
		Actor:          "test:audit",
		OccurredAt:     time.Now(),
		Lines: []journal.Line{
			{AccountCode: "equity:opening:USDT_BEP20", Amount: money.Amount{Asset: money.USDT_BEP20, Units: 1_000000}},
			{AccountCode: "position:corridor:USDT_BEP20", Amount: money.Amount{Asset: money.USDT_BEP20, Units: -1_000000}},
		},
	}
}

func TestAudit_SuccessfulWriteIsLoggedWithRequiredFields(t *testing.T) {
	pool := testPool(t)
	dbPool := &db.Pool{Pool: pool}
	buf := captureLogs(t)

	req := balancedEntryReq(idemKey(t, "audit-success"))
	err := db.Tx(context.Background(), dbPool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := journal.Post(ctx, tx, req)
		return err
	})
	require.NoError(t, err)

	logs := buf.String()
	for _, want := range []string{
		`"msg":"ledger write"`,
		`"actor":"test:audit"`,
		`"idempotency_key":"` + req.IdempotencyKey + `"`,
		`"entry_type":"test_audit"`,
		`"result":"created"`,
		`"equity:opening:USDT_BEP20"`,
		`"position:corridor:USDT_BEP20"`,
	} {
		assert.Containsf(t, logs, want, "audit log missing %q; full output:\n%s", want, logs)
	}
}

// TestAudit_WriteRolledBackByALaterFailureIsNotLoggedAsSuccess is the
// property that makes db.OnCommit necessary at all: C1.2's balance
// invariant is a deferred constraint trigger, and orders.Transition posts
// an entry and then does a version-CAS update in the same transaction --
// either can roll back everything after Post has already returned
// success. This simulates that shape directly: Post succeeds, but the
// surrounding db.Tx closure still fails and the transaction rolls back.
func TestAudit_WriteRolledBackByALaterFailureIsNotLoggedAsSuccess(t *testing.T) {
	pool := testPool(t)
	dbPool := &db.Pool{Pool: pool}
	buf := captureLogs(t)

	req := balancedEntryReq(idemKey(t, "audit-rollback"))
	sentinel := errors.New("something else in this transaction failed")
	err := db.Tx(context.Background(), dbPool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := journal.Post(ctx, tx, req); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	logs := buf.String()
	assert.NotContains(t, logs, `"msg":"ledger write"`,
		"a write whose enclosing transaction rolled back must never be logged as having succeeded")

	var count int
	scanErr := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM journal_entries WHERE idempotency_key = $1`, req.IdempotencyKey).Scan(&count)
	require.NoError(t, scanErr)
	assert.Zero(t, count, "the entry must not have actually persisted either")
}

func TestAudit_RejectedWriteIsLoggedImmediately(t *testing.T) {
	pool := testPool(t)
	buf := captureLogs(t)

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)

	unbalanced := journal.EntryRequest{
		IdempotencyKey: idemKey(t, "audit-rejected"),
		EntryType:      "test_audit",
		Actor:          "test:audit",
		OccurredAt:     time.Now(),
		Lines: []journal.Line{
			{AccountCode: "equity:opening:USDT_BEP20", Amount: money.Amount{Asset: money.USDT_BEP20, Units: 1_000000}},
		},
	}
	_, err = journal.Post(ctx, tx, unbalanced)
	require.Error(t, err, "a single-line entry can never balance")

	logs := buf.String()
	for _, want := range []string{
		`"msg":"ledger write rejected"`,
		`"result":"error"`,
		`"idempotency_key":"` + unbalanced.IdempotencyKey + `"`,
		`"entry_type":"test_audit"`,
	} {
		assert.Containsf(t, logs, want, "rejected-write audit log missing %q; full output:\n%s", want, logs)
	}
}
