package server

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

// listView is one saved view of the Incidents list: a tab that sets a
// filter through the query.
type listView struct {
	Key, Label string
}

var listViews = []listView{
	{"", "All"}, {"open", "Open"}, {"mine", "Mine"}, {"unowned", "Unowned"},
	{"critical", "Critical"}, {"week", "This week"}, {"postmortem", "Needs postmortem"},
}

// listCap is how many incidents the list reads at most.
const listCap = 300

// unresolvedStates is every state a person still works.
var unresolvedStates = []incident.State{incident.Open, incident.Acknowledged, incident.Mitigated}

// histRow is one row of the Incidents list: an incident, or the newest of
// several with the same title on the same service in one UTC day.
type histRow struct {
	I incident.Incident
	// Count is how many the row folds; Day names their day for the count.
	Count int
	Day   string
	// Repeats is the list of every one the row folds.
	Repeats string
	// TTA and TTR are the times to acknowledge and to restore, "" without
	// one; their V fields sort, with a missing time last.
	TTA, TTR   string
	TTAv, TTRv int64
	Age        time.Duration
}

// noTime sorts after every real duration.
const noTime = int64(1 << 40)

func newHistRow(i incident.Incident, now time.Time) histRow {
	h := histRow{I: i, Count: 1, TTAv: noTime, TTRv: noTime, Age: i.Age(now)}
	if i.AcknowledgedAt != nil {
		d := i.AcknowledgedAt.Sub(i.OpenedAt)
		h.TTA, h.TTAv = humanDuration(d), int64(d.Seconds())
	}
	if i.ResolvedAt != nil {
		d := i.ResolvedAt.Sub(i.OpenedAt)
		h.TTR, h.TTRv = humanDuration(d), int64(d.Seconds())
	}
	return h
}

// histRows keeps the list's order and, when fold is set, folds repeats of
// a title on a service in one UTC day into the first of them.
func histRows(list []incident.Incident, fold bool, now time.Time) []histRow {
	out := make([]histRow, 0, len(list))
	at := map[string]int{}
	today := now.UTC().Truncate(24 * time.Hour)
	for _, i := range list {
		day := i.OpenedAt.UTC().Truncate(24 * time.Hour)
		key := day.Format("2006-01-02") + "\x00" + i.Service + "\x00" + i.Title
		if k, ok := at[key]; ok && fold {
			out[k].Count++
			continue
		}
		h := newHistRow(i, now)
		switch {
		case day.Equal(today):
			h.Day = "today"
		case day.Equal(today.Add(-24 * time.Hour)):
			h.Day = "yesterday"
		default:
			h.Day = "on " + day.Format("Jan 2")
		}
		h.Repeats = "/history?" + url.Values{"q": {i.Title}, "service": {i.Service}, "fold": {"0"}}.Encode()
		at[key] = len(out)
		out = append(out, h)
	}
	return out
}

// boardCol is one column of the board: the incidents in one phase.
type boardCol struct {
	Phase string
	Rows  []histRow
	// More is how many the column leaves out; N counts them all.
	More, N int
}

func histBoard(rows []histRow) []boardCol {
	cols := []boardCol{{Phase: "open"}, {Phase: "acknowledged"}, {Phase: "mitigated"}, {Phase: "settling"}, {Phase: "resolved"}}
	for _, r := range rows {
		for k := range cols {
			if cols[k].Phase != r.I.Phase() {
				continue
			}
			// Resolved folds to a count: the board is the work still open.
			if cols[k].Phase == "resolved" {
				cols[k].More++
			} else {
				cols[k].Rows = append(cols[k].Rows, r)
			}
		}
	}
	for k := range cols {
		c := cols[k].Rows
		// Worst first, then the oldest: what has waited longest.
		sort.SliceStable(c, func(a, b int) bool {
			if ra, rb := c[a].I.Severity.Rank(), c[b].I.Severity.Rank(); ra != rb {
				return ra < rb
			}
			return c[a].I.OpenedAt.Before(c[b].I.OpenedAt)
		})
		cols[k].N = len(c) + cols[k].More
	}
	return cols
}

