// Package annals posts every decision dispatch makes to Annals,
// through platformkit's annals client: who opened, acknowledged,
// mitigated, resolved, or reopened which incident, and every routing
// decision about a notice: an event taken or dropped, a page started,
// moved on, acknowledged, or left unanswered, a send given up, and every
// change an admin makes to how notices are routed.
package annals

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	kit "github.com/orchard-suite/platformkit/v3/annals"
	"go.opentelemetry.io/otel/trace"

	"github.com/orchard-suite/dispatch/internal/incident"
)

// Source is the Annals source dispatch's token is issued for, and the
// target system of every event.
const Source = "dispatch"

const postFor = 15 * time.Second

type Poster struct {
	client  *kit.Client
	log     *slog.Logger
	outcome func(ctx context.Context, outcome string)
}

// New makes a poster. Without a URL or token it is off and posts nothing.
func New(url, token string, log *slog.Logger, outcome func(ctx context.Context, outcome string)) *Poster {
	return &Poster{client: kit.New(url, token, Source), log: log, outcome: outcome}
}

func (p *Poster) Enabled() bool { return p != nil && p.client != nil }

// Opened posts incident.open for an incident someone opened.
func (p *Poster) Opened(ctx context.Context, inc incident.Incident, change incident.Event, by incident.Actor) {
	p.post(ctx, "open", inc, change, by)
}

// Changed posts the move to the incident's current state:
// incident.acknowledge, incident.mitigate, incident.resolve, or
// incident.reopen.
func (p *Poster) Changed(ctx context.Context, inc incident.Incident, change incident.Event, by incident.Actor) {
	verb := map[incident.State]string{
		incident.Acknowledged: "acknowledge",
		incident.Mitigated:    "mitigate",
		incident.Resolved:     "resolve",
		incident.Open:         "reopen",
	}[inc.State]
	if verb == "" {
		p.log.ErrorContext(ctx, "annals event not built: no verb for the state", "incident", inc.ID, "state", inc.State)
		return
	}
	p.post(ctx, verb, inc, change, by)
}

func (p *Poster) post(ctx context.Context, verb string, inc incident.Incident, change incident.Event, by incident.Actor) {
	if !p.Enabled() {
		return
	}
	p.send(ctx, Event(verb, inc, change, by))
}

// Decision is one routing decision: its verb, such as page.acknowledge,
// what it was about, who made it, and the key that makes a retried post
// count once.
type Decision struct {
	Verb   string
	Kind   string
	ID     string
	Title  string
	By     incident.Actor
	Key    string
	Labels map[string]string
	At     time.Time
	// Outcome is ok unless said otherwise, such as failed for a send given up.
	Outcome string
	// Reason says why an outcome other than ok came about. The annals
	// refuses such an outcome without one.
	Reason string
	// Parties are who else was in it, such as the person who covers a
	// schedule. A person goes here, never in Title.
	Parties []kit.Party
}

// Decide posts one routing decision. It never blocks the decision.
func (p *Poster) Decide(ctx context.Context, d Decision) {
	if !p.Enabled() {
		return
	}
	p.send(ctx, DecisionEvent(d))
}

// actorOf is who acted, as Annals names them. A person signs in by
// email, which is also their id here; the email goes in its own field, so
// Annals's directory finds the person whichever id it was posted as.
func actorOf(by incident.Actor) kit.Actor {
	a := kit.Actor{Kind: string(by.Kind), ID: clip(by.ID, kit.MaxShort)}
	if by.Kind != incident.ActorPerson {
		a.OnBehalfOf = clip(by.For, kit.MaxShort)
	} else if emailRe.MatchString(by.ID) && len(by.ID) <= kit.MaxShort {
		a.Email = strings.ToLower(by.ID)
	}
	return a
}

// emailRe is an address Annals takes as an email.
var emailRe = regexp.MustCompile(`^[^\s@<>"']+@[^\s@<>"']+\.[A-Za-z]{2,}$`)

// DecisionEvent is Annals's record of one routing decision.
func DecisionEvent(d Decision) kit.Event {
	actor := actorOf(d.By)
	outcome := d.Outcome
	if outcome == "" {
		outcome = "ok"
	}
	labels := map[string]string{}
	for k, v := range d.Labels {
		labels[k] = clip(v, kit.MaxLabelLen)
	}
	return kit.Event{
		At:       d.At.UTC(),
		EventKey: clip(d.Key, kit.MaxShort),
		Actor:    actor,
		Action: kit.Action{
			Verb:    d.Verb,
			Target:  kit.Target{System: Source, Kind: d.Kind, ID: clip(d.ID, kit.MaxShort), Title: kit.Clip(d.Title)},
			Outcome: outcome,
			Reason:  kit.Clip(d.Reason),
		},
		Parties: d.Parties,
		Labels:  labels,
	}
}

// PersonParty is a person in a decision, by the email they sign in with.
func PersonParty(role, email string) kit.Party {
	p := kit.Party{Role: role, Kind: kit.PersonKind, ID: clip(email, kit.MaxShort)}
	if emailRe.MatchString(email) && len(email) <= kit.MaxShort {
		p.Email = strings.ToLower(email)
	}
	return p
}

// send posts one event in its own goroutine, so a decision never waits on
// Annals. A refusal or a failure is logged and dropped: Annals
// would refuse a refused event again, and the decision already stands.
func (p *Poster) send(ctx context.Context, ev kit.Event) {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		ev.TraceID = sc.TraceID().String()
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, postFor)
		defer cancel()
		err := p.client.Post(ctx, ev)
		switch {
		case err == nil:
			p.log.InfoContext(ctx, "annals event posted", "verb", ev.Action.Verb, "target", ev.Action.Target.ID, "event_key", ev.EventKey)
			p.outcome(ctx, "posted")
		case kit.Reason(err) != "":
			p.log.ErrorContext(ctx, "annals event refused", "verb", ev.Action.Verb, "target", ev.Action.Target.ID, "reason", kit.Reason(err), "error", err)
			p.outcome(ctx, "refused")
		default:
			p.log.WarnContext(ctx, "annals event failed", "verb", ev.Action.Verb, "target", ev.Action.Target.ID, "error", err)
			p.outcome(ctx, "failed")
		}
	}()
}

// Event is Annals's record of one decision. Its key is the timeline
// row the change wrote, so a retried post is stored once and a second
// resolve after a reopen is stored again.
func Event(verb string, inc incident.Incident, change incident.Event, by incident.Actor) kit.Event {
	actor := actorOf(by)
	return kit.Event{
		At:       change.At.UTC(),
		EventKey: fmt.Sprintf("incident:%d:%s:%d", inc.ID, verb, change.ID),
		Actor:    actor,
		Action: kit.Action{
			Verb:    "incident." + verb,
			Target:  kit.Target{System: Source, Kind: "incident", ID: fmt.Sprint(inc.ID), Title: kit.Clip(inc.Title)},
			Outcome: "ok",
		},
		Labels: map[string]string{
			"severity": string(inc.Severity),
			"service":  clip(inc.Service, kit.MaxLabelLen),
		},
	}
}

// clip drops control characters and keeps s within n bytes without
// cutting a character in half.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
