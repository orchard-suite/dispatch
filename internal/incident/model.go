// Package incident is the model: what an incident is, which states it may
// move between, and how alerts find the incident they belong to. It holds
// no storage and no transport, so every rule here is tested on its own.
package incident

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Severity comes from the alert's severity label. A person may change it.
type Severity string

const (
	Critical Severity = "critical"
	Warning  Severity = "warning"
	Info     Severity = "info"
)

// Severities is every severity, worst first. Lists sort by this order.
var Severities = []Severity{Critical, Warning, Info}

// ParseSeverity reads a label value. Anything it does not know is a warning,
// so an alert with no severity still gets a person's eyes.
func ParseSeverity(s string) Severity {
	switch Severity(strings.ToLower(strings.TrimSpace(s))) {
	case Critical:
		return Critical
	case Info:
		return Info
	default:
		return Warning
	}
}

// Rank orders severities, worst first, for sorting.
func (s Severity) Rank() int {
	for i, v := range Severities {
		if v == s {
			return i
		}
	}
	return len(Severities)
}

// State is where an incident stands. Open, then a person acknowledges it,
// then mitigates (the bleeding stopped), then resolves (restored).
type State string

const (
	Open         State = "open"
	Acknowledged State = "acknowledged"
	Mitigated    State = "mitigated"
	Resolved     State = "resolved"
)

var States = []State{Open, Acknowledged, Mitigated, Resolved}

// ErrTransition is a state change the model does not allow.
var ErrTransition = errors.New("that change is not allowed from this state")

// Transition says whether an incident may move from one state to another.
// Forward moves may skip a step (a person may resolve an open incident
// without acknowledging it). The only move back is reopening a resolved one.
func Transition(from, to State) error {
	switch {
	case from == to:
		return fmt.Errorf("%w: already %s", ErrTransition, to)
	case from == Resolved && to == Open:
		return nil
	case from == Resolved:
		return fmt.Errorf("%w: a resolved incident can only be reopened", ErrTransition)
	case to == Open:
		return fmt.Errorf("%w: only a resolved incident can be reopened", ErrTransition)
	case rank(to) > rank(from):
		return nil
	default:
		return fmt.Errorf("%w: %s does not go back to %s", ErrTransition, from, to)
	}
}

func rank(s State) int {
	for i, v := range States {
		if v == s {
			return i
		}
	}
	return -1
}

// Actor is who made a change: a person signed in on the desk, or a system
// such as an API token or Alertmanager. For is the name a system gave for
// whoever it acted for, such as the actor in an API request's body.
type Actor struct {
	Kind ActorKind
	ID   string
	For  string
}

type ActorKind string

const (
	ActorPerson ActorKind = "person"
	ActorSystem ActorKind = "system"
)

// Person is someone signed in on the desk, by email.
func Person(email string) Actor { return Actor{Kind: ActorPerson, ID: email} }

// System is a caller that is not a person: a token's name, or Alertmanager.
func System(name string) Actor { return Actor{Kind: ActorSystem, ID: name} }

// Name is how the timeline writes the actor: who it acted for, when given.
func (a Actor) Name() string {
	if a.For != "" {
		return a.For
	}
	return a.ID
}

// Source says how an incident was opened.
type Source string

const (
	SourceAlertmanager Source = "alertmanager"
	SourceAPI          Source = "api"
	SourceDesk         Source = "desk"
)

// Incident is the record a desk holds.
type Incident struct {
	ID       int64    `json:"id"`
	Title    string   `json:"title"`
	Severity Severity `json:"severity"`
	State    State    `json:"state"`
	// Service is the component the incident is on, from the mapping rules.
	Service string `json:"service"`
	// Workload is the Orrery workload the service resolved to, when known.
	Workload string `json:"workload,omitempty"`
	// GroupKey joins alerts into this incident.
	GroupKey string `json:"group_key,omitempty"`
	// Owner is the person who acknowledged it, or who it was assigned to.
	Owner  string `json:"owner,omitempty"`
	Source Source `json:"source"`
	// AutoResolve lets the last alert resolving resolve an info incident.
	AutoResolve bool `json:"auto_resolve"`
	// Public puts the title on the status page.
	Public         bool       `json:"public"`
	OpenedAt       time.Time  `json:"opened_at"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	MitigatedAt    *time.Time `json:"mitigated_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
	// LastUpdate is the newest timeline line, for the list.
	LastUpdate string `json:"last_update"`
	// FiringAlerts counts the alerts in this incident still firing.
	FiringAlerts int `json:"firing_alerts"`
	// Alerts counts every alert the incident holds, firing or not.
	Alerts int `json:"alerts"`
	// QuietSince is when its last alert stopped firing, when none fires now.
	QuietSince *time.Time `json:"quiet_since,omitempty"`
	// ClosesAt is when the quiet rule will resolve it, while it is
	// settling: unresolved, opened by alerts, and none of them firing.
	ClosesAt *time.Time `json:"closes_at,omitempty"`
}

// Settling says whether an incident's alerts have all stopped and it is
// only waiting to close: nothing for a person to work.
func (i Incident) Settling() bool { return i.ClosesAt != nil }

