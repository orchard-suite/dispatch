package notice

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Weight is how hard an event pushes on the people it reaches. A notice
// waits in their list; notify reaches them in hours they keep; a page
// works up its chain until someone acknowledges, quiet hours or not.
// Channels take every weight their subscriptions take.
type Weight string

const (
	WeightNotice Weight = "notice"
	WeightNotify Weight = "notify"
	WeightPage   Weight = "page"
)

// Weights are every weight, lightest first.
var Weights = []Weight{WeightNotice, WeightNotify, WeightPage}

// ParseWeight reads a weight's word; empty is none.
func ParseWeight(s string) (Weight, error) {
	switch w := Weight(strings.TrimSpace(s)); w {
	case "", WeightNotice, WeightNotify, WeightPage:
		return w, nil
	}
	return "", fmt.Errorf("%w: weight is notice, notify, or page", ErrInvalid)
}

// Move is what an event does to the thread its dedup key names: the
// first trigger opens it, a later one adds to it without telling anyone
// again, acknowledge ends its pages, and resolve closes it, so the next
// trigger opens a new one.
type Move string

const (
	Trigger     Move = "trigger"
	Acknowledge Move = "acknowledge"
	Resolve     Move = "resolve"
)

// ParseMove reads a move's word; empty is trigger.
func ParseMove(s string) (Move, error) {
	switch a := Move(strings.TrimSpace(s)); a {
	case "":
		return Trigger, nil
	case Trigger, Acknowledge, Resolve:
		return a, nil
	}
	return "", fmt.Errorf("%w: action is trigger, acknowledge, or resolve", ErrInvalid)
}

// ThreadState is where a thread is.
type ThreadState string

const (
	ThreadOpen         ThreadState = "open"
	ThreadAcknowledged ThreadState = "acknowledged"
	ThreadResolved     ThreadState = "resolved"
)

// Thread is the events one producer sent under one dedup key, from the
// trigger that opened it to the resolve that closed it.
type Thread struct {
	ID             int64       `json:"id"`
	Producer       string      `json:"producer"`
	DedupKey       string      `json:"dedup_key"`
	State          ThreadState `json:"state"`
	Title          string      `json:"title"`
	Events         int         `json:"events"`
	OpenedAt       time.Time   `json:"opened_at"`
	AcknowledgedAt *time.Time  `json:"acknowledged_at,omitempty"`
	AcknowledgedBy string      `json:"acknowledged_by,omitempty"`
	ResolvedAt     *time.Time  `json:"resolved_at,omitempty"`
}

// RuleStep is one medium of a rule and how many minutes after the event
// it is sent. A later step of a page is sent only while the page waits.
type RuleStep struct {
	Medium Medium `json:"medium"`
	After  int    `json:"after_minutes"`
}

// Rules are a person's steps for each weight. A weight with no entry
// takes the default: a page every medium the person has at once, notify
// every one but SMS at once, and a notice none.
type Rules map[Weight][]RuleStep

// Validate checks every step: a known medium, 0 to 240 minutes, at most
// eight steps a weight.
func (r Rules) Validate() error {
	for w, steps := range r {
		if _, err := ParseWeight(string(w)); err != nil || w == "" {
			return fmt.Errorf("%w: rules are for notice, notify, or page", ErrInvalid)
		}
		if len(steps) > 8 {
			return fmt.Errorf("%w: at most eight steps for %s", ErrInvalid, w)
		}
		for _, s := range steps {
			if _, ok := ParseMedium(string(s.Medium)); !ok {
				return fmt.Errorf("%w: %q is not a medium", ErrInvalid, s.Medium)
			}
			if s.After < 0 || s.After > 240 {
				return fmt.Errorf("%w: a step waits 0 to 240 minutes", ErrInvalid)
			}
		}
	}
	return nil
}

// For is the steps a weight takes, the default when the person set none.
func (r Rules) For(w Weight, p Person) []RuleStep {
	if steps, ok := r[w]; ok {
		return steps
	}
	var out []RuleStep
	for _, m := range Media {
		if !p.Has(m) || (w == WeightNotify && m == SMS) || w == WeightNotice {
			continue
		}
		out = append(out, RuleStep{Medium: m})
	}
	return out
}

