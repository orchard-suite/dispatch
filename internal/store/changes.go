package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"

	"github.com/orchard-suite/dispatch/internal/incident"
)

// ChangeFilter is which changes to read: on a service or touching one of
// the entities (either matches; neither given is every change), made in
// [Since, Until), newest first, at most Limit.
type ChangeFilter struct {
	Service  string
	Entities []change.Ref
	Since    time.Time
	Until    time.Time
	Limit    int
}

// MaxChanges is the most changes one read answers.
const MaxChanges = 500

const changeColumns = `c.id, c.producer, c.service, c.kind, c.title, c.url, c.by_whom, c.at, c.run, c.record_sender, c.record_key, c.digest, c.effects, c.route, c.outcome, c.ended_at`

// AddChange keeps a change and the things it touched. A change whose
// record is already kept is not kept again: the kept one comes back, with
// false.
func (s *Store) AddChange(ctx context.Context, c incident.Change) (incident.Change, bool, error) {
	kept := false
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if c.Record != nil {
			had, err := s.ChangeByRecord(ctx, tx, *c.Record)
			if err == nil {
				c = had
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		id, err := s.InsertChange(ctx, tx, c)
		if err != nil {
			return err
		}
		// A posted change is announced as it is kept.
		if _, err := tx.ExecContext(ctx, `UPDATE changes SET announced = 1 WHERE id = ?`, id); err != nil {
			return err
		}
		kept = true
		c, err = s.Change(ctx, tx, id)
		return err
	})
	return c, kept, err
}