// Phase is the state a person reads: settling while it waits to close,
// else its state.
func (i Incident) Phase() string {
	if i.Settling() {
		return "settling"
	}
	return string(i.State)
}

// Age is how long the incident has been open, or was open.
func (i Incident) Age(now time.Time) time.Duration {
	if i.ResolvedAt != nil {
		return i.ResolvedAt.Sub(i.OpenedAt)
	}
	return now.Sub(i.OpenedAt)
}

// Alert is one Alertmanager alert as it was last seen in an incident.
type Alert struct {
	Fingerprint string            `json:"fingerprint"`
	Name        string            `json:"alertname"`
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"starts_at"`
	EndsAt      *time.Time        `json:"ends_at,omitempty"`
}

// Event is one line of the timeline.
type Event struct {
	ID         int64     `json:"id"`
	IncidentID int64     `json:"incident_id"`
	At         time.Time `json:"at"`
	Kind       EventKind `json:"kind"`
	// Actor is the person, or "dispatch" or the source for machine events.
	Actor string `json:"actor"`
	Text  string `json:"text"`
	// Public puts a note on the status page.
	Public bool `json:"public"`
}

type EventKind string

const (
	EventOpened        EventKind = "opened"
	EventAlertFiring   EventKind = "alert_firing"
	EventAlertResolved EventKind = "alert_resolved"
	EventAcknowledged  EventKind = "acknowledged"
	EventMitigated     EventKind = "mitigated"
	EventResolved      EventKind = "resolved"
	EventReopened      EventKind = "reopened"
	EventAssigned      EventKind = "assigned"
	EventSeverity      EventKind = "severity"
	EventNote          EventKind = "note"
	EventPostmortem    EventKind = "postmortem"
	EventAction        EventKind = "action"
	EventPublic        EventKind = "public"
)

// EventForState is the timeline kind a state change writes.
func EventForState(to State) EventKind {
	switch to {
	case Acknowledged:
		return EventAcknowledged
	case Mitigated:
		return EventMitigated
	case Resolved:
		return EventResolved
	default:
		return EventReopened
	}
}

// Postmortem is written after resolution, in Markdown.
type Postmortem struct {
	IncidentID int64     `json:"incident_id"`
	Body       string    `json:"body"`
	UpdatedAt  time.Time `json:"updated_at"`
	UpdatedBy  string    `json:"updated_by"`
}

// PostmortemTemplate is what the editor starts with.
const PostmortemTemplate = `## What happened

## Impact

## Timeline

## Cause

## What we change
`

// ActionItem is one thing to change after an incident.
type ActionItem struct {
	ID         int64     `json:"id"`
	IncidentID int64     `json:"incident_id"`
	Text       string    `json:"text"`
	Owner      string    `json:"owner,omitempty"`
	Done       bool      `json:"done"`
	CreatedAt  time.Time `json:"created_at"`
	// IssueURL is the issue that tracks it, when one was opened.
	IssueURL string `json:"issue_url,omitempty"`
}

// Roles an incident has, in the order the page shows them.
var Roles = []string{"commander", "comms", "scribe"}

// ValidRole says whether r is one of Roles.
func ValidRole(r string) bool {
	for _, v := range Roles {
		if v == r {
			return true
		}
	}
	return false
}

// Link is one incident tied to another: related, or merged into it.
type Link struct {
	ID    int64     `json:"id"`
	Title string    `json:"title"`
	Kind  string    `json:"kind"`
	By    string    `json:"by"`
	At    time.Time `json:"at"`
	// Into is true when this incident was merged into the other.
	Into bool `json:"into,omitempty"`
}

// GroupKey is what joins alerts into one incident: the alert's name and
// the service it mapped to. A window in the settings says how long after a
// resolution the same key reopens the incident instead of opening a new one.
func GroupKey(alertname, service string) string {
	return alertname + "|" + service
}

// Title for an incident opened by an alert: the summary annotation when
// there is one, else the alert's name.
func TitleFor(alertname string, annotations map[string]string) string {
	if s := strings.TrimSpace(annotations["summary"]); s != "" {
		return s
	}
	return alertname
}

// ShouldAutoResolve lets a machine resolve an info incident the moment its
// last alert resolves, when auto_resolve is on.
func ShouldAutoResolve(i Incident, firing int) bool {
	return i.AutoResolve && i.Severity == Info && firing == 0 && i.State != Resolved
}

// ShouldResolveQuiet lets a machine resolve an incident of any severity
// once alerts opened it, none fires now, and the last stopped at least
// window ago. A window of zero turns it off. An incident a person opened
// holds no alerts, so it never resolves this way.
func ShouldResolveQuiet(i Incident, window time.Duration, now time.Time) bool {
	at := ClosesAt(i, window)
	return at != nil && !now.Before(*at)
}

// ClosesAt is when the quiet rule resolves an incident, or nil when it
// never will: the window is off, it is resolved, a person opened it, or
// an alert of it still fires.
func ClosesAt(i Incident, window time.Duration) *time.Time {
	if window <= 0 || i.State == Resolved || i.Alerts == 0 || i.FiringAlerts > 0 || i.QuietSince == nil {
		return nil
	}
	at := i.QuietSince.Add(window)
	return &at
}
