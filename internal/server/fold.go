package server

import (
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
)

// runGap is the longest pause between two alert changes that still counts as
// one run of flapping.
const runGap = 30 * time.Minute

// Entry is one row of a folded timeline: a single event, or a run of alert
// changes that came close together.
type Entry struct {
	Event incident.Event
	Run   *Run
}

// Run is a burst of alert firing and resolving folded into one row.
type Run struct {
	From, To       time.Time
	Fired, Cleared int
	// Alert is the alert name of the first change, for the row's label.
	Alert string
	// After is how long after the incident opened a lone clear came, when the
	// row before it is the opening; zero otherwise.
	After time.Duration
}

// Changes is how many alert changes the run folds.
func (r Run) Changes() int { return r.Fired + r.Cleared }

// Span is how long the run lasted.
func (r Run) Span() time.Duration { return r.To.Sub(r.From) }

// foldTimeline folds alert changes less than runGap apart into runs, and
// keeps every other event as its own row. A lone alert change stays a run of
// one, so a row is either a person's or the desk's move, or what alerts did.
func foldTimeline(events []incident.Event) []Entry {
	events = append([]incident.Event{}, events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	out := make([]Entry, 0, len(events))
	for _, e := range events {
		flap := e.Kind == incident.EventAlertFiring || e.Kind == incident.EventAlertResolved
		if !flap {
			out = append(out, Entry{Event: e})
			continue
		}
		if n := len(out); n > 0 && out[n-1].Run != nil && e.At.Sub(out[n-1].Run.To) < runGap {
			r := out[n-1].Run
			r.To = e.At
			if e.Kind == incident.EventAlertFiring {
				r.Fired++
			} else {
				r.Cleared++
			}
			continue
		}
		r := &Run{From: e.At, To: e.At, Alert: alertOf(e.Text)}
		if e.Kind == incident.EventAlertFiring {
			r.Fired = 1
		} else {
			r.Cleared = 1
			if n := len(out); n > 0 && out[n-1].Run == nil && out[n-1].Event.Kind == incident.EventOpened {
				r.After = e.At.Sub(out[n-1].Event.At)
			}
		}
		out = append(out, Entry{Event: e, Run: r})
	}
	return out
}

// alertOf is the alert name at the head of an alert change's text, which the
// desk writes as "<name> firing" or "<name> resolved".
func alertOf(text string) string {
	for i := 0; i < len(text); i++ {
		if text[i] == ' ' {
			return text[:i]
		}
	}
	return text
}

// AlertGroup is every alert of one name in an incident.
type AlertGroup struct {
	Name     string
	Firing   int
	Resolved int
	// Summary is the newest alert's summary annotation.
	Summary string
	// Labels are the labels every alert of the group shares.
	Labels map[string]string
	// Varying names the labels that differ between the group's alerts.
	Varying []string
	Last    time.Time
}

// Count is every alert in the group.
func (g AlertGroup) Count() int { return g.Firing + g.Resolved }

// groupAlerts folds alerts by name, firing groups first, so an incident with
// sixty KubeJobFailed alerts shows one row that says sixty.
func groupAlerts(alerts []incident.Alert) []AlertGroup {
	byName := map[string]*AlertGroup{}
	order := []string{}
	varying := map[string]map[string]bool{}
	for _, a := range alerts {
		g := byName[a.Name]
		if g == nil {
			g = &AlertGroup{Name: a.Name, Labels: map[string]string{}}
			for k, v := range a.Labels {
				g.Labels[k] = v
			}
			byName[a.Name] = g
			varying[a.Name] = map[string]bool{}
			order = append(order, a.Name)
		} else {
			for k, v := range g.Labels {
				if a.Labels[k] != v {
					delete(g.Labels, k)
					varying[a.Name][k] = true
				}
			}
			for k := range a.Labels {
				if _, ok := g.Labels[k]; !ok {
					varying[a.Name][k] = true
				}
			}
		}
		if a.Status == "firing" {
			g.Firing++
		} else {
			g.Resolved++
		}
		at := a.StartsAt
		if a.EndsAt != nil {
			at = *a.EndsAt
		}
		if at.After(g.Last) {
			g.Last = at
			if s := a.Annotations["summary"]; s != "" {
				g.Summary = s
			}
		}
	}
	out := make([]AlertGroup, 0, len(order))
	for _, name := range order {
		g := byName[name]
		delete(g.Labels, "alertname")
		for k := range varying[name] {
			g.Varying = append(g.Varying, k)
		}
		sort.Strings(g.Varying)
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Firing > 0 && out[j].Firing == 0 })
	return out
}

