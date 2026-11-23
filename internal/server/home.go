package server

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

// Home is the triage desk: the queue of what is open, a saved view over
// it, and one incident previewed beside it, or the estate when none is.

// homeViews are the saved views over the queue, in tab order.
var homeViews = []struct{ Key, Label string }{
	{"all", "All open"}, {"mine", "Mine"}, {"unowned", "Unowned"}, {"critical", "Critical"}, {"settling", "Settling"},
}

// viewTab is one saved view's tab.
type viewTab struct {
	Key, Label, Href string
	Count            int
	On               bool
	Hot              bool
}

// homeRow is one open incident in the queue, with its next move and its
// age bar on the queue's shared clock.
type homeRow struct {
	incident.Incident
	// Move is its primary move, and Say the short line for why, as the
	// phone's queue shows them.
	Move Move
	Say  string
	Bar  ageBar
	// Paging fires with nobody on it: the queue marks it first. Needs
	// says it waits for a person, so the phone gives it its action.
	Paging bool
	Needs  bool
	On     bool
	Href   string
	// Back is where a move made from the row returns, and At is the
	// moment the page was drawn, for its clocks.
	Back string
	At   time.Time
}

// ageBar places an incident on the queue's clock, in percent of its width:
// from when it opened to now, and for one that is settling, from when its
// alerts went quiet to when it closes itself.
type ageBar struct {
	Left, Width float64
	// Clipped says it opened before the clock starts.
	Clipped bool
	// Now is where now sits on the clock.
	Now float64
	// Fires are where its alerts fired, and Moves where a person moved
	// it, on the same clock, so the row's spine carries the case file's marks.
	Fires []float64
	Moves []spineMark
	// Quiet and Rest are the settling stretch: from when its alerts went
	// quiet, for as long as it waits to close itself.
	Quiet, Rest float64
}

// spineMark is a move on a row's spine.
type spineMark struct {
	Left float64
	Kind incident.EventKind
}

// queueSection is one fold of the queue.
type queueSection struct {
	Key, Name, Why string
	Rows           []homeRow
	Hot            bool
}

// boardColumn is one state's column on the board.
type boardColumn struct {
	Name string
	Rows []homeRow
}

// queueClock is the shared time axis every row's age bar is drawn on.
type queueClock struct {
	From, To time.Time
	Now      float64
	Labels   []Tick
}

// preview is the incident in the right pane: the incident page's own
// summary, phase bar, and next move, made short.
type preview struct {
	I       incident.Incident
	Summary Summary
	Next    NextMove
	Primary Move
	Others  []Move
	// Take offers to take it: someone else holds it, or nobody does and
	// acknowledging is not the primary move.
	Take   bool
	Phases []Phase
	// Strip is the case file's alert ribbon at the incident's own scale,
	// and Differ says how its alerts differ when there are several.
	Strip  StripView
	Differ string
	Mine   bool
	// Latest are the newest timeline lines, newest first.
	// Repeated alert changes fold into one run.
	Latest []Entry
}

