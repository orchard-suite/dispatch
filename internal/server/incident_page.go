package server

import (
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
)

// summaryPrefix marks a note as the incident's written summary. The page
// leads with the newest such note.
const summaryPrefix = "Summary: "

// quietGap is the shortest pause between two timeline rows that earns a
// "quiet" marker between them.
const quietGap = 2 * time.Hour

func (s *Server) pageIncident(w http.ResponseWriter, r *http.Request) {
	s.renderIncident(w, r, "overview")
}

func (s *Server) pagePostmortem(w http.ResponseWriter, r *http.Request) {
	s.renderIncident(w, r, "postmortem")
}

// renderIncident draws one incident in its frame: the title, the tabs, and
// the properties rail, with the tab's own body.
func (s *Server) renderIncident(w http.ResponseWriter, r *http.Request, tab string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.message(w, r, http.StatusNotFound, "Not found", "There is no such incident.")
		return
	}
	what := "Incident"
	if tab == "postmortem" {
		what = "Postmortem"
	}
	ctx := r.Context()
	v, err := s.view(ctx, id)
	if s.pageFailed(w, r, err, what) {
		return
	}
	changes, err := s.desk.ChangesNear(ctx, v.Incident)
	if s.pageFailed(w, r, err, what) {
		return
	}
	roles, err := s.store.Roles(ctx, id)
	if s.pageFailed(w, r, err, what) {
		return
	}
	links, err := s.store.Links(ctx, id)
	if s.pageFailed(w, r, err, what) {
		return
	}
	room, _ := s.store.TeamsChannel(ctx, id)
	assisted, err := s.store.AssistNotes(ctx, id)
	if s.pageFailed(w, r, err, what) {
		return
	}
	people, err := s.store.People(ctx)
	if s.pageFailed(w, r, err, what) {
		return
	}
	me := door.From(ctx).Email
	now := time.Now()
	folded := foldTimeline(v.Timeline)
	groups := groupAlerts(v.Alerts)
	body := incident.PostmortemTemplate
	if v.Postmortem != nil {
		body = v.Postmortem.Body
	}
	known := []string{me, v.Owner}
	for _, p := range people {
		known = append(known, p.Email)
	}
	for _, p := range roles {
		known = append(known, p)
	}
	activity, highlights := activityOf(folded)
	summary := summaryOf(v.Incident, v.Timeline, groups)
	for n, it := range activity {
		if it.Entry != nil && it.Entry.Run == nil && summary.Event != 0 && it.Entry.Event.ID == summary.Event {
			activity[n].IsSummary = true
		}
	}
	open := 0
	for _, a := range v.Actions {
		if !a.Done {
			open++
		}
	}
	s.render(w, r, http.StatusOK, "incident", map[string]any{
		"Tab": tab, "I": v.Incident, "Alerts": v.Alerts, "Timeline": v.Timeline, "Postmortem": v.Postmortem, "Actions": v.Actions,
		"Body": body, "Saved": r.URL.Query().Has("saved"), "Changes": changes,
		"Roles": roles, "RoleNames": incident.Roles, "Links": links, "About": s.desk.About(ctx, v.Incident),
		"Template": s.desk.UpdateTemplate(ctx, v.Incident), "Rooms": s.desk.HasRooms(), "Room": room,
		"Assistant": s.desk.HasAssistant(), "Assisted": assisted, "Severities": incident.Severities,
		"Groups": groups, "Summary": summary,
		"Activity": activity, "Entries": len(folded), "Highlights": highlights,
		"Phases": phasesOf(v.Incident), "Moves": movesOf(v.Incident), "Next": nextMoveWith(v.Incident, v.Postmortem != nil, v.Actions, now),
		"AddFirst": v.ResolvedAt != nil && (len(v.Actions) == 0 || open > 0),
		"Strip":    incidentStrip(v.Incident, v.Timeline, v.Alerts, changes, now), "Differ": alertsDiffer(v.Alerts),
		"People": addressBook(known), "GroupParts": strings.Split(v.GroupKey, "|"),
	})
}

// formSummary writes the incident's summary as a note marked as one, so
// the page leads with it and the timeline keeps who wrote it and when.
func (s *Server) formSummary(w http.ResponseWriter, r *http.Request) {
	s.incidentForm(w, r, "Summary", func(id int64) error {
		text := strings.TrimSpace(r.FormValue("text"))
		if text == "" {
			return fmt.Errorf("%w: a summary needs text", desk.ErrInput)
		}
		_, err := s.desk.Note(r.Context(), id, summaryPrefix+text, false, door.From(r.Context()).Email)
		return err
	})
}

