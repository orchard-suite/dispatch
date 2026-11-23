// Package outcomes is the maths an outcomes board reads: incidents, time
// to acknowledge, and time to restore, per service over a window.
package outcomes

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
)

// Report is the JSON GET /v1/outcomes answers. It is a contract: a field
// here is never renamed, only added to.
type Report struct {
	Window   string    `json:"window"`
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Total    Row       `json:"total"`
	Services []Row     `json:"services"`
	// People is each person's on-call load over the window, most pages
	// first.
	People []Load `json:"people"`
}

// Load is one person's on-call load: the pages that told them, how many
// at night, how many they acknowledged, and their hours on call.
type Load struct {
	Person       string  `json:"person"`
	Pages        int     `json:"pages"`
	NightPages   int     `json:"night_pages"`
	Acknowledged int     `json:"acknowledged"`
	HoursOnCall  float64 `json:"hours_on_call"`
}

// Page is one page as the numbers count it.
type Page struct {
	Service   string
	StartedAt time.Time
	Answered  bool
	AckBy     string
	Told      []string
}

// AddPages counts the pages started in the window on each service's row
// and the total, and builds each person's load from the pages that told
// them and their hours on call. night says whether a time is at night.
func AddPages(r *Report, pages []Page, hours map[string]float64, night func(time.Time) bool) {
	rows := map[string]*Row{}
	for i := range r.Services {
		rows[r.Services[i].Service] = &r.Services[i]
	}
	people := map[string]*Load{}
	load := func(p string) *Load {
		if people[p] == nil {
			people[p] = &Load{Person: p}
		}
		return people[p]
	}
	for _, p := range pages {
		if p.StartedAt.Before(r.Since) || !p.StartedAt.Before(r.Until) {
			continue
		}
		atNight := night(p.StartedAt)
		targets := []*Row{&r.Total}
		if p.Service != "" {
			if rows[p.Service] == nil {
				r.Services = append(r.Services, Row{Service: p.Service, BySeverity: map[string]int{}})
				rows = map[string]*Row{}
				for i := range r.Services {
					rows[r.Services[i].Service] = &r.Services[i]
				}
			}
			targets = append(targets, rows[p.Service])
		}
		for _, row := range targets {
			row.Pages++
			if atNight {
				row.NightPages++
			}
			if !p.Answered {
				row.UnansweredPages++
			}
		}
		for _, who := range p.Told {
			l := load(who)
			l.Pages++
			if atNight {
				l.NightPages++
			}
		}
		if p.AckBy != "" {
			load(p.AckBy).Acknowledged++
		}
	}
	for who, h := range hours {
		load(who).HoursOnCall = h
	}
	sortServices(r.Services)
	r.People = make([]Load, 0, len(people))
	for _, l := range people {
		r.People = append(r.People, *l)
	}
	sort.Slice(r.People, func(i, j int) bool {
		if r.People[i].Pages != r.People[j].Pages {
			return r.People[i].Pages > r.People[j].Pages
		}
		return r.People[i].Person < r.People[j].Person
	})
}

// Row is one service's numbers. MTTA and MTTR are means in seconds over
// the incidents that have the figure; null when none do.
type Row struct {
	Service string `json:"service,omitempty"`
	// Incidents opened in the window.
	Incidents int `json:"incidents"`
	// Open is how many of them are not yet resolved.
	Open int `json:"open"`
	// BySeverity counts the opened incidents by severity.
	BySeverity map[string]int `json:"by_severity"`
	// Acknowledged and Resolved count the incidents behind MTTA and MTTR.
	Acknowledged int      `json:"acknowledged"`
	Resolved     int      `json:"resolved"`
	MTTASeconds  *float64 `json:"mtta_seconds"`
	MTTRSeconds  *float64 `json:"mttr_seconds"`
	// TTRTrend is the restore time of the window's second half against its
	// first: "improving", "steady", "worsening", or "" with too little data.
	TTRTrend string `json:"ttr_trend,omitempty"`
	// Pages counts the chains' pages started in the window, NightPages
	// those started between 22:00 and 07:00 in the desk's zone, and
	// UnansweredPages those nobody acknowledged.
	Pages           int `json:"pages"`
	NightPages      int `json:"night_pages"`
	UnansweredPages int `json:"unanswered_pages"`
}

