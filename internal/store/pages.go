package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

// AddMessage queues an event for one person by one medium. A person is
// sent an event once per medium, and once more per chain page that reaches
// them.
// It is sent at next, which may be later than now.
// A page's later round tells the person again.
func (s *Store) AddMessage(ctx context.Context, eventID int64, person string, medium notice.Medium, pageID int64, round int, next, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO messages (event_id, person, medium, page_id, round, state, next_at, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?) ON CONFLICT DO NOTHING`,
		eventID, person, string(medium), pageID, round, fmtTime(next), fmtTime(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

const messageColumns = `id, event_id, person, medium, page_id, state, attempts, next_at, status, last_error, sent_at, created_at, delivered_at, opened_at, receipt`

func scanMessage(r interface{ Scan(...any) error }, extra ...any) (notice.Message, error) {
	var m notice.Message
	var medium, state, next, created string
	var sent, delivered, opened sql.NullString
	if err := r.Scan(append([]any{&m.ID, &m.EventID, &m.Person, &medium, &m.PageID, &state, &m.Attempts, &next, &m.Status, &m.LastError, &sent, &created, &delivered, &opened, &m.Receipt}, extra...)...); err != nil {
		return m, err
	}
	m.Medium, m.State, m.NextAt, m.SentAt, m.CreatedAt = notice.Medium(medium), notice.DeliveryState(state), parseTime(next), parseTimePtr(sent), parseTime(created)
	m.DeliveredAt, m.OpenedAt = parseTimePtr(delivered), parseTimePtr(opened)
	return m, nil
}

// Message reads one message.
func (s *Store) Message(ctx context.Context, id int64) (notice.Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// Deliver writes the far end's report on a message: delivered or not, in
// its own words.
func (s *Store) Deliver(ctx context.Context, id int64, delivered bool, words string, at time.Time) error {
	var when any
	if delivered {
		when = fmtTime(at)
	}
	return s.affected(s.db.ExecContext(ctx, `UPDATE messages SET delivered_at = COALESCE(delivered_at, ?), receipt = ? WHERE id = ?`, when, words, id))
}

// Opened marks every message of a page to a person as opened, once.
func (s *Store) Opened(ctx context.Context, pageID int64, person string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET opened_at = ? WHERE page_id = ? AND person = ? AND opened_at IS NULL AND state = 'sent'`, fmtTime(at), pageID, person)
	return err
}

