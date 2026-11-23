package server

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"

	"github.com/orchard-suite/platformkit/v3/door"
	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/outcomes"
)

//go:embed templates
var templateFS embed.FS

var pages = map[string]*template.Template{}

// nzData is one noise row and the loudest alert's count, for its bar.
type nzData struct {
	N     NoiseRow
	Top   int
	Admin bool
}

// stripData is a strip and what it says, for screen readers.
type stripData struct {
	Label string
	Cells []Cell
}

var funcs = template.FuncMap{
	"ago":       ago,
	"dur":       humanDuration,
	"when":      func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	"whenp":     func(t *time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
	"clock":     clock,
	"byline":    byline,
	"span":      span,
	"secs":      outcomes.Describe,
	"markdown":  renderMarkdown,
	"who":       personName,
	"initials":  initials,
	"named":     named,
	"whole":     whole,
	"breakable": breakable,
	"iso":       func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"isop": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	},
	"cap":     func(v any) string { return capitalize(fmt.Sprint(v)) },
	"percent": percent,
	"inc":     func(i int) int { return i + 1 },
	"sub":     func(a, b int) int { return a - b },
	"times": func(n int) string {
		switch n {
		case 1:
			return "once"
		case 2:
			return "twice"
		}
		return fmt.Sprintf("%d times", n)
	},
	"within": func(d time.Duration) string {
		if d < time.Minute {
			return "in under a minute"
		}
		return "within " + humanDuration(d)
	},
	"initial": func(s string) string {
		if s == "" {
			return "?"
		}
		return s[:1]
	},
	"primary": primaryMove,
	"nz":      func(n NoiseRow, top int) nzData { return nzData{N: n, Top: top} },
	"nzA":     func(n NoiseRow, top int, admin bool) nzData { return nzData{N: n, Top: top, Admin: admin} },
	"moveLabel": func(st incident.State) string {
		switch st {
		case incident.Open:
			return "Reopen"
		case incident.Acknowledged:
			return "Acknowledge"
		case incident.Mitigated:
			return "Mitigate"
		}
		return "Resolve"
	},
	"strip": func(label string, cells []Cell) stripData { return stripData{Label: label, Cells: cells} },
	"dict": func(pairs ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[pairs[i]] = pairs[i+1]
		}
		return m
	},
}

func init() {
	for _, name := range []string{"incidents", "incident", "history", "components", "settings", "notifications", "message", "people", "page", "me", "schedules"} {
		pages[name] = template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/reach.html", "templates/rows.html", "templates/ribbon.html", "templates/install.html", "templates/"+name+".html"))
	}
	pages["status"] = template.Must(template.New("status.html").Funcs(funcs).ParseFS(templateFS, "templates/status.html", "templates/install.html"))
}

var md = goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(html.WithHardWraps()))

// renderMarkdown renders a postmortem. Raw HTML is dropped by goldmark's
// default renderer, so the body cannot carry a script.
func renderMarkdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String())
}

// isAdmin says whether the person the gate admitted holds the admin role.
func (s *Server) isAdmin(r *http.Request) bool {
	return door.From(r.Context()).Has(s.cfg.AdminRole)
}

// onDesk says whether the person holds a role the desk admits. Someone
// who does not may open Me and nothing else.
func (s *Server) onDesk(r *http.Request) bool {
	return door.From(r.Context()).HasAny(s.cfg.DeskRole, s.cfg.AdminRole)
}

// anyone admits everyone the front door signed in, with or without a
// desk role, and logs which. Only Me, its saves, and what its page loads
// are served this way.
func (s *Server) anyone(h http.Handler) http.Handler {
	return s.open.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.onDesk(r) {
			s.logRequest(r.Context(), slog.LevelDebug, "me admitted: holds a desk role", r)
		} else {
			s.logRequest(r.Context(), slog.LevelInfo, "me admitted: signed in with no desk role, so only Me is open", r)
		}
		h.ServeHTTP(w, r)
	}))
}

// home is the desk's first page. Someone signed in with no desk role is
// sent to Me, the one page open to them.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if !s.onDesk(r) {
		s.logRequest(r.Context(), slog.LevelInfo, "home refused: no desk role, sent to Me", r)
		http.Redirect(w, r, "/me", http.StatusSeeOther)
		return
	}
	s.logRequest(r.Context(), slog.LevelDebug, "home admitted", r)
	s.learn(s.pageIncidents)(w, r)
}

