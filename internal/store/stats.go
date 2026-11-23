package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// PageStat is one page as the numbers see it: the service its event was
// about, when it started, how it ended, and who it told.
type PageStat struct {
	ID        int64
	Service   string
	StartedAt time.Time
	State     notice.PageState
	AckBy     string
	Told      []string
}

// PageStats lists the pages started in [since, until).
func (s *Store) PageStats(ctx context.Context, since, until time.Time) ([]PageStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, COALESCE(json_extract(e.body, '$.labels.service'), ''), p.started_at, p.state, p.acknowledged_by,
  COALESCE((SELECT json_group_array(DISTINCT m.person) FROM messages m WHERE m.page_id = p.id AND m.state = 'sent'), '[]')
FROM pages p JOIN notice_events e ON e.id = p.event_id WHERE p.started_at >= ? AND p.started_at < ? ORDER BY p.id`, fmtTime(since), fmtTime(until))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PageStat
	for rows.Next() {
		var p PageStat
		var started, state, told string
		if err := rows.Scan(&p.ID, &p.Service, &started, &state, &p.AckBy, &told); err != nil {
			return nil, err
		}
		p.StartedAt, p.State = parseTime(started), notice.PageState(state)
		_ = json.Unmarshal([]byte(told), &p.Told)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PagesOf counts the pages that told a person since a time.
func (s *Store) PagesOf(ctx context.Context, person string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(DISTINCT page_id) FROM messages WHERE person = ? AND page_id <> 0 AND state = 'sent' AND sent_at >= ?`, person, fmtTime(since)).Scan(&n)
	return n, err
}

// Noise is one alert name's record: how often it fired, how often it
// paged, and how often a person acted on what it opened.
type Noise struct {
	Alert string `json:"alert"`
	Fired int    `json:"fired"`
	Paged int    `json:"paged"`
	Acted int    `json:"acted"`
}

// NoiseSince is every alert name that fired since a time, the loudest
// for the least action first.
func (s *Store) NoiseSince(ctx context.Context, since time.Time) ([]Noise, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH fired AS (
  SELECT json_extract(body, '$.labels.alertname') AS alert, count(*) AS n FROM notice_events
   WHERE kind = 'alert.firing' AND received_at >= ? GROUP BY 1
), paged AS (
  SELECT a.alertname AS alert, count(DISTINCT i.id) AS n, count(DISTINCT CASE WHEN i.acknowledged_at IS NOT NULL OR i.owner <> '' THEN i.id END) AS acted
    FROM alerts a JOIN incidents i ON i.id = a.incident_id WHERE i.opened_at >= ? GROUP BY 1
), names AS (SELECT alert FROM fired UNION SELECT alert FROM paged)
SELECT names.alert, COALESCE(fired.n, 0) + COALESCE(paged.n, 0), COALESCE(paged.n, 0), COALESCE(paged.acted, 0)
  FROM names LEFT JOIN fired ON fired.alert = names.alert LEFT JOIN paged ON paged.alert = names.alert
 WHERE names.alert IS NOT NULL
 ORDER BY (COALESCE(fired.n, 0) + COALESCE(paged.n, 0) - COALESCE(paged.acted, 0)) DESC, names.alert LIMIT 50`, fmtTime(since), fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Noise
	for rows.Next() {
		var n Noise
		if err := rows.Scan(&n.Alert, &n.Fired, &n.Paged, &n.Acted); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Firing is one moment an incident's alert fired, or the incident opened or
// reopened, which an alert firing does.
type Firing struct {
	IncidentID int64
	At         time.Time
	Kind       incident.EventKind
	// Text is the timeline line, which names the alert.
	Text string
}

// FiringsSince is every firing since a time, oldest first.
func (s *Store) FiringsSince(ctx context.Context, since time.Time) ([]Firing, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT incident_id, at, kind, text FROM events
 WHERE kind IN ('opened', 'reopened', 'alert_firing') AND at >= ? ORDER BY at`, fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Firing
	for rows.Next() {
		var f Firing
		var at string
		if err := rows.Scan(&f.IncidentID, &at, &f.Kind, &f.Text); err != nil {
			return nil, err
		}
		f.At = parseTime(at)
		out = append(out, f)
	}
	return out, rows.Err()
}

// AlertLabelsSince is the labels of every alert that reached the desk since
// a time: each notice of one firing, and each alert an incident holds.
// Settings counts what a route would catch with it.
func (s *Store) AlertLabelsSince(ctx context.Context, since time.Time) ([]map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT json_extract(body, '$.labels') FROM notice_events WHERE kind = 'alert.firing' AND received_at >= ?
UNION ALL
SELECT a.labels FROM alerts a JOIN incidents i ON i.id = a.incident_id WHERE a.starts_at >= ?`, fmtTime(since), fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m map[string]string
		if raw.Valid && json.Unmarshal([]byte(raw.String), &m) == nil && len(m) > 0 {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}