func (s *Server) pageHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	me := door.From(r.Context()).Email
	f := store.Filter{Search: strings.TrimSpace(q.Get("q")), Service: q.Get("service"), Severity: incident.Severity(q.Get("severity")), Limit: listCap}
	if st := q.Get("state"); st != "" {
		f.States = []incident.State{incident.State(st)}
	}
	view := q.Get("view")
	switch view {
	case "open":
		f.States = unresolvedStates
	case "mine":
		f.Owner = me
	case "unowned":
		f.Unowned = true
	case "critical":
		f.Severity = incident.Critical
	case "week":
		f.Since = now.Add(-7 * 24 * time.Hour)
	case "postmortem":
		f.States, f.NoPostmortem = []incident.State{incident.Resolved}, true
	default:
		view = ""
	}
	list, err := s.store.List(r.Context(), f)
	if s.pageFailed(w, r, err, "Incidents") {
		return
	}
	// A postmortem is owed on what a person worked, not on what closed itself.
	if view == "postmortem" {
		kept := list[:0]
		for _, i := range list {
			if i.Owner != "" {
				kept = append(kept, i)
			}
		}
		list = kept
	}
	fold := q.Get("fold") != "0"
	layout := "list"
	if q.Get("layout") == "board" {
		layout = "board"
	}
	rows := histRows(list, fold, now)
	// keep is the query without one key, for links that change only it.
	keep := func(drop ...string) string {
		v := url.Values{}
		for k, vals := range q {
			v[k] = vals
		}
		for _, d := range drop {
			v.Del(d)
		}
		if e := v.Encode(); e != "" {
			return "?" + e
		}
		return ""
	}
	with := func(k, val string) string {
		v := url.Values{}
		for key, vals := range q {
			v[key] = vals
		}
		if val == "" {
			v.Del(k)
		} else {
			v.Set(k, val)
		}
		if e := v.Encode(); e != "" {
			return "/history?" + e
		}
		return "/history"
	}
	tabs := make([]map[string]any, 0, len(listViews))
	for _, lv := range listViews {
		v := url.Values{}
		if lv.Key != "" {
			v.Set("view", lv.Key)
		}
		if layout == "board" {
			v.Set("layout", "board")
		}
		href := "/history"
		if e := v.Encode(); e != "" {
			href += "?" + e
		}
		tabs = append(tabs, map[string]any{"Key": lv.Key, "Label": lv.Label, "Href": href, "On": lv.Key == view})
	}
	s.render(w, r, http.StatusOK, "history", map[string]any{
		"Rows": rows, "Total": len(list), "Capped": len(list) == listCap, "Board": histBoard(rows), "Layout": layout, "Fold": fold,
		"View": view, "Tabs": tabs, "Query": f.Search, "Service": q.Get("service"), "Severity": q.Get("severity"), "State": q.Get("state"),
		"Severities": incident.Severities, "States": incident.States, "Filtered": f.Search != "" || q.Get("service") != "" || q.Get("severity") != "" || q.Get("state") != "",
		"ListHref": with("layout", ""), "BoardHref": with("layout", "board"), "ResolvedHref": resolvedHref(q), "FoldHref": with("fold", ""), "AllHref": with("fold", "0"),
		"NoService": "/history" + keep("service"),
	})
}

// openBar is an incident still open, drawn on its service's lane from
// when it opened to the now edge, as Home draws its estate.
type openBar struct {
	I           incident.Incident
	Left, Width float64
	// Clipped says it opened before the clock starts. Ahead puts its label
	// after its start; a bar near now has its label end where it starts.
	Clipped, Ahead bool
	Right          float64
}

// serviceLane is one service's row on the Services page: its heat cells,
// its open incidents as bars, and its numbers for the window.
type serviceLane struct {
	Lane
	Row    desk.Component
	HasRow bool
	Bars   []openBar
	// MTTAv and MTTRv sort the means in seconds, a missing one last.
	MTTAv, MTTRv int64
}