// admin admits only the admin role.
func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			s.logRequest(r.Context(), slog.LevelWarn, "settings refused: not an admin", r)
			s.message(w, r, http.StatusForbidden, "Settings", "Only an admin changes the desk's settings.")
			return
		}
		next(w, r)
	}
}

// Session is the desk.
func (s *Server) Session() http.Handler {
	mux := http.NewServeMux()
	handle := func(pattern string, h http.Handler) { mux.Handle(pattern, s.gate.Wrap(h)) }
	page := func(pattern string, h http.HandlerFunc) { handle(pattern, s.learn(h)) }
	change := func(pattern string, h http.HandlerFunc) { handle(pattern, door.SameOrigin(s.publicOrigin, s.log, h)) }
	// Me is open to everyone the front door signed in. Every other route
	// needs a desk role.
	mine := func(pattern string, h http.Handler) { mux.Handle(pattern, s.anyone(h)) }
	mineChange := func(pattern string, h http.HandlerFunc) { mine(pattern, door.SameOrigin(s.publicOrigin, s.log, h)) }
	serveInstall(mux)
	mux.Handle("GET /{$}", s.open.Wrap(http.HandlerFunc(s.home)))
	handle("GET /live", http.HandlerFunc(s.liveStream))
	mine("GET /desk.js", http.HandlerFunc(s.deskScript))
	change("POST /settings/lists/{list}", s.admin(s.formSettingsRow))
	page("GET /history", s.pageHistory)
	page("GET /components", s.pageComponents)
	page("GET /incidents/{id}", s.pageIncident)
	page("GET /incidents/{id}/postmortem", s.pagePostmortem)
	change("POST /incidents", s.formOpen)
	change("POST /incidents/{id}/state", s.formState)
	change("POST /incidents/{id}/assign", s.formAssign)
	change("POST /incidents/{id}/note", s.formNote)
	change("POST /incidents/{id}/summary", s.formSummary)
	change("POST /incidents/{id}/severity", s.formSeverity)
	change("POST /incidents/{id}/public", s.formPublic)
	change("POST /incidents/{id}/auto-resolve", s.formAutoResolve)
	change("POST /incidents/{id}/postmortem", s.formPostmortem)
	change("POST /incidents/{id}/actions", s.formAction)
	change("POST /incidents/{id}/actions/{aid}/done", s.formActionDone)
	page("GET /settings", s.admin(s.pageSettings))
	page("GET /settings/{place}", s.admin(s.pageSettings))
	change("POST /settings", s.admin(s.formSettings))
	change("POST /tokens", s.admin(s.formMintToken))
	change("POST /windows", s.admin(s.formAddWindow))
	change("POST /windows/{id}/end", s.admin(s.formEndWindow))
	change("POST /heartbeats", s.admin(s.formSaveHeartbeat))
	change("POST /heartbeats/{name}/delete", s.admin(s.formDeleteHeartbeat))
	change("POST /tokens/{id}/revoke", s.admin(s.formRevokeToken))
	page("GET /notifications", s.admin(s.pageNotifications))
	change("POST /notifications/channels", s.admin(s.formAddChannel))
	change("POST /notifications/channels/{id}/test", s.admin(s.formTestChannel))
	change("POST /notifications/channels/{id}/delete", s.admin(s.formDeleteChannel))
	change("POST /notifications/channels/{id}/rotate", s.admin(s.formRotateSecret))
	change("POST /notifications/channels/{id}/drop-old", s.admin(s.formDropOldSecret))
	change("POST /notifications/subscriptions", s.admin(s.formAddSubscription))
	change("POST /notifications/subscriptions/{id}/pause", s.admin(s.formPauseSubscription))
	change("POST /notifications/subscriptions/{id}/delete", s.admin(s.formDeleteSubscription))
	page("GET /people", s.admin(s.pagePeople))
	change("POST /people", s.admin(s.formSavePerson))
	change("POST /people/{email}/delete", s.admin(s.formDeletePerson))
	change("POST /groups", s.admin(s.formSaveGroup))
	change("POST /groups/{slug}/delete", s.admin(s.formDeleteGroup))
	change("POST /chains", s.admin(s.formSaveChain))
	change("POST /chains/{slug}/delete", s.admin(s.formDeleteChain))
	page("GET /schedules", s.pageSchedules)
	change("POST /schedules", s.admin(s.formSaveSchedule))
	change("POST /schedules/{slug}/delete", s.admin(s.formDeleteSchedule))
	change("POST /schedules/{slug}/covers", s.formCover)
	change("POST /schedules/{slug}/covers/{id}/delete", s.formDeleteCover)
	change("POST /me/calendar", s.formCalendar)
	change("POST /me/token", s.formAgentToken)
	change("POST /incidents/{id}/snooze", s.formSnooze)
	change("POST /incidents/{id}/roles", s.formRole)
	change("POST /incidents/{id}/links", s.formLink)
	change("POST /incidents/{id}/room", s.formRoom)
	change("POST /incidents/{id}/assist", s.formAssist)
	page("GET /status", s.pageInternalStatus)
	change("POST /incidents/{id}/escalate", s.formEscalateIncident)
	mine("GET /me", s.learn(s.pageMe))
	mineChange("POST /me", s.formSaveMe)
	mineChange("POST /me/kinds", s.formKinds)
	page("GET /pages/{id}", s.pagePage)
	change("POST /pages/{id}/acknowledge", s.formAcknowledgePage)
	change("POST /pages/{id}/escalate", s.formEscalatePage)
	mine("POST /api/telemetry", s.relay)
	return telemetry.Handler(mux, "session")
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	data["Email"] = door.From(r.Context()).Email
	data["Admin"] = s.isAdmin(r)
	// Desk is false for someone signed in with no desk role: their page
	// carries no rail, no counts, no shift, and no stream.
	desk := s.onDesk(r)
	data["Desk"] = desk
	data["Nav"] = page
	data["Version"] = s.cfg.Version
	data["Now"] = time.Now()
	learnNames(r.Context(), s.store)
	if desk {
		// The rail's Live count; a failed count leaves the badge off rather than the page.
		if counts, err := s.store.OpenBySeverity(r.Context()); err == nil {
			n := 0
			for _, c := range counts {
				n += c
			}
			data["LiveCount"], data["LiveHot"] = n, counts[string(incident.Critical)] > 0
		}
		data["Shift"] = s.shift(r)
	}
	if _, ok := data["Place"]; !ok {
		data["Place"] = ""
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	if err := pages[page].Execute(w, data); err != nil {
		s.logRequest(r.Context(), slog.LevelError, "page render failed", r, "page", page, "error", err)
	}
}

func (s *Server) message(w http.ResponseWriter, r *http.Request, status int, title, text string) {
	s.render(w, r, status, "message", map[string]any{"Title": title, "Text": text})
}

// pageFailed answers a desk error on a page and reports whether there was one.
func (s *Server) pageFailed(w http.ResponseWriter, r *http.Request, err error, what string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, desk.ErrNotFound):
		s.message(w, r, http.StatusNotFound, "Not found", "There is no such incident.")
	case errors.Is(err, desk.ErrInput):
		s.message(w, r, http.StatusBadRequest, what, capitalize(strings.TrimPrefix(err.Error(), desk.ErrInput.Error()+": "))+".")
	default:
		s.logRequest(r.Context(), slog.LevelError, what+" failed", r, "error", err)
		s.message(w, r, http.StatusInternalServerError, what, "Something went wrong. The log has it.")
	}
	return true
}

