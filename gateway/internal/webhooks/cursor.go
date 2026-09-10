package webhooks

import (
	"context"
	"fmt"

	"gateway/internal/db"
)

// getCursor returns webhook_cursors' current value for state -- ""
// means "the first page, no updated_after," same convention
// screening's own discovery_cursor uses.
func getCursor(ctx context.Context, pool *db.Pool, state string) (string, error) {
	var cursor string
	if err := pool.QueryRow(ctx, `SELECT cursor FROM webhook_cursors WHERE state = $1`, state).Scan(&cursor); err != nil {
		return "", fmt.Errorf("webhooks: reading cursor for %q: %w", state, err)
	}
	return cursor, nil
}

// setCursor persists cursor as webhook_cursors' new value for state.
func setCursor(ctx context.Context, pool *db.Pool, state, cursor string) error {
	if _, err := pool.Exec(ctx, `UPDATE webhook_cursors SET cursor = $1, updated_at = now() WHERE state = $2`, cursor, state); err != nil {
		return fmt.Errorf("webhooks: saving cursor for %q: %w", state, err)
	}
	return nil
}
