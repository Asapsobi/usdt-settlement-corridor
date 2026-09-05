// Package db owns the Postgres connection pool and the transaction helper
// every write path in the watcher runs through. Same pattern as C1's own
// internal/db, deliberately -- this is its own database though, separate
// from C1's: the watcher is a standalone service that calls C1 over HTTP
// like every other component does, never sharing a schema or a connection
// with it.
package db

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Queryer is satisfied by both *Pool and pgx.Tx, so a function that reads
// or writes doesn't need to care whether it's running standalone or
// inside a caller's transaction. Declared once here, not per-package: C1
// established this pattern (accounts.Queryer, reused directly by
// internal/journal rather than each package declaring its own
// structurally-identical copy), and internal/addresses and internal/chain
// both need the exact same shape, so this is its natural, neutral home.
type Queryer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Config is the connection configuration, read from the environment.
type Config struct {
	DatabaseURL string
}

// ConfigFromEnv reads Config from WATCHER_DATABASE_URL. It is the only
// environment variable this package reads.
func ConfigFromEnv() (Config, error) {
	url := os.Getenv("WATCHER_DATABASE_URL")
	if url == "" {
		return Config{}, errors.New("db: WATCHER_DATABASE_URL is not set")
	}
	return Config{DatabaseURL: url}, nil
}

// Pool wraps a pgxpool.Pool. It exists so callers depend on this package's
// type, not pgxpool's, keeping the driver swappable in principle.
type Pool struct {
	*pgxpool.Pool
}

// Open creates and validates a connection pool against cfg.DatabaseURL.
func Open(ctx context.Context, cfg Config) (*Pool, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// Tx runs fn inside a transaction on p, committing if fn returns nil and
// rolling back if fn returns an error or panics. The panic is re-thrown
// after rollback so callers see it normally.
//
// This intentionally does not carry C1's OnCommit mechanism (a hook for
// running a callback only after a real commit, used there for audit
// logging tied to a deferred balance-check trigger) -- nothing in this
// service needs that yet. Add it when a chunk actually does, not before.
func Tx(ctx context.Context, p *Pool, fn func(ctx context.Context, tx pgx.Tx) error) (err error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}

	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback(ctx)
			panic(r)
		}
	}()

	if err := fn(ctx, tx); err != nil {
		if rbErr := tx.Rollback(ctx); rbErr != nil {
			return fmt.Errorf("db: rollback after %w: %v", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}
	return nil
}
