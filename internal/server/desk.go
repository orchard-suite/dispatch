package server

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

// stripCells is how many cells a service's week strip has: one per four hours.
const stripCells = 42

// stripSpan is how far back a strip reaches.
const stripSpan = 7 * 24 * time.Hour

// Cell is one stretch of a strip. It shows what alerts did in it: the worst
// severity that fired, or that an incident stayed open with nothing firing.
type Cell struct {
	From  time.Time
	Fired incident.Severity
	// Quiet is an incident open through the stretch with no alert firing.
	Quiet bool
	// Count is how many times alerts fired in the stretch.
	Count int
	// Future is a stretch that starts after now.
	Future bool
}

// Class is the cell's style: a severity when something fired, "quiet" when
// an incident sat open, and "" when nothing happened.
func (c Cell) Class() string {
	switch {
	case c.Future:
		return "future"
	case c.Fired != "":
		return string(c.Fired)
	case c.Quiet:
		return "quiet"
	}
	return ""
}

// Live is an open incident with its own strip.
type Live struct {
	incident.Incident
	Cells   []Cell
	Firings int
}

// Lane is one service's week: its strip, what is open on it now, and how it
// has done.
type Lane struct {
	Service   string
	Cells     []Cell
	Open      []incident.Incident
	Incidents int
	// Firings counts every time an alert on the service fired in the week.
	Firings int
	// Restore is the mean time to restore its resolved incidents, zero with none.
	Restore time.Duration
}

// Worst is the worst severity open on the lane now, or "" when none is.
func (l Lane) Worst() incident.Severity {
	if len(l.Open) == 0 {
		return ""
	}
	w := l.Open[0].Severity
	for _, i := range l.Open[1:] {
		if i.Severity.Rank() < w.Rank() {
			w = i.Severity
		}
	}
	return w
}

// strips builds a lane per service and a strip per open incident from the
// incidents that touch the week before now and the firings in it. Lanes with
// something open come first, worst first, then the busiest.
func strips(list []incident.Incident, firings []store.Firing, now time.Time) ([]Lane, []Live) {
	return stripsBetween(list, firings, now.Add(-stripSpan), now, now)
}

// stripsBetween is strips from start to end, still in stripCells cells. An
// open incident sits quiet only up to now; cells after now stay empty.
func stripsBetween(list []incident.Incident, firings []store.Firing, start, end, now time.Time) ([]Lane, []Live) {
	step := end.Sub(start) / stripCells
	cellOf := func(t time.Time) int { return int(t.Sub(start) / step) }
	fresh := func() []Cell {
		c := make([]Cell, stripCells)
		for k := range c {
			c[k].From = start.Add(time.Duration(k) * step)
			c[k].Future = c[k].From.After(now)
		}
		return c
	}
	byID := map[int64]incident.Incident{}
	byService := map[string]*Lane{}
	restores := map[string][]time.Duration{}
	live := map[int64]*Live{}
	var liveOrder []int64
	for _, i := range list {
		byID[i.ID] = i
		l := byService[i.Service]
		if l == nil {
			l = &Lane{Service: i.Service, Cells: fresh()}
			byService[i.Service] = l
		}
		l.Incidents++
		if i.State != incident.Resolved {
			l.Open = append(l.Open, i)
			live[i.ID] = &Live{Incident: i, Cells: fresh()}
			liveOrder = append(liveOrder, i.ID)
		} else if i.ResolvedAt != nil {
			restores[i.Service] = append(restores[i.Service], i.ResolvedAt.Sub(i.OpenedAt))
		}
	}
	fire := func(c *Cell, sev incident.Severity) {
		c.Count++
		if c.Fired == "" || sev.Rank() < c.Fired.Rank() {
			c.Fired = sev
		}
	}
	for _, f := range firings {
		i, ok := byID[f.IncidentID]
		k := cellOf(f.At)
		if !ok || k < 0 || k >= stripCells {
			continue
		}
		l := byService[i.Service]
		l.Firings++
		fire(&l.Cells[k], i.Severity)
		if v := live[i.ID]; v != nil {
			v.Firings++
			fire(&v.Cells[k], i.Severity)
		}
	}
	for _, id := range liveOrder {
		v := live[id]
		l := byService[v.Service]
		for k := max(cellOf(v.OpenedAt), 0); k <= min(cellOf(now), stripCells-1); k++ {
			if v.Cells[k].Fired == "" {
				v.Cells[k].Quiet = true
			}
			if l.Cells[k].Fired == "" {
				l.Cells[k].Quiet = true
			}
		}
	}
	lanes := make([]Lane, 0, len(byService))
	for name, l := range byService {
		if r := restores[name]; len(r) > 0 {
			var sum time.Duration
			for _, d := range r {
				sum += d
			}
			l.Restore = sum / time.Duration(len(r))
		}
		lanes = append(lanes, *l)
	}
	sort.Slice(lanes, func(i, j int) bool {
		a, b := lanes[i], lanes[j]
		if (len(a.Open) > 0) != (len(b.Open) > 0) {
			return len(a.Open) > 0
		}
		if wa, wb := a.Worst().Rank(), b.Worst().Rank(); len(a.Open) > 0 && wa != wb {
			return wa < wb
		}
		if a.Firings != b.Firings {
			return a.Firings > b.Firings
		}
		return a.Service < b.Service
	})
	lives := make([]Live, 0, len(liveOrder))
	for _, id := range liveOrder {
		lives = append(lives, *live[id])
	}
	// Still firing first, then worst, then oldest.
	sort.SliceStable(lives, func(i, j int) bool {
		a, b := lives[i], lives[j]
		if (a.FiringAlerts > 0) != (b.FiringAlerts > 0) {
			return a.FiringAlerts > 0
		}
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() < b.Severity.Rank()
		}
		return a.OpenedAt.Before(b.OpenedAt)
	})
	return lanes, lives
}

