package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
)

// querier is *sql.DB or *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const incidentColumns = `i.id, i.title, i.severity, i.state, i.service, i.workload, i.group_key, i.owner, i.source,
 i.auto_resolve, i.public, i.opened_at, i.acknowledged_at, i.mitigated_at, i.resolved_at, i.updated_at, i.last_update,
 (SELECT count(*) FROM alerts a WHERE a.incident_id = i.id AND a.status = 'firing'),
 (SELECT count(*) FROM alerts a WHERE a.incident_id = i.id),
 (SELECT max(a.ends_at) FROM alerts a WHERE a.incident_id = i.id AND a.ends_at IS NOT NULL),
 (SELECT json_extract(s.value, '$.quiet_resolve') FROM settings s WHERE s.key = 'settings')`

func scanIncident(row interface{ Scan(...any) error }) (incident.Incident, error) {
	var i incident.Incident
	var opened, updated string
	var acked, mitigated, resolved, quiet sql.NullString
	var window sql.NullInt64
	var auto, public int
	err := row.Scan(&i.ID, &i.Title, &i.Severity, &i.State, &i.Service, &i.Workload, &i.GroupKey, &i.Owner, &i.Source,
		&auto, &public, &opened, &acked, &mitigated, &resolved, &updated, &i.LastUpdate, &i.FiringAlerts, &i.Alerts, &quiet, &window)
	if err != nil {
		return i, err
	}
	i.AutoResolve = auto == 1
	i.Public = public == 1
	i.OpenedAt = parseTime(opened)
	i.UpdatedAt = parseTime(updated)
	i.AcknowledgedAt = parseTimePtr(acked)
	i.MitigatedAt = parseTimePtr(mitigated)
	i.ResolvedAt = parseTimePtr(resolved)
	if i.FiringAlerts == 0 {
		i.QuietSince = parseTimePtr(quiet)
	}
	// The quiet window is read in the same query, so an incident read
	// inside a transaction says when it closes too. Settings saved before
	// the window existed have none and take the default.
	quietWindow := Defaults().QuietResolve
	if window.Valid {
		quietWindow = time.Duration(window.Int64)
	}
	i.ClosesAt = incident.ClosesAt(i, quietWindow)
	return i, nil
}

// Insert creates an incident and returns it with its id.
func (s *Store) Insert(ctx context.Context, q querier, i incident.Incident) (incident.Incident, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO incidents (title, severity, state, service, workload, group_key, owner, source,
 auto_resolve, public, opened_at, acknowledged_at, mitigated_at, resolved_at, updated_at, last_update)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.Title, i.Severity, i.State, i.Service, i.Workload, i.GroupKey, i.Owner, i.Source,
		b2i(i.AutoResolve), b2i(i.Public), fmtTime(i.OpenedAt), fmtTimePtr(i.AcknowledgedAt), fmtTimePtr(i.MitigatedAt), fmtTimePtr(i.ResolvedAt), fmtTime(i.UpdatedAt), i.LastUpdate)
	if err != nil {
		return i, err
	}
	i.ID, err = res.LastInsertId()
	return i, err
}

// Update writes every mutable column of an incident.
func (s *Store) Update(ctx context.Context, q querier, i incident.Incident) error {
	res, err := q.ExecContext(ctx, `UPDATE incidents SET title=?, severity=?, state=?, service=?, workload=?, group_key=?, owner=?,
 auto_resolve=?, public=?, acknowledged_at=?, mitigated_at=?, resolved_at=?, updated_at=?, last_update=? WHERE id=?`,
		i.Title, i.Severity, i.State, i.Service, i.Workload, i.GroupKey, i.Owner,
		b2i(i.AutoResolve), b2i(i.Public), fmtTimePtr(i.AcknowledgedAt), fmtTimePtr(i.MitigatedAt), fmtTimePtr(i.ResolvedAt), fmtTime(i.UpdatedAt), i.LastUpdate, i.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DB() *sql.DB { return s.db }

// Get reads one incident.
func (s *Store) Get(ctx context.Context, q querier, id int64) (incident.Incident, error) {
	i, err := scanIncident(q.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents i WHERE i.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	}
	return i, err
}

// FindByGroup finds the incident an alert with this key joins: the newest
// unresolved one, or one resolved after `resolvedSince` (the grouping
// window). Nothing found is ErrNotFound.
func (s *Store) FindByGroup(ctx context.Context, q querier, key string, resolvedSince time.Time) (incident.Incident, error) {
	i, err := scanIncident(q.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents i
 WHERE i.group_key = ? AND (i.state != 'resolved' OR i.resolved_at >= ?)
 ORDER BY (i.state != 'resolved') DESC, i.opened_at DESC LIMIT 1`, key, fmtTime(resolvedSince)))
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	}
	return i, err
}

