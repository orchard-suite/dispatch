package store

import (
	"context"
	"database/sql"
)

// Mark is a fingerprint of everything a desk page shows that moves on its
// own: incidents and their timelines, alerts, pages, and messages to
// people. Two equal marks mean nothing a page shows has changed between
// them; a different mark means a page should fetch itself again.
func (s *Store) Mark(ctx context.Context) (string, error) {
	var m sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MAX(id) FROM events), 0) || '.' ||
		COALESCE((SELECT MAX(updated_at) FROM incidents), '') || '.' ||
		(SELECT COUNT(*) FROM alerts WHERE status = 'firing') || '.' ||
		COALESCE((SELECT MAX(starts_at) FROM alerts), '') || '.' ||
		COALESCE((SELECT MAX(id) FROM pages), 0) || '.' ||
		(SELECT COUNT(*) FROM pages WHERE ended_at IS NULL) || '.' ||
		COALESCE((SELECT MAX(id) FROM messages), 0) || '.' ||
		COALESCE((SELECT MAX(id) FROM overrides), 0) || '.' ||
		COALESCE((SELECT value FROM settings WHERE key = 'settings'), '')`).Scan(&m)
	return m.String, err
}
