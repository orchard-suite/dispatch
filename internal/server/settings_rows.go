package server

import (
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/github"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

// settingsList is one list on a settings place that is edited a row at a
// time: what it is called, where its place is, and how a row is read from
// a form and written back into the settings.
type settingsList struct {
	place string
	// line builds one row in the list's text form from the form's fields.
	line func(r *http.Request) string
	// get and set move the list between the settings and its text form.
	get func(st store.Settings) []string
	set func(st *store.Settings, lines []string) []string
}

// matchText reads a row's match: a label and its value, and any more as
// "key=value, key=value" in the match field.
func matchText(r *http.Request) string {
	var parts []string
	if k, v := strings.TrimSpace(r.FormValue("label")), strings.TrimSpace(r.FormValue("value")); k != "" || v != "" {
		parts = append(parts, k+"="+v)
	}
	if more := strings.TrimSpace(r.FormValue("match")); more != "" {
		parts = append(parts, more)
	}
	return strings.Join(parts, ", ")
}

func lines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func pairsList(m map[string]string) []string { return lines(formatPairs(m)) }

var settingsLists = map[string]settingsList{
	"routes": {
		place: "/settings/routing",
		line: func(r *http.Request) string {
			l := matchText(r) + " => " + strings.TrimSpace(r.FormValue("action"))
			if r.FormValue("action") == string(incident.Rate) {
				l += " " + strings.TrimSpace(r.FormValue("severity"))
			}
			return l
		},
		get: func(st store.Settings) []string { return lines(incident.FormatRoutes(st.Routes)) },
		set: func(st *store.Settings, ls []string) []string {
			routes, bad := incident.ParseRoutes(strings.Join(ls, "\n"))
			st.Routes = routes
			return bad
		},
	},
	"rules": {
		place: "/settings/routing",
		line:  func(r *http.Request) string { return matchText(r) + " -> " + strings.TrimSpace(r.FormValue("service")) },
		get:   func(st store.Settings) []string { return lines(incident.FormatRules(st.Rules)) },
		set: func(st *store.Settings, ls []string) []string {
			rules, bad := incident.ParseRules(strings.Join(ls, "\n"))
			st.Rules = rules
			return bad
		},
	},
	"components": {
		place: "/settings/status",
		line: func(r *http.Request) string {
			return strings.TrimSpace(r.FormValue("key")) + " = " + strings.TrimSpace(r.FormValue("name"))
		},
		get: func(st store.Settings) []string { return pairsList(st.PublicComponents) },
		set: func(st *store.Settings, ls []string) []string {
			m, bad := parsePairs(strings.Join(ls, "\n"), func(string, string) bool { return true })
			st.PublicComponents = m
			return bad
		},
	},
	"templates": {
		place: "/settings/status",
		line: func(r *http.Request) string {
			return strings.TrimSpace(r.FormValue("key")) + " = " + strings.TrimSpace(r.FormValue("text"))
		},
		get: func(st store.Settings) []string { return pairsList(st.UpdateTemplates) },
		set: func(st *store.Settings, ls []string) []string {
			m, bad := parsePairs(strings.Join(ls, "\n"), func(k, _ string) bool { return incident.ParseSeverity(k) == incident.Severity(k) })
			st.UpdateTemplates = m
			return bad
		},
	},
	"repos": {
		place: "/settings/machines",
		line: func(r *http.Request) string {
			return strings.TrimSpace(r.FormValue("key")) + " = " + strings.TrimSpace(r.FormValue("repo"))
		},
		get: func(st store.Settings) []string { return pairsList(st.IssueRepos) },
		set: func(st *store.Settings, ls []string) []string {
			m, bad := parsePairs(strings.Join(ls, "\n"), func(_, v string) bool { return github.ValidRepo(v) })
			st.IssueRepos = m
			return bad
		},
	},
}

// formSettingsRow adds, replaces, removes, or moves one row of a settings
// list. A row that does not read is refused whole and nothing is saved.
func (s *Server) formSettingsRow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("list")
	list, ok := settingsLists[name]
	if !ok {
		s.message(w, r, http.StatusNotFound, "Settings", "There is no such list.")
		return
	}
	st, err := s.store.Settings(r.Context())
	if s.pageFailed(w, r, err, "Settings") {
		return
	}
	rows := list.get(st)
	i, _ := strconv.Atoi(r.FormValue("i"))
	inRange := i >= 0 && i < len(rows)
	op := r.FormValue("op")
	switch op {
	case "add":
		rows = append(rows, list.line(r))
	case "replace":
		if !inRange {
			s.message(w, r, http.StatusBadRequest, "Settings", "That row is gone. Reload the page.")
			return
		}
		rows[i] = list.line(r)
	case "remove":
		if !inRange {
			s.message(w, r, http.StatusBadRequest, "Settings", "That row is gone. Reload the page.")
			return
		}
		rows = slices.Delete(rows, i, i+1)
	case "up", "down":
		j := i - 1
		if op == "down" {
			j = i + 1
		}
		if !inRange || j < 0 || j >= len(rows) {
			s.message(w, r, http.StatusBadRequest, "Settings", "That row cannot move there.")
			return
		}
		rows[i], rows[j] = rows[j], rows[i]
	default:
		s.message(w, r, http.StatusBadRequest, "Settings", "That change is not one the desk knows.")
		return
	}
	if bad := list.set(&st, rows); len(bad) > 0 {
		s.message(w, r, http.StatusBadRequest, "Settings", "This row does not read: "+bad[0]+".")
		return
	}
	if s.pageFailed(w, r, s.store.SaveSettings(r.Context(), st), "Settings") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "settings row saved", r, "by", door.From(r.Context()).Email, "list", name, "op", op)
	s.decideAdmin(r, "settings.save", "settings", name, op)
	http.Redirect(w, r, list.place+"?saved="+name, http.StatusSeeOther)
}