// Summary is what the incident page leads with, and where it came from.
type Summary struct {
	Text string
	// From says where the text came from, for the line under it.
	From string
	By   string
	At   time.Time
	// Written is a summary someone wrote with Edit, which Edit starts from.
	Written bool
	// Event is the timeline entry the summary came from, which the
	// timeline folds to one line; zero when it came from elsewhere.
	Event int64
}

// summaryOf picks the incident's lead: the newest written summary, else
// the newest status update, else what the person who opened it wrote, else
// the newest note a person wrote, else the alert's own summary.
func summaryOf(i incident.Incident, events []incident.Event, groups []AlertGroup) Summary {
	var update *incident.Event
	for n := len(events) - 1; n >= 0; n-- {
		e := events[n]
		if e.Kind != incident.EventNote {
			continue
		}
		if t, ok := strings.CutPrefix(e.Text, summaryPrefix); ok {
			return Summary{Text: t, From: "summary", By: e.Actor, At: e.At, Written: true, Event: e.ID}
		}
		if e.Public && update == nil {
			update = &events[n]
		}
	}
	if update != nil {
		return Summary{Text: update.Text, From: "status update", By: update.Actor, At: update.At, Event: update.ID}
	}
	for _, e := range events {
		if e.Kind != incident.EventOpened || !strings.HasPrefix(e.Text, "Opened by ") {
			continue
		}
		if _, d, ok := strings.Cut(e.Text, ": "); ok && strings.TrimSpace(d) != "" {
			return Summary{Text: strings.TrimSpace(d), From: "description", By: e.Actor, At: e.At}
		}
	}
	for n := len(events) - 1; n >= 0; n-- {
		if e := events[n]; isWritten(e) {
			return Summary{Text: e.Text, From: "note", By: e.Actor, At: e.At, Event: e.ID}
		}
	}
	for _, g := range groups {
		if g.Summary != "" && g.Summary != i.Title {
			return Summary{Text: g.Summary, From: "alert", By: g.Name, At: g.Last}
		}
	}
	return Summary{}
}

// ActivityItem is one line of the Activity timeline: a day's header, a
// marker for a long quiet stretch, or an entry.
type ActivityItem struct {
	Day time.Time
	// Count is how many entries the day holds; HasHi says one of them is a
	// highlight, so the header stays when only highlights show.
	Count int
	HasHi bool
	Quiet string
	Entry *Entry
	// Hi marks a highlight: a state move, a note, a role, a status update,
	// or a merge.
	Hi bool
	// Written is a note a person wrote, shown as its own body.
	Written bool
	// IsSummary is the note the page leads with, folded to one line.
	IsSummary bool
}

// activityOf lays the folded timeline out newest first, under a header
// per UTC day, with a marker wherever two rows are far apart. It answers
// the items and how many entries are highlights.
func activityOf(folded []Entry) ([]ActivityItem, int) {
	entries := newestFirst(folded)
	out := make([]ActivityItem, 0, len(entries)+8)
	highlights := 0
	header := -1
	var prevStart time.Time
	for n := range entries {
		e := &entries[n]
		start, end := e.Event.At, e.Event.At
		if e.Run != nil {
			start, end = e.Run.From, e.Run.To
		}
		if n > 0 {
			if gap := prevStart.Sub(end); gap >= quietGap {
				out = append(out, ActivityItem{Quiet: humanDuration(gap) + " quiet"})
			}
		}
		day := start.UTC().Truncate(24 * time.Hour)
		if header < 0 || !out[header].Day.Equal(day) {
			out = append(out, ActivityItem{Day: day})
			header = len(out) - 1
		}
		hi := isHighlight(*e)
		if hi {
			highlights++
			out[header].HasHi = true
		}
		out[header].Count++
		out = append(out, ActivityItem{Entry: e, Hi: hi, Written: isWritten(e.Event) && e.Run == nil})
		prevStart = start
	}
	return out, highlights
}