// primaryMove is the state the incident's page offers as its main button:
// acknowledge what still fires, resolve what went quiet, settles, or was
// mitigated.
func primaryMove(i incident.Incident) incident.State {
	if i.Settling() {
		return incident.Resolved
	}
	switch i.State {
	case incident.Open:
		if i.FiringAlerts == 0 && i.Alerts > 0 {
			return incident.Resolved
		}
		return incident.Acknowledged
	case incident.Acknowledged:
		return incident.Mitigated
	case incident.Mitigated:
		return incident.Resolved
	}
	return ""
}

// clock is a moment short enough for a timeline row, in UTC: the day and
// time this year, the full date before.
func clock(t time.Time) string {
	t = t.UTC()
	if t.Year() == time.Now().UTC().Year() {
		return t.Format("Jan 2, 15:04")
	}
	return t.Format("Jan 2 2006, 15:04")
}

// Ribbon is an incident's life drawn left to right: the bursts of alert
// changes, and the moments a person moved it.
type Ribbon struct {
	From, To time.Time
	Bursts   []Stretch
	Marks    []Mark
	// Changes are deploys and other changes to the service, placed where
	// they happened; one before the incident opened sits at its left edge.
	Changes []ChangeMark
}

// ChangeMark is one change on the ribbon.
type ChangeMark struct {
	Left  float64
	Title string
	At    time.Time
}

// Has says whether the ribbon draws a kind of mark, so its legend lists only
// what is there: "fired", "cleared", or an event kind.
func (r Ribbon) Has(kind string) bool {
	for _, b := range r.Bursts {
		if (kind == "fired" && b.Fired) || (kind == "cleared" && !b.Fired) {
			return true
		}
	}
	for _, m := range r.Marks {
		if string(m.Kind) == kind {
			return true
		}
	}
	return kind == "change" && len(r.Changes) > 0
}

// withChanges places changes on the ribbon.
func (r Ribbon) withChanges(changes []incident.Change) Ribbon {
	span := float64(r.To.Sub(r.From))
	for _, c := range changes {
		left := 0.0
		if span > 0 {
			left = min(max(float64(c.At.Sub(r.From))/span*100, 0), 100)
		}
		r.Changes = append(r.Changes, ChangeMark{Left: left, Title: c.Title, At: c.At})
	}
	return r
}

// Stretch is a span of the ribbon, in percent of its width. Fired says an
// alert fired in it; a burst of clears alone is not.
type Stretch struct {
	Left, Width float64
	Fired       bool
	Run         Run
}

// Mark is one move on the ribbon.
type Mark struct {
	Left float64
	Kind incident.EventKind
	At   time.Time
}

// incidentRibbon draws the incident from when it opened to when it resolved,
// or now: each run of alert changes the timeline folds is one burst, so
// sixty flapping alerts read as the bursts they came in.
func incidentRibbon(i incident.Incident, events []incident.Event, now time.Time) Ribbon {
	end := now
	if i.ResolvedAt != nil {
		end = *i.ResolvedAt
	}
	r := Ribbon{From: i.OpenedAt, To: end}
	span := float64(end.Sub(i.OpenedAt))
	if span <= 0 {
		return r
	}
	pos := func(t time.Time) float64 {
		p := float64(t.Sub(i.OpenedAt)) / span * 100
		return min(max(p, 0), 100)
	}
	for _, e := range foldTimeline(events) {
		if e.Run != nil {
			left := pos(e.Run.From)
			r.Bursts = append(r.Bursts, Stretch{Left: left, Width: pos(e.Run.To) - left, Fired: e.Run.Fired > 0, Run: *e.Run})
			continue
		}
		switch e.Event.Kind {
		case incident.EventOpened:
			// The ribbon's left edge is the opening; a reopening is a mark.
		case incident.EventReopened, incident.EventAcknowledged, incident.EventMitigated, incident.EventResolved:
			r.Marks = append(r.Marks, Mark{Left: pos(e.Event.At), Kind: e.Event.Kind, At: e.Event.At})
		}
	}
	return r
}

// newestFirst turns a folded timeline around, so the latest move is on top.
func newestFirst(entries []Entry) []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[len(entries)-1-i] = e
	}
	return out
}

// byline is who to credit under a timeline line: a person's short name, or
// nothing when the desk did it or the line already names them.
func byline(e incident.Event) string {
	if e.Actor == "" || e.Actor == "dispatch" || strings.Contains(e.Text, e.Actor) {
		return ""
	}
	return e.Actor
}

// span says from when to when, with the date once when both are on one day.
func span(from, to time.Time) string {
	from, to = from.UTC(), to.UTC()
	if from.Year() == to.Year() && from.YearDay() == to.YearDay() {
		return clock(from) + " to " + to.Format("15:04")
	}
	return clock(from) + " to " + clock(to)
}
