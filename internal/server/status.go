package server

import (
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// StatusComponent is one line of the public page.
type StatusComponent struct {
	Name string
	// State is operational, degraded, or down.
	State string
}

// StatusIncident is an open incident the desk chose to show: its title,
// state, severity, when it opened, and its public notes. No owner, no
// labels, no service key, no internal note.
type StatusIncident struct {
	Title    string
	Severity incident.Severity
	State    incident.State
	OpenedAt time.Time
	Updates  []StatusUpdate
}

type StatusUpdate struct {
	At   time.Time
	Text string
}

// StatusPage is everything the public page shows.
type StatusPage struct {
	Title      string
	Overall    string
	Components []StatusComponent
	Incidents  []StatusIncident
	Resolved   []StatusIncident
	At         time.Time
}

// Status is the public port.
func (s *Server) Status() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	serveInstall(mux)
	mux.HandleFunc("GET /{$}", s.pageStatus)
	mux.HandleFunc("GET /status.json", s.statusJSON)
	mux.HandleFunc("POST /follow", s.statusFollow)
	mux.HandleFunc("GET /confirm", s.statusConfirm)
	mux.HandleFunc("GET /unsubscribe", s.statusUnsubscribe)
	return telemetry.Handler(mux, "status")
}

// statusSay shows the status page with one line above it.
func (s *Server) statusSay(w http.ResponseWriter, r *http.Request, code int, said string) {
	page, err := s.status(r)
	if err != nil {
		http.Error(w, "The status page could not be built.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(code)
	_ = pages["status"].Execute(w, map[string]any{"Page": page, "Version": s.cfg.Version, "Said": said, "Follow": s.desk.Notices() != nil})
}

// statusFollow takes an address that wants the status page's updates and
// mails it a link to confirm; nothing is sent to it until it does. The
// page answers the same whatever the address, so it names nobody.
func (s *Server) statusFollow(w http.ResponseWriter, r *http.Request) {
	email, err := notice.NormalEmail(r.FormValue("email"))
	if err != nil {
		s.statusSay(w, r, http.StatusBadRequest, "That is not an email address.")
		return
	}
	if err := s.desk.Follow(r.Context(), email); err != nil {
		s.logRequest(r.Context(), slog.LevelWarn, "status follow failed", r, "error", err)
	}
	s.statusSay(w, r, http.StatusOK, "If that address can take mail, it has a link to confirm. Nothing comes until you follow it.")
}

func (s *Server) statusConfirm(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.Confirm(r.Context(), r.URL.Query().Get("token")); err != nil {
		s.statusSay(w, r, http.StatusNotFound, "That link does not confirm anything.")
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "status follower confirmed", r)
	s.statusSay(w, r, http.StatusOK, "Confirmed. Updates on public incidents come by email; each has a link to stop them.")
}

func (s *Server) statusUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Unsubscribe(r.Context(), r.URL.Query().Get("token")); err != nil {
		s.statusSay(w, r, http.StatusNotFound, "That link stops nothing; you may already have stopped.")
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "status follower left", r)
	s.statusSay(w, r, http.StatusOK, "Stopped. No more updates come to you.")
}

// pageInternalStatus is the status page for the people behind the front
// door: every service with an open incident, and every open incident with
// all its notes.
func (s *Server) pageInternalStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings, err := s.store.Settings(ctx)
	if s.pageFailed(w, r, err, "Status") {
		return
	}
	open, err := s.store.List(ctx, store.Filter{States: []incident.State{incident.Open, incident.Acknowledged, incident.Mitigated}})
	if s.pageFailed(w, r, err, "Status") {
		return
	}
	resolved, err := s.store.List(ctx, store.Filter{States: []incident.State{incident.Resolved}, Since: time.Now().Add(-7 * 24 * time.Hour), Limit: 50})
	if s.pageFailed(w, r, err, "Status") {
		return
	}
	inside := settings
	inside.PublicComponents = map[string]string{}
	for k, v := range settings.PublicComponents {
		inside.PublicComponents[k] = v
	}
	for i := range open {
		open[i].Public = true
		if _, named := inside.PublicComponents[open[i].Service]; !named {
			inside.PublicComponents[open[i].Service] = open[i].Service
		}
	}
	for i := range resolved {
		resolved[i].Public = true
	}
	notes := func(id int64) []incident.Event {
		events, _ := s.store.Events(ctx, id)
		for i := range events {
			events[i].Public = true
		}
		return events
	}
	page := BuildStatus(inside, open, resolved, notes, time.Now())
	page.Title = settings.StatusTitle + ", inside"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := pages["status"].Execute(w, map[string]any{"Page": page, "Version": s.cfg.Version, "Inside": true}); err != nil {
		s.logRequest(ctx, slog.LevelError, "status render failed", r, "error", err)
	}
}

