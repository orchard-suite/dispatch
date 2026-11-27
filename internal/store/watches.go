package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"

	"github.com/orchard-suite/dispatch/internal/incident"
)

// StartWatch keeps a watch on a change from when it ended until due. A
// change already watched keeps its watch; false says so.
func (s *Store) StartWatch(ctx context.Context, q querier, changeID int64, from, due, now time.Time) (bool, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO watches (change_id, state, from_at, due_at, created_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (change_id) DO NOTHING`,
		changeID, incident.WatchWatching, fmtTime(from), fmtTime(due), fmtTime(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DueWatches lists the change ids whose watch window has closed.
func (s *Store) DueWatches(ctx context.Context, now time.Time) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT change_id FROM watches WHERE state = ? AND due_at <= ? ORDER BY due_at`, incident.WatchWatching, fmtTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ChangeWatch reads a change's watch.
func (s *Store) ChangeWatch(ctx context.Context, q querier, changeID int64) (incident.Watch, error) {
	var w incident.Watch
	var from, due, sights, incidents string
	var done sql.NullString
	err := q.QueryRowContext(ctx, `SELECT state, from_at, due_at, verdict, sights, incidents, done_at FROM watches WHERE change_id = ?`, changeID).
		Scan(&w.State, &from, &due, &w.Verdict, &sights, &incidents, &done)
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	if err != nil {
		return w, err
	}
	w.From, w.Due, w.Done = parseTime(from), parseTime(due), parseTimePtr(done)
	if err := json.Unmarshal([]byte(sights), &w.Sights); err != nil {
		return w, err
	}
	return w, json.Unmarshal([]byte(incidents), &w.Incidents)
}

// EndWatch keeps what a watch saw and closes it.
func (s *Store) EndWatch(ctx context.Context, q querier, changeID int64, verdict string, sights []change.Sight, incidents []change.Incident, now time.Time) error {
	sb, err := json.Marshal(nonNil(sights))
	if err != nil {
		return err
	}
	ib, err := json.Marshal(nonNil(incidents))
	if err != nil {
		return err
	}
	return s.affected(q.ExecContext(ctx, `UPDATE watches SET state = ?, verdict = ?, sights = ?, incidents = ?, done_at = ? WHERE change_id = ? AND state = ?`,
		incident.WatchDone, verdict, string(sb), string(ib), fmtTime(now), changeID, incident.WatchWatching))
}

// Send is one observation saved to be handed on to one target.
type Send struct {
	ID       int64
	ChangeID int64
	Target   string
	Body     []byte
	Attempts int
}

// SaveSend saves an observation to hand on to a target.
func (s *Store) SaveSend(ctx context.Context, q querier, changeID int64, target string, body []byte, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO change_sends (change_id, target, body, state, next_at, created_at) VALUES (?, ?, ?, 'pending', ?, ?) ON CONFLICT (change_id, target) DO NOTHING`,
		changeID, target, string(body), fmtTime(now), fmtTime(now))
	return err
}

// DueSends lists the sends whose next try is due, oldest first.
func (s *Store) DueSends(ctx context.Context, now time.Time, limit int) ([]Send, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, change_id, target, body, attempts FROM change_sends WHERE state = 'pending' AND next_at <= ? ORDER BY next_at, id LIMIT ?`, fmtTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Send
	for rows.Next() {
		var x Send
		var body string
		if err := rows.Scan(&x.ID, &x.ChangeID, &x.Target, &body, &x.Attempts); err != nil {
			return nil, err
		}
		x.Body = []byte(body)
		out = append(out, x)
	}
	return out, rows.Err()
}

// SendTaken marks a send taken by its target.
func (s *Store) SendTaken(ctx context.Context, id int64, now time.Time) error {
	return s.affected(s.db.ExecContext(ctx, `UPDATE change_sends SET state = 'sent', attempts = attempts + 1, last_error = '', sent_at = ? WHERE id = ?`, fmtTime(now), id))
}

// SendAgain keeps why a send was not taken and when it is tried next.
func (s *Store) SendAgain(ctx context.Context, id int64, next time.Time, why string) error {
	return s.affected(s.db.ExecContext(ctx, `UPDATE change_sends SET attempts = attempts + 1, next_at = ?, last_error = ? WHERE id = ?`, fmtTime(next), why, id))
}

// Sends lists a change's sends: each target, whether it was taken, and the
// last refusal.
func (s *Store) Sends(ctx context.Context, q querier, changeID int64) ([]incident.WatchSend, error) {
	rows, err := q.QueryContext(ctx, `SELECT target, state, attempts, last_error FROM change_sends WHERE change_id = ? ORDER BY id`, changeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.WatchSend
	for rows.Next() {
		var x incident.WatchSend
		if err := rows.Scan(&x.Target, &x.State, &x.Attempts, &x.LastError); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// StrayEnds lists the change.ended events whose change.stated event never
// came, received before a time and not yet told.
func (s *Store) StrayEnds(ctx context.Context, before time.Time) ([]ChangeEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.event, e.sender, e.key, e.of_sender, e.of_key, e.at, e.body FROM change_events e
WHERE e.event = ? AND e.told = 0 AND e.received_at < ?
  AND NOT EXISTS (SELECT 1 FROM change_events s WHERE s.sender = e.of_sender AND s.key = e.of_key)
ORDER BY e.received_at`,
		change.EventEnded, fmtTime(before))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChangeEvent
	for rows.Next() {
		var e ChangeEvent
		var ofSender, ofKey, at, body string
		if err := rows.Scan(&e.Event, &e.Sender, &e.Key, &ofSender, &ofKey, &at, &body); err != nil {
			return nil, err
		}
		e.Of = &change.EventRef{Sender: ofSender, Key: ofKey}
		e.At, e.Body = parseTime(at), []byte(body)
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkTold marks a change event as told about.
func (s *Store) MarkTold(ctx context.Context, sender, key string) error {
	return s.affected(s.db.ExecContext(ctx, `UPDATE change_events SET told = 1 WHERE sender = ? AND key = ?`, sender, key))
}