// Queue is the live set by who is on it.
type Queue struct {
	Nobody   []incident.Incident
	Working  []incident.Incident
	Watching []incident.Incident
}

// queue splits open incidents: nobody owns it, someone is working it, or it is
// mitigated and being watched.
func queue(open []incident.Incident) Queue {
	var q Queue
	for _, i := range open {
		switch {
		case i.State == incident.Mitigated:
			q.Watching = append(q.Watching, i)
		case i.Owner == "":
			q.Nobody = append(q.Nobody, i)
		default:
			q.Working = append(q.Working, i)
		}
	}
	return q
}

// Day is the incidents that opened on one calendar day, UTC, with repeats
// of the same title on the same service folded into one group.
type Day struct {
	Label  string
	Groups []Repeat
	Count  int
	// Critical, Warning, and Info count the day's incidents by severity.
	Critical, Warning, Info int
}

// Repeat is one incident, or several with the same title on the same service
// in a day. Latest is the newest of them.
type Repeat struct {
	Latest   incident.Incident
	Count    int
	First    time.Time
	Restored time.Duration
}

// byDay groups incidents by the day they opened, newest day first, keeping
// each day's order.
func byDay(list []incident.Incident, now time.Time) []Day {
	sorted := append([]incident.Incident{}, list...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].OpenedAt.After(sorted[j].OpenedAt) })
	var out []Day
	today := now.UTC().Truncate(24 * time.Hour)
	for _, i := range sorted {
		d := i.OpenedAt.UTC().Truncate(24 * time.Hour)
		label := d.Format("Monday, January 2")
		switch {
		case d.Equal(today):
			label = "Today"
		case d.Equal(today.Add(-24 * time.Hour)):
			label = "Yesterday"
		case d.Year() != today.Year():
			label = d.Format("January 2, 2006")
		}
		if n := len(out); n == 0 || out[n-1].Label != label {
			out = append(out, Day{Label: label})
		}
		day := &out[len(out)-1]
		day.Count++
		switch i.Severity {
		case incident.Critical:
			day.Critical++
		case incident.Warning:
			day.Warning++
		default:
			day.Info++
		}
		folded := false
		for k := range day.Groups {
			g := &day.Groups[k]
			if g.Latest.Title == i.Title && g.Latest.Service == i.Service {
				g.Count++
				g.First = i.OpenedAt
				if i.ResolvedAt != nil {
					g.Restored += i.ResolvedAt.Sub(i.OpenedAt)
				}
				folded = true
				break
			}
		}
		if !folded {
			g := Repeat{Latest: i, Count: 1, First: i.OpenedAt}
			if i.ResolvedAt != nil {
				g.Restored = i.ResolvedAt.Sub(i.OpenedAt)
			}
			day.Groups = append(day.Groups, g)
		}
	}
	for d := range out {
		for k := range out[d].Groups {
			if g := &out[d].Groups[k]; g.Count > 1 {
				g.Restored /= time.Duration(g.Count)
			}
		}
	}
	return out
}

// DayBar is one day of the outcomes chart: what opened, by severity, and how
// many of those a person acknowledged.
type DayBar struct {
	Day                     time.Time
	Critical, Warning, Info int
	Acknowledged            int
}