// InsertChange writes a change and its things, and answers its id.
func (s *Store) InsertChange(ctx context.Context, q querier, c incident.Change) (int64, error) {
	effects, err := json.Marshal(nonNil(c.Effects))
	if err != nil {
		return 0, err
	}
	var sender, key string
	if c.Record != nil {
		sender, key = c.Record.Sender, c.Record.Key
	}
	res, err := q.ExecContext(ctx, `INSERT INTO changes (producer, service, kind, title, url, by_whom, at, run, record_sender, record_key, digest, effects, route, outcome, ended_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Producer, c.Service, c.Kind, c.Title, c.URL, c.By, fmtTime(c.At), c.Run, sender, key, c.Digest, string(effects), c.Route, c.Outcome, fmtTimePtr(c.EndedAt))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, t := range c.Things {
		if _, err := q.ExecContext(ctx, `INSERT INTO change_things (change_id, blueprint, key) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, id, t.Blueprint, t.Key); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// EndChange keeps how a change ended. Its time becomes its end, which is
// when it took effect, so the incidents near it are joined from then.
func (s *Store) EndChange(ctx context.Context, q querier, id int64, outcome string, at time.Time) error {
	return s.affected(q.ExecContext(ctx, `UPDATE changes SET outcome = ?, ended_at = ?, at = ? WHERE id = ?`, outcome, fmtTime(at), fmtTime(at), id))
}

// ClaimAnnouncement marks a change announced when it ended having run and
// was not announced before, and says whether it did.
func (s *Store) ClaimAnnouncement(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE changes SET announced = 1 WHERE id = ? AND announced = 0 AND outcome IN (?, ?)`, id, change.OutcomeOK, change.OutcomeFailed)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RouteChange keeps the judge's route for a change.
func (s *Store) RouteChange(ctx context.Context, q querier, id int64, route string) error {
	return s.affected(q.ExecContext(ctx, `UPDATE changes SET route = ? WHERE id = ?`, route, id))
}

// Change reads one change with its things and events.
func (s *Store) Change(ctx context.Context, q querier, id int64) (incident.Change, error) {
	out, err := s.changes(ctx, q, `SELECT `+changeColumns+` FROM changes c WHERE c.id = ?`, id)
	if err != nil {
		return incident.Change{}, err
	}
	if len(out) == 0 {
		return incident.Change{}, ErrNotFound
	}
	return out[0], nil
}

// ChangeByRecord reads the change a record names.
func (s *Store) ChangeByRecord(ctx context.Context, q querier, r change.EventRef) (incident.Change, error) {
	out, err := s.changes(ctx, q, `SELECT `+changeColumns+` FROM changes c WHERE c.record_sender = ? AND c.record_key = ?`, r.Sender, r.Key)
	if err != nil {
		return incident.Change{}, err
	}
	if len(out) == 0 {
		return incident.Change{}, ErrNotFound
	}
	return out[0], nil
}

// Changes reads the changes a filter names.
func (s *Store) Changes(ctx context.Context, f ChangeFilter) ([]incident.Change, error) {
	var where []string
	var args []any
	var match []string
	if f.Service != "" {
		match = append(match, `c.service = ?`)
		args = append(args, f.Service)
	}
	for _, e := range f.Entities {
		match = append(match, `EXISTS (SELECT 1 FROM change_things t WHERE t.change_id = c.id AND t.blueprint = ? AND t.key = ?)`)
		args = append(args, e.Blueprint, e.Key)
	}
	if len(match) > 0 {
		where = append(where, "("+strings.Join(match, " OR ")+")")
	}
	if !f.Since.IsZero() {
		where = append(where, `c.at >= ?`)
		args = append(args, fmtTime(f.Since))
	}
	if !f.Until.IsZero() {
		where = append(where, `c.at < ?`)
		args = append(args, fmtTime(f.Until))
	}
	limit := f.Limit
	if limit <= 0 || limit > MaxChanges {
		limit = MaxChanges
	}
	query := `SELECT ` + changeColumns + ` FROM changes c`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY c.at DESC, c.id DESC LIMIT ?`
	args = append(args, limit)
	return s.changes(ctx, s.db, query, args...)
}

func (s *Store) changes(ctx context.Context, q querier, query string, args ...any) ([]incident.Change, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []incident.Change
	for rows.Next() {
		var c incident.Change
		var at, sender, key, effects string
		var ended sql.NullString
		if err := rows.Scan(&c.ID, &c.Producer, &c.Service, &c.Kind, &c.Title, &c.URL, &c.By, &at, &c.Run, &sender, &key, &c.Digest, &effects, &c.Route, &c.Outcome, &ended); err != nil {
			rows.Close()
			return nil, err
		}
		c.At, c.EndedAt = parseTime(at), parseTimePtr(ended)
		if key != "" {
			c.Record = &change.EventRef{Sender: sender, Key: key}
		}
		if err := json.Unmarshal([]byte(effects), &c.Effects); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Things, err = s.changeThings(ctx, q, out[i].ID); err != nil {
			return nil, err
		}
		if out[i].Events, err = s.changeEventNames(ctx, q, out[i].ID); err != nil {
			return nil, err
		}
		w, err := s.ChangeWatch(ctx, q, out[i].ID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return nil, err
		default:
			if w.Sends, err = s.Sends(ctx, q, out[i].ID); err != nil {
				return nil, err
			}
			out[i].Watch = &w
		}
	}
	return out, nil
}

func (s *Store) changeThings(ctx context.Context, q querier, id int64) ([]change.Ref, error) {
	rows, err := q.QueryContext(ctx, `SELECT blueprint, key FROM change_things WHERE change_id = ? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []change.Ref
	for rows.Next() {
		var r change.Ref
		if err := rows.Scan(&r.Blueprint, &r.Key); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) changeEventNames(ctx context.Context, q querier, id int64) ([]incident.ChangeEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT event, sender, key, at FROM change_events WHERE change_id = ? ORDER BY at, rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.ChangeEvent
	for rows.Next() {
		var e incident.ChangeEvent
		var at string
		if err := rows.Scan(&e.Event, &e.Sender, &e.Key, &at); err != nil {
			return nil, err
		}
		e.At = parseTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ChangeEvent is one change event as kept: the kit's event in Body, and
// the change it belongs to once its change.stated event is kept.
type ChangeEvent struct {
	Event    string
	Sender   string
	Key      string
	Of       *change.EventRef
	ChangeID int64
	At       time.Time
	Body     []byte
}

// HasChangeEvent says whether an event with this sender and key is kept.
func (s *Store) HasChangeEvent(ctx context.Context, q querier, sender, key string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM change_events WHERE sender = ? AND key = ?`, sender, key).Scan(&n)
	return n > 0, err
}

// InsertChangeEvent keeps one change event.
func (s *Store) InsertChangeEvent(ctx context.Context, q querier, e ChangeEvent, received time.Time) error {
	var ofSender, ofKey string
	if e.Of != nil {
		ofSender, ofKey = e.Of.Sender, e.Of.Key
	}
	var changeID any
	if e.ChangeID != 0 {
		changeID = e.ChangeID
	}
	_, err := q.ExecContext(ctx, `INSERT INTO change_events (sender, key, event, of_sender, of_key, change_id, at, body, received_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Sender, e.Key, e.Event, ofSender, ofKey, changeID, fmtTime(e.At), string(e.Body), fmtTime(received))
	return err
}

// ChangeEventsOf reads the events that name a change.stated event as
// theirs, oldest first, whether or not that event is kept yet.
func (s *Store) ChangeEventsOf(ctx context.Context, q querier, of change.EventRef) ([]ChangeEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT event, sender, key, of_sender, of_key, COALESCE(change_id, 0), at, body FROM change_events WHERE of_sender = ? AND of_key = ? ORDER BY at, rowid`, of.Sender, of.Key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChangeEvent
	for rows.Next() {
		var e ChangeEvent
		var ofSender, ofKey, at, body string
		if err := rows.Scan(&e.Event, &e.Sender, &e.Key, &ofSender, &ofKey, &e.ChangeID, &at, &body); err != nil {
			return nil, err
		}
		e.Of = &change.EventRef{Sender: ofSender, Key: ofKey}
		e.At, e.Body = parseTime(at), []byte(body)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AttachChangeEvents ties the events that name a change.stated event to
// the change it made.
func (s *Store) AttachChangeEvents(ctx context.Context, q querier, of change.EventRef, changeID int64) error {
	_, err := q.ExecContext(ctx, `UPDATE change_events SET change_id = ? WHERE of_sender = ? AND of_key = ? AND change_id IS NULL`, changeID, of.Sender, of.Key)
	return err
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// StateChange gives a change kept before its statement the statement's
// digest and effects, and the things it names.
func (s *Store) StateChange(ctx context.Context, q querier, id int64, digest string, effects []change.Effect, things []change.Ref) error {
	b, err := json.Marshal(nonNil(effects))
	if err != nil {
		return err
	}
	if err := s.affected(q.ExecContext(ctx, `UPDATE changes SET digest = ?, effects = ? WHERE id = ?`, digest, string(b), id)); err != nil {
		return err
	}
	for _, t := range things {
		if _, err := q.ExecContext(ctx, `INSERT INTO change_things (change_id, blueprint, key) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, id, t.Blueprint, t.Key); err != nil {
			return err
		}
	}
	return nil
}