// isHighlight says whether an entry shows under Highlights.
func isHighlight(e Entry) bool {
	if e.Run != nil {
		return false
	}
	switch e.Event.Kind {
	case incident.EventOpened, incident.EventAcknowledged, incident.EventMitigated, incident.EventResolved,
		incident.EventReopened, incident.EventAssigned, incident.EventNote:
		return true
	}
	return e.Event.Public
}

// isWritten says whether a note is a person's own words, not a line the
// desk wrote about something they did.
func isWritten(e incident.Event) bool {
	return e.Kind == incident.EventNote && strings.Contains(e.Actor, "@") && !strings.Contains(e.Text, e.Actor)
}

// Phase is one step of the phase bar.
type Phase struct {
	Name  string
	At    *time.Time
	Class string
	// Skipped says the incident passed the step without stopping there.
	Skipped bool
}

// phasesOf is the phase bar: open, acknowledged, mitigated, resolved,
// with when each was reached.
func phasesOf(i incident.Incident) []Phase {
	at := []*time.Time{&i.OpenedAt, i.AcknowledgedAt, i.MitigatedAt, i.ResolvedAt}
	cur := slices.Index(incident.States, i.State)
	out := make([]Phase, len(incident.States))
	for n, st := range incident.States {
		p := Phase{Name: capitalize(string(st))}
		switch {
		case n == cur:
			p.Class, p.At = "now", at[n]
		case n < cur:
			p.Class, p.At, p.Skipped = "done", at[n], at[n] == nil
		}
		out[n] = p
	}
	return out
}

// Move is one state change the page offers, with its key.
type Move struct {
	To      incident.State
	Label   string
	Key     string
	Primary bool
}

// movesOf is every move the incident allows, the primary one first.
func movesOf(i incident.Incident) []Move {
	primary := primaryMove(i)
	var out []Move
	for _, st := range incident.States {
		if incident.Transition(i.State, st) != nil {
			continue
		}
		m := Move{To: st, Primary: st == primary}
		switch st {
		case incident.Open:
			m.Label, m.Key = "Reopen", "O"
		case incident.Acknowledged:
			m.Label, m.Key = "Acknowledge", "A"
		case incident.Mitigated:
			m.Label, m.Key = "Mitigate", "M"
		case incident.Resolved:
			m.Label, m.Key = "Resolve", "R"
		}
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b Move) int {
		if a.Primary == b.Primary {
			return 0
		}
		if a.Primary {
			return -1
		}
		return 1
	})
	return out
}

// NextMove is the sentence that says what to do now, long for the page
// and short for the phone's action bar.
type NextMove struct {
	Say, Short string
	// Hot says nobody holds what still needs a person.
	Hot bool
}

// nextMoveOf is the next move for an incident whose action items are not
// at hand.
func nextMoveOf(i incident.Incident, written bool, now time.Time) NextMove {
	return nextMoveWith(i, written, nil, now)
}

// nextMoveWith is the next move, told from the postmortem and its action
// items once the incident is resolved.
func nextMoveWith(i incident.Incident, written bool, actions []incident.ActionItem, now time.Time) NextMove {
	owner := personName(i.Owner)
	switch {
	case i.Settling():
		return NextMove{
			Say:   fmt.Sprintf("Quiet for %s. It closes itself on %s UTC unless an alert fires.", humanDuration(now.Sub(*i.QuietSince)), i.ClosesAt.UTC().Format("Jan 2 at 15:04")),
			Short: "Quiet now. It closes itself unless an alert fires.",
		}
	case i.State == incident.Resolved:
		say, short := fmt.Sprintf("Restored in %s. ", humanDuration(i.Age(now))), "Restored. "
		if i.FiringAlerts > 0 {
			say = fmt.Sprintf("Resolved while %d of its alerts still fire; reopen it if the problem is back. ", i.FiringAlerts)
			short = fmt.Sprintf("Resolved, %d still firing. ", i.FiringAlerts)
		}
		open := 0
		for _, a := range actions {
			if !a.Done {
				open++
			}
		}
		switch {
		case written && open == 1:
			return NextMove{Say: say + "Close out the last open action item.", Short: short + "Close out 1 action item."}
		case written && open > 1:
			return NextMove{Say: say + fmt.Sprintf("Close out its %d open action items.", open), Short: short + fmt.Sprintf("Close out %d action items.", open)}
		case written && len(actions) > 0:
			return NextMove{Say: say + "The postmortem and every action item are done.", Short: short + "Postmortem and action items done."}
		case written:
			return NextMove{Say: say + "The postmortem is written; add the action items.", Short: short + "Add action items."}
		}
		return NextMove{Say: say + "Write the postmortem and its action items.", Short: short + "Write the postmortem."}
	case i.State == incident.Mitigated:
		return NextMove{Say: "The impact stopped. Resolve it once the service is restored.", Short: "Impact stopped. Resolve once restored."}
	case i.State == incident.Acknowledged:
		return NextMove{Say: owner + " is on it. Mitigate once the impact stops.", Short: owner + " is on it. Mitigate when impact stops."}
	case i.FiringAlerts > 0:
		return NextMove{
			Say:   fmt.Sprintf("%d firing and nobody on it. Acknowledge to take it.", i.FiringAlerts),
			Short: fmt.Sprintf("%d firing, nobody on it.", i.FiringAlerts), Hot: i.Owner == "",
		}
	case i.QuietSince != nil:
		q := humanDuration(now.Sub(*i.QuietSince))
		return NextMove{Say: "Quiet for " + q + ". Resolve it if it is fixed, or acknowledge to take it.", Short: "Quiet for " + q + ". Resolve or take it."}
	}
	return NextMove{Say: "Nobody holds it. Acknowledge to take it.", Short: "Nobody holds it yet.", Hot: i.Owner == ""}
}

