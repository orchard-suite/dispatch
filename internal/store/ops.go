package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
)

// AddWindow keeps a maintenance window.
func (s *Store) AddWindow(ctx context.Context, w incident.Window) (incident.Window, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO windows (service, starts_at, ends_at, reason, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		w.Service, fmtTime(w.StartsAt), fmtTime(w.EndsAt), w.Reason, w.CreatedBy, fmtTime(w.CreatedAt))
	if err != nil {
		return w, err
	}
	w.ID, err = res.LastInsertId()
	return w, err
}

// Windows lists the windows that have not ended by now, soonest first.
func (s *Store) Windows(ctx context.Context, now time.Time) ([]incident.Window, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, service, starts_at, ends_at, reason, created_by, created_at FROM windows WHERE ends_at > ? ORDER BY starts_at, id`, fmtTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.Window
	for rows.Next() {
		var w incident.Window
		var starts, ends, created string
		if err := rows.Scan(&w.ID, &w.Service, &starts, &ends, &w.Reason, &w.CreatedBy, &created); err != nil {
			return nil, err
		}
		w.StartsAt, w.EndsAt, w.CreatedAt = parseTime(starts), parseTime(ends), parseTime(created)
		out = append(out, w)
	}
	return out, rows.Err()
}

// EndWindow ends a window now.
func (s *Store) EndWindow(ctx context.Context, id int64, now time.Time) error {
	return s.affected(s.db.ExecContext(ctx, `UPDATE windows SET ends_at = ? WHERE id = ? AND ends_at > ?`, fmtTime(now), id, fmtTime(now)))
}

// InWindow says whether a window quiets service at now.
func (s *Store) InWindow(ctx context.Context, service string, now time.Time) (bool, error) {
	ws, err := s.Windows(ctx, now)
	if err != nil {
		return false, err
	}
	for _, w := range ws {
		if w.Covers(service, now) {
			return true, nil
		}
	}
	return false, nil
}

const heartbeatColumns = `name, interval_seconds, grace_seconds, service, severity, last_at, late, created_by, created_at`

func scanHeartbeat(r interface{ Scan(...any) error }) (incident.Heartbeat, error) {
	var h incident.Heartbeat
	var interval, grace int64
	var sev, created string
	var last sql.NullString
	if err := r.Scan(&h.Name, &interval, &grace, &h.Service, &sev, &last, &h.Late, &h.CreatedBy, &created); err != nil {
		return h, err
	}
	h.Interval, h.Grace = time.Duration(interval)*time.Second, time.Duration(grace)*time.Second
	h.Severity, h.LastAt, h.CreatedAt = incident.Severity(sev), parseTimePtr(last), parseTime(created)
	return h, nil
}

// SaveHeartbeat adds a heartbeat or changes its interval, grace, service,
// and severity, keeping when it last checked in.
func (s *Store) SaveHeartbeat(ctx context.Context, h incident.Heartbeat) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO heartbeats (name, interval_seconds, grace_seconds, service, severity, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET interval_seconds = excluded.interval_seconds, grace_seconds = excluded.grace_seconds, service = excluded.service, severity = excluded.severity`,
		h.Name, int64(h.Interval/time.Second), int64(h.Grace/time.Second), h.Service, string(h.Severity), h.CreatedBy, fmtTime(h.CreatedAt))
	return err
}

// Heartbeat reads one heartbeat.
func (s *Store) Heartbeat(ctx context.Context, name string) (incident.Heartbeat, error) {
	h, err := scanHeartbeat(s.db.QueryRowContext(ctx, `SELECT `+heartbeatColumns+` FROM heartbeats WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	return h, err
}

// Heartbeats lists every heartbeat, by name.
func (s *Store) Heartbeats(ctx context.Context) ([]incident.Heartbeat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+heartbeatColumns+` FROM heartbeats ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.Heartbeat
	for rows.Next() {
		h, err := scanHeartbeat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Beat records a check-in and says whether the heartbeat had been late.
func (s *Store) Beat(ctx context.Context, name string, at time.Time) (bool, error) {
	var late bool
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT late FROM heartbeats WHERE name = ?`, name).Scan(&late); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE heartbeats SET last_at = ?, late = 0 WHERE name = ?`, fmtTime(at), name)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return late, err
}

// MarkLate marks a heartbeat late, once; it says whether it was not yet.
func (s *Store) MarkLate(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE heartbeats SET late = 1 WHERE name = ? AND late = 0`, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) DeleteHeartbeat(ctx context.Context, name string) error {
	return s.affected(s.db.ExecContext(ctx, `DELETE FROM heartbeats WHERE name = ?`, name))
}
