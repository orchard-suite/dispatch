package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

// The settings places, each a page of its own: what it is called in the
// path, and the template's name for it.
const (
	placeGeneral  = "general"
	placeRouting  = "routing"
	placeStatus   = "status"
	placeMachines = "machines"
)

// placePath is where a place lives.
func placePath(place string) string {
	if place == placeGeneral {
		return "/settings"
	}
	return "/settings/" + place
}

// countWindow is how far back a route's or rule's count of alerts looks.
const countWindow = 7 * 24 * time.Hour

// pair is one label and its glob, or one key and its value.
type pair struct{ K, V string }

func sortedPairs(m map[string]string) []pair {
	out := make([]pair, 0, len(m))
	for k, v := range m {
		out = append(out, pair{k, v})
	}
	slices.SortFunc(out, func(a, b pair) int { return strings.Compare(a.K, b.K) })
	return out
}

// restText is every pair after the first, as "key=value, key=value", the
// way a row's form takes the labels past its first.
func restText(ps []pair) string {
	if len(ps) < 2 {
		return ""
	}
	parts := make([]string, 0, len(ps)-1)
	for _, p := range ps[1:] {
		parts = append(parts, p.K+"="+p.V)
	}
	return strings.Join(parts, ", ")
}

// rowTools is what a row's move and remove buttons post: its list, its
// position, and whether it moves at all (a keyed list is kept by key).
type rowTools struct {
	List       string
	I          int
	Move, Last bool
}

func toolsFor(list string, i, n int, move bool) rowTools {
	return rowTools{List: list, I: i, Move: move, Last: i == n-1}
}

// routeRow is one route as the Routing place lists it: its position, what
// it matches, what it does, and how many alerts it caught.
type routeRow struct {
	I, N     int
	Match    []pair
	Rest     string
	Action   incident.RouteAction
	Severity incident.Severity
	Then     string
	Caught   int
	Tools    rowTools
}

func (r routeRow) First() pair {
	if len(r.Match) == 0 {
		return pair{}
	}
	return r.Match[0]
}

// thenWords is what a route does, as the end of a sentence.
func thenWords(a incident.RouteAction, sev incident.Severity) string {
	switch a {
	case incident.Drop:
		return "drop it"
	case incident.Quiet:
		return "make it a notice"
	case incident.Raise:
		return "page"
	case incident.Rate:
		return "rate it " + string(sev)
	}
	return string(a)
}

func lineRoute(i, n int, rt incident.Route, caught int) routeRow {
	m := sortedPairs(rt.Match)
	return routeRow{I: i, N: i + 1, Match: m, Rest: restText(m), Action: rt.Action, Severity: rt.Severity, Then: thenWords(rt.Action, rt.Severity), Caught: caught,
		Tools: toolsFor("routes", i, n, true)}
}

// ruleRow is one mapping rule as the Routing place lists it.
type ruleRow struct {
	I, N    int
	Match   []pair
	Rest    string
	Service string
	Caught  int
	Tools   rowTools
}

func (r ruleRow) First() pair {
	if len(r.Match) == 0 {
		return pair{}
	}
	return r.Match[0]
}

// pairRow is one row of a keyed list: a public component, a template, a
// repository.
type pairRow struct {
	I     int
	K, V  string
	Tools rowTools
}

func pairRows(list string, m map[string]string) []pairRow {
	ps := sortedPairs(m)
	out := make([]pairRow, 0, len(ps))
	for i, p := range ps {
		out = append(out, pairRow{I: i, K: p.K, V: p.V, Tools: toolsFor(list, i, len(ps), false)})
	}
	return out
}

// caught counts, for each route, the alerts of the last week it was the
// first to fit, and the alerts no route fitted. An extra route counts as
// if it were added at the end.
func caught(labels []map[string]string, routes []incident.Route) (counts []int, rest int) {
	counts = make([]int, len(routes))
	for _, l := range labels {
		if n := incident.ApplyRoutes(l, routes).Route; n > 0 {
			counts[n-1]++
		} else {
			rest++
		}
	}
	return counts, rest
}