// AxisTick is one label on the alert strip's time axis.
type AxisTick struct {
	Left  float64
	Label string
}

// ribbonTicks labels the strip's axis at a step that gives a handful of
// labels: minutes for an incident minutes old, days for one days old.
func ribbonTicks(i incident.Incident, now time.Time) []AxisTick {
	from, to := i.OpenedAt.UTC(), now.UTC()
	if i.ResolvedAt != nil {
		to = i.ResolvedAt.UTC()
	}
	span := to.Sub(from)
	if span <= 0 {
		return nil
	}
	step, format := 24*time.Hour, "Jan 2"
	if span < 36*time.Hour {
		format = "15:04"
		for _, m := range []time.Duration{1, 2, 5, 10, 15, 30, 60, 120, 180, 360, 720} {
			if step = m * time.Minute; span/step <= 7 {
				break
			}
		}
	} else {
		for span/step > 8 {
			step += 24 * time.Hour
		}
	}
	var out []AxisTick
	for t := from.Truncate(step).Add(step); t.Before(to); t = t.Add(step) {
		left := float64(t.Sub(from)) / float64(span) * 100
		if left < 6 || left > 92 {
			continue
		}
		out = append(out, AxisTick{Left: left, Label: t.Format(format)})
	}
	return out
}

// stripOf is the incident's ribbon with what the timeline cannot say: the
// alert that opened it fired from the opening, and alerts firing now fire
// until now.
func stripOf(i incident.Incident, events []incident.Event, alerts []incident.Alert, changes []incident.Change, now time.Time) Ribbon {
	r := incidentRibbon(i, events, now).withChanges(changes)
	span := float64(r.To.Sub(r.From))
	if span <= 0 {
		return r
	}
	pos := func(t time.Time) float64 { return min(max(float64(t.Sub(r.From))/span*100, 0), 100) }
	for n, b := range r.Bursts {
		if b.Run.After > 0 {
			r.Bursts[n].Fired, r.Bursts[n].Left, r.Bursts[n].Width = true, 0, pos(b.Run.To)
		}
	}
	if i.ResolvedAt == nil {
		first := now
		for _, a := range alerts {
			if a.Status == "firing" && a.StartsAt.Before(first) {
				first = a.StartsAt
			}
		}
		if first.Before(now) {
			left := pos(first)
			r.Bursts = append(r.Bursts, Stretch{Left: left, Width: 100 - left, Fired: true, Run: Run{From: first, To: now}})
		}
	}
	return r
}

// separators are where a long name may break: after these, and before an
// inner capital.
const separators = "-_/|.:"

// breakable is a long name, such as a service or an alert, with a break
// chance after each separator and before each inner capital, so it wraps
// between words and never inside one.
func breakable(s string) template.HTML {
	var b strings.Builder
	prev := rune(0)
	for _, r := range s {
		if unicode.IsUpper(r) && unicode.IsLower(prev) {
			b.WriteString("<wbr>")
		}
		b.WriteString(template.HTMLEscapeString(string(r)))
		if strings.ContainsRune(separators, r) {
			b.WriteString("<wbr>")
		}
		prev = r
	}
	return template.HTML(b.String())
}