// ParseRule reads one weight's steps from a line such as "teams, sms+5",
// a medium with an optional wait in minutes after a plus. "none" is no
// steps at all.
func ParseRule(line string) ([]RuleStep, error) {
	line = strings.TrimSpace(line)
	if line == "none" {
		return []RuleStep{}, nil
	}
	var out []RuleStep
	for _, word := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
		name, wait, _ := strings.Cut(word, "+")
		m, ok := ParseMedium(name)
		if !ok {
			return nil, fmt.Errorf("%w: %q is not teams, discord, sms, or email", ErrInvalid, name)
		}
		s := RuleStep{Medium: m}
		if wait != "" {
			n, err := strconv.Atoi(wait)
			if err != nil {
				return nil, fmt.Errorf("%w: %q waits a whole number of minutes", ErrInvalid, word)
			}
			s.After = n
		}
		out = append(out, s)
	}
	return out, nil
}

// FormatRule writes steps the way ParseRule reads them.
func FormatRule(steps []RuleStep) string {
	if len(steps) == 0 {
		return "none"
	}
	words := make([]string, 0, len(steps))
	for _, s := range steps {
		w := string(s.Medium)
		if s.After > 0 {
			w += "+" + strconv.Itoa(s.After)
		}
		words = append(words, w)
	}
	return strings.Join(words, ", ")
}

// Quiet is the hours a person is told of nothing but a page, in their
// own time zone. A message that falls in them waits until they end.
type Quiet struct {
	From string `json:"from"`
	To   string `json:"to"`
	Zone string `json:"zone"`
}

func clock(s string) (int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// Validate checks the hours and the zone.
func (q Quiet) Validate() error {
	_, a := clock(q.From)
	_, b := clock(q.To)
	if !a || !b || q.From == q.To {
		return fmt.Errorf("%w: quiet hours are two different times such as 22:00 and 07:00", ErrInvalid)
	}
	if _, err := time.LoadLocation(q.Zone); err != nil || q.Zone == "" {
		return fmt.Errorf("%w: %q is not a time zone such as America/New_York", ErrInvalid, q.Zone)
	}
	return nil
}

// Until is when quiet ends if t falls in it, else t itself.
func (q *Quiet) Until(t time.Time) time.Time {
	if q == nil {
		return t
	}
	loc, err := time.LoadLocation(q.Zone)
	from, ok1 := clock(q.From)
	to, ok2 := clock(q.To)
	if err != nil || !ok1 || !ok2 {
		return t
	}
	local := t.In(loc)
	now := local.Hour()*60 + local.Minute()
	in := false
	if from < to {
		in = now >= from && now < to
	} else {
		in = now >= from || now < to
	}
	if !in {
		return t
	}
	end := time.Date(local.Year(), local.Month(), local.Day(), to/60, to%60, 0, 0, loc)
	if !end.After(local) {
		end = end.AddDate(0, 0, 1)
	}
	return end.In(t.Location())
}

// Send is one message a person's rules call for: a medium and when.
type Send struct {
	Medium Medium
	At     time.Time
}

// Plan is when a person is told of an event of weight w by each medium
// their rules give and the desk can send. A message in quiet hours waits
// until they end; a page never waits for them.
func (p Person) Plan(w Weight, can func(Medium) bool, now time.Time) []Send {
	var out []Send
	seen := map[Medium]bool{}
	for _, step := range p.Rules.For(w, p) {
		if seen[step.Medium] || !p.Has(step.Medium) || !can(step.Medium) {
			continue
		}
		seen[step.Medium] = true
		at := now.Add(time.Duration(step.After) * time.Minute)
		if w != WeightPage {
			at = p.Quiet.Until(at)
		}
		out = append(out, Send{Medium: step.Medium, At: at})
	}
	return out
}

// Outgoing is one message on its way to one person: the event, the link
// that acknowledges its page ("" for none), and the ids a medium tags it
// with so a reply or a delivery report finds it again.
type Outgoing struct {
	Person    Person
	Event     Event
	Ack       string
	PageID    int64
	MessageID int64
}