// svcClock is the Services page's clock: N whole cells of one unit, the
// same 4-hour cells as Home's estate on seven days.
type svcClock struct {
	Start, End time.Time
	Step       time.Duration
	N          int
	// Unit names a cell for the legend.
	Unit string
}

func clockFor(window string, now time.Time) svcClock {
	end := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	switch window {
	case "30d":
		return svcClock{Start: end.Add(-30 * 24 * time.Hour), End: end, Step: 24 * time.Hour, N: 30, Unit: "a day"}
	case "90d":
		return svcClock{Start: end.Add(-90 * 24 * time.Hour), End: end, Step: 72 * time.Hour, N: 30, Unit: "3 days"}
	}
	return svcClock{Start: now.Add(-stripSpan), End: now, Step: 4 * time.Hour, N: stripCells, Unit: "4 hours"}
}

func (c svcClock) pos(t time.Time) float64 {
	return min(max(float64(t.Sub(c.Start))/float64(c.End.Sub(c.Start))*100, 0), 100)
}

// ticks label the clock: each day's start on seven days, as Home does,
// and every fifth cell on the longer windows.
func (c svcClock) ticks() []Tick {
	var out []Tick
	if c.Step < 24*time.Hour {
		for d := c.Start.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour); d.Before(c.End); d = d.Add(24 * time.Hour) {
			out = append(out, Tick{Label: d.Format("Mon"), Left: c.pos(d)})
		}
		return out
	}
	for k := 0; k < c.N; k += 5 {
		t := c.Start.Add(time.Duration(k) * c.Step)
		out = append(out, Tick{Label: t.Format("Jan 2"), Left: c.pos(t)})
	}
	return out
}

// cells is one service's heat on the clock: the worst severity that fired
// in each cell, or that an incident sat open with nothing firing.
func (c svcClock) cells(list []incident.Incident, firings []store.Firing, now time.Time) []Cell {
	out := make([]Cell, c.N)
	for k := range out {
		out[k].From = c.Start.Add(time.Duration(k) * c.Step)
		out[k].Future = out[k].From.After(now)
	}
	cell := func(t time.Time) int { return int(t.Sub(c.Start) / c.Step) }
	sev := map[int64]incident.Severity{}
	for _, i := range list {
		sev[i.ID] = i.Severity
	}
	for _, f := range firings {
		s, ok := sev[f.IncidentID]
		k := cell(f.At)
		if !ok || f.At.Before(c.Start) || k >= c.N {
			continue
		}
		out[k].Count++
		if out[k].Fired == "" || s.Rank() < out[k].Fired.Rank() {
			out[k].Fired = s
		}
	}
	for _, i := range list {
		if i.State == incident.Resolved {
			continue
		}
		for k := max(cell(i.OpenedAt), 0); k <= min(cell(now), c.N-1); k++ {
			if out[k].Fired == "" {
				out[k].Quiet = true
			}
		}
	}
	return out
}

// opened counts the incidents that opened in each cell, by severity.
func (c svcClock) opened(list []incident.Incident) []DayBar {
	out := make([]DayBar, c.N)
	for k := range out {
		out[k].Day = c.Start.Add(time.Duration(k) * c.Step)
	}
	for _, i := range list {
		if i.OpenedAt.Before(c.Start) {
			continue
		}
		k := int(i.OpenedAt.Sub(c.Start) / c.Step)
		if k >= c.N {
			continue
		}
		switch i.Severity {
		case incident.Critical:
			out[k].Critical++
		case incident.Warning:
			out[k].Warning++
		default:
			out[k].Info++
		}
	}
	return out
}

// minBar is the shortest an open bar is drawn, in percent of the clock.
const minBar = 2.5

