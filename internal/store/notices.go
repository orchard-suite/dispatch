package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

const noticeSchema = `
CREATE TABLE IF NOT EXISTS channels (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL,
  target TEXT NOT NULL,
  created_at TEXT NOT NULL,
  created_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS subscriptions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  topics TEXT NOT NULL,
  producers TEXT NOT NULL DEFAULT '[]',
  anchor TEXT NOT NULL DEFAULT '',
  match TEXT NOT NULL DEFAULT '{}',
  paused INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  created_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS notice_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  producer TEXT NOT NULL,
  key TEXT NOT NULL,
  kind TEXT NOT NULL,
  body TEXT NOT NULL,
  received_at TEXT NOT NULL,
  UNIQUE (producer, key)
);
CREATE TABLE IF NOT EXISTS deliveries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL REFERENCES notice_events(id) ON DELETE CASCADE,
  channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  subscription_id INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at TEXT NOT NULL,
  status INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  sent_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE (event_id, channel_id)
);
CREATE INDEX IF NOT EXISTS deliveries_due ON deliveries(state, next_at);
`

// AddChannel stores a channel whose target is already sealed.
func (s *Store) AddChannel(ctx context.Context, c notice.Channel) (notice.Channel, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO channels (name, kind, target, secret, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?)`,
		c.Name, string(c.Kind), c.Target, c.Secret, fmtTime(c.CreatedAt), c.CreatedBy)
	if err != nil {
		return c, err
	}
	c.ID, err = res.LastInsertId()
	return c, err
}

// Channels lists every channel, by name, with its target still sealed.
func (s *Store) Channels(ctx context.Context) ([]notice.Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, kind, target, secret, old_secret, created_at, created_by FROM channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notice.Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Channel reads one channel, its target still sealed.
func (s *Store) Channel(ctx context.Context, id int64) (notice.Channel, error) {
	c, err := scanChannel(s.db.QueryRowContext(ctx, `SELECT id, name, kind, target, secret, old_secret, created_at, created_by FROM channels WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

func scanChannel(r interface{ Scan(...any) error }) (notice.Channel, error) {
	var c notice.Channel
	var kind, at string
	if err := r.Scan(&c.ID, &c.Name, &kind, &c.Target, &c.Secret, &c.OldSecret, &at, &c.CreatedBy); err != nil {
		return c, err
	}
	c.Kind, c.CreatedAt = notice.Kind(kind), parseTime(at)
	return c, nil
}

// SetSecrets replaces a channel's sealed signing secrets.
func (s *Store) SetSecrets(ctx context.Context, id int64, secret, old string) error {
	return s.affected(s.db.ExecContext(ctx, `UPDATE channels SET secret = ?, old_secret = ? WHERE id = ?`, secret, old, id))
}

// DeleteChannel removes a channel, its subscriptions, and its deliveries.
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	return s.affected(s.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id))
}

func (s *Store) AddSubscription(ctx context.Context, sub notice.Subscription) (notice.Subscription, error) {
	topics, producers, match := jsonOf(sub.Topics), jsonOf(sub.Producers), jsonOf(sub.Match)
	var channel any
	if sub.ChannelID != 0 {
		channel = sub.ChannelID
	}
	target := ""
	if sub.Target != nil {
		target = sub.Target.String()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO subscriptions (channel_id, target, topics, producers, anchor, match, paused, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		channel, target, topics, producers, sub.Anchor, match, sub.Paused, fmtTime(sub.CreatedAt), sub.CreatedBy)
	if err != nil {
		return sub, err
	}
	sub.ID, err = res.LastInsertId()
	return sub, err
}

// Subscriptions lists every subscription, oldest first.
func (s *Store) Subscriptions(ctx context.Context) ([]notice.Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(channel_id, 0), target, topics, producers, anchor, match, paused, created_at, created_by FROM subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notice.Subscription
	for rows.Next() {
		var sub notice.Subscription
		var target, topics, producers, match, at string
		if err := rows.Scan(&sub.ID, &sub.ChannelID, &target, &topics, &producers, &sub.Anchor, &match, &sub.Paused, &at, &sub.CreatedBy); err != nil {
			return nil, err
		}
		if target != "" {
			if t, err := notice.ParseTarget(target); err == nil {
				sub.Target = &t
			}
		}
		_ = json.Unmarshal([]byte(topics), &sub.Topics)
		_ = json.Unmarshal([]byte(producers), &sub.Producers)
		_ = json.Unmarshal([]byte(match), &sub.Match)
		sub.CreatedAt = parseTime(at)
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *Store) SetSubscriptionPaused(ctx context.Context, id int64, paused bool) error {
	return s.affected(s.db.ExecContext(ctx, `UPDATE subscriptions SET paused = ? WHERE id = ?`, paused, id))
}

func (s *Store) DeleteSubscription(ctx context.Context, id int64) error {
	return s.affected(s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id))
}

// InsertEvent stores an event, or finds the one its producer already sent
// under the same key. fresh is false for a repeat.
func (s *Store) InsertEvent(ctx context.Context, e notice.Event, at time.Time) (stored notice.Event, fresh bool, err error) {
	body, err := json.Marshal(e)
	if err != nil {
		return e, false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO notice_events (producer, key, kind, body, received_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (producer, key) DO NOTHING`,
		e.Producer, e.Key, e.Kind, string(body), fmtTime(at))
	if err != nil {
		return e, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		e.ID, err = res.LastInsertId()
		return e, true, err
	}
	var id int64
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT id, body FROM notice_events WHERE producer = ? AND key = ?`, e.Producer, e.Key).Scan(&id, &raw); err != nil {
		return e, false, err
	}
	var old notice.Event
	if err := json.Unmarshal([]byte(raw), &old); err != nil {
		return e, false, err
	}
	old.ID = id
	return old, false, nil
}

// Event reads one stored event.
func (s *Store) Event(ctx context.Context, id int64) (notice.Event, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT body FROM notice_events WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return notice.Event{}, ErrNotFound
	}
	if err != nil {
		return notice.Event{}, err
	}
	var e notice.Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return e, err
	}
	e.ID = id
	return e, nil
}

