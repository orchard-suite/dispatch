package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

const directorySchema = `
CREATE TABLE IF NOT EXISTS people (
  email TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  via TEXT NOT NULL DEFAULT '[]',
  teams_id TEXT NOT NULL DEFAULT '',
  learned INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS groups (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS group_members (
  group_slug TEXT NOT NULL REFERENCES groups(slug) ON DELETE CASCADE,
  email TEXT NOT NULL,
  PRIMARY KEY (group_slug, email)
);
CREATE TABLE IF NOT EXISTS chains (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  steps TEXT NOT NULL
);
`

// upgrades are the schema changes past the CREATE statements, in order;
// PRAGMA user_version counts how many have run.
var upgrades = []string{
	// 1: a subscription sends to a channel or to a person, group, or chain.
	`CREATE TABLE subscriptions_next (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id INTEGER REFERENCES channels(id) ON DELETE CASCADE,
  target TEXT NOT NULL DEFAULT '',
  topics TEXT NOT NULL,
  producers TEXT NOT NULL DEFAULT '[]',
  anchor TEXT NOT NULL DEFAULT '',
  match TEXT NOT NULL DEFAULT '{}',
  paused INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  created_by TEXT NOT NULL
);
INSERT INTO subscriptions_next (id, channel_id, topics, producers, anchor, match, paused, created_at, created_by)
  SELECT id, channel_id, topics, producers, anchor, match, paused, created_at, created_by FROM subscriptions;
DROP TABLE subscriptions;
ALTER TABLE subscriptions_next RENAME TO subscriptions;
CREATE TABLE pages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL REFERENCES notice_events(id) ON DELETE CASCADE,
  chain TEXT NOT NULL,
  steps TEXT NOT NULL,
  step INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL,
  next_at TEXT NOT NULL,
  started_at TEXT NOT NULL,
  acknowledged_at TEXT,
  acknowledged_by TEXT NOT NULL DEFAULT '',
  ended_at TEXT
);
CREATE INDEX pages_due ON pages(state, next_at);
CREATE TABLE messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL REFERENCES notice_events(id) ON DELETE CASCADE,
  person TEXT NOT NULL,
  medium TEXT NOT NULL,
  page_id INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at TEXT NOT NULL,
  status INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  sent_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE (event_id, person, medium, page_id)
);
CREATE INDEX messages_due ON messages(state, next_at);`,
	// 2: a person keeps their own SMS number, Discord account, rules by
	// weight, and quiet hours; the media they took become their rules.
	// Threads hold a producer's events under one dedup key, and the inbox
	// is every event that reached a person, whatever its weight.
	`ALTER TABLE people ADD COLUMN phone TEXT NOT NULL DEFAULT '';
ALTER TABLE people ADD COLUMN discord_id TEXT NOT NULL DEFAULT '';
ALTER TABLE people ADD COLUMN rules TEXT NOT NULL DEFAULT '';
ALTER TABLE people ADD COLUMN quiet TEXT NOT NULL DEFAULT '';
UPDATE people SET rules = json_object(
  'page', json((SELECT json_group_array(json_object('medium', value, 'after_minutes', 0)) FROM json_each(people.via))),
  'notify', json((SELECT json_group_array(json_object('medium', value, 'after_minutes', 0)) FROM json_each(people.via))))
  WHERE via <> '[]';
CREATE TABLE threads (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  producer TEXT NOT NULL,
  dedup_key TEXT NOT NULL,
  state TEXT NOT NULL,
  title TEXT NOT NULL,
  events INTEGER NOT NULL DEFAULT 1,
  opened_at TEXT NOT NULL,
  acknowledged_at TEXT,
  acknowledged_by TEXT NOT NULL DEFAULT '',
  resolved_at TEXT
);
CREATE INDEX threads_key ON threads(producer, dedup_key, state);
CREATE TABLE inbox (
  event_id INTEGER NOT NULL REFERENCES notice_events(id) ON DELETE CASCADE,
  person TEXT NOT NULL,
  weight TEXT NOT NULL,
  at TEXT NOT NULL,
  PRIMARY KEY (event_id, person)
);
CREATE INDEX inbox_person ON inbox(person, at);`,
	// 3: a message keeps its receipt chain past sent: when the far end said
	// the person's device has it, when they opened its link, and the far
	// end's own words.
	`ALTER TABLE messages ADD COLUMN delivered_at TEXT;
ALTER TABLE messages ADD COLUMN opened_at TEXT;
ALTER TABLE messages ADD COLUMN receipt TEXT NOT NULL DEFAULT '';
CREATE INDEX messages_page ON messages(page_id, person);`,
	// 4: a webhook channel signs what it posts, with a sealed secret and,
	// while a receiver changes over, the one being retired.
	`ALTER TABLE channels ADD COLUMN secret TEXT NOT NULL DEFAULT '';
ALTER TABLE channels ADD COLUMN old_secret TEXT NOT NULL DEFAULT '';`,
	// 5: maintenance windows quiet a service for a while, heartbeats are
	// the check-ins whose silence is an alert, and changes are the deploys
	// a service's incidents show beside them.
	`CREATE TABLE windows (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  service TEXT NOT NULL,
  starts_at TEXT NOT NULL,
  ends_at TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX windows_ends ON windows(ends_at);
CREATE TABLE heartbeats (
  name TEXT PRIMARY KEY,
  interval_seconds INTEGER NOT NULL,
  grace_seconds INTEGER NOT NULL DEFAULT 0,
  service TEXT NOT NULL DEFAULT '',
  severity TEXT NOT NULL DEFAULT 'critical',
  last_at TEXT,
  late INTEGER NOT NULL DEFAULT 0,
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE changes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  producer TEXT NOT NULL,
  service TEXT NOT NULL,
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  url TEXT NOT NULL DEFAULT '',
  by_whom TEXT NOT NULL DEFAULT '',
  at TEXT NOT NULL
);
CREATE INDEX changes_service ON changes(service, at);`,
	// 6: schedules and their overrides, who each last had on call and the
	// day it last told of a gap, a person's calendar link, a chain's
	// repeats, a page's round and a message's, and the reminders a snooze
	// leaves.
	`CREATE TABLE schedules (
  slug TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  zone TEXT NOT NULL,
  layers TEXT NOT NULL
);
CREATE TABLE overrides (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  schedule TEXT NOT NULL REFERENCES schedules(slug) ON DELETE CASCADE,
  person TEXT NOT NULL,
  from_at TEXT NOT NULL,
  to_at TEXT NOT NULL,
  by_whom TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX overrides_schedule ON overrides(schedule, to_at);
CREATE TABLE schedule_watch (
  slug TEXT PRIMARY KEY REFERENCES schedules(slug) ON DELETE CASCADE,
  person TEXT NOT NULL DEFAULT '',
  since TEXT NOT NULL,
  gap_told TEXT NOT NULL DEFAULT ''
);
ALTER TABLE people ADD COLUMN calendar TEXT NOT NULL DEFAULT '';
ALTER TABLE chains ADD COLUMN repeat INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pages ADD COLUMN round INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pages ADD COLUMN repeat INTEGER NOT NULL DEFAULT 0;
CREATE TABLE reminders (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  person TEXT NOT NULL,
  due_at TEXT NOT NULL,
  done INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX reminders_due ON reminders(done, due_at);
CREATE TABLE messages_next (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL REFERENCES notice_events(id) ON DELETE CASCADE,
  person TEXT NOT NULL,
  medium TEXT NOT NULL,
  page_id INTEGER NOT NULL DEFAULT 0,
  round INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at TEXT NOT NULL,
  status INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  sent_at TEXT,
  created_at TEXT NOT NULL,
  delivered_at TEXT,
  opened_at TEXT,
  receipt TEXT NOT NULL DEFAULT '',
  UNIQUE (event_id, person, medium, page_id, round)
);
INSERT INTO messages_next (id, event_id, person, medium, page_id, state, attempts, next_at, status, last_error, sent_at, created_at, delivered_at, opened_at, receipt)
  SELECT id, event_id, person, medium, page_id, state, attempts, next_at, status, last_error, sent_at, created_at, delivered_at, opened_at, receipt FROM messages;
DROP TABLE messages;
ALTER TABLE messages_next RENAME TO messages;
CREATE INDEX messages_due ON messages(state, next_at);
CREATE INDEX messages_page ON messages(page_id, person);`,
	// 7: who holds each role on an incident, incidents linked to or merged
	// into each other, the people who follow the status page by email, an
	// action item's issue, and an incident's Teams channel.
	`CREATE TABLE incident_roles (
  incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  role TEXT NOT NULL,
  person TEXT NOT NULL,
  PRIMARY KEY (incident_id, role)
);
CREATE TABLE incident_links (
  from_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  to_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  by_whom TEXT NOT NULL,
  at TEXT NOT NULL,
  PRIMARY KEY (from_id, to_id)
);
CREATE TABLE status_subscribers (
  email TEXT PRIMARY KEY,
  token TEXT NOT NULL UNIQUE,
  confirmed INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
ALTER TABLE action_items ADD COLUMN issue_url TEXT NOT NULL DEFAULT '';
ALTER TABLE incidents ADD COLUMN teams_channel TEXT NOT NULL DEFAULT '';`,
	// 8: what the assistant wrote about an incident, and the evidence it
	// read; and tokens issued to one person, which act as them.
	`CREATE TABLE assist_notes (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  state TEXT NOT NULL,
  text TEXT NOT NULL DEFAULT '',
  evidence TEXT NOT NULL DEFAULT '[]',
  asked_by TEXT NOT NULL,
  asked_at TEXT NOT NULL
);
CREATE INDEX assist_incident ON assist_notes(incident_id, id);
ALTER TABLE tokens ADD COLUMN person TEXT NOT NULL DEFAULT '';`,
	// 9: the kinds of event each producer declared it sends, and how each
	// person chose to be told of a kind: a list of media, empty for their
	// list only.
	`CREATE TABLE event_kinds (
  kind TEXT PRIMARY KEY,
  producer TEXT NOT NULL,
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  weight TEXT NOT NULL,
  declared_at TEXT NOT NULL
);
CREATE INDEX event_kinds_producer ON event_kinds(producer, kind);
CREATE TABLE person_kinds (
  email TEXT NOT NULL REFERENCES people(email) ON DELETE CASCADE,
  kind TEXT NOT NULL REFERENCES event_kinds(kind) ON DELETE CASCADE,
  media TEXT NOT NULL,
  set_at TEXT NOT NULL,
  PRIMARY KEY (email, kind)
);`,
	// 10: when a person agreed on Me to be texted at their SMS number.
	`ALTER TABLE people ADD COLUMN sms_consent_at TEXT NOT NULL DEFAULT '';`,
	// 11: a change names the catalog entities it touched, the run that made
	// it, and its record (the sender and key of its change.stated event),
	// and carries what its change events said; the events themselves are
	// kept once each by sender and key.
	`ALTER TABLE changes ADD COLUMN run TEXT NOT NULL DEFAULT '';
ALTER TABLE changes ADD COLUMN record_sender TEXT NOT NULL DEFAULT '';
ALTER TABLE changes ADD COLUMN record_key TEXT NOT NULL DEFAULT '';
ALTER TABLE changes ADD COLUMN digest TEXT NOT NULL DEFAULT '';
ALTER TABLE changes ADD COLUMN effects TEXT NOT NULL DEFAULT '[]';
ALTER TABLE changes ADD COLUMN route TEXT NOT NULL DEFAULT '';
ALTER TABLE changes ADD COLUMN outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE changes ADD COLUMN ended_at TEXT;
CREATE UNIQUE INDEX changes_record ON changes(record_sender, record_key) WHERE record_key <> '';
CREATE INDEX changes_at ON changes(at);
CREATE TABLE change_things (
  change_id INTEGER NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
  blueprint TEXT NOT NULL,
  key TEXT NOT NULL,
  PRIMARY KEY (change_id, blueprint, key)
);
CREATE INDEX change_things_entity ON change_things(blueprint, key);
CREATE TABLE change_events (
  sender TEXT NOT NULL,
  key TEXT NOT NULL,
  event TEXT NOT NULL,
  of_sender TEXT NOT NULL DEFAULT '',
  of_key TEXT NOT NULL DEFAULT '',
  change_id INTEGER REFERENCES changes(id) ON DELETE CASCADE,
  at TEXT NOT NULL,
  body TEXT NOT NULL,
  received_at TEXT NOT NULL,
  PRIMARY KEY (sender, key)
);
CREATE INDEX change_events_of ON change_events(of_sender, of_key);
CREATE INDEX change_events_change ON change_events(change_id, at);`,
	// 12: a change being watched, from when it ended until its window
	// closes, and what the watch saw; each observation saved to be handed
	// on to Annals and to the catalog until each takes it; and an end whose
	// statement never came, once it has been told.
	`CREATE TABLE watches (
  change_id INTEGER PRIMARY KEY REFERENCES changes(id) ON DELETE CASCADE,
  state TEXT NOT NULL,
  from_at TEXT NOT NULL,
  due_at TEXT NOT NULL,
  verdict TEXT NOT NULL DEFAULT '',
  sights TEXT NOT NULL DEFAULT '[]',
  incidents TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL,
  done_at TEXT
);
CREATE INDEX watches_due ON watches(state, due_at);
CREATE TABLE change_sends (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  change_id INTEGER NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
  target TEXT NOT NULL,
  body TEXT NOT NULL,
  state TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  sent_at TEXT,
  UNIQUE (change_id, target)
);
CREATE INDEX change_sends_due ON change_sends(state, next_at);
ALTER TABLE change_events ADD COLUMN told INTEGER NOT NULL DEFAULT 0;`,
}