// addressBook is everyone a page may name in a picker, once each, by name.
func addressBook(addresses []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range addresses {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" || seen[a] || !strings.Contains(a, "@") {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(personName(a), personName(b)) })
	return out
}

// alertsDiffer says how an incident's alerts differ, for its lead: "14
// KubeJobFailed alerts, differing by job_name: a, b, c and 11 more". It
// names the label with the most distinct values in each group of two or
// more; one alert, or none, answers "".
func alertsDiffer(alerts []incident.Alert) string {
	if len(alerts) < 2 {
		return ""
	}
	var parts, lone []string
	for _, g := range groupAlerts(alerts) {
		if g.Count() < 2 {
			lone = append(lone, g.Name)
			continue
		}
		label, values := differBy(g, alerts)
		if label == "" {
			parts = append(parts, fmt.Sprintf("%d %s alerts", g.Count(), g.Name))
			continue
		}
		shown := strings.Join(values[:min(3, len(values))], ", ")
		if more := len(values) - 3; more > 0 {
			shown += fmt.Sprintf(" and %d more", more)
		}
		parts = append(parts, fmt.Sprintf("%d %s alerts, differing by %s: %s", g.Count(), g.Name, label, shown))
	}
	if len(lone) > 0 {
		if len(parts) == 0 {
			return fmt.Sprintf("%d alerts: %s", len(lone), strings.Join(lone, ", "))
		}
		parts = append(parts, "and "+strings.Join(lone, ", "))
	}
	return strings.Join(parts, "; ")
}

// differBy is the label whose values differ most between a group's
// alerts, and those values in order; "" when they differ in nothing.
func differBy(g AlertGroup, alerts []incident.Alert) (string, []string) {
	label, values := "", []string{}
	for _, k := range g.Varying {
		seen := map[string]bool{}
		for _, a := range alerts {
			if v := a.Labels[k]; a.Name == g.Name && v != "" {
				seen[v] = true
			}
		}
		if len(seen) > len(values) {
			label, values = k, make([]string, 0, len(seen))
			for v := range seen {
				values = append(values, v)
			}
		}
	}
	slices.Sort(values)
	return label, values
}

// StripView is what the "ribbon" template draws: an incident's alert
// strip at its own time scale. Lanes are one per alert, or one per value
// of the label its alerts differ by; Single says one alert has fired the
// whole time, which reads as one line from Since.
type StripView struct {
	Ribbon     Ribbon
	Ticks      []AxisTick
	Lanes      []StripLane
	Single     bool
	Since      time.Time
	For        time.Duration
	HasFired   bool
	HasCleared bool
	Severity   incident.Severity
	Resolved   bool
}

// StripLane is one row of the strip.
type StripLane struct {
	Label string
	Spans []StripSpan
	// More is how many lanes this one folds, for "+N more".
	More int
}

// StripSpan is a stretch of a lane in percent of its width: firing, or a
// clear with no firing seen.
type StripSpan struct {
	Left, Width float64
	Cleared     bool
	Title       string
}

// stripLanes is the most lanes the strip draws before it folds the rest.
const stripLanes = 8

// incidentStrip is the alert strip for an incident: when each of its
// alerts fired, when it was moved or noted, and the changes near it,
// opening to now.
func incidentStrip(i incident.Incident, events []incident.Event, alerts []incident.Alert, changes []incident.Change, now time.Time) StripView {
	r := stripOf(i, events, alerts, changes, now)
	span := float64(r.To.Sub(r.From))
	pos := func(t time.Time) float64 {
		if span <= 0 {
			return 0
		}
		return min(max(float64(t.Sub(r.From))/span*100, 0), 100)
	}
	for _, e := range events {
		if isWritten(e) {
			r.Marks = append(r.Marks, Mark{Left: pos(e.At), Kind: incident.EventNote, At: e.At})
		}
	}
	v := StripView{Ribbon: r, Ticks: ribbonTicks(i, now), Severity: i.Severity, Resolved: i.ResolvedAt != nil}
	v.Lanes = alertLanes(events, alerts, r.From, r.To, pos)
	for _, l := range v.Lanes {
		for _, sp := range l.Spans {
			v.HasFired = v.HasFired || !sp.Cleared
			v.HasCleared = v.HasCleared || sp.Cleared
		}
	}
	if len(alerts) == 1 && alerts[0].Status == "firing" && i.ResolvedAt == nil && len(v.Lanes) == 1 &&
		len(v.Lanes[0].Spans) == 1 && v.Lanes[0].Spans[0].Left < 1 && !v.HasCleared {
		v.Single, v.Since = true, alerts[0].StartsAt
		if v.Since.Before(i.OpenedAt) {
			v.Since = i.OpenedAt
		}
		v.For = now.Sub(v.Since)
	}
	return v
}

// opener says whether an alert of this name opened the incident, so it
// fired from the opening.
func opener(events []incident.Event, name string) bool {
	for _, e := range events {
		if e.Kind == incident.EventOpened {
			return strings.Contains(e.Text, " "+name+" firing")
		}
	}
	return false
}

// alertLanes draws each alert's own runs: an alert alone under its name
// keeps the timeline's runs of that name; alerts that share a name get a
// lane per value of the label they differ by, each from when its alerts
// started to when they ended or now.
func alertLanes(events []incident.Event, alerts []incident.Alert, from, to time.Time, pos func(time.Time) float64) []StripLane {
	mk := func(a, b time.Time, cleared bool, title string) StripSpan {
		l := pos(a)
		return StripSpan{Left: l, Width: max(pos(b)-l, 0), Cleared: cleared, Title: title}
	}
	end := func(a incident.Alert) time.Time {
		switch {
		case a.Status == "firing":
			return to
		case a.EndsAt != nil:
			return *a.EndsAt
		}
		return a.StartsAt
	}
	changes := map[string][]incident.Event{}
	for _, e := range events {
		if e.Kind == incident.EventAlertFiring || e.Kind == incident.EventAlertResolved {
			changes[alertOf(e.Text)] = append(changes[alertOf(e.Text)], e)
		}
	}
	groups := groupAlerts(alerts)
	var lanes []StripLane
	for _, g := range groups {
		var members []incident.Alert
		for _, a := range alerts {
			if a.Name == g.Name {
				members = append(members, a)
			}
		}
		if len(members) == 1 {
			a, lane := members[0], StripLane{Label: g.Name}
			// The alert that opened the incident fired from the opening;
			// after that each firing runs to the clear that follows it.
			start, open := from, opener(events, g.Name)
			for _, e := range changes[g.Name] {
				switch {
				case e.Kind == incident.EventAlertFiring && !open:
					start, open = e.At, true
				case e.Kind == incident.EventAlertResolved && open:
					lane.Spans = append(lane.Spans, mk(start, e.At, false, span(start, e.At)))
					open = false
				case e.Kind == incident.EventAlertResolved:
					lane.Spans = append(lane.Spans, mk(e.At, e.At, true, "cleared "+clock(e.At)))
				}
			}
			if open && len(changes[g.Name]) > 0 {
				lane.Spans = append(lane.Spans, mk(start, end(a), false, "since "+clock(start)))
			}
			if len(changes[g.Name]) == 0 {
				lane.Spans = append(lane.Spans, mk(a.StartsAt, end(a), false, "since "+clock(a.StartsAt)))
			}
			lanes = append(lanes, lane)
			continue
		}
		label, _ := differBy(g, alerts)
		byValue, order := map[string]*StripLane{}, []string{}
		for _, a := range members {
			v := a.Labels[label]
			if label == "" || v == "" {
				v = g.Name
			} else if len(groups) > 1 {
				v = g.Name + " " + v
			}
			l := byValue[v]
			if l == nil {
				l = &StripLane{Label: v}
				byValue[v] = l
				order = append(order, v)
			}
			l.Spans = append(l.Spans, mk(a.StartsAt, end(a), false, a.Status+" since "+clock(a.StartsAt)))
		}
		slices.Sort(order)
		for _, v := range order {
			lanes = append(lanes, *byValue[v])
		}
	}
	if len(lanes) > stripLanes {
		rest := StripLane{More: len(lanes) - stripLanes + 1}
		rest.Label = fmt.Sprintf("+%d more", rest.More)
		for _, l := range lanes[stripLanes-1:] {
			rest.Spans = append(rest.Spans, l.Spans...)
		}
		lanes = append(lanes[:stripLanes-1], rest)
	}
	return lanes
}
