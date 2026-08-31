//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`. This is the C1.1 acceptance proof: the
// normal_side/type CHECK constraint holds even against raw SQL that
// bypasses this package entirely, Create and Seed are idempotent, and the
// deliberate "no chain/asset pairing enforcement" choice is exercised and
// documented, not just asserted in a comment.
package accounts_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"ledger/internal/accounts"
	"ledger/internal/money"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}

	applyMigrations(t, url)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func applyMigrations(t *testing.T, url string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", url)
	require.NoError(t, err)
	defer sqlDB.Close()

	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.Up(sqlDB, migrationsDir(t)))
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}

func TestCreateIsIdempotentOnCode(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	code := uniqueCode(t, "asset:cex:idempotent_test")

	first, err := accounts.Create(ctx, pool, code, accounts.Asset, money.USDT_BEP20)
	require.NoError(t, err)

	second, err := accounts.Create(ctx, pool, code, accounts.Asset, money.USDT_BEP20)
	require.NoError(t, err)

	require.Equal(t, first.ID, second.ID, "second Create must return the same account id")
}

func TestSeedIsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	require.NoError(t, accounts.Seed(ctx, pool))
	require.NoError(t, accounts.Seed(ctx, pool), "second Seed call must be a no-op, not an error")

	acc, err := accounts.GetByCode(ctx, pool, "revenue:fee")
	require.NoError(t, err)
	require.Equal(t, accounts.Revenue, acc.Type)
	require.Equal(t, money.USDT_TRC20, acc.Asset)
	require.EqualValues(t, -1, acc.NormalSide)
}

// TestNormalSideCheckConstraintHoldsAgainstRawSQL is the ship gate for
// this chunk: the (type, normal_side) invariant must be enforced by
// Postgres itself, not merely by the Go layer that happens to compute
// normal_side correctly today.
func TestNormalSideCheckConstraintHoldsAgainstRawSQL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	code := uniqueCode(t, "asset:cex:check_constraint_test")
	_, err := pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, 'ASSET', 'USDT_BEP20', -1)
	`, code)
	require.Error(t, err, "ASSET with normal_side -1 must be rejected by the CHECK constraint")

	code2 := uniqueCode(t, "liability:customer:check_constraint_test")
	_, err = pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, 'LIABILITY', 'USDT_TRC20', 1)
	`, code2)
	require.Error(t, err, "LIABILITY with normal_side +1 must be rejected by the CHECK constraint")

	code3 := uniqueCode(t, "position:check_constraint_test")
	_, err = pool.Exec(ctx, `
		INSERT INTO accounts (code, type, asset, normal_side)
		VALUES ($1, 'POSITION', 'TRX', 1)
	`, code3)
	require.Error(t, err, "POSITION with nonzero normal_side must be rejected by the CHECK constraint")
}

// TestAccountModelDoesNotEnforceChainAssetPairing documents, per the C1.1
// spec, that creating "asset:tron:slot:1" with asset USDT_BEP20 succeeds:
// the chart-of-accounts model has no notion of which chain a slot code
// implies, or which asset belongs on that chain. That pairing is C5's
// business, not C1's. This test exists so the choice reads as deliberate.
func TestAccountModelDoesNotEnforceChainAssetPairing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	code := uniqueCode(t, "asset:tron:slot")
	acc, err := accounts.Create(ctx, pool, code, accounts.Asset, money.USDT_BEP20)
	require.NoError(t, err, "C1 does not know that a tron:slot code implies USDT_TRC20; that pairing belongs to C5")
	require.Equal(t, money.USDT_BEP20, acc.Asset)
}

func TestGetByCodeNotFound(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	_, err := accounts.GetByCode(ctx, pool, uniqueCode(t, "asset:cex:does_not_exist"))
	require.ErrorIs(t, err, accounts.ErrAccountNotFound)
}

func TestListFiltersByPrefixAndType(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	prefix := uniqueCode(t, "asset:cex:list_test")
	_, err := accounts.Create(ctx, pool, prefix+":a", accounts.Asset, money.USDT_BEP20)
	require.NoError(t, err)
	_, err = accounts.Create(ctx, pool, prefix+":b", accounts.Asset, money.USDT_TRC20)
	require.NoError(t, err)

	got, err := accounts.List(ctx, pool, accounts.ListFilter{CodePrefix: prefix})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, prefix+":a", got[0].Code)
	require.Equal(t, prefix+":b", got[1].Code)
}

func uniqueCode(t *testing.T, base string) string {
	t.Helper()
	return base + ":" + t.Name()
}
