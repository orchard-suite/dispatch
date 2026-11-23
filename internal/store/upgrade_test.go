package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/orchard-suite/dispatch/internal/notice"
)

// A database from before people, groups, and chains keeps its subscriptions.
func TestUpgradeKeepsOldSubscriptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`
CREATE TABLE channels (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE, kind TEXT NOT NULL, target TEXT NOT NULL, created_at TEXT NOT NULL, created_by TEXT NOT NULL);
CREATE TABLE subscriptions (id INTEGER PRIMARY KEY AUTOINCREMENT, channel_id INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE, topics TEXT NOT NULL, producers TEXT NOT NULL DEFAULT '[]', anchor TEXT NOT NULL DEFAULT '', match TEXT NOT NULL DEFAULT '{}', paused INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, created_by TEXT NOT NULL);
INSERT INTO channels (name, kind, target, created_at, created_by) VALUES ('ops', 'teams', 'sealed', '2026-11-01T00:00:00Z', 'admin@example.com');
INSERT INTO subscriptions (channel_id, topics, match, paused, created_at, created_by) VALUES (1, '["incident.*"]', '{"env":"Prod"}', 1, '2026-11-01T00:00:00Z', 'admin@example.com');`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	subs, err := st.Subscriptions(ctx)
	if err != nil || len(subs) != 1 {
		t.Fatalf("subscriptions %v %v", subs, err)
	}
	s := subs[0]
	if s.ChannelID != 1 || s.Target != nil || !s.Paused || s.Match["env"] != "Prod" || s.Topics[0] != "incident.*" {
		t.Errorf("kept %+v", s)
	}
	added, err := st.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Chain: "oncall"}, Topics: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	subs, _ = st.Subscriptions(ctx)
	if subs[1].ID != added.ID || subs[1].ChannelID != 0 || subs[1].Target == nil || subs[1].Target.Chain != "oncall" {
		t.Errorf("new %+v", subs[1])
	}
	// Opening again runs nothing twice.
	st.Close()
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if subs, _ := st.Subscriptions(ctx); len(subs) != 2 {
		t.Errorf("after reopening: %d subscriptions", len(subs))
	}
	// Deleting the channel still takes its subscription.
	if err := st.DeleteChannel(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if subs, _ := st.Subscriptions(ctx); len(subs) != 1 {
		t.Errorf("after deleting the channel: %d subscriptions", len(subs))
	}
}

// The media a person took become their page and notify rules.
func TestUpgradeTurnsMediaIntoRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`
CREATE TABLE people (email TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', via TEXT NOT NULL DEFAULT '[]', teams_id TEXT NOT NULL DEFAULT '', learned INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL);
INSERT INTO people (email, via, created_at) VALUES ('a@example.com', '["teams"]', '2026-11-01T00:00:00Z'), ('b@example.com', '[]', '2026-11-01T00:00:00Z');
CREATE TABLE pages (id INTEGER PRIMARY KEY AUTOINCREMENT, event_id INTEGER NOT NULL, chain TEXT NOT NULL, steps TEXT NOT NULL, step INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, next_at TEXT NOT NULL, started_at TEXT NOT NULL, acknowledged_at TEXT, acknowledged_by TEXT NOT NULL DEFAULT '', ended_at TEXT);
CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, event_id INTEGER NOT NULL, person TEXT NOT NULL, medium TEXT NOT NULL, page_id INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, next_at TEXT NOT NULL, status INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', sent_at TEXT, created_at TEXT NOT NULL, UNIQUE (event_id, person, medium, page_id));
PRAGMA user_version = 1;`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := st.Person(context.Background(), "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []notice.Weight{notice.WeightPage, notice.WeightNotify} {
		if steps := a.Rules[w]; len(steps) != 1 || steps[0].Medium != notice.TeamsChat {
			t.Errorf("%s: %+v", w, a.Rules)
		}
	}
	if _, set := a.Rules[notice.WeightNotice]; set {
		t.Errorf("notice stays at its default: %+v", a.Rules)
	}
	if b, _ := st.Person(context.Background(), "b@example.com"); b.Rules != nil {
		t.Errorf("every medium stays the default: %+v", b.Rules)
	}
}

func TestNoiseCountsFiringsPagesAndActions(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		st.InsertEvent(ctx, notice.Event{Producer: "alertmanager", Kind: "alert.firing", Key: fmt.Sprint("w", i), Title: "x", Labels: map[string]string{"alertname": "DiskSlow"}}, now)
	}
	noise, err := st.NoiseSince(ctx, now.Add(-time.Hour))
	if err != nil || len(noise) != 1 || noise[0] != (Noise{Alert: "DiskSlow", Fired: 3}) {
		t.Errorf("noise %+v %v", noise, err)
	}
}
