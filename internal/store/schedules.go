package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

// SaveSchedule adds a schedule or replaces its name, zone, and layers,
// keeping its overrides.
func (s *Store) SaveSchedule(ctx context.Context, sc notice.Schedule) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedules (slug, name, zone, layers) VALUES (?, ?, ?, ?) ON CONFLICT (slug) DO UPDATE SET name = excluded.name, zone = excluded.zone, layers = excluded.layers`,
		sc.Slug, sc.Name, sc.Zone, jsonOf(sc.Layers))
	return err
}

func (s *Store) DeleteSchedule(ctx context.Context, slug string) error {
	return s.affected(s.db.ExecContext(ctx, `DELETE FROM schedules WHERE slug = ?`, slug))
}

// Schedule reads one schedule with the overrides that have not ended by
// since.
func (s *Store) Schedule(ctx context.Context, slug string, since time.Time) (notice.Schedule, error) {
	sc := notice.Schedule{Slug: slug}
	var layers string
	err := s.db.QueryRowContext(ctx, `SELECT name, zone, layers FROM schedules WHERE slug = ?`, slug).Scan(&sc.Name, &sc.Zone, &layers)
	if errors.Is(err, sql.ErrNoRows) {
		return sc, ErrNotFound
	}
	if err != nil {
		return sc, err
	}
	if err := json.Unmarshal([]byte(layers), &sc.Layers); err != nil {
		return sc, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, person, from_at, to_at, by_whom, reason FROM overrides WHERE schedule = ? AND to_at > ? ORDER BY id`, slug, fmtTime(since))
	if err != nil {
		return sc, err
	}
	defer rows.Close()
	for rows.Next() {
		var o notice.Override
		var from, to string
		if err := rows.Scan(&o.ID, &o.Person, &from, &to, &o.By, &o.Reason); err != nil {
			return sc, err
		}
		o.From, o.To = parseTime(from), parseTime(to)
		sc.Overrides = append(sc.Overrides, o)
	}
	return sc, rows.Err()
}

// Schedules lists every schedule with its overrides that have not ended by
// since.
func (s *Store) Schedules(ctx context.Context, since time.Time) ([]notice.Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slug FROM schedules ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			rows.Close()
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	rows.Close()
	out := make([]notice.Schedule, 0, len(slugs))
	for _, slug := range slugs {
		sc, err := s.Schedule(ctx, slug, since)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

// AddOverride puts a person on call for a while.
func (s *Store) AddOverride(ctx context.Context, slug string, o notice.Override) (notice.Override, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO overrides (schedule, person, from_at, to_at, by_whom, reason) VALUES (?, ?, ?, ?, ?, ?)`,
		slug, o.Person, fmtTime(o.From), fmtTime(o.To), o.By, o.Reason)
	if err != nil {
		return o, err
	}
	o.ID, err = res.LastInsertId()
	return o, err
}

func (s *Store) DeleteOverride(ctx context.Context, slug string, id int64) error {
	return s.affected(s.db.ExecContext(ctx, `DELETE FROM overrides WHERE schedule = ? AND id = ?`, slug, id))
}

// Watched is who a schedule last had on call, since when, and the day it
// last told of a gap.
type Watched struct {
	Person  string
	Since   time.Time
	GapTold string
}

// Watched reads what the desk last saw of a schedule; a schedule never
// seen is ErrNotFound.
func (s *Store) Watched(ctx context.Context, slug string) (Watched, error) {
	var w Watched
	var since string
	err := s.db.QueryRowContext(ctx, `SELECT person, since, gap_told FROM schedule_watch WHERE slug = ?`, slug).Scan(&w.Person, &since, &w.GapTold)
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	w.Since = parseTime(since)
	return w, err
}

// Watch keeps what the desk saw of a schedule.
func (s *Store) Watch(ctx context.Context, slug string, w Watched) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO schedule_watch (slug, person, since, gap_told) VALUES (?, ?, ?, ?) ON CONFLICT (slug) DO UPDATE SET person = excluded.person, since = excluded.since, gap_told = excluded.gap_told`,
		slug, w.Person, fmtTime(w.Since), w.GapTold)
	return err
}

// NewCalendar gives a person a new calendar link's secret, answered once;
// only its hash is kept, and the old link stops working.
func (s *Store) NewCalendar(ctx context.Context, email string) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := "cal_" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(secret))
	if err := s.affected(s.db.ExecContext(ctx, `UPDATE people SET calendar = ? WHERE email = ?`, hex.EncodeToString(sum[:]), email)); err != nil {
		return "", err
	}
	return secret, nil
}

// CalendarOwner is the person whose calendar link secret this is.
func (s *Store) CalendarOwner(ctx context.Context, secret string) (string, error) {
	sum := sha256.Sum256([]byte(secret))
	var email string
	err := s.db.QueryRowContext(ctx, `SELECT email FROM people WHERE calendar = ? AND calendar <> ''`, hex.EncodeToString(sum[:])).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return email, err
}

// Reminder is a snooze: the person to page about an incident when it is
// due, if the incident is still not mitigated or resolved.
type Reminder struct {
	ID         int64
	IncidentID int64
	Person     string
	DueAt      time.Time
}

func (s *Store) AddReminder(ctx context.Context, r Reminder) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO reminders (incident_id, person, due_at) VALUES (?, ?, ?)`, r.IncidentID, r.Person, fmtTime(r.DueAt))
	return err
}

// DueReminders takes every reminder due by now, marking each done.
func (s *Store) DueReminders(ctx context.Context, now time.Time) ([]Reminder, error) {
	var out []Reminder
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, incident_id, person, due_at FROM reminders WHERE done = 0 AND due_at <= ? ORDER BY due_at`, fmtTime(now))
		if err != nil {
			return err
		}
		for rows.Next() {
			var r Reminder
			var due string
			if err := rows.Scan(&r.ID, &r.IncidentID, &r.Person, &due); err != nil {
				rows.Close()
				return err
			}
			r.DueAt = parseTime(due)
			out = append(out, r)
		}
		rows.Close()
		_, err = tx.ExecContext(ctx, `UPDATE reminders SET done = 1 WHERE done = 0 AND due_at <= ?`, fmtTime(now))
		return err
	})
	return out, err
}

// Reminders lists an incident's reminders still to come.
func (s *Store) Reminders(ctx context.Context, incidentID int64) ([]Reminder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, incident_id, person, due_at FROM reminders WHERE done = 0 AND incident_id = ? ORDER BY due_at`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reminder
	for rows.Next() {
		var r Reminder
		var due string
		if err := rows.Scan(&r.ID, &r.IncidentID, &r.Person, &due); err != nil {
			return nil, err
		}
		r.DueAt = parseTime(due)
		out = append(out, r)
	}
	return out, rows.Err()
}