// ruleCounts counts, for each mapping rule, the alerts it was the first
// to fit. Rules fit the way routes do, so the routes' own walk counts them.
func ruleCounts(labels []map[string]string, rules []incident.Rule) []int {
	as := make([]incident.Route, len(rules))
	for i, r := range rules {
		as[i] = incident.Route{Match: r.Match, Action: incident.Drop}
	}
	counts, _ := caught(labels, as)
	return counts
}

// savedSay is what a place says after a list or form was saved.
var savedSay = map[string]string{
	"routes": "Routes saved.", "rules": "Mapping rules saved.", "components": "Public components saved.",
	"templates": "Templates saved.", "repos": "Repositories saved.", "general": "Settings saved.", "status": "Status page saved.",
	"window": "Window saved.", "heartbeat": "Heartbeat saved.", "token": "Token revoked.",
}

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request) {
	place := r.PathValue("place")
	if place == "" {
		// The Noise panel's Quiet it once pointed here; its route belongs on Routing.
		if q := r.URL.Query().Get("route"); q != "" {
			http.Redirect(w, r, "/settings/routing?route="+url.QueryEscape(q), http.StatusSeeOther)
			return
		}
		place = placeGeneral
	}
	switch place {
	case placeRouting, placeStatus, placeMachines, placeGeneral:
	default:
		s.message(w, r, http.StatusNotFound, "Settings", "There is no such settings place.")
		return
	}
	s.renderSettings(w, r, http.StatusOK, place, nil, "", "")
}

// renderSettings shows one settings place. bad is what a form could not
// read; fresh is a token's secret, shown once.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, place string, bad []string, fresh, freshName string) {
	ctx := r.Context()
	settings, err := s.store.Settings(ctx)
	if s.pageFailed(w, r, err, "Settings") {
		return
	}
	data := map[string]any{"Place": place, "Settings": settings, "Bad": bad}
	if k := r.URL.Query().Get("saved"); r.URL.Query().Has("saved") {
		say := savedSay[k]
		if say == "" {
			say = "Saved."
		}
		data["Saved"] = say
	}
	switch place {
	case placeGeneral:
		data["WindowMinutes"] = int(settings.GroupingWindow / time.Minute)
		data["QuietHours"] = int(settings.QuietResolve / time.Hour)
	case placeRouting:
		labels, err := s.store.AlertLabelsSince(ctx, time.Now().Add(-countWindow))
		if s.pageFailed(w, r, err, "Settings") {
			return
		}
		counts, rest := caught(labels, settings.Routes)
		routes := make([]routeRow, 0, len(settings.Routes))
		for i, rt := range settings.Routes {
			routes = append(routes, lineRoute(i, len(settings.Routes), rt, counts[i]))
		}
		data["Routes"], data["Rest"], data["Alerts"] = routes, rest, len(labels)
		// A route the desk suggested, such as Quiet it on a noisy alert,
		// is a new row ready to save, counted as if it were added last.
		if q := strings.TrimSpace(r.URL.Query().Get("route")); q != "" {
			if parsed, bad := incident.ParseRoutes(q); len(parsed) == 1 && len(bad) == 0 {
				with := append(slices.Clone(settings.Routes), parsed[0])
				c, _ := caught(labels, with)
				data["Draft"] = lineRoute(len(settings.Routes), len(with), parsed[0], c[len(settings.Routes)])
			}
		}
		rc := ruleCounts(labels, settings.Rules)
		rules := make([]ruleRow, 0, len(settings.Rules))
		for i, ru := range settings.Rules {
			m := sortedPairs(ru.Match)
			rules = append(rules, ruleRow{I: i, N: i + 1, Match: m, Rest: restText(m), Service: ru.Service, Caught: rc[i], Tools: toolsFor("rules", i, len(settings.Rules), true)})
		}
		data["Rules"] = rules
	case placeStatus:
		data["Components"] = pairRows("components", settings.PublicComponents)
		data["Templates"] = pairRows("templates", settings.UpdateTemplates)
		data["Severities"] = []incident.Severity{incident.Critical, incident.Warning, incident.Info}
	case placeMachines:
		tokens, err := s.store.Tokens(ctx)
		if s.pageFailed(w, r, err, "Settings") {
			return
		}
		windows, err := s.store.Windows(ctx, time.Now())
		if s.pageFailed(w, r, err, "Settings") {
			return
		}
		beats, err := s.store.Heartbeats(ctx)
		if s.pageFailed(w, r, err, "Settings") {
			return
		}
		data["Tokens"], data["Windows"], data["Heartbeats"] = tokens, windows, beats
		data["Repos"] = pairRows("repos", settings.IssueRepos)
		data["PublicURL"], data["SharedSecret"] = s.cfg.PublicURL, s.cfg.AlertmanagerToken != ""
		data["Fresh"], data["FreshName"] = fresh, freshName
	}
	s.render(w, r, status, "settings", data)
}