func (s *Server) formOpen(w http.ResponseWriter, r *http.Request) {
	inc, err := s.desk.Open(r.Context(), desk.OpenRequest{
		Title: r.FormValue("title"), Severity: incident.Severity(r.FormValue("severity")),
		Service: r.FormValue("service"), Description: r.FormValue("description"), Public: r.FormValue("public") == "on",
	}, incident.SourceDesk, incident.Person(door.From(r.Context()).Email))
	if s.pageFailed(w, r, err, "Open an incident") {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/incidents/%d", inc.ID), http.StatusSeeOther)
}

// incidentForm runs a change on the incident in the path and goes back to it.
func (s *Server) incidentForm(w http.ResponseWriter, r *http.Request, what string, fn func(id int64) error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.message(w, r, http.StatusNotFound, "Not found", "There is no such incident.")
		return
	}
	if s.pageFailed(w, r, fn(id), what) {
		return
	}
	// A move made from the desk returns to the desk; any other back is ignored.
	if back := r.FormValue("back"); back == "/" || strings.HasPrefix(back, "/?") {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if r.FormValue("back") == "postmortem" {
		http.Redirect(w, r, fmt.Sprintf("/incidents/%d/postmortem", id), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/incidents/%d", id), http.StatusSeeOther)
}

func (s *Server) formState(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "Change the state", func(id int64) error {
		_, err := s.desk.Change(r.Context(), id, incident.State(r.FormValue("to")), incident.Person(door.From(r.Context()).Email))
		return err
	})
}

func (s *Server) formAssign(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "Assign", func(id int64) error {
		owner := r.FormValue("owner")
		if r.FormValue("me") == "1" {
			owner = door.From(r.Context()).Email
		}
		_, err := s.desk.Assign(r.Context(), id, owner, door.From(r.Context()).Email)
		return err
	})
}

func (s *Server) formNote(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "Add a note", func(id int64) error {
		_, err := s.desk.Note(r.Context(), id, r.FormValue("text"), r.FormValue("public") == "on", door.From(r.Context()).Email)
		return err
	})
}