func (s *Server) pageIncidents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	me := door.From(ctx).Email
	q := r.URL.Query()
	open, err := s.store.List(ctx, store.Filter{States: []incident.State{incident.Open, incident.Acknowledged, incident.Mitigated}})
	if s.pageFailed(w, r, err, "Home") {
		return
	}
	week, err := s.store.List(ctx, store.Filter{Since: now.Add(-stripSpan)})
	if s.pageFailed(w, r, err, "Home") {
		return
	}
	touched := append([]incident.Incident{}, week...)
	seen := map[int64]bool{}
	for _, i := range week {
		seen[i.ID] = true
	}
	for _, i := range open {
		if !seen[i.ID] {
			touched = append(touched, i)
		}
	}
	firings, err := s.store.FiringsSince(ctx, now.Add(-stripSpan))
	if s.pageFailed(w, r, err, "Home") {
		return
	}
	noise, err := s.store.NoiseSince(ctx, now.Add(-stripSpan))
	if s.pageFailed(w, r, err, "Home") {
		return
	}
	report, err := s.desk.Outcomes(ctx, "7d")
	if s.pageFailed(w, r, err, "Home") {
		return
	}
	lanes, _ := strips(touched, firings, now)
	fired := map[int64][]time.Time{}
	for _, f := range firings {
		if f.Kind == incident.EventAlertFiring || f.Kind == incident.EventOpened || f.Kind == incident.EventReopened {
			fired[f.IncidentID] = append(fired[f.IncidentID], f.At)
		}
	}

	view := q.Get("view")
	if !validView(view) {
		view = "all"
	}
	board := q.Get("layout") == "board"
	clock := newQueueClock(open, now)
	selected := int64(0)
	none := q.Get("i") == "none"
	if id, err := strconv.ParseInt(q.Get("i"), 10, 64); err == nil {
		selected = id
	}

	var visible []homeRow
	for _, i := range open {
		if inView(i, view, me) {
			visible = append(visible, homeRow{Incident: i, Move: primaryOf(i), Say: nextMoveOf(i, false, now).Short, Bar: clock.bar(i, fired[i.ID]), Paging: paging(i), Needs: needsYou(i)})
		}
	}
	sections := sectionsOf(visible, me)
	if selected == 0 && !none {
		if rows := sections[0].Rows; len(rows) > 0 {
			selected = rows[0].ID
		}
	}
	found := false
	for _, row := range visible {
		found = found || row.ID == selected
	}
	if !found {
		selected = 0
	}
	keep := func(i int64) string { return homeHref(view, board, i) }
	back := homeHref(view, board, selected)
	mark := func(rows []homeRow) {
		for k := range rows {
			rows[k].On = rows[k].ID == selected
			rows[k].Href = keep(rows[k].ID)
			rows[k].Back, rows[k].At = back, now
		}
	}
	for k := range sections {
		mark(sections[k].Rows)
	}
	columns := boardOf(visible)
	for k := range columns {
		mark(columns[k].Rows)
	}

	tabs := make([]viewTab, 0, len(homeViews))
	for _, v := range homeViews {
		n := 0
		for _, i := range open {
			if inView(i, v.Key, me) {
				n++
			}
		}
		tabs = append(tabs, viewTab{Key: v.Key, Label: v.Label, Count: n, On: v.Key == view, Href: homeHref(v.Key, board, 0), Hot: v.Key == "unowned" && n > 0})
	}

	var pv *preview
	if selected != 0 {
		for _, row := range visible {
			if row.ID != selected {
				continue
			}
			p, err := s.previewOf(r, row.Incident, me, now)
			if s.pageFailed(w, r, err, "Home") {
				return
			}
			pv = &p
		}
	}

	noiseList := noiseRows(noise, firings)
	if len(noiseList) > 6 {
		noiseList = noiseList[:6]
	}
	top, acted := 0, 0
	for _, n := range noiseList {
		top = max(top, n.Firings)
		acted += n.Acted
	}
	var loud []Lane
	var silent []string
	for _, l := range lanes {
		if l.Firings == 0 && len(l.Open) == 0 {
			silent = append(silent, l.Service)
			continue
		}
		loud = append(loud, l)
	}
	s.render(w, r, http.StatusOK, "incidents", map[string]any{
		"Headline": headline(open, me, now), "Tabs": tabs, "View": view, "Board": board,
		"ListHref": homeHref(view, false, selected), "BoardHref": homeHref(view, true, selected), "EstateHref": homeHref(view, board, -1),
		"Sections": sections, "Folded": foldedLine(sections), "Columns": columns, "Visible": len(visible), "OpenCount": len(open), "Clock": clock,
		"Preview": pv, "Back": back,
		"Lanes": loud, "Silent": silent, "LaneBars": estateBars(loud, now), "LaneTicks": weekTicks(now),
		"Noise": noiseList, "NoiseTop": top, "NoneActed": len(noiseList) > 0 && acted == 0, "Week": report.Total,
	})
}

func validView(v string) bool {
	for _, h := range homeViews {
		if h.Key == v {
			return true
		}
	}
	return false
}

