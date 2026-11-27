package incident

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"
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

// Change is a deploy or another change, kept by the service and the
// catalog entities it touched, and shown beside the incidents on them.
type Change struct {
	ID       int64  `json:"id"`
	Producer string `json:"producer"`
	// Service is the component the change is to, as a producer names it.
	// A change names its service, its things, or both.
	Service string `json:"service,omitempty"`
	// Things are the catalog entities the change touched.
	Things []change.Ref `json:"things,omitempty"`
	Kind   string       `json:"kind"`
	Title  string       `json:"title"`
	URL    string       `json:"url,omitempty"`
	By     string       `json:"by,omitempty"`
	At     time.Time    `json:"at"`
	// Run is the outside run that made the change, such as a pipeline
	// run's URL or a hub run's id.
	Run string `json:"run,omitempty"`
	// Record is the change's own record: the sender of its change.stated
	// event and that event's key. A change is kept once per record.
	Record *change.EventRef `json:"record,omitempty"`
	// Digest is the statement's digest, from its change.stated event.
	Digest string `json:"digest,omitempty"`
	// Effects are what the statement predicted the change does.
	Effects []change.Effect `json:"effects,omitempty"`
	// Route is the judge's route, from change.routed: no_one or people.
	Route string `json:"route,omitempty"`
	// Outcome is how it ended, from change.ended: ok, failed, or refused.
	Outcome string     `json:"outcome,omitempty"`
	EndedAt *time.Time `json:"ended_at,omitempty"`
	// Events are the change events kept for it, oldest first.
	Events []ChangeEvent `json:"events,omitempty"`
}

// ChangeEvent is one change event the desk kept, by its name, sender,
// key, and time.
type ChangeEvent struct {
	Event  string    `json:"event"`
	Sender string    `json:"sender"`
	Key    string    `json:"key"`
	At     time.Time `json:"at"`
}

// Name is how a change is shown: its service, or else its first thing.
func (c Change) Name() string {
	if c.Service != "" || len(c.Things) == 0 {
		return c.Service
	}
	return c.Things[0].Blueprint + ":" + c.Things[0].Key
}

// Touches says whether the change touched one of the entities.
func (c Change) Touches(entities []change.Ref) bool {
	for _, t := range c.Things {
		if slices.Contains(entities, t) {
			return true
		}
	}
	return false
}

var changeKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)

// ValidChangeKind says whether s names a kind of change, such as deploy.
func ValidChangeKind(s string) bool { return changeKind.MatchString(s) }

// ChangeKindOf is the kind of change an artifact's kind makes, such as
// terraform_plan for terraform-plan; change when it names none.
func ChangeKindOf(artifact string) string {
	k := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, strings.ToLower(artifact))
	if len(k) > 31 {
		k = k[:31]
	}
	if !ValidChangeKind(k) {
		return "change"
	}
	return k
}

// Leaning is how the desk reads, from the catalog's relations, which
// things lean on which: a thing leans on another when its trouble can come
// from the other's. Each of Relations names a relation whose source leans
// on its target, or, written "<-relation", one whose target leans on its
// source. Either may name the blueprint at the relation's source, as
// "blueprint:relation" or "<-blueprint:relation", so only edges from that
// blueprint count. Depth is how many such hops to follow.
type Leaning struct {
	Relations []string `json:"relations,omitempty"`
	Depth     int      `json:"depth,omitempty"`
}

// MaxLeanDepth is the most hops leaning is followed.
const MaxLeanDepth = 4

var relationName = regexp.MustCompile(`^(<-)?([a-z][a-z0-9_]{0,62}:)?[a-z][a-z0-9_]{0,62}$`)

// Validate checks the relations and the depth.
func (l Leaning) Validate() error {
	for _, r := range l.Relations {
		if !relationName.MatchString(r) {
			return fmt.Errorf("leans_on names relations, such as depends_on, <-service, or <-k8s_workload:workload; %q is not one", r)
		}
	}
	if l.Depth < 0 || l.Depth > MaxLeanDepth {
		return fmt.Errorf("depth is 0 to %d hops", MaxLeanDepth)
	}
	return nil
}

// Hops is how many hops to follow: Depth, or one when relations are named
// with none.
func (l Leaning) Hops() int {
	if len(l.Relations) == 0 {
		return 0
	}
	return max(l.Depth, 1)
}

// Leans says how an edge by a relation from an entity of a blueprint
// leans: forward when its source leans on its target, back when its target
// leans on its source.
func (l Leaning) Leans(from, relation string) (forward, back bool) {
	for _, r := range l.Relations {
		r, isBack := strings.CutPrefix(r, "<-")
		if bp, rel, ok := strings.Cut(r, ":"); ok {
			if bp != from {
				continue
			}
			r = rel
		}
		if r != relation {
			continue
		}
		if isBack {
			back = true
		} else {
			forward = true
		}
	}
	return forward, back
}