// FindByAlert finds the newest unresolved incident that holds an alert.
func (s *Store) FindByAlert(ctx context.Context, q querier, fingerprint string) (incident.Incident, error) {
	i, err := scanIncident(q.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents i
 WHERE i.state != 'resolved' AND i.id IN (SELECT incident_id FROM alerts WHERE fingerprint = ?)
 ORDER BY i.opened_at DESC LIMIT 1`, fingerprint))
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	}
	return i, err
}

// Filter narrows a list.
type Filter struct {
	// States keeps these states; empty keeps all.
	States   []incident.State
	Service  string
	Severity incident.Severity
	// Search matches the title, service, or owner.
	Search string
	// Since keeps incidents opened at or after it.
	Since time.Time
	// Owner keeps incidents this person owns, ignoring case.
	Owner string
	// Unowned keeps incidents nobody owns.
	Unowned bool
	// NoPostmortem keeps incidents with no postmortem written.
	NoPostmortem bool
	Limit        int
}

// List reads incidents, open ones first by severity and age, then the rest
// newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]incident.Incident, error) {
	var where []string
	var args []any
	if len(f.States) > 0 {
		marks := make([]string, len(f.States))
		for i, st := range f.States {
			marks[i] = "?"
			args = append(args, st)
		}
		where = append(where, "i.state IN ("+strings.Join(marks, ",")+")")
	}
	if f.Service != "" {
		where = append(where, "i.service = ?")
		args = append(args, f.Service)
	}
	if f.Severity != "" {
		where = append(where, "i.severity = ?")
		args = append(args, f.Severity)
	}
	if f.Search != "" {
		like := "%" + strings.ToLower(f.Search) + "%"
		where = append(where, "(lower(i.title) LIKE ? OR lower(i.service) LIKE ? OR lower(i.owner) LIKE ? OR CAST(i.id AS TEXT) = ?)")
		args = append(args, like, like, like, strings.TrimPrefix(f.Search, "#"))
	}
	if !f.Since.IsZero() {
		where = append(where, "i.opened_at >= ?")
		args = append(args, fmtTime(f.Since))
	}
	if f.Owner != "" {
		where = append(where, "lower(i.owner) = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.Owner)))
	}
	if f.Unowned {
		where = append(where, "i.owner = ''")
	}
	if f.NoPostmortem {
		where = append(where, "NOT EXISTS (SELECT 1 FROM postmortems p WHERE p.incident_id = i.id)")
	}
	query := `SELECT ` + incidentColumns + ` FROM incidents i`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += ` ORDER BY (i.state = 'resolved') ASC,
 CASE i.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END ASC, i.opened_at DESC`
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.Incident
	for rows.Next() {
		i, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	// An incident only waiting to close sorts after the ones being worked
	// and before the resolved, keeping the query's order within each.
	sort.SliceStable(out, func(a, b int) bool { return listRank(out[a]) < listRank(out[b]) })
	return out, rows.Err()
}

func listRank(i incident.Incident) int {
	switch {
	case i.State == incident.Resolved:
		return 2
	case i.Settling():
		return 1
	}
	return 0
}

// OpenBySeverity counts unresolved incidents for the gauge.
func (s *Store) OpenBySeverity(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT severity, count(*) FROM incidents WHERE state != 'resolved' GROUP BY severity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var sev string
		var n int
		if err := rows.Scan(&sev, &n); err != nil {
			return nil, err
		}
		out[sev] = n
	}
	return out, rows.Err()
}

// UpsertAlert records an alert's latest state in an incident.
func (s *Store) UpsertAlert(ctx context.Context, q querier, incidentID int64, a incident.Alert) error {
	labels, _ := json.Marshal(a.Labels)
	annotations, _ := json.Marshal(a.Annotations)
	_, err := q.ExecContext(ctx, `INSERT INTO alerts (incident_id, fingerprint, alertname, status, labels, annotations, starts_at, ends_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT(incident_id, fingerprint) DO UPDATE SET status=excluded.status, labels=excluded.labels,
 annotations=excluded.annotations, starts_at=excluded.starts_at, ends_at=excluded.ends_at`,
		incidentID, a.Fingerprint, a.Name, a.Status, string(labels), string(annotations), fmtTime(a.StartsAt), fmtTimePtr(a.EndsAt))
	return err
}

// GetAlert reads one alert of an incident, ErrNotFound when it is new.
func (s *Store) GetAlert(ctx context.Context, q querier, incidentID int64, fingerprint string) (incident.Alert, error) {
	row := q.QueryRowContext(ctx, `SELECT fingerprint, alertname, status, labels, annotations, starts_at, ends_at FROM alerts WHERE incident_id = ? AND fingerprint = ?`, incidentID, fingerprint)
	a, err := scanAlert(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func scanAlert(row interface{ Scan(...any) error }) (incident.Alert, error) {
	var a incident.Alert
	var labels, annotations, starts string
	var ends sql.NullString
	if err := row.Scan(&a.Fingerprint, &a.Name, &a.Status, &labels, &annotations, &starts, &ends); err != nil {
		return a, err
	}
	_ = json.Unmarshal([]byte(labels), &a.Labels)
	_ = json.Unmarshal([]byte(annotations), &a.Annotations)
	a.StartsAt = parseTime(starts)
	a.EndsAt = parseTimePtr(ends)
	return a, nil
}

// Alerts lists an incident's alerts.
func (s *Store) Alerts(ctx context.Context, incidentID int64) ([]incident.Alert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT fingerprint, alertname, status, labels, annotations, starts_at, ends_at FROM alerts WHERE incident_id = ? ORDER BY starts_at`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// FiringCount counts the alerts of an incident still firing.
func (s *Store) FiringCount(ctx context.Context, q querier, incidentID int64) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM alerts WHERE incident_id = ? AND status = 'firing'`, incidentID).Scan(&n)
	return n, err
}

// AddEvent appends a timeline line and makes it the incident's last update.
func (s *Store) AddEvent(ctx context.Context, q querier, e incident.Event) (incident.Event, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO events (incident_id, at, kind, actor, text, public) VALUES (?, ?, ?, ?, ?, ?)`,
		e.IncidentID, fmtTime(e.At), e.Kind, e.Actor, e.Text, b2i(e.Public))
	if err != nil {
		return e, err
	}
	e.ID, _ = res.LastInsertId()
	_, err = q.ExecContext(ctx, `UPDATE incidents SET last_update = ?, updated_at = ? WHERE id = ?`, e.Text, fmtTime(e.At), e.IncidentID)
	return e, err
}

// Events reads an incident's timeline, oldest first.
func (s *Store) Events(ctx context.Context, incidentID int64) ([]incident.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, incident_id, at, kind, actor, text, public FROM events WHERE incident_id = ? ORDER BY id`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.Event
	for rows.Next() {
		var e incident.Event
		var at string
		var public int
		if err := rows.Scan(&e.ID, &e.IncidentID, &at, &e.Kind, &e.Actor, &e.Text, &public); err != nil {
			return nil, err
		}
		e.At = parseTime(at)
		e.Public = public == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// PublicEvents reads the notes marked public across these incidents.
func (s *Store) PublicEvents(ctx context.Context, incidentID int64) ([]incident.Event, error) {
	all, err := s.Events(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	var out []incident.Event
	for _, e := range all {
		if e.Public {
			out = append(out, e)
		}
	}
	return out, nil
}

// Postmortem reads an incident's postmortem, ErrNotFound when none.
func (s *Store) Postmortem(ctx context.Context, incidentID int64) (incident.Postmortem, error) {
	var p incident.Postmortem
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT incident_id, body, updated_at, updated_by FROM postmortems WHERE incident_id = ?`, incidentID).Scan(&p.IncidentID, &p.Body, &at, &p.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.UpdatedAt = parseTime(at)
	return p, err
}

// SavePostmortem writes the postmortem.
func (s *Store) SavePostmortem(ctx context.Context, q querier, p incident.Postmortem) error {
	_, err := q.ExecContext(ctx, `INSERT INTO postmortems (incident_id, body, updated_at, updated_by) VALUES (?, ?, ?, ?)
 ON CONFLICT(incident_id) DO UPDATE SET body=excluded.body, updated_at=excluded.updated_at, updated_by=excluded.updated_by`,
		p.IncidentID, p.Body, fmtTime(p.UpdatedAt), p.UpdatedBy)
	return err
}

// ActionItems lists an incident's action items.
func (s *Store) ActionItems(ctx context.Context, incidentID int64) ([]incident.ActionItem, error) {
	return s.actionItems(ctx, `incident_id = ?`, incidentID)
}

// OpenActionItems lists the action items not done on every incident,
// oldest first; with an owner, only theirs, ignoring case.
func (s *Store) OpenActionItems(ctx context.Context, owner string) ([]incident.ActionItem, error) {
	if owner = strings.TrimSpace(owner); owner != "" {
		return s.actionItems(ctx, `done = 0 AND lower(owner) = ?`, strings.ToLower(owner))
	}
	return s.actionItems(ctx, `done = 0`)
}

func (s *Store) actionItems(ctx context.Context, where string, args ...any) ([]incident.ActionItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, incident_id, text, owner, done, created_at, issue_url FROM action_items WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.ActionItem
	for rows.Next() {
		var a incident.ActionItem
		var at string
		var done int
		if err := rows.Scan(&a.ID, &a.IncidentID, &a.Text, &a.Owner, &done, &at, &a.IssueURL); err != nil {
			return nil, err
		}
		a.Done = done == 1
		a.CreatedAt = parseTime(at)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddActionItem(ctx context.Context, q querier, a incident.ActionItem) (incident.ActionItem, error) {
	res, err := q.ExecContext(ctx, `INSERT INTO action_items (incident_id, text, owner, done, created_at) VALUES (?, ?, ?, 0, ?)`, a.IncidentID, a.Text, a.Owner, fmtTime(a.CreatedAt))
	if err != nil {
		return a, err
	}
	a.ID, _ = res.LastInsertId()
	return a, nil
}

func (s *Store) SetActionDone(ctx context.Context, q querier, incidentID, id int64, done bool) error {
	res, err := q.ExecContext(ctx, `UPDATE action_items SET done = ? WHERE id = ? AND incident_id = ?`, b2i(done), id, incidentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