// inView says whether an open incident belongs to a saved view.
func inView(i incident.Incident, view, me string) bool {
	switch view {
	case "mine":
		return sameAddress(i.Owner, me)
	case "unowned":
		return i.Owner == "" && !i.Settling()
	case "critical":
		return i.Severity == incident.Critical
	case "settling":
		return i.Settling()
	}
	return true
}

func sameAddress(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

// needsYou is an incident a person must move: nobody holds it, or it
// still pages, firing and not acknowledged.
func needsYou(i incident.Incident) bool {
	return !i.Settling() && (i.Owner == "" || (i.State == incident.Open && i.FiringAlerts > 0))
}

// worstFirst sorts rows the way Needs you reads: paging first, then the
// worst severity, then the oldest.
func worstFirst(rows []homeRow) {
	sort.SliceStable(rows, func(a, b int) bool {
		if pa, pb := paging(rows[a].Incident), paging(rows[b].Incident); pa != pb {
			return pa
		}
		if rows[a].Severity.Rank() != rows[b].Severity.Rank() {
			return rows[a].Severity.Rank() < rows[b].Severity.Rank()
		}
		return rows[a].OpenedAt.Before(rows[b].OpenedAt)
	})
}

// paging is an incident that fires with nobody on it and nobody has
// acknowledged: the queue puts these first.
func paging(i incident.Incident) bool {
	return i.State == incident.Open && i.Owner == "" && i.FiringAlerts > 0 && !i.Settling()
}

// sectionsOf folds the queue: needs you, yours, settling, others. Needs
// you puts paging first, then worst, then oldest; settling closes soonest
// first.
func sectionsOf(rows []homeRow, me string) []queueSection {
	out := []queueSection{
		{Key: "needs", Name: "Needs you", Why: "nobody holds it, or it still pages"},
		{Key: "yours", Name: "Yours", Why: "you hold it"},
		{Key: "settling", Name: "Settling", Why: "every alert stopped; it closes itself"},
		{Key: "others", Name: "Others", Why: "someone else holds it"},
	}
	for _, row := range rows {
		k := 3
		switch {
		case row.Settling():
			k = 2
		case needsYou(row.Incident):
			k = 0
		case sameAddress(row.Owner, me):
			k = 1
		}
		out[k].Rows = append(out[k].Rows, row)
	}
	worstFirst(out[0].Rows)
	worstFirst(out[1].Rows)
	worstFirst(out[3].Rows)
	sort.SliceStable(out[2].Rows, func(a, b int) bool { return out[2].Rows[a].ClosesAt.Before(*out[2].Rows[b].ClosesAt) })
	out[0].Hot = len(out[0].Rows) > 0
	return out
}

// foldedLine names the queue's empty sections in one quiet line.
func foldedLine(sections []queueSection) string {
	var names []string
	for _, sec := range sections {
		if len(sec.Rows) == 0 {
			names = append(names, sec.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ", ") + ": none"
}

// boardOf puts the queue in columns by state, settling apart.
func boardOf(rows []homeRow) []boardColumn {
	cols := []boardColumn{{Name: "Open"}, {Name: "Acknowledged"}, {Name: "Mitigated"}, {Name: "Settling"}}
	for _, row := range rows {
		k := 0
		switch {
		case row.Settling():
			k = 3
		case row.State == incident.Acknowledged:
			k = 1
		case row.State == incident.Mitigated:
			k = 2
		}
		cols[k].Rows = append(cols[k].Rows, row)
	}
	for _, c := range cols {
		worstFirst(c.Rows)
	}
	return cols
}

// homeHref is Home with a view, a layout, and a selection: 0 keeps the
// default selection, -1 asks for none so the estate shows.
func homeHref(view string, board bool, selected int64) string {
	v := url.Values{}
	if view != "" && view != "all" {
		v.Set("view", view)
	}
	if board {
		v.Set("layout", "board")
	}
	switch {
	case selected > 0:
		v.Set("i", strconv.FormatInt(selected, 10))
	case selected < 0:
		v.Set("i", "none")
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// newQueueClock spans the queue's rows: from the oldest open incident,
// at most a week back and at least an hour, to now, and on past now as far
// as the soonest-to-be-late settling incident closes, at most a third more.
func newQueueClock(open []incident.Incident, now time.Time) queueClock {
	from := now.Add(-time.Hour)
	to := now
	for _, i := range open {
		if i.OpenedAt.Before(from) {
			from = i.OpenedAt
		}
		if i.ClosesAt != nil && i.ClosesAt.After(to) {
			to = *i.ClosesAt
		}
	}
	if from.Before(now.Add(-stripSpan)) {
		from = now.Add(-stripSpan)
	}
	if limit := now.Add(now.Sub(from) / 3); to.After(limit) {
		to = limit
	}
	c := queueClock{From: from, To: to}
	c.Now = c.at(now)
	c.Labels = []Tick{{Label: humanDuration(now.Sub(from)) + " ago", Left: 0}, {Label: "now", Left: c.Now}}
	if to.After(now.Add(time.Minute)) {
		c.Labels = append(c.Labels, Tick{Label: "+" + humanDuration(to.Sub(now)), Left: 100})
	}
	return c
}

// at is a moment's place on the clock, in percent, held inside it.
func (c queueClock) at(t time.Time) float64 {
	span := c.To.Sub(c.From)
	if span <= 0 {
		return 100
	}
	return min(max(float64(t.Sub(c.From))/float64(span)*100, 0), 100)
}

// bar is an incident's age on the clock.
func (c queueClock) bar(i incident.Incident, fired []time.Time) ageBar {
	end := c.Now
	b := ageBar{Left: c.at(i.OpenedAt), Clipped: i.OpenedAt.Before(c.From), Now: c.Now}
	// A tick a pixel or so from the last says nothing new; keep one.
	last := -1.0
	for _, t := range fired {
		if t.Before(c.From) {
			continue
		}
		if x := c.at(t); x-last >= 0.5 {
			b.Fires = append(b.Fires, x)
			last = x
		}
	}
	for _, m := range []struct {
		at   *time.Time
		kind incident.EventKind
	}{{i.AcknowledgedAt, incident.EventAcknowledged}, {i.MitigatedAt, incident.EventMitigated}} {
		if m.at != nil && !m.at.Before(c.From) {
			b.Moves = append(b.Moves, spineMark{Left: c.at(*m.at), Kind: m.kind})
		}
	}
	if i.Settling() && i.QuietSince != nil {
		b.Quiet = c.at(*i.QuietSince)
		b.Rest = max(c.at(*i.ClosesAt)-b.Quiet, 0)
		end = b.Quiet
	}
	b.Width = max(end-b.Left, 0)
	return b
}

// headline is the one sentence over the queue: what is open, how bad, and
// whether anyone is on it.
func headline(open []incident.Incident, me string, now time.Time) string {
	var working, settling, nobody []incident.Incident
	mine := 0
	var oldest time.Duration
	for _, i := range open {
		if i.Settling() {
			settling = append(settling, i)
			continue
		}
		working = append(working, i)
		oldest = max(oldest, i.Age(now))
		if i.Owner == "" {
			nobody = append(nobody, i)
		} else if sameAddress(i.Owner, me) {
			mine++
		}
	}
	// One count of open everywhere: the sentence counts every unresolved
	// incident, as the tab and the rail do, and says how many only settle.
	if len(open) == 0 {
		return "All quiet. Nothing is open."
	}
	if len(working) == 0 {
		return fmt.Sprintf("%d open, all settling: %s %s unless an alert fires. Nothing needs a person.", len(settling), plural(len(settling), "it closes", "they close"), plural(len(settling), "itself", "themselves"))
	}
	lead := fmt.Sprintf("%d open: %d %s a person (%s)", len(open), len(working), plural(len(working), "needs", "need"), severityMix(working))
	if len(settling) > 0 {
		lead += fmt.Sprintf(", %d settling", len(settling))
	}
	switch {
	case len(nobody) == len(working):
		return fmt.Sprintf("%s. Nobody is on %s; the oldest is %s old.", lead, plural(len(working), "it", "any of them"), humanDuration(oldest))
	case len(nobody) > 0:
		return fmt.Sprintf("%s. Nobody is on %d of them; the oldest is %s old.", lead, len(nobody), humanDuration(oldest))
	case mine == len(working):
		return fmt.Sprintf("%s. You hold %s.", lead, plural(len(working), "it", "every one"))
	}
	return lead + ". Every one has an owner."
}

// severityMix is "4 critical, 2 warning".
func severityMix(list []incident.Incident) string {
	counts := map[incident.Severity]int{}
	for _, i := range list {
		counts[i.Severity]++
	}
	var parts []string
	for _, sev := range incident.Severities {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, sev))
		}
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// primaryOf is the one move the incident offers first.
func primaryOf(i incident.Incident) Move {
	for _, m := range movesOf(i) {
		if m.Primary {
			return homeMove(m, i)
		}
	}
	return Move{}
}

// homeMove names a move by what it does, the same in every view on Home:
// Acknowledge acknowledges and takes it, Take only takes it.
func homeMove(m Move, i incident.Incident) Move {
	switch {
	case m.To == incident.Acknowledged:
		m.Label = "Acknowledge"
	case m.To == incident.Resolved && i.Settling():
		m.Label = "Resolve now"
	}
	return m
}

// previewOf reads what the right pane shows for one incident.
func (s *Server) previewOf(r *http.Request, i incident.Incident, me string, now time.Time) (preview, error) {
	events, err := s.store.Events(r.Context(), i.ID)
	if err != nil {
		return preview{}, err
	}
	alerts, err := s.store.Alerts(r.Context(), i.ID)
	if err != nil {
		return preview{}, err
	}
	p := preview{I: i, Mine: sameAddress(i.Owner, me), Summary: summaryOf(i, events, groupAlerts(alerts)), Next: nextMoveOf(i, false, now), Phases: phasesOf(i)}
	for _, m := range movesOf(i) {
		m = homeMove(m, i)
		switch {
		case m.Primary:
			p.Primary = m
		case m.To != incident.Open:
			p.Others = append(p.Others, m)
		}
	}
	changes, err := s.desk.ChangesNear(r.Context(), i)
	if err != nil {
		return preview{}, err
	}
	p.Strip, p.Differ = incidentStrip(i, events, alerts, changes, now), alertsDiffer(alerts)
	p.Latest = newestFirst(foldTimeline(events))
	if len(p.Latest) > 5 {
		p.Latest = p.Latest[:5]
	}
	p.Take = !p.Mine && !(p.Primary.To == incident.Acknowledged && i.Owner == "")
	return p, nil
}

// estateBar is an open incident drawn on its service's lane, from when it
// opened (or the week's start) to now.
type estateBar struct {
	ID       int64
	Severity incident.Severity
	Left     float64
	Width    float64
	Settling bool
	Clipped  bool
	Title    string
	Owner    string
	// Ahead puts the label after the bar's start; a bar near now has its
	// label before it, ending where it starts, so the label stays on the lane.
	Ahead bool
	Right float64
}

// estateBars are each lane's open incidents on the week's clock.
func estateBars(lanes []Lane, now time.Time) map[string][]estateBar {
	from := now.Add(-stripSpan)
	pos := func(t time.Time) float64 {
		return min(max(float64(t.Sub(from))/float64(stripSpan)*100, 0), 100)
	}
	out := map[string][]estateBar{}
	for _, l := range lanes {
		for _, i := range l.Open {
			left := pos(i.OpenedAt)
			out[l.Service] = append(out[l.Service], estateBar{ID: i.ID, Severity: i.Severity, Left: left, Width: 100 - left, Settling: i.Settling(), Clipped: i.OpenedAt.Before(from), Title: i.Title, Owner: i.Owner, Ahead: left < 70, Right: 100 - left})
		}
	}
	return out
}

// weekTicks label the week's clock at each day's start, UTC.
func weekTicks(now time.Time) []Tick {
	from := now.Add(-stripSpan)
	var out []Tick
	for d := from.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour); d.Before(now); d = d.Add(24 * time.Hour) {
		out = append(out, Tick{Label: d.Format("Mon"), Left: float64(d.Sub(from)) / float64(stripSpan) * 100})
	}
	return out
}