// AddDelivery queues an event for a channel. An event goes to a channel
// once however many of its subscriptions take it.
func (s *Store) AddDelivery(ctx context.Context, eventID, channelID, subscriptionID int64, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO deliveries (event_id, channel_id, subscription_id, state, next_at, created_at) VALUES (?, ?, ?, 'pending', ?, ?) ON CONFLICT (event_id, channel_id) DO NOTHING`,
		eventID, channelID, subscriptionID, fmtTime(at), fmtTime(at))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Due lists pending deliveries whose time has come, oldest first.
func (s *Store) Due(ctx context.Context, now time.Time, limit int) ([]notice.Delivery, error) {
	return s.deliveries(ctx, `WHERE state = 'pending' AND next_at <= ? ORDER BY next_at, id LIMIT ?`, fmtTime(now), limit)
}

// DeliveryLine is a delivery as the Notifications page lists it.
type DeliveryLine struct {
	notice.Delivery
	Channel string
	Kind    string
	Title   string
}

// RecentDeliveries lists the newest deliveries with their channel and event.
func (s *Store) RecentDeliveries(ctx context.Context, limit int) ([]DeliveryLine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT d.id, d.event_id, d.channel_id, d.subscription_id, d.state, d.attempts, d.next_at, d.status, d.last_error, d.sent_at, d.created_at, c.name, e.kind, e.body
FROM deliveries d JOIN channels c ON c.id = d.channel_id JOIN notice_events e ON e.id = d.event_id ORDER BY d.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeliveryLine
	for rows.Next() {
		var l DeliveryLine
		var body string
		var state, next, created string
		var sent sql.NullString
		if err := rows.Scan(&l.ID, &l.EventID, &l.ChannelID, &l.SubscriptionID, &state, &l.Attempts, &next, &l.Status, &l.LastError, &sent, &created, &l.Channel, &l.Kind, &body); err != nil {
			return nil, err
		}
		l.State, l.NextAt, l.SentAt, l.CreatedAt = notice.DeliveryState(state), parseTime(next), parseTimePtr(sent), parseTime(created)
		var e notice.Event
		_ = json.Unmarshal([]byte(body), &e)
		l.Title = e.Title
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) deliveries(ctx context.Context, where string, args ...any) ([]notice.Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, event_id, channel_id, subscription_id, state, attempts, next_at, status, last_error, sent_at, created_at FROM deliveries `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notice.Delivery
	for rows.Next() {
		var d notice.Delivery
		var state, next, created string
		var sent sql.NullString
		if err := rows.Scan(&d.ID, &d.EventID, &d.ChannelID, &d.SubscriptionID, &state, &d.Attempts, &next, &d.Status, &d.LastError, &sent, &created); err != nil {
			return nil, err
		}
		d.State, d.NextAt, d.SentAt, d.CreatedAt = notice.DeliveryState(state), parseTime(next), parseTimePtr(sent), parseTime(created)
		out = append(out, d)
	}
	return out, rows.Err()
}

// SaveDelivery writes what came of one attempt.
func (s *Store) SaveDelivery(ctx context.Context, d notice.Delivery) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET state = ?, attempts = ?, next_at = ?, status = ?, last_error = ?, sent_at = ? WHERE id = ?`,
		string(d.State), d.Attempts, fmtTime(d.NextAt), d.Status, d.LastError, fmtTimePtr(d.SentAt), d.ID)
	return err
}

func (s *Store) affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func jsonOf(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
