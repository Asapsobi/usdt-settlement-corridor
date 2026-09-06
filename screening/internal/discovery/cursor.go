package discovery

import (
	"context"
	"fmt"
)

// getCursor returns discovery_cursor's current value, or "" if it has
// never been set (a fresh deployment, or the cursor column is still its
// seeded NULL) -- "" is PollFundedOrders' own convention for "the first
// page, no updated_after".
func getCursor(ctx context.Context, q Queryer) (string, error) {
	var cursor *string
	if err := q.QueryRow(ctx, `SELECT cursor FROM discovery_cursor WHERE id = 1`).Scan(&cursor); err != nil {
		return "", fmt.Errorf("discovery: reading cursor: %w", err)
	}
	if cursor == nil {
		return "", nil
	}
	return *cursor, nil
}

// setCursor persists cursor as discovery_cursor's new value.
func setCursor(ctx context.Context, q Queryer, cursor string) error {
	if _, err := q.Exec(ctx, `UPDATE discovery_cursor SET cursor = $1, updated_at = now() WHERE id = 1`, cursor); err != nil {
		return fmt.Errorf("discovery: saving cursor: %w", err)
	}
	return nil
}