// upgrade runs the upgrades this database has not had, each in its own
// transaction with the version it brings.
func (s *Store) upgrade() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(upgrades); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(upgrades[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upgrade %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// LearnPerson keeps someone who signed in to the desk, once.
func (s *Store) LearnPerson(ctx context.Context, email string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO people (email, learned, created_at) VALUES (?, 1, ?) ON CONFLICT (email) DO NOTHING`, email, fmtTime(at))
	return err
}

// SavePerson adds a person or changes their name, addresses, consent to
// texts, rules, and quiet hours.
func (s *Store) SavePerson(ctx context.Context, p notice.Person) error {
	consent := ""
	if p.SMSConsentAt != nil {
		consent = fmtTime(*p.SMSConsentAt)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO people (email, name, phone, sms_consent_at, discord_id, rules, quiet, learned, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)
ON CONFLICT (email) DO UPDATE SET name = excluded.name, phone = excluded.phone, sms_consent_at = excluded.sms_consent_at, discord_id = excluded.discord_id, rules = excluded.rules, quiet = excluded.quiet`,
		p.Email, p.Name, p.Phone, consent, p.DiscordID, rulesJSON(p.Rules), quietJSON(p.Quiet), fmtTime(p.CreatedAt))
	return err
}

func rulesJSON(r notice.Rules) string {
	if r == nil {
		return ""
	}
	return jsonOf(r)
}

func quietJSON(q *notice.Quiet) string {
	if q == nil {
		return ""
	}
	return jsonOf(q)
}

// SetTeamsID caches a person's Teams account.
func (s *Store) SetTeamsID(ctx context.Context, email, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE people SET teams_id = ? WHERE email = ?`, id, email)
	return err
}

func (s *Store) DeletePerson(ctx context.Context, email string) error {
	return s.affected(s.db.ExecContext(ctx, `DELETE FROM people WHERE email = ?`, email))
}

const personColumns = `email, name, phone, sms_consent_at, discord_id, rules, quiet, teams_id, learned, created_at`

func scanPerson(r interface{ Scan(...any) error }) (notice.Person, error) {
	var p notice.Person
	var consent, rules, quiet, at string
	if err := r.Scan(&p.Email, &p.Name, &p.Phone, &consent, &p.DiscordID, &rules, &quiet, &p.TeamsID, &p.Learned, &at); err != nil {
		return p, err
	}
	if consent != "" {
		t := parseTime(consent)
		p.SMSConsentAt = &t
	}
	if rules != "" {
		_ = json.Unmarshal([]byte(rules), &p.Rules)
	}
	if quiet != "" {
		p.Quiet = &notice.Quiet{}
		if json.Unmarshal([]byte(quiet), p.Quiet) != nil {
			p.Quiet = nil
		}
	}
	p.CreatedAt = parseTime(at)
	return p, nil
}

// PersonByPhone reads the person whose SMS number is phone.
func (s *Store) PersonByPhone(ctx context.Context, phone string) (notice.Person, error) {
	p, err := scanPerson(s.db.QueryRowContext(ctx, `SELECT `+personColumns+` FROM people WHERE phone = ? AND phone <> ''`, phone))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// PersonByDiscord reads the person whose Discord account is id.
func (s *Store) PersonByDiscord(ctx context.Context, id string) (notice.Person, error) {
	p, err := scanPerson(s.db.QueryRowContext(ctx, `SELECT `+personColumns+` FROM people WHERE discord_id = ? AND discord_id <> ''`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// Person reads one person.
func (s *Store) Person(ctx context.Context, email string) (notice.Person, error) {
	p, err := scanPerson(s.db.QueryRowContext(ctx, `SELECT `+personColumns+` FROM people WHERE email = ?`, email))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// People lists everyone, by address.
func (s *Store) People(ctx context.Context) ([]notice.Person, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+personColumns+` FROM people ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notice.Person
	for rows.Next() {
		p, err := scanPerson(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveGroup adds a group or replaces its name and members.
func (s *Store) SaveGroup(ctx context.Context, g notice.Group) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO groups (slug, name) VALUES (?, ?) ON CONFLICT (slug) DO UPDATE SET name = excluded.name`, g.Slug, g.Name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_slug = ?`, g.Slug); err != nil {
			return err
		}
		for _, m := range g.Members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO group_members (group_slug, email) VALUES (?, ?) ON CONFLICT DO NOTHING`, g.Slug, m); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DeleteGroup(ctx context.Context, slug string) error {
	return s.affected(s.db.ExecContext(ctx, `DELETE FROM groups WHERE slug = ?`, slug))
}

// Group reads one group with its members.
func (s *Store) Group(ctx context.Context, slug string) (notice.Group, error) {
	g := notice.Group{Slug: slug}
	err := s.db.QueryRowContext(ctx, `SELECT name FROM groups WHERE slug = ?`, slug).Scan(&g.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT email FROM group_members WHERE group_slug = ? ORDER BY email`, slug)
	if err != nil {
		return g, err
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return g, err
		}
		g.Members = append(g.Members, m)
	}
	return g, rows.Err()
}

