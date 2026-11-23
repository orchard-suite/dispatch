package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

const threadColumns = `id, producer, dedup_key, state, title, events, opened_at, acknowledged_at, acknowledged_by, resolved_at`

func scanThread(r interface{ Scan(...any) error }) (notice.Thread, error) {
	var t notice.Thread
	var state, opened string
	var acked, resolved sql.NullString
	if err := r.Scan(&t.ID, &t.Producer, &t.DedupKey, &state, &t.Title, &t.Events, &opened, &acked, &t.AcknowledgedBy, &resolved); err != nil {
		return t, err
	}
	t.State, t.OpenedAt, t.AcknowledgedAt, t.ResolvedAt = notice.ThreadState(state), parseTime(opened), parseTimePtr(acked), parseTimePtr(resolved)
	return t, nil
}

// LiveThread reads the producer's thread under a dedup key that is not yet
// resolved.
func (s *Store) LiveThread(ctx context.Context, producer, key string) (notice.Thread, error) {
	t, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE producer = ? AND dedup_key = ? AND state <> 'resolved' ORDER BY id DESC LIMIT 1`, producer, key))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// Thread reads one thread.
func (s *Store) Thread(ctx context.Context, id int64) (notice.Thread, error) {
	t, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// OpenThread starts a thread.
func (s *Store) OpenThread(ctx context.Context, producer, key, title string, at time.Time) (notice.Thread, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO threads (producer, dedup_key, state, title, opened_at) VALUES (?, ?, 'open', ?, ?)`, producer, key, title, fmtTime(at))
	if err != nil {
		return notice.Thread{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return notice.Thread{}, err
	}
	return s.Thread(ctx, id)
}

// TouchThread counts one more event on a thread.
func (s *Store) TouchThread(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE threads SET events = events + 1 WHERE id = ?`, id)
	return err
}

// MoveThread acknowledges or resolves a thread. It says false when the
// thread was already there or past it.
func (s *Store) MoveThread(ctx context.Context, id int64, to notice.ThreadState, by string, at time.Time) (bool, error) {
	var res sql.Result
	var err error
	switch to {
	case notice.ThreadAcknowledged:
		res, err = s.db.ExecContext(ctx, `UPDATE threads SET state = 'acknowledged', acknowledged_at = ?, acknowledged_by = ?, events = events + 1 WHERE id = ? AND state = 'open'`, fmtTime(at), by, id)
	case notice.ThreadResolved:
		res, err = s.db.ExecContext(ctx, `UPDATE threads SET state = 'resolved', resolved_at = ?, events = events + 1 WHERE id = ? AND state <> 'resolved'`, fmtTime(at), id)
	default:
		return false, errors.New("a thread moves to acknowledged or resolved")
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ActivePagesOfThread lists the pages still working for a thread's events.
func (s *Store) ActivePagesOfThread(ctx context.Context, thread int64) ([]notice.Page, error) {
	return s.pages(ctx, `WHERE state = 'active' AND event_id IN (SELECT id FROM notice_events WHERE json_extract(body, '$.thread') = ?) ORDER BY id`, thread)
}

// AddInbox puts an event in a person's list.
func (s *Store) AddInbox(ctx context.Context, eventID int64, person string, w notice.Weight, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO inbox (event_id, person, weight, at) VALUES (?, ?, ?, ?) ON CONFLICT DO UPDATE SET weight = CASE WHEN excluded.weight = 'page' THEN 'page' ELSE inbox.weight END`,
		eventID, person, string(w), fmtTime(at))
	return err
}

// InboxLine is one event in a person's list.
type InboxLine struct {
	Event  notice.Event
	Weight notice.Weight
	At     time.Time
}

// Inbox lists the newest events that reached a person.
func (s *Store) Inbox(ctx context.Context, person string, limit int) ([]InboxLine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.body, i.weight, i.at FROM inbox i JOIN notice_events e ON e.id = i.event_id WHERE i.person = ? ORDER BY i.at DESC, e.id DESC LIMIT ?`, person, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxLine
	for rows.Next() {
		var l InboxLine
		var id int64
		var body, weight, at string
		if err := rows.Scan(&id, &body, &weight, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &l.Event); err != nil {
			return nil, err
		}
		l.Event.ID, l.Weight, l.At = id, notice.Weight(weight), parseTime(at)
		out = append(out, l)
	}
	return out, rows.Err()
}