// PageMessages lists every message a page sent, in order.
func (s *Store) PageMessages(ctx context.Context, pageID int64) ([]notice.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE page_id = ? ORDER BY id`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notice.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ActivePageOf is the newest active page that sent a person a message by
// medium, which is what their reply on that medium answers.
func (s *Store) ActivePageOf(ctx context.Context, person string, medium notice.Medium) (notice.Page, error) {
	p, err := scanPage(s.db.QueryRowContext(ctx, `SELECT `+pageColumns+` FROM pages WHERE state = 'active' AND id IN (SELECT page_id FROM messages WHERE person = ? AND medium = ? AND state = 'sent') ORDER BY id DESC LIMIT 1`, person, string(medium)))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// DueMessages lists pending messages whose time has come, oldest first.
func (s *Store) DueMessages(ctx context.Context, now time.Time, limit int) ([]notice.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageColumns+` FROM messages WHERE state = 'pending' AND next_at <= ? ORDER BY next_at, id LIMIT ?`, fmtTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notice.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveMessage writes what came of one attempt.
func (s *Store) SaveMessage(ctx context.Context, m notice.Message) error {
	_, err := s.db.ExecContext(ctx, `UPDATE messages SET state = ?, attempts = ?, next_at = ?, status = ?, last_error = ?, sent_at = ? WHERE id = ?`,
		string(m.State), m.Attempts, fmtTime(m.NextAt), m.Status, m.LastError, fmtTimePtr(m.SentAt), m.ID)
	return err
}

// MessageLine is a message as the Notifications page lists it.
type MessageLine struct {
	notice.Message
	Kind  string
	Title string
}

// RecentMessages lists the newest messages to people with their event.
func (s *Store) RecentMessages(ctx context.Context, limit int) ([]MessageLine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.event_id, m.person, m.medium, m.page_id, m.state, m.attempts, m.next_at, m.status, m.last_error, m.sent_at, m.created_at, m.delivered_at, m.opened_at, m.receipt, e.kind, e.body
FROM messages m JOIN notice_events e ON e.id = m.event_id ORDER BY m.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageLine
	for rows.Next() {
		var l MessageLine
		var body string
		m, err := scanMessage(rows, &l.Kind, &body)
		if err != nil {
			return nil, err
		}
		l.Message = m
		var e notice.Event
		_ = json.Unmarshal([]byte(body), &e)
		l.Title = e.Title
		out = append(out, l)
	}
	return out, rows.Err()
}

// AddPage starts a page on its first step.
func (s *Store) AddPage(ctx context.Context, p notice.Page) (notice.Page, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO pages (event_id, chain, steps, step, repeat, state, next_at, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.EventID, p.Chain, jsonOf(p.Steps), p.Step, p.Repeat, string(p.State), fmtTime(p.NextAt), fmtTime(p.StartedAt))
	if err != nil {
		return p, err
	}
	p.ID, err = res.LastInsertId()
	return p, err
}

const pageColumns = `id, event_id, chain, steps, step, round, repeat, state, next_at, started_at, acknowledged_at, acknowledged_by, ended_at`

func scanPage(r interface{ Scan(...any) error }) (notice.Page, error) {
	var p notice.Page
	var steps, state, next, started string
	var acked, ended sql.NullString
	if err := r.Scan(&p.ID, &p.EventID, &p.Chain, &steps, &p.Step, &p.Round, &p.Repeat, &state, &next, &started, &acked, &p.AcknowledgedBy, &ended); err != nil {
		return p, err
	}
	_ = json.Unmarshal([]byte(steps), &p.Steps)
	p.State, p.NextAt, p.StartedAt, p.AcknowledgedAt, p.EndedAt = notice.PageState(state), parseTime(next), parseTime(started), parseTimePtr(acked), parseTimePtr(ended)
	return p, nil
}

func (s *Store) pages(ctx context.Context, where string, args ...any) ([]notice.Page, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pageColumns+` FROM pages `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notice.Page
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Page reads one page.
func (s *Store) Page(ctx context.Context, id int64) (notice.Page, error) {
	p, err := scanPage(s.db.QueryRowContext(ctx, `SELECT `+pageColumns+` FROM pages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// DuePages lists active pages whose next step is due.
func (s *Store) DuePages(ctx context.Context, now time.Time) ([]notice.Page, error) {
	return s.pages(ctx, `WHERE state = 'active' AND next_at <= ? ORDER BY next_at, id`, fmtTime(now))
}

// RecentPages lists active pages first, then the newest ended ones.
func (s *Store) RecentPages(ctx context.Context, limit int) ([]notice.Page, error) {
	return s.pages(ctx, `ORDER BY state = 'active' DESC, id DESC LIMIT ?`, limit)
}

// ActivePagesFor lists the active pages started for a producer's events
// whose keys start with prefix.
func (s *Store) ActivePagesFor(ctx context.Context, producer, prefix string) ([]notice.Page, error) {
	return s.pages(ctx, `WHERE state = 'active' AND event_id IN (SELECT id FROM notice_events WHERE producer = ? AND substr(key, 1, ?) = ?) ORDER BY id`, producer, len(prefix), prefix)
}

// StepPage moves an active page to a step of a round, unless it ended
// meanwhile.
func (s *Store) StepPage(ctx context.Context, id int64, step, round int, next time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE pages SET step = ?, round = ?, next_at = ? WHERE id = ? AND state = 'active'`, step, round, fmtTime(next), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// EndPage ends an active page as acknowledged or unanswered. It says false
// when the page had already ended.
func (s *Store) EndPage(ctx context.Context, id int64, state notice.PageState, by string, at time.Time) (bool, error) {
	var acked any
	if state == notice.PageAcknowledged {
		acked = fmtTime(at)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE pages SET state = ?, acknowledged_at = ?, acknowledged_by = ?, ended_at = ? WHERE id = ? AND state = 'active'`,
		string(state), acked, by, fmtTime(at), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