// Groups lists every group with its members.
func (s *Store) Groups(ctx context.Context) ([]notice.Group, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slug FROM groups ORDER BY slug`)
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
	out := make([]notice.Group, 0, len(slugs))
	for _, slug := range slugs {
		g, err := s.Group(ctx, slug)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// SaveChain adds a chain or replaces its name and steps.
func (s *Store) SaveChain(ctx context.Context, c notice.Chain) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chains (slug, name, steps, repeat) VALUES (?, ?, ?, ?) ON CONFLICT (slug) DO UPDATE SET name = excluded.name, steps = excluded.steps, repeat = excluded.repeat`,
		c.Slug, c.Name, jsonOf(c.Steps), c.Repeat)
	return err
}

func (s *Store) DeleteChain(ctx context.Context, slug string) error {
	return s.affected(s.db.ExecContext(ctx, `DELETE FROM chains WHERE slug = ?`, slug))
}

// Chain reads one chain.
func (s *Store) Chain(ctx context.Context, slug string) (notice.Chain, error) {
	c := notice.Chain{Slug: slug}
	var steps string
	err := s.db.QueryRowContext(ctx, `SELECT name, steps, repeat FROM chains WHERE slug = ?`, slug).Scan(&c.Name, &steps, &c.Repeat)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	_ = json.Unmarshal([]byte(steps), &c.Steps)
	return c, err
}

// Chains lists every chain.
func (s *Store) Chains(ctx context.Context) ([]notice.Chain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT slug, name, steps, repeat FROM chains ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notice.Chain
	for rows.Next() {
		var c notice.Chain
		var steps string
		if err := rows.Scan(&c.Slug, &c.Name, &steps, &c.Repeat); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(steps), &c.Steps)
		out = append(out, c)
	}
	return out, rows.Err()
}