// BuildStatus redacts the desk into the public page. Only components the
// settings name are shown; only incidents marked public are listed, and
// only their public notes.
func BuildStatus(settings store.Settings, open, resolved []incident.Incident, notes func(id int64) []incident.Event, now time.Time) StatusPage {
	page := StatusPage{Title: settings.StatusTitle, Overall: "operational", At: now}
	worst := map[string]incident.Severity{}
	for _, i := range open {
		if w, ok := worst[i.Service]; !ok || i.Severity.Rank() < w.Rank() {
			worst[i.Service] = i.Severity
		}
	}
	for key, name := range settings.PublicComponents {
		c := StatusComponent{Name: name, State: "operational"}
		switch worst[key] {
		case incident.Critical:
			c.State = "down"
		case incident.Warning:
			c.State = "degraded"
		}
		page.Components = append(page.Components, c)
		if c.State == "down" || (c.State == "degraded" && page.Overall != "down") {
			page.Overall = c.State
		}
	}
	sort.Slice(page.Components, func(i, j int) bool { return page.Components[i].Name < page.Components[j].Name })
	redact := func(i incident.Incident) StatusIncident {
		si := StatusIncident{Title: i.Title, Severity: i.Severity, State: i.State, OpenedAt: i.OpenedAt}
		for _, e := range notes(i.ID) {
			if e.Public && e.Kind == incident.EventNote {
				si.Updates = append(si.Updates, StatusUpdate{At: e.At, Text: e.Text})
			}
		}
		return si
	}
	for _, i := range open {
		if i.Public {
			page.Incidents = append(page.Incidents, redact(i))
		}
	}
	for _, i := range resolved {
		if i.Public && i.ResolvedAt != nil && now.Sub(*i.ResolvedAt) < 7*24*time.Hour {
			page.Resolved = append(page.Resolved, redact(i))
		}
	}
	return page
}

func (s *Server) status(r *http.Request) (StatusPage, error) {
	ctx := r.Context()
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return StatusPage{}, err
	}
	open, err := s.store.List(ctx, store.Filter{States: []incident.State{incident.Open, incident.Acknowledged, incident.Mitigated}})
	if err != nil {
		return StatusPage{}, err
	}
	resolved, err := s.store.List(ctx, store.Filter{States: []incident.State{incident.Resolved}, Since: time.Now().Add(-14 * 24 * time.Hour), Limit: 50})
	if err != nil {
		return StatusPage{}, err
	}
	notes := func(id int64) []incident.Event {
		events, err := s.store.PublicEvents(ctx, id)
		if err != nil {
			s.logRequest(ctx, slog.LevelError, "status notes failed", r, "error", err)
		}
		return events
	}
	return BuildStatus(settings, open, resolved, notes, time.Now()), nil
}

func (s *Server) pageStatus(w http.ResponseWriter, r *http.Request) {
	page, err := s.status(r)
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "status page failed", r, "error", err)
		http.Error(w, "The status page could not be built.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := pages["status"].Execute(w, map[string]any{"Page": page, "Version": s.cfg.Version, "Follow": s.desk.Notices() != nil}); err != nil {
		s.logRequest(r.Context(), slog.LevelError, "status render failed", r, "error", err)
	}
}

func (s *Server) statusJSON(w http.ResponseWriter, r *http.Request) {
	page, err := s.status(r)
	if err != nil {
		s.logRequest(r.Context(), slog.LevelError, "status json failed", r, "error", err)
		writeError(w, http.StatusInternalServerError, "the status could not be built")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=30")
	writeJSON(w, http.StatusOK, page)
}