var ErrWindow = errors.New("window must be a number of hours or days, such as 24h or 30d, up to 365d")

// ParseWindow reads "30d" or "24h". The default is thirty days.
func ParseWindow(s string) (time.Duration, string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 30 * 24 * time.Hour, "30d", nil
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 {
		return 0, "", ErrWindow
	}
	var d time.Duration
	switch s[len(s)-1] {
	case 'd':
		d = time.Duration(n) * 24 * time.Hour
	case 'h':
		d = time.Duration(n) * time.Hour
	default:
		return 0, "", ErrWindow
	}
	if d > 365*24*time.Hour {
		return 0, "", ErrWindow
	}
	return d, s, nil
}

// Build computes the report over incidents opened in [since, until).
func Build(incidents []incident.Incident, window string, since, until time.Time) Report {
	r := Report{Window: window, Since: since, Until: until, Services: []Row{}, People: []Load{}}
	perService := map[string]*acc{}
	total := &acc{}
	for _, i := range incidents {
		if i.OpenedAt.Before(since) || !i.OpenedAt.Before(until) {
			continue
		}
		a := perService[i.Service]
		if a == nil {
			a = &acc{}
			perService[i.Service] = a
		}
		a.add(i)
		total.add(i)
	}
	r.Total = total.row("", since, until)
	for service, a := range perService {
		r.Services = append(r.Services, a.row(service, since, until))
	}
	sortServices(r.Services)
	return r
}

// sortServices puts the most incidents first, then by name.
func sortServices(rows []Row) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Incidents != rows[j].Incidents {
			return rows[i].Incidents > rows[j].Incidents
		}
		return rows[i].Service < rows[j].Service
	})
}

type acc struct {
	incidents, open int
	bySeverity      map[string]int
	tta, ttr        []float64
	ttrAt           []time.Time
}

func (a *acc) add(i incident.Incident) {
	if a.bySeverity == nil {
		a.bySeverity = map[string]int{}
	}
	a.incidents++
	a.bySeverity[string(i.Severity)]++
	if i.State != incident.Resolved {
		a.open++
	}
	if i.AcknowledgedAt != nil {
		a.tta = append(a.tta, i.AcknowledgedAt.Sub(i.OpenedAt).Seconds())
	}
	if i.ResolvedAt != nil {
		a.ttr = append(a.ttr, i.ResolvedAt.Sub(i.OpenedAt).Seconds())
		a.ttrAt = append(a.ttrAt, i.OpenedAt)
	}
}

func (a *acc) row(service string, since, until time.Time) Row {
	return Row{
		Service:      service,
		Incidents:    a.incidents,
		Open:         a.open,
		BySeverity:   a.bySeverity,
		Acknowledged: len(a.tta),
		Resolved:     len(a.ttr),
		MTTASeconds:  mean(a.tta),
		MTTRSeconds:  mean(a.ttr),
		TTRTrend:     trend(a.ttr, a.ttrAt, since.Add(until.Sub(since)/2)),
	}
}

func mean(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	m := sum / float64(len(xs))
	return &m
}

// trend compares the mean restore time after the midpoint with the one
// before. Twenty percent either way is a move; less is steady. It needs
// two restored incidents on each side to say anything.
func trend(ttr []float64, at []time.Time, mid time.Time) string {
	var before, after []float64
	for i, v := range ttr {
		if at[i].Before(mid) {
			before = append(before, v)
		} else {
			after = append(after, v)
		}
	}
	if len(before) < 2 || len(after) < 2 {
		return ""
	}
	b, a := *mean(before), *mean(after)
	switch {
	case b == 0:
		return "steady"
	case a < b*0.8:
		return "improving"
	case a > b*1.2:
		return "worsening"
	default:
		return "steady"
	}
}

// Describe is the sentence the desk shows under a number.
func Describe(seconds *float64) string {
	if seconds == nil {
		return "no data"
	}
	d := time.Duration(*seconds * float64(time.Second)).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("%dh %dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		if h := int(d.Hours()) % 24; h > 0 {
			return fmt.Sprintf("%dd %dh", int(d.Hours())/24, h)
		}
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}
