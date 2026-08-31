// Package db owns the Postgres connection pool and the transaction helper
// every write path in the ledger runs through. It knows nothing about
// accounts, journals, or orders.
package db

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config is the connection configuration, read from the environment.
type Config struct {
	DatabaseURL string
}

// ConfigFromEnv reads Config from LEDGER_DATABASE_URL. It is the only
// environment variable this package reads.
func ConfigFromEnv() (Config, error) {
	url := os.Getenv("LEDGER_DATABASE_URL")
	if url == "" {
		return Config{}, errors.New("db: LEDGER_DATABASE_URL is not set")
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
func Tx(ctx context.Context, p *Pool, fn func(ctx context.Context, tx pgx.Tx) error) (err error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
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