// Total is every incident that opened on the day.
func (d DayBar) Total() int { return d.Critical + d.Warning + d.Info }

// dayBars counts incidents by the UTC day they opened, one bar a day from
// since to now, empty days kept so the chart's spacing is time.
func dayBars(list []incident.Incident, since, now time.Time) []DayBar {
	first := since.UTC().Truncate(24 * time.Hour)
	n := int(now.UTC().Truncate(24*time.Hour).Sub(first)/(24*time.Hour)) + 1
	bars := make([]DayBar, max(n, 0))
	for k := range bars {
		bars[k].Day = first.Add(time.Duration(k) * 24 * time.Hour)
	}
	for _, i := range list {
		k := int(i.OpenedAt.UTC().Truncate(24*time.Hour).Sub(first) / (24 * time.Hour))
		if k < 0 || k >= len(bars) {
			continue
		}
		switch i.Severity {
		case incident.Critical:
			bars[k].Critical++
		case incident.Warning:
			bars[k].Warning++
		default:
			bars[k].Info++
		}
		if i.AcknowledgedAt != nil {
			bars[k].Acknowledged++
		}
	}
	return bars
}

// earliest is when the first of the incidents opened, or zero for none.
func earliest(list []incident.Incident) time.Time {
	var first time.Time
	for _, i := range list {
		if first.IsZero() || i.OpenedAt.Before(first) {
			first = i.OpenedAt
		}
	}
	return first
}

// NoiseRow is one alert name's week: how often it fired, how many incidents
// it made, and how many of those a person acted on.
type NoiseRow struct {
	Alert                     string
	Firings, Incidents, Acted int
}

// noiseRows counts firings per alert name from the timeline, where a firing
// is one alert firing, and joins the store's incidents and actions per name.
// The loudest by firings come first: that is the alert to tune.
func noiseRows(noise []store.Noise, firings []store.Firing) []NoiseRow {
	byName := map[string]*NoiseRow{}
	for _, n := range noise {
		byName[n.Alert] = &NoiseRow{Alert: n.Alert, Incidents: n.Paged, Acted: n.Acted}
	}
	for _, f := range firings {
		name := firingAlert(f)
		if name == "" {
			continue
		}
		r := byName[name]
		if r == nil {
			r = &NoiseRow{Alert: name}
			byName[name] = r
		}
		r.Firings++
	}
	out := make([]NoiseRow, 0, len(byName))
	for _, r := range byName {
		r.Firings = max(r.Firings, r.Incidents)
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Firings != out[j].Firings {
			return out[i].Firings > out[j].Firings
		}
		return out[i].Alert < out[j].Alert
	})
	return out
}

// firingAlert is the alert a firing names: "<name> firing" for an alert
// that fired, "Opened from <source>: <name> firing on <service>" for one
// that opened its incident.
func firingAlert(f store.Firing) string {
	switch f.Kind {
	case incident.EventAlertFiring:
		return alertOf(f.Text)
	case incident.EventOpened, incident.EventReopened:
		if i := strings.Index(f.Text, ": "); i >= 0 {
			return alertOf(f.Text[i+2:])
		}
	}
	return ""
}

// Tick is a date on a time axis, in percent of its width.
type Tick struct {
	Label string
	Left  float64
}

// dayTicks labels about five of n whole days from first, each at the middle
// of its day, so a chart bar and a strip cell for that day sit under it.
func dayTicks(first time.Time, n int) []Tick {
	if n <= 0 {
		return nil
	}
	out := []Tick{}
	last := -1
	for k := 0; k <= 4; k++ {
		d := (n - 1) * k / 4
		if d == last {
			continue
		}
		last = d
		label := first.Add(time.Duration(d) * 24 * time.Hour).Format("Jan 2")
		if d == n-1 {
			label = "today"
		}
		out = append(out, Tick{Label: label, Left: (float64(d) + 0.5) / float64(n) * 100})
	}
	return out
}

// openMix says how many are open and how bad: "4 critical" when they share a
// severity, "3 (2 critical, 1 warning)" when they do not.
func openMix(open []incident.Incident) string {
	counts := map[incident.Severity]int{}
	for _, i := range open {
		counts[i.Severity]++
	}
	var parts []string
	for _, sev := range incident.Severities {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, sev))
		}
	}
	switch len(parts) {
	case 0:
		return "0"
	case 1:
		return parts[0]
	}
	return fmt.Sprintf("%d (%s)", len(open), strings.Join(parts, ", "))
}
