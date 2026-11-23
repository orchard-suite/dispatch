// Package store keeps incidents, their alerts, timelines, postmortems,
// action items, API tokens, and settings in one SQLite file in WAL mode.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and brings its schema up.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// One writer at a time keeps SQLite's locking out of the handlers.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS incidents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  title TEXT NOT NULL,
  severity TEXT NOT NULL,
  state TEXT NOT NULL,
  service TEXT NOT NULL DEFAULT '',
  workload TEXT NOT NULL DEFAULT '',
  group_key TEXT NOT NULL DEFAULT '',
  owner TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL,
  auto_resolve INTEGER NOT NULL DEFAULT 0,
  public INTEGER NOT NULL DEFAULT 0,
  opened_at TEXT NOT NULL,
  acknowledged_at TEXT,
  mitigated_at TEXT,
  resolved_at TEXT,
  updated_at TEXT NOT NULL,
  last_update TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS incidents_state ON incidents(state, opened_at);
CREATE INDEX IF NOT EXISTS incidents_group ON incidents(group_key, opened_at);
CREATE TABLE IF NOT EXISTS alerts (
  incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  fingerprint TEXT NOT NULL,
  alertname TEXT NOT NULL,
  status TEXT NOT NULL,
  labels TEXT NOT NULL,
  annotations TEXT NOT NULL,
  starts_at TEXT NOT NULL,
  ends_at TEXT,
  PRIMARY KEY (incident_id, fingerprint)
);
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  at TEXT NOT NULL,
  kind TEXT NOT NULL,
  actor TEXT NOT NULL,
  text TEXT NOT NULL,
  public INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS events_incident ON events(incident_id, id);
CREATE TABLE IF NOT EXISTS postmortems (
  incident_id INTEGER PRIMARY KEY REFERENCES incidents(id) ON DELETE CASCADE,
  body TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  updated_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS action_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  incident_id INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
  text TEXT NOT NULL,
  owner TEXT NOT NULL DEFAULT '',
  done INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  hash TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  created_by TEXT NOT NULL,
  last_used_at TEXT
);
CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
` + noticeSchema + botSchema + directorySchema)
	if err != nil {
		return err
	}
	return s.upgrade()
}

// Tx runs fn in one transaction.
func (s *Store) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

const timeLayout = time.RFC3339Nano

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(timeLayout, s)
	return t
}

func parseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTime(s.String)
	return &t
}