// formSettings saves the plain fields of one settings place, named by the
// form's place field. A place changes only its own fields, so saving one
// never touches another's.
func (s *Server) formSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.message(w, r, http.StatusBadRequest, "Settings", "The form could not be read.")
		return
	}
	place := r.PostFormValue("place")
	settings, err := s.store.Settings(r.Context())
	if s.pageFailed(w, r, err, "Settings") {
		return
	}
	refuse := func(why string) { s.renderSettings(w, r, http.StatusBadRequest, place, []string{why}, "", "") }
	switch place {
	case placeGeneral:
		// Each row of General posts only its own field; a field the form
		// does not carry keeps its value.
		has := func(k string) bool { _, ok := r.PostForm[k]; return ok }
		number := func(k string, max int, why string) (int, bool) {
			n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue(k)))
			if err != nil || n < 0 || n > max {
				refuse(why)
				return 0, false
			}
			return n, true
		}
		if has("window_minutes") {
			n, ok := number("window_minutes", 7*24*60, "the grouping window must be 0 to 10080 minutes")
			if !ok {
				return
			}
			settings.GroupingWindow = time.Duration(n) * time.Minute
		}
		if has("quiet_resolve_hours") {
			n, ok := number("quiet_resolve_hours", 30*24, "quiet hours before an incident resolves must be 0 to 720")
			if !ok {
				return
			}
			settings.QuietResolve = time.Duration(n) * time.Hour
		}
		if has("auto_resolve_info") {
			settings.AutoResolveInfo = r.PostFormValue("auto_resolve_info") == "on"
		}
		if has("zone") {
			z := strings.TrimSpace(r.PostFormValue("zone"))
			if _, err := time.LoadLocation(z); z != "" && err != nil {
				refuse(z + " is not a time zone")
				return
			}
			settings.Zone = z
		}
		if has("person_pages") {
			n, ok := number("person_pages", 10000, "a budget is 0 or more pages")
			if !ok {
				return
			}
			settings.PersonPages = n
		}
		if has("service_pages") {
			n, ok := number("service_pages", 10000, "a budget is 0 or more pages")
			if !ok {
				return
			}
			settings.ServicePages = n
		}
	case placeStatus:
		t := strings.TrimSpace(r.FormValue("status_title"))
		if t == "" || len(t) > 80 {
			refuse("the status page needs a title of at most 80 characters")
			return
		}
		settings.StatusTitle = t
	default:
		s.message(w, r, http.StatusBadRequest, "Settings", "That form names no settings place.")
		return
	}
	if s.pageFailed(w, r, s.store.SaveSettings(r.Context(), settings), "Settings") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "settings saved", r, "by", door.From(r.Context()).Email, "place", place)
	s.decideAdmin(r, "settings.save", "settings", place, "")
	http.Redirect(w, r, placePath(place)+"?saved="+place, http.StatusSeeOther)
}

func (s *Server) formMintToken(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.renderSettings(w, r, http.StatusBadRequest, placeMachines, []string{"a token needs a name"}, "", "")
		return
	}
	tok, secret, err := s.store.MintToken(r.Context(), name, door.From(r.Context()).Email)
	if s.pageFailed(w, r, err, "Tokens") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "token minted", r, "token", tok.Name, "by", door.From(r.Context()).Email)
	s.decideAdmin(r, "token.issue", "token", tok.Name, tok.Name)
	s.renderSettings(w, r, http.StatusOK, placeMachines, nil, secret, tok.Name)
}

