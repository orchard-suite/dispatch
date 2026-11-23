package notice

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Schedule says who is on call at any moment: its layers, the later one
// winning where it has someone, and its overrides above them all.
type Schedule struct {
	Slug      string     `json:"slug"`
	Name      string     `json:"name"`
	Zone      string     `json:"zone"`
	Layers    []Layer    `json:"layers"`
	Overrides []Override `json:"overrides,omitempty"`
}

// Layer is a rotation: its members take turns of Length each, the first
// turn starting at Start. Hours, when set, keeps the layer to part of each
// day in the schedule's zone, so layers by hour follow the sun.
type Layer struct {
	Name    string        `json:"name"`
	Members []string      `json:"members"`
	Start   time.Time     `json:"start"`
	Length  time.Duration `json:"length"`
	Hours   *Hours        `json:"hours,omitempty"`
}

// Hours is part of a day, From to To, on the days given (every day when
// none), in the schedule's zone. To before From runs past midnight.
type Hours struct {
	From string         `json:"from"`
	To   string         `json:"to"`
	Days []time.Weekday `json:"days,omitempty"`
}

// Override puts one person on call for a while, above every layer: a
// cover, or one half of a swap.
type Override struct {
	ID     int64     `json:"id"`
	Person string    `json:"person"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	By     string    `json:"by"`
	Reason string    `json:"reason,omitempty"`
}

// Validate checks a schedule: a slug, a name, a zone, and one to five
// layers, each with members and a turn from an hour to eight weeks.
func (s Schedule) Validate() error {
	switch {
	case !ValidSlug(s.Slug):
		return fmt.Errorf("%w: a schedule needs a slug of lowercase words joined by hyphens", ErrInvalid)
	case strings.TrimSpace(s.Name) == "" || len(s.Name) > 80:
		return fmt.Errorf("%w: a schedule needs a name of at most 80 characters", ErrInvalid)
	case len(s.Layers) == 0 || len(s.Layers) > 5:
		return fmt.Errorf("%w: a schedule has 1 to 5 layers", ErrInvalid)
	}
	if _, err := time.LoadLocation(s.Zone); err != nil || s.Zone == "" {
		return fmt.Errorf("%w: %q is not a time zone such as America/New_York", ErrInvalid, s.Zone)
	}
	for i, l := range s.Layers {
		if len(l.Members) == 0 || len(l.Members) > 50 {
			return fmt.Errorf("%w: layer %d needs 1 to 50 members", ErrInvalid, i+1)
		}
		if l.Length < time.Hour || l.Length > 8*7*24*time.Hour {
			return fmt.Errorf("%w: layer %d's turn is an hour to eight weeks", ErrInvalid, i+1)
		}
		if l.Start.IsZero() {
			return fmt.Errorf("%w: layer %d needs a start", ErrInvalid, i+1)
		}
		if l.Hours != nil {
			_, a := clock(l.Hours.From)
			_, b := clock(l.Hours.To)
			if !a || !b || l.Hours.From == l.Hours.To {
				return fmt.Errorf("%w: layer %d's hours are two different times such as 09:00 and 17:00", ErrInvalid, i+1)
			}
		}
	}
	return nil
}

func (s Schedule) loc() *time.Location {
	loc, err := time.LoadLocation(s.Zone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// covers says whether the hours hold t in loc.
func (h *Hours) covers(t time.Time, loc *time.Location) bool {
	if h == nil {
		return true
	}
	local := t.In(loc)
	from, _ := clock(h.From)
	to, _ := clock(h.To)
	now := local.Hour()*60 + local.Minute()
	day := local.Weekday()
	if from > to && now < to {
		// The small hours belong to the day the span began.
		day = (day + 6) % 7
	}
	if len(h.Days) > 0 {
		found := false
		for _, d := range h.Days {
			if d == day {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	if from < to {
		return now >= from && now < to
	}
	return now >= from || now < to
}

// at is who the layer has on call at t, or "".
func (l Layer) at(t time.Time, loc *time.Location) string {
	if t.Before(l.Start) || !l.Hours.covers(t, loc) {
		return ""
	}
	turn := int(t.Sub(l.Start) / l.Length)
	return l.Members[turn%len(l.Members)]
}

// OnCall is who is on call at t: an override's person, else the last
// layer with someone, else nobody.
func (s Schedule) OnCall(t time.Time) string {
	for i := len(s.Overrides) - 1; i >= 0; i-- {
		o := s.Overrides[i]
		if !t.Before(o.From) && t.Before(o.To) {
			return o.Person
		}
	}
	loc := s.loc()
	for i := len(s.Layers) - 1; i >= 0; i-- {
		if p := s.Layers[i].at(t, loc); p != "" {
			return p
		}
	}
	return ""
}

// Shift is one person on call, or nobody when Person is "", for a stretch.
type Shift struct {
	Person string    `json:"person"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
}

