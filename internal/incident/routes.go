package incident

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// RouteAction is what a route does to an alert that fits it.
type RouteAction string

const (
	// Drop throws the alert away: nothing is opened, sent, or kept.
	Drop RouteAction = "drop"
	// Quiet makes the alert a notice, whatever its severity.
	Quiet RouteAction = "notice"
	// Raise makes the alert page, whatever its severity.
	Raise RouteAction = "page"
	// Rate sets the alert's severity.
	Rate RouteAction = "severity"
)

// A Route changes what an alert does before the desk decides whether it
// pages. Routes are tried in order and the first that fits wins. A match
// value is a glob.
type Route struct {
	Match    map[string]string `json:"match"`
	Action   RouteAction       `json:"action"`
	Severity Severity          `json:"severity,omitempty"`
}

// Routed is what the routes made of an alert.
type Routed struct {
	Dropped bool
	// Pages is whether it pages, after the routes.
	Pages  bool
	Labels map[string]string
	// Route is the 1-based route that fitted, or 0.
	Route int
}

// ApplyRoutes runs an alert's labels through the routes.
func ApplyRoutes(labels map[string]string, routes []Route) Routed {
	out := Routed{Labels: labels}
	for i, r := range routes {
		if !fits(labels, r.Match) {
			continue
		}
		out.Route = i + 1
		switch r.Action {
		case Drop:
			out.Dropped = true
			return out
		case Quiet:
			out.Pages = false
			return out
		case Raise:
			out.Pages = true
			return out
		case Rate:
			copied := make(map[string]string, len(labels))
			for k, v := range labels {
				copied[k] = v
			}
			copied["severity"] = string(r.Severity)
			out.Labels = copied
			out.Pages = Pages(copied)
			return out
		}
	}
	out.Pages = Pages(labels)
	return out
}

// ParseRoutes reads the settings form: one route a line, the matches, then
// "=>" and what to do: "drop", "notice", "page", or "severity <level>",
// such as "alertname=Watchdog => drop". A line it cannot read is returned.
func ParseRoutes(text string) ([]Route, []string) {
	var routes []Route
	var bad []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := parseRoute(line)
		if err != nil {
			bad = append(bad, line)
			continue
		}
		routes = append(routes, r)
	}
	return routes, bad
}

func parseRoute(line string) (Route, error) {
	lhs, rhs, ok := strings.Cut(line, "=>")
	if !ok {
		return Route{}, fmt.Errorf("no =>")
	}
	r := Route{Match: map[string]string{}}
	for _, pair := range strings.Split(lhs, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return Route{}, fmt.Errorf("bad match")
		}
		if _, err := path.Match(v, ""); err != nil {
			return Route{}, err
		}
		r.Match[k] = v
	}
	words := strings.Fields(rhs)
	if len(words) == 0 {
		return Route{}, fmt.Errorf("no action")
	}
	r.Action = RouteAction(words[0])
	switch r.Action {
	case Drop, Quiet, Raise:
		if len(words) != 1 {
			return Route{}, fmt.Errorf("extra words")
		}
	case Rate:
		if len(words) != 2 || ParseSeverity(words[1]) != Severity(words[1]) {
			return Route{}, fmt.Errorf("severity needs a level")
		}
		r.Severity = Severity(words[1])
	default:
		return Route{}, fmt.Errorf("unknown action")
	}
	return r, nil
}

// FormatRoutes is the inverse of ParseRoutes.
func FormatRoutes(routes []Route) string {
	var b strings.Builder
	for _, r := range routes {
		keys := make([]string, 0, len(r.Match))
		for k := range r.Match {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k + "=" + r.Match[k])
		}
		b.WriteString(" => " + string(r.Action))
		if r.Action == Rate {
			b.WriteString(" " + string(r.Severity))
		}
		b.WriteString("\n")
	}
	return b.String()
}