func (s *Server) formSeverity(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "Change the severity", func(id int64) error {
		_, err := s.desk.SetSeverity(r.Context(), id, incident.Severity(r.FormValue("severity")), door.From(r.Context()).Email)
		return err
	})
}

func (s *Server) formPublic(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "Status page", func(id int64) error {
		_, err := s.desk.SetPublic(r.Context(), id, r.FormValue("public") == "1", door.From(r.Context()).Email)
		return err
	})
}

func (s *Server) formAutoResolve(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "Auto-resolve", func(id int64) error {
		_, err := s.desk.SetAutoResolve(r.Context(), id, r.FormValue("on") == "1", door.From(r.Context()).Email)
		return err
	})
}

func (s *Server) formPostmortem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.message(w, r, http.StatusNotFound, "Not found", "There is no such incident.")
		return
	}
	if s.pageFailed(w, r, s.desk.SavePostmortem(r.Context(), id, r.FormValue("body"), door.From(r.Context()).Email), "Postmortem") {
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/incidents/%d/postmortem?saved", id), http.StatusSeeOther)
}

func (s *Server) formAction(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "Action item", func(id int64) error {
		return s.desk.AddAction(r.Context(), id, r.FormValue("text"), r.FormValue("owner"), door.From(r.Context()).Email)
	})
}

func (s *Server) formActionDone(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "Action item", func(id int64) error {
		aid, err := strconv.ParseInt(r.PathValue("aid"), 10, 64)
		if err != nil {
			return desk.ErrNotFound
		}
		return s.desk.SetActionDone(r.Context(), id, aid, r.FormValue("done") == "1", door.From(r.Context()).Email)
	})
}

func ago(t time.Time) string {
	return humanDuration(time.Since(t))
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		// Past a day, minutes are noise: days and hours.
		days, h := int(d.Hours())/24, int(d.Hours())%24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd %dh", days, h)
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func percent(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

// parsePairs reads "key = value" lines, skipping blanks and # comments; a
// line that is not a pair, or that ok refuses, is returned.
func parsePairs(text string, ok func(k, v string) bool) (map[string]string, []string) {
	out := map[string]string{}
	var bad []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !found || k == "" || v == "" || !ok(k, v) {
			bad = append(bad, line)
			continue
		}
		out[k] = v
	}
	return out, bad
}

// formatPairs writes pairs the way parsePairs reads them, by key.
func formatPairs(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + " = " + m[k] + "\n")
	}
	return b.String()
}

// paged says whether anyone on the on-call load was paged at all; a table
// of zeros says less than a sentence.
func paged(people []outcomes.Load) bool {
	for _, p := range people {
		if p.Pages > 0 || p.Acknowledged > 0 {
			return true
		}
	}
	return false
}
