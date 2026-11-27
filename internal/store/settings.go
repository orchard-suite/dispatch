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

// Settings is what the desk's Settings page changes. The zero value is
// filled in by Defaults.
type Settings struct {
	// GroupingWindow is how long after an incident resolves the same group
	// key reopens it instead of opening a new incident.
	GroupingWindow time.Duration `json:"grouping_window"`
	// Rules map alert labels to a service.
	Rules []incident.Rule `json:"rules"`
	// Routes drop an alert, make it a notice or a page, or set its
	// severity, before the desk decides whether it pages.
	Routes []incident.Route `json:"routes,omitempty"`
	// AutoResolveInfo sets auto_resolve on new info incidents.
	AutoResolveInfo bool `json:"auto_resolve_info"`
	// QuietResolve resolves an incident of any severity once none of its
	// alerts has fired for this long; 0 turns it off.
	QuietResolve time.Duration `json:"quiet_resolve"`
	// PublicComponents is what the status page shows: service key to the
	// name shown. A service not here is never named in public.
	PublicComponents map[string]string `json:"public_components"`
	// StatusTitle heads the status page.
	StatusTitle string `json:"status_title"`
	// UpdateTemplates start a status update, by severity; {title},
	// {service}, and {state} are filled in.
	UpdateTemplates map[string]string `json:"update_templates,omitempty"`
	// IssueRepos is where an action item's issue opens, by service, as
	// owner/repo; "*" is every other service.
	IssueRepos map[string]string `json:"issue_repos,omitempty"`
	// Zone is the desk's time zone, for what counts as night: 22:00 to
	// 07:00 there.
	Zone string `json:"zone,omitempty"`
	// PersonPages is how many pages a person takes in seven days before
	// notify-weight messages to them wait for the morning; 0 is no limit.
	PersonPages int `json:"person_pages,omitempty"`
	// ServicePages is how many pages a service may cause in seven days
	// before the desk says it is over budget; 0 is no limit.
	ServicePages int `json:"service_pages,omitempty"`
	// Leaning is which catalog relations say a thing leans on another, for
	// joining incidents to changes and for the watch.
	Leaning incident.Leaning `json:"leaning,omitempty"`
	// Kinds are the estate's effect kinds beside the suite's base ones,
	// and the base ones whose watch the estate changed.
	Kinds []change.Kind `json:"kinds,omitempty"`
}

// Night says whether t falls between 22:00 and 07:00 in the desk's zone.
func (s Settings) Night(t time.Time) bool {
	loc, err := time.LoadLocation(s.Zone)
	if err != nil || s.Zone == "" {
		loc = time.UTC
	}
	h := t.In(loc).Hour()
	return h >= 22 || h < 7
}

func Defaults() Settings {
	return Settings{
		GroupingWindow:   15 * time.Minute,
		AutoResolveInfo:  true,
		QuietResolve:     24 * time.Hour,
		PublicComponents: map[string]string{},
		StatusTitle:      "Status",
	}
}

const settingsKey = "settings"

// Settings reads the settings, with defaults where none were saved.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	st := Defaults()
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, settingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return Defaults(), err
	}
	if st.PublicComponents == nil {
		st.PublicComponents = map[string]string{}
	}
	if st.GroupingWindow <= 0 {
		st.GroupingWindow = Defaults().GroupingWindow
	}
	if st.StatusTitle == "" {
		st.StatusTitle = Defaults().StatusTitle
	}
	return st, nil
}

func (s *Store) SaveSettings(ctx context.Context, st Settings) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, settingsKey, string(raw))
	return err
}
