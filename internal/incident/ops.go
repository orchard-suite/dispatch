package incident

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Window quiets a service for a while: an alert on a service it matches
// pages nobody, and an event about it tells no person. Service is a glob.
type Window struct {
	ID        int64     `json:"id"`
	Service   string    `json:"service"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Reason    string    `json:"reason"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate checks a window: a glob, an end after its start, at most thirty
// days long.
func (w Window) Validate() error {
	if _, err := path.Match(w.Service, ""); err != nil || strings.TrimSpace(w.Service) == "" {
		return fmt.Errorf("a window needs a service, or a glob such as shop-*")
	}
	if !w.EndsAt.After(w.StartsAt) || w.EndsAt.Sub(w.StartsAt) > 30*24*time.Hour {
		return fmt.Errorf("a window ends after it starts, at most thirty days later")
	}
	if len(w.Reason) > 300 {
		return fmt.Errorf("a reason is at most 300 characters")
	}
	return nil
}

// Covers says whether the window quiets service at t.
func (w Window) Covers(service string, t time.Time) bool {
	if t.Before(w.StartsAt) || !t.Before(w.EndsAt) {
		return false
	}
	ok, _ := path.Match(w.Service, service)
	return ok
}

// Heartbeat is a check-in a product promises every Interval; past the
// Interval and its Grace without one, the desk raises an alert.
type Heartbeat struct {
	Name      string        `json:"name"`
	Interval  time.Duration `json:"interval"`
	Grace     time.Duration `json:"grace"`
	Service   string        `json:"service"`
	Severity  Severity      `json:"severity"`
	LastAt    *time.Time    `json:"last_at,omitempty"`
	Late      bool          `json:"late"`
	CreatedBy string        `json:"created_by"`
	CreatedAt time.Time     `json:"created_at"`
}

var heartbeatName = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// Validate checks a heartbeat: a lowercase name, an interval from a
// minute to a week, a grace of at most a day.
func (h Heartbeat) Validate() error {
	switch {
	case !heartbeatName.MatchString(h.Name):
		return fmt.Errorf("a heartbeat's name is lowercase words joined by dots, dashes, or underscores")
	case h.Interval < time.Minute || h.Interval > 7*24*time.Hour:
		return fmt.Errorf("a heartbeat checks in every minute to every week")
	case h.Grace < 0 || h.Grace > 24*time.Hour:
		return fmt.Errorf("a heartbeat's grace is at most a day")
	}
	return nil
}

// Due is when the heartbeat is late without another check-in: its last
// check-in, or its making when it never checked in, plus the interval and
// grace.
func (h Heartbeat) Due() time.Time {
	from := h.CreatedAt
	if h.LastAt != nil {
		from = *h.LastAt
	}
	return from.Add(h.Interval + h.Grace)
}

// ServiceName is the heartbeat's service, or its name.
func (h Heartbeat) ServiceName() string {
	if h.Service != "" {
		return h.Service
	}
	return h.Name
}

// Change is a deploy or another change to a service, shown beside the
// service's incidents.
type Change struct {
	ID       int64     `json:"id"`
	Producer string    `json:"producer"`
	Service  string    `json:"service"`
	Kind     string    `json:"kind"`
	Title    string    `json:"title"`
	URL      string    `json:"url,omitempty"`
	By       string    `json:"by,omitempty"`
	At       time.Time `json:"at"`
}

var changeKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

// ValidChangeKind says whether s names a kind of change, such as deploy.
func ValidChangeKind(s string) bool { return changeKind.MatchString(s) }
