package addresses

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// panicIfCalledQueryer is a Queryer that fails the test the moment any
// method is invoked. Used to prove ErrNotConfigured is returned BEFORE
// Assign ever touches the database, not merely in addition to a query
// that would have failed anyway for some other reason.
type panicIfCalledQueryer struct{ t *testing.T }

func (q panicIfCalledQueryer) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	q.t.Helper()
	q.t.Fatal("Exec called: Assign should have returned ErrNotConfigured before touching the database")
	return pgconn.CommandTag{}, nil
}

func (q panicIfCalledQueryer) QueryRow(context.Context, string, ...any) pgx.Row {
	q.t.Helper()
	q.t.Fatal("QueryRow called: Assign should have returned ErrNotConfigured before touching the database")
	return nil
}

func (q panicIfCalledQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.t.Helper()
	q.t.Fatal("Query called: Assign should have returned ErrNotConfigured before touching the database")
	return nil, nil
}

// TestAssign_NotConfigured exercises the ErrNotConfigured guard directly.
// This needs no database and is not integration-tagged: the check it
// tests is a plain Go comparison that runs before Assign ever reaches a
// Queryer, and xpub is unexported, so clearing it to reproduce "never
// configured" can only be done from inside this package -- an external
// test package (addresses_test, used by every other test in this
// directory) has no way to reach it, same as any other importer.
//
// Saves and restores the package-level xpub var around the test so a test
// binary that runs this alongside the integration suite (same package,
// same process, `go test ./...` with -tags=integration) doesn't leave
// other tests unconfigured afterward.
func TestAssign_NotConfigured(t *testing.T) {
	saved := xpub
	defer func() { xpub = saved }()
	xpub = ""

	_, err := Assign(context.Background(), panicIfCalledQueryer{t}, 1, "ext", "cust", time.Now(), time.Now())
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}
