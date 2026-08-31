package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"ledger/internal/money"
)

var ErrAccountNotFound = errors.New("accounts: not found")

// Queryer is satisfied by both *pgxpool.Pool and pgx.Tx, so account
// operations can run standalone or inside a caller's transaction without
// this package depending on which one it got.
type Queryer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Create inserts an account for code, deriving normal_side from t. It is
// idempotent on code: a second Create with the same code returns the
// account as it actually exists in the database, with no error -- even if
// the second call's (type, asset) differ from what's stored. Create does
// not detect or report that mismatch; nothing in C1.1 requires it to.
func Create(ctx context.Context, q Queryer, code string, t Type, asset money.Asset) (Account, error) {
	if _, err := ParseCode(code); err != nil {
		return Account{}, err
	}
	if !t.Valid() {
		return Account{}, fmt.Errorf("%w: %q", ErrUnknownAccountType, string(t))
	}
	if !asset.Valid() {
		return Account{}, fmt.Errorf("%w: %q", money.ErrUnknownAsset, string(asset))
	}
	normalSide, err := NormalSideFor(t)
	if err != nil {
		return Account{}, err
	}

	row := q.QueryRow(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (code) DO NOTHING
		RETURNING id, code, type, asset, normal_side, is_active, opened_at, metadata
	`, code, string(t), string(asset), normalSide)

	acc, err := scanAccount(row)
	if err == nil {
		return acc, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("accounts: create %q: %w", code, err)
	}

	// ON CONFLICT DO NOTHING returned no row: the account already existed.
	return GetByCode(ctx, q, code)
}

// GetByCode looks up an account by its exact code.
func GetByCode(ctx context.Context, q Queryer, code string) (Account, error) {
	row := q.QueryRow(ctx, `
		SELECT id, code, type, asset, normal_side, is_active, opened_at, metadata
		FROM accounts
		WHERE code = $1
	`, code)

	acc, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: %q", ErrAccountNotFound, code)
	}
	if err != nil {
		return Account{}, fmt.Errorf("accounts: get %q: %w", code, err)
	}
	return acc, nil
}

// ListFilter narrows List. The zero value matches every active account.
type ListFilter struct {
	CodePrefix      string
	Type            *Type
	Asset           *money.Asset
	IncludeInactive bool
}

// List returns accounts matching filter, ordered by code.
func List(ctx context.Context, q Queryer, filter ListFilter) ([]Account, error) {
	var where []string
	var args []any

	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if filter.CodePrefix != "" {
		where = append(where, "code LIKE "+arg(filter.CodePrefix+"%"))
	}
	if filter.Type != nil {
		where = append(where, "type = "+arg(string(*filter.Type)))
	}
	if filter.Asset != nil {
		where = append(where, "asset = "+arg(string(*filter.Asset)))
	}
	if !filter.IncludeInactive {
		where = append(where, "is_active = true")
	}

	query := `SELECT id, code, type, asset, normal_side, is_active, opened_at, metadata FROM accounts`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY code"

	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("accounts: list: %w", err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		acc, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("accounts: list: %w", err)
		}
		out = append(out, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("accounts: list: %w", err)
	}
	return out, nil
}

// scanRow is the subset of pgx.Row and pgx.Rows that scanAccount needs.
type scanRow interface {
	Scan(dest ...any) error
}

func scanAccount(row scanRow) (Account, error) {
	var acc Account
	var typ, asset string
	err := row.Scan(&acc.ID, &acc.Code, &typ, &asset, &acc.NormalSide, &acc.IsActive, &acc.OpenedAt, &acc.Metadata)
	if err != nil {
		return Account{}, err
	}
	acc.Type = Type(typ)
	acc.Asset = money.Asset(asset)
	return acc, nil
}