// Shifts is who is on call from from to to, as unbroken stretches. A
// stretch with nobody is a gap in coverage.
func (s Schedule) Shifts(from, to time.Time) []Shift {
	if !to.After(from) {
		return nil
	}
	loc := s.loc()
	edges := map[time.Time]bool{from: true, to: true}
	add := func(t time.Time) {
		if t.After(from) && t.Before(to) {
			edges[t] = true
		}
	}
	for _, l := range s.Layers {
		first := l.Start
		if from.After(first) {
			first = l.Start.Add(time.Duration(from.Sub(l.Start)/l.Length) * l.Length)
		}
		for t := first; t.Before(to); t = t.Add(l.Length) {
			add(t)
		}
		if l.Hours != nil {
			for day := from.In(loc).AddDate(0, 0, -1); day.Before(to.In(loc).AddDate(0, 0, 1)); day = day.AddDate(0, 0, 1) {
				for _, hhmm := range []string{l.Hours.From, l.Hours.To} {
					m, _ := clock(hhmm)
					add(time.Date(day.Year(), day.Month(), day.Day(), m/60, m%60, 0, 0, loc))
				}
			}
		}
	}
	for _, o := range s.Overrides {
		add(o.From)
		add(o.To)
	}
	times := make([]time.Time, 0, len(edges))
	for t := range edges {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	var out []Shift
	for i := 0; i+1 < len(times); i++ {
		p := s.OnCall(times[i])
		if n := len(out); n > 0 && out[n-1].Person == p {
			out[n-1].To = times[i+1]
			continue
		}
		out = append(out, Shift{Person: p, From: times[i], To: times[i+1]})
	}
	return out
}

// Gaps is the stretches from from to to with nobody on call.
func (s Schedule) Gaps(from, to time.Time) []Shift {
	var out []Shift
	for _, sh := range s.Shifts(from, to) {
		if sh.Person == "" {
			out = append(out, sh)
		}
	}
	return out
}

// Until is when the person on call at t stops being on call, looking at
// most a week ahead.
func (s Schedule) Until(t time.Time) time.Time {
	shifts := s.Shifts(t, t.Add(7*24*time.Hour))
	if len(shifts) == 0 {
		return t
	}
	return shifts[0].To
}

// ParseLayer reads a layer from the schedule form: its members, the start
// of the first turn in the schedule's zone, the turn's length, and
// optional hours, such as members "a@example.com, b@example.com", start
// "2026-11-09 09:00", every "1w", hours "09:00-17:00 mon-fri".
func ParseLayer(name, members, start, every, hours string, loc *time.Location) (Layer, error) {
	l := Layer{Name: strings.TrimSpace(name)}
	for _, m := range strings.FieldsFunc(members, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == '\r' }) {
		email, err := NormalEmail(m)
		if err != nil {
			return l, err
		}
		l.Members = append(l.Members, email)
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(start), loc)
	if err != nil {
		return l, fmt.Errorf("%w: a layer starts at a time such as 2026-11-09 09:00", ErrInvalid)
	}
	l.Start = t
	if l.Length, err = ParseLength(every); err != nil {
		return l, err
	}
	if h := strings.TrimSpace(hours); h != "" {
		if l.Hours, err = ParseHours(h); err != nil {
			return l, err
		}
	}
	return l, nil
}

// ParseLength reads a turn's length: a number of hours, days, or weeks,
// such as 12h, 1d, or 2w.
func ParseLength(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var n int
	var unit string
	if _, err := fmt.Sscanf(s, "%d%s", &n, &unit); err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: a turn is a number of hours, days, or weeks, such as 12h, 1d, or 1w", ErrInvalid)
	}
	switch unit {
	case "h":
		return time.Duration(n) * time.Hour, nil
	case "d":
		return time.Duration(n) * 24 * time.Hour, nil
	case "w":
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("%w: a turn is a number of hours, days, or weeks, such as 12h, 1d, or 1w", ErrInvalid)
}

var weekdays = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

// ParseHours reads "09:00-17:00", optionally followed by days such as
// "mon-fri" or "sat,sun".
func ParseHours(s string) (*Hours, error) {
	bad := fmt.Errorf("%w: hours are such as 09:00-17:00 or 17:00-09:00 mon-fri", ErrInvalid)
	fields := strings.Fields(s)
	if len(fields) == 0 || len(fields) > 2 {
		return nil, bad
	}
	from, to, ok := strings.Cut(fields[0], "-")
	if _, a := clock(from); !a {
		return nil, bad
	}
	if _, b := clock(to); !b || !ok || from == to {
		return nil, bad
	}
	h := &Hours{From: from, To: to}
	if len(fields) == 2 {
		for _, part := range strings.Split(fields[1], ",") {
			a, b, span := strings.Cut(part, "-")
			first, ok1 := weekdays[a]
			if !ok1 {
				return nil, bad
			}
			if !span {
				h.Days = append(h.Days, first)
				continue
			}
			last, ok2 := weekdays[b]
			if !ok2 {
				return nil, bad
			}
			for d := first; ; d = (d + 1) % 7 {
				h.Days = append(h.Days, d)
				if d == last {
					break
				}
			}
		}
	}
	return h, nil
}

// String writes hours the way ParseHours reads them.
func (h *Hours) String() string {
	if h == nil {
		return ""
	}
	s := h.From + "-" + h.To
	if len(h.Days) == 0 {
		return s
	}
	names := []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	parts := make([]string, 0, len(h.Days))
	for _, d := range h.Days {
		parts = append(parts, names[d])
	}
	return s + " " + strings.Join(parts, ",")
}

// LengthString writes a turn's length the way ParseLength reads it.
func LengthString(d time.Duration) string {
	switch {
	case d%(7*24*time.Hour) == 0:
		return fmt.Sprintf("%dw", d/(7*24*time.Hour))
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return fmt.Sprintf("%dh", d/time.Hour)
}