// bars are a service's open incidents on the clock, oldest first.
func (c svcClock) bars(open []incident.Incident, now time.Time) []openBar {
	sorted := append([]incident.Incident{}, open...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].OpenedAt.Before(sorted[b].OpenedAt) })
	out := make([]openBar, 0, len(sorted))
	for _, i := range sorted {
		left := c.pos(i.OpenedAt)
		right := c.pos(now)
		// A bar opened moments ago still reads as a bar: it reaches back
		// at least minBar from now.
		if right-left < minBar {
			left = max(right-minBar, 0)
		}
		out = append(out, openBar{I: i, Left: left, Width: right - left, Clipped: i.OpenedAt.Before(c.Start), Ahead: left < 70, Right: 100 - left})
	}
	return out
}

func (s *Server) pageComponents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Seven days is the default: the same clock as Home's estate.
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "7d"
	}
	comps, report, err := s.desk.Components(ctx, window)
	if s.pageFailed(w, r, err, "Services") {
		return
	}
	noise, err := s.store.NoiseSince(ctx, report.Since)
	if s.pageFailed(w, r, err, "Services") {
		return
	}
	list, err := s.store.List(ctx, store.Filter{Since: report.Since})
	if s.pageFailed(w, r, err, "Services") {
		return
	}
	open, err := s.store.List(ctx, store.Filter{States: unresolvedStates})
	if s.pageFailed(w, r, err, "Services") {
		return
	}
	now := time.Now()
	clk := clockFor(report.Window, now)
	// An incident still open from before the window still sits on its lane.
	seen := map[int64]bool{}
	for _, i := range list {
		seen[i.ID] = true
	}
	touched := append([]incident.Incident{}, list...)
	for _, i := range open {
		if !seen[i.ID] {
			touched = append(touched, i)
		}
	}
	firings, err := s.store.FiringsSince(ctx, clk.Start)
	if s.pageFailed(w, r, err, "Services") {
		return
	}
	rowOf := map[string]desk.Component{}
	for _, c := range comps {
		rowOf[c.Service] = c
	}
	bySvc := map[string][]incident.Incident{}
	for _, i := range touched {
		bySvc[i.Service] = append(bySvc[i.Service], i)
	}
	strips, _ := stripsBetween(touched, firings, clk.Start, clk.End, now)
	lanes := make([]serviceLane, 0, len(strips))
	for _, l := range strips {
		sl := serviceLane{Lane: l, Bars: clk.bars(l.Open, now)}
		sl.Cells = clk.cells(bySvc[l.Service], firings, now)
		sl.Row, sl.HasRow = rowOf[l.Service]
		sl.MTTAv, sl.MTTRv = noTime, noTime
		if sl.Row.MTTASeconds != nil {
			sl.MTTAv = int64(*sl.Row.MTTASeconds)
		}
		if sl.Row.MTTRSeconds != nil {
			sl.MTTRv = int64(*sl.Row.MTTRSeconds)
		}
		lanes = append(lanes, sl)
	}
	rows := noiseRows(noise, firings)
	loudest, anyActed := 0, false
	for _, n := range rows {
		loudest = max(loudest, n.Firings)
		anyActed = anyActed || n.Acted > 0
	}
	bars := clk.opened(touched)
	tallest := 1
	for _, b := range bars {
		tallest = max(tallest, b.Total())
	}
	// The headline names the loudest alert: the one to quiet first.
	var top *NoiseRow
	if len(rows) > 0 {
		top = &rows[0]
	}
	s.render(w, r, http.StatusOK, "components", map[string]any{
		"Lanes": lanes, "Noise": rows, "Loudest": loudest, "AnyActed": anyActed, "Bars": bars, "Tallest": tallest,
		"Ticks": clk.ticks(), "Clock": clk, "Top": top, "Report": report, "Window": report.Window,
		"Windows": []string{"7d", "30d", "90d"}, "People": report.People, "Paged": paged(report.People),
	})
}

// resolvedHref is the list of what the board folds: the same search, the
// resolved ones.
func resolvedHref(q url.Values) string {
	v := url.Values{}
	for k, vals := range q {
		if k != "layout" && k != "state" {
			v[k] = vals
		}
	}
	// A view that names its own states would hide the resolved.
	if w := v.Get("view"); w == "open" {
		v.Del("view")
	}
	v.Set("state", "resolved")
	return "/history?" + v.Encode()
}