func (s *Server) formRevokeToken(w http.ResponseWriter, r *http.Request) {
	err := s.store.RevokeToken(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, r, http.StatusNotFound, "Tokens", "There is no such token.")
		return
	}
	if s.pageFailed(w, r, err, "Tokens") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "token revoked", r, "token", r.PathValue("id"), "by", door.From(r.Context()).Email)
	s.decideAdmin(r, "token.revoke", "token", r.PathValue("id"), "")
	http.Redirect(w, r, "/settings/machines?saved=token", http.StatusSeeOther)
}

// formAddWindow quiets a service from a time to a time, in the zone the
// browser sends as its offset in minutes.
func (s *Server) formAddWindow(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.FormValue("tz_offset"))
	zone := time.FixedZone("browser", -offset*60)
	starts, err1 := time.ParseInLocation("2006-01-02T15:04", r.FormValue("starts_at"), zone)
	ends, err2 := time.ParseInLocation("2006-01-02T15:04", r.FormValue("ends_at"), zone)
	if r.FormValue("starts_at") == "" {
		starts, err1 = time.Now(), nil
	}
	if err1 != nil || err2 != nil {
		s.renderSettings(w, r, http.StatusBadRequest, placeMachines, []string{"a window needs an end, and a start or now"}, "", "")
		return
	}
	win, err := s.desk.AddWindow(r.Context(), incident.Window{Service: strings.TrimSpace(r.FormValue("service")), StartsAt: starts.UTC(), EndsAt: ends.UTC(),
		Reason: strings.TrimSpace(r.FormValue("reason")), CreatedBy: door.From(r.Context()).Email})
	if errors.Is(err, desk.ErrInput) {
		s.renderSettings(w, r, http.StatusBadRequest, placeMachines, []string{strings.TrimPrefix(err.Error(), desk.ErrInput.Error()+": ")}, "", "")
		return
	}
	if s.pageFailed(w, r, err, "Settings") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "window added", r, "window", win.ID, "by", door.From(r.Context()).Email)
	s.decideAdmin(r, "window.add", "window", strconv.FormatInt(win.ID, 10), win.Service)
	http.Redirect(w, r, "/settings/machines?saved=window", http.StatusSeeOther)
}

func (s *Server) formEndWindow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.store.EndWindow(r.Context(), id, time.Now())
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) && s.pageFailed(w, r, err, "Settings") {
		return
	}
	s.decideAdmin(r, "window.end", "window", r.PathValue("id"), "")
	http.Redirect(w, r, "/settings/machines?saved=window", http.StatusSeeOther)
}

func (s *Server) formSaveHeartbeat(w http.ResponseWriter, r *http.Request) {
	interval, err1 := time.ParseDuration(strings.TrimSpace(r.FormValue("interval")))
	var grace time.Duration
	var err2 error
	if g := strings.TrimSpace(r.FormValue("grace")); g != "" {
		grace, err2 = time.ParseDuration(g)
	}
	if err1 != nil || err2 != nil {
		s.renderSettings(w, r, http.StatusBadRequest, placeMachines, []string{"an interval and a grace are durations, such as 15m or 1h"}, "", "")
		return
	}
	h := incident.Heartbeat{Name: strings.TrimSpace(r.FormValue("name")), Interval: interval, Grace: grace, Service: strings.TrimSpace(r.FormValue("service")),
		Severity: incident.Severity(r.FormValue("severity")), CreatedBy: door.From(r.Context()).Email}
	err := s.desk.SaveHeartbeat(r.Context(), h)
	if errors.Is(err, desk.ErrInput) {
		s.renderSettings(w, r, http.StatusBadRequest, placeMachines, []string{strings.TrimPrefix(err.Error(), desk.ErrInput.Error()+": ")}, "", "")
		return
	}
	if s.pageFailed(w, r, err, "Settings") {
		return
	}
	s.decideAdmin(r, "heartbeat.save", "heartbeat", h.Name, h.Name)
	http.Redirect(w, r, "/settings/machines?saved=heartbeat", http.StatusSeeOther)
}

func (s *Server) formDeleteHeartbeat(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteHeartbeat(r.Context(), r.PathValue("name"))
	if err != nil && !errors.Is(err, store.ErrNotFound) && s.pageFailed(w, r, err, "Settings") {
		return
	}
	s.decideAdmin(r, "heartbeat.remove", "heartbeat", r.PathValue("name"), "")
	http.Redirect(w, r, "/settings/machines?saved=heartbeat", http.StatusSeeOther)
}
