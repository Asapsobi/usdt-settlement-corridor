package journal

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"ledger/internal/accounts"
	"ledger/internal/money"
)

// Balance reads accountCode's balance from the account_balances cache --
// the fast path, and the one every caller should use unless it has a
// specific reason not to (see BalanceComputed, BalanceAsOf).
//
// An account with no account_balances row yet -- nothing has ever posted
// against it -- has a balance of zero. That is not an error case; a fresh
// account genuinely holds nothing.
func Balance(ctx context.Context, q accounts.Queryer, accountCode string) (money.Amount, error) {
	acc, err := accounts.GetByCode(ctx, q, accountCode)
	if err != nil {
		return money.Amount{}, err
	}

	var units int64
	err = q.QueryRow(ctx, `SELECT balance_units FROM account_balances WHERE account_id = $1`, acc.ID).Scan(&units)
	if errors.Is(err, pgx.ErrNoRows) {
		return money.Amount{Asset: acc.Asset, Units: 0}, nil
	}
	if err != nil {
		return money.Amount{}, fmt.Errorf("journal: balance for %q: %w", accountCode, err)
	}
	return money.Amount{Asset: acc.Asset, Units: units}, nil
}

// BalanceComputed sums journal_lines directly for accountCode, ignoring
// the cache entirely. This is the slow path: a full scan of that account's
// lines. It exists to verify the cache (see VerifyBalances), not to be
// the normal read path.
func BalanceComputed(ctx context.Context, q accounts.Queryer, accountCode string) (money.Amount, error) {
	acc, err := accounts.GetByCode(ctx, q, accountCode)
	if err != nil {
		return money.Amount{}, err
	}

	var units int64
	err = q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_units), 0) FROM journal_lines WHERE account_id = $1
	`, acc.ID).Scan(&units)
	if err != nil {
		return money.Amount{}, fmt.Errorf("journal: computed balance for %q: %w", accountCode, err)
	}
	return money.Amount{Asset: acc.Asset, Units: units}, nil
}

// BalanceAsOf sums journal_lines for accountCode up to and including
// maxEntryID -- accountCode's balance at the moment entry maxEntryID was
// posted, not now. Reconciliation needs this because a chain snapshot is
// taken at a moment while the ledger keeps moving: comparing "what the
// chain showed at block N" against "the account's current balance" would
// be comparing two different points in time.
func BalanceAsOf(ctx context.Context, q accounts.Queryer, accountCode string, maxEntryID int64) (money.Amount, error) {
	acc, err := accounts.GetByCode(ctx, q, accountCode)
	if err != nil {
		return money.Amount{}, err
	}

	var units int64
	err = q.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_units), 0)
		FROM journal_lines
		WHERE account_id = $1 AND entry_id <= $2
	`, acc.ID, maxEntryID).Scan(&units)
	if err != nil {
		return money.Amount{}, fmt.Errorf("journal: balance as of entry %d for %q: %w", maxEntryID, accountCode, err)
	}
	return money.Amount{Asset: acc.Asset, Units: units}, nil
}

// Discrepancy is one account where the account_balances cache disagrees
// with what journal_lines actually sums to.
type Discrepancy struct {
	AccountID     int64
	AccountCode   string
	Asset         money.Asset
	CachedUnits   int64
	ComputedUnits int64
}

// VerifyBalances compares every account's cached balance against the sum
// of its journal_lines in one query, rather than one query per account --
// this is what keeps it cheap enough to run every minute at any realistic
// account count. It starts from the full set of accounts, not from
// account_balances or journal_lines alone, so it catches drift in both
// directions: a cache row whose value is wrong, and a cache row that is
// simply missing despite journal_lines showing activity (which the
// architecture in post.go should make impossible, but this is the check
// that would catch it if it weren't).
func VerifyBalances(ctx context.Context, q accounts.Queryer) ([]Discrepancy, error) {
	rows, err := q.Query(ctx, `
		SELECT
			a.id,
			a.code,
			a.asset,
			COALESCE(ab.balance_units, 0) AS cached,
			COALESCE(computed.total, 0) AS computed
		FROM accounts a
		LEFT JOIN account_balances ab ON ab.account_id = a.id
		LEFT JOIN (
			SELECT account_id, SUM(amount_units) AS total
			FROM journal_lines
			GROUP BY account_id
		) computed ON computed.account_id = a.id
		WHERE COALESCE(ab.balance_units, 0) <> COALESCE(computed.total, 0)
		ORDER BY a.code
	`)
	if err != nil {
		return nil, fmt.Errorf("journal: verify balances: %w", err)
	}
	defer rows.Close()

	var out []Discrepancy
	for rows.Next() {
		var d Discrepancy
		var asset string
		if err := rows.Scan(&d.AccountID, &d.AccountCode, &asset, &d.CachedUnits, &d.ComputedUnits); err != nil {
			return nil, fmt.Errorf("journal: verify balances: %w", err)
		}
		d.Asset = money.Asset(asset)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal: verify balances: %w", err)
	}
	return out, nil
}

// TrialBalance sums journal_lines per asset across every account -- the
// journal's own claim about itself, deliberately computed independently
// of account_balances so that a broken cache can never make this check
// look healthier than the ledger actually is. Every individual entry
// already balances to zero per asset (enforced in C1.2/C1.3, both in Go
// and by Postgres); TrialBalance is the assertion that this held on
// every single entry ever posted, which is why the doc calls it the
// strongest health signal in the system -- it is double-entry closure
// checked at the scale of the whole ledger, not just one entry at a time.
func TrialBalance(ctx context.Context, q accounts.Queryer) (map[money.Asset]int64, error) {
	rows, err := q.Query(ctx, `SELECT asset, SUM(amount_units) FROM journal_lines GROUP BY asset`)
	if err != nil {
		return nil, fmt.Errorf("journal: trial balance: %w", err)
	}
	defer rows.Close()

	out := make(map[money.Asset]int64)
	for rows.Next() {
		var asset string
		var total int64
		if err := rows.Scan(&asset, &total); err != nil {
			return nil, fmt.Errorf("journal: trial balance: %w", err)
		}
		out[money.Asset(asset)] = total
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal: trial balance: %w", err)
	}
	return out, nil
}
