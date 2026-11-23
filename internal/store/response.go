package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
)

// SetRole gives a person a role on an incident, or takes it away with "".
func (s *Store) SetRole(ctx context.Context, q querier, id int64, role, person string) error {
	if person == "" {
		_, err := q.ExecContext(ctx, `DELETE FROM incident_roles WHERE incident_id = ? AND role = ?`, id, role)
		return err
	}
	_, err := q.ExecContext(ctx, `INSERT INTO incident_roles (incident_id, role, person) VALUES (?, ?, ?) ON CONFLICT (incident_id, role) DO UPDATE SET person = excluded.person`, id, role, person)
	return err
}

// Roles is who holds each role on an incident.
func (s *Store) Roles(ctx context.Context, id int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, person FROM incident_roles WHERE incident_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var role, person string
		if err := rows.Scan(&role, &person); err != nil {
			return nil, err
		}
		out[role] = person
	}
	return out, rows.Err()
}

// AddLink ties one incident to another.
func (s *Store) AddLink(ctx context.Context, q querier, from, to int64, kind, by string, at time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO incident_links (from_id, to_id, kind, by_whom, at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (from_id, to_id) DO UPDATE SET kind = excluded.kind`, from, to, kind, by, fmtTime(at))
	return err
}

// Links lists the incidents tied to one, either way.
func (s *Store) Links(ctx context.Context, id int64) ([]incident.Link, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.to_id, i.title, l.kind, l.by_whom, l.at, 1 FROM incident_links l JOIN incidents i ON i.id = l.to_id WHERE l.from_id = ?
UNION ALL SELECT l.from_id, i.title, l.kind, l.by_whom, l.at, 0 FROM incident_links l JOIN incidents i ON i.id = l.from_id WHERE l.to_id = ? ORDER BY 5`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.Link
	for rows.Next() {
		var l incident.Link
		var at string
		var outgoing bool
		if err := rows.Scan(&l.ID, &l.Title, &l.Kind, &l.By, &at, &outgoing); err != nil {
			return nil, err
		}
		l.At, l.Into = parseTime(at), outgoing && l.Kind == "merged"
		out = append(out, l)
	}
	return out, rows.Err()
}

// MoveAlerts moves an incident's alerts to another, keeping the other's
// copy of any alert both hold.
func (s *Store) MoveAlerts(ctx context.Context, q querier, from, to int64) error {
	if _, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO alerts (incident_id, fingerprint, alertname, status, labels, annotations, starts_at, ends_at)
SELECT ?, fingerprint, alertname, status, labels, annotations, starts_at, ends_at FROM alerts WHERE incident_id = ?`, to, from); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `DELETE FROM alerts WHERE incident_id = ?`, from)
	return err
}

// Subscribe adds an address to the status page's followers, unconfirmed,
// and answers its token; an address already there keeps its token.
func (s *Store) Subscribe(ctx context.Context, email string, at time.Time) (string, bool, error) {
	var token string
	var confirmed bool
	err := s.db.QueryRowContext(ctx, `SELECT token, confirmed FROM status_subscribers WHERE email = ?`, email).Scan(&token, &confirmed)
	if err == nil {
		return token, confirmed, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM status_subscribers`).Scan(&n); err != nil {
		return "", false, err
	}
	if n >= 5000 {
		return "", false, errors.New("the status page has as many followers as it takes")
	}
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", false, err
	}
	token = hex.EncodeToString(b)
	_, err = s.db.ExecContext(ctx, `INSERT INTO status_subscribers (email, token, created_at) VALUES (?, ?, ?)`, email, token, fmtTime(at))
	return token, false, err
}

// Confirm confirms the follower a token names and says their address.
func (s *Store) Confirm(ctx context.Context, token string) (string, error) {
	var email string
	err := s.db.QueryRowContext(ctx, `UPDATE status_subscribers SET confirmed = 1 WHERE token = ? RETURNING email`, token).Scan(&email)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return email, err
}

// Unsubscribe removes the follower a token names.
func (s *Store) Unsubscribe(ctx context.Context, token string) error {
	return s.affected(s.db.ExecContext(ctx, `DELETE FROM status_subscribers WHERE token = ?`, token))
}

// Follower is a confirmed follower of the status page.
type Follower struct {
	Email string
	Token string
}

// Followers lists the confirmed followers.
func (s *Store) Followers(ctx context.Context) ([]Follower, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email, token FROM status_subscribers WHERE confirmed = 1 ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Follower
	for rows.Next() {
		var f Follower
		if err := rows.Scan(&f.Email, &f.Token); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetIssue keeps the issue that tracks an action item.
func (s *Store) SetIssue(ctx context.Context, id int64, url string) error {
	return s.affected(s.db.ExecContext(ctx, `UPDATE action_items SET issue_url = ? WHERE id = ?`, url, id))
}

// OpenIssues lists action items not done that have an issue.
func (s *Store) OpenIssues(ctx context.Context) ([]incident.ActionItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, incident_id, text, issue_url FROM action_items WHERE done = 0 AND issue_url <> '' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []incident.ActionItem
	for rows.Next() {
		var a incident.ActionItem
		if err := rows.Scan(&a.ID, &a.IncidentID, &a.Text, &a.IssueURL); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetTeamsChannel keeps an incident's Teams channel, "team-id/channel-id".
func (s *Store) SetTeamsChannel(ctx context.Context, id int64, channel string) error {
	return s.affected(s.db.ExecContext(ctx, `UPDATE incidents SET teams_channel = ? WHERE id = ?`, channel, id))
}

// TeamsChannel is an incident's Teams channel, or "".
func (s *Store) TeamsChannel(ctx context.Context, id int64) (string, error) {
	var ch string
	err := s.db.QueryRowContext(ctx, `SELECT teams_channel FROM incidents WHERE id = ?`, id).Scan(&ch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return ch, err
}
