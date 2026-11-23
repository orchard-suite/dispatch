// Package desk is the incident desk's logic: alerts become incidents,
// people move them through their states, and every change is written to
// the timeline, counted, announced, and pushed to the catalog.
package desk

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/github"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/metrics"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/notify"
	"github.com/orchard-suite/dispatch/internal/orrery"
	"github.com/orchard-suite/dispatch/internal/outbox"
	"github.com/orchard-suite/dispatch/internal/outcomes"
	"github.com/orchard-suite/dispatch/internal/store"
)

// Machine is the actor name for changes dispatch makes itself.
const Machine = "dispatch"

var (
	machine          = incident.System(Machine)
	fromAlertmanager = incident.System(string(incident.SourceAlertmanager))
)

var (
	ErrNotFound = store.ErrNotFound
	ErrInput    = errors.New("bad input")
)

type Desk struct {
	Store   *store.Store
	metrics *metrics.Metrics
	notify  *notify.Webhook
	orrery  *orrery.Client
	annals  *annals.Poster
	outbox  *outbox.Outbox
	log     *slog.Logger
	deskURL string
	// statusURL is the public status page, for its followers' links.
	statusURL string
	rooms     Rooms
	roomsFrom incident.Severity
	issues    *github.Client
	assistant Assistant
	// door is the hub's tool door, which lends investigations their data
	// tools; nil lends none.
	door *orrery.Door
	// Now is replaced in tests.
	Now func() time.Time
}

// New makes the desk. A nil outbox sends no notices. A person who
// acknowledges a page about an incident, wherever they do it, acknowledges
// the incident.
func New(st *store.Store, m *metrics.Metrics, n *notify.Webhook, o *orrery.Client, l *annals.Poster, box *outbox.Outbox, log *slog.Logger, deskURL string) *Desk {
	d := &Desk{Store: st, metrics: m, notify: n, orrery: o, annals: l, outbox: box, log: log, deskURL: strings.TrimRight(deskURL, "/"), Now: time.Now}
	if box != nil {
		box.OnAcknowledge(d.pageAcknowledged)
		box.UseHush(d.Hushed)
	}
	return d
}

// pageAcknowledged acknowledges the open incident a page was about, as the
// person who acknowledged the page.
func (d *Desk) pageAcknowledged(ctx context.Context, p notice.Page, e notice.Event, by string) {
	if e.Producer != Machine {
		return
	}
	var id, change int64
	if _, err := fmt.Sscanf(e.Key, "incident:%d:%d", &id, &change); err != nil {
		return
	}
	inc, err := d.Store.Get(ctx, d.Store.DB(), id)
	if err != nil || inc.State != incident.Open {
		return
	}
	if _, err := d.Change(ctx, id, incident.Acknowledged, incident.Person(by)); err != nil {
		d.log.ErrorContext(ctx, "incident not acknowledged from its page", "incident", id, "page", p.ID, "error", err)
		return
	}
	d.log.InfoContext(ctx, "incident acknowledged from its page", "incident", id, "page", p.ID)
}

// Annals is where decisions are posted; it posts nothing when off.
func (d *Desk) Annals() *annals.Poster { return d.annals }

// Notices is the outbox events go through; nil when the desk sends none.
func (d *Desk) Notices() *outbox.Outbox { return d.outbox }

// Stats feeds the gauges.
func (d *Desk) Stats() metrics.Stats {
	open, err := d.Store.OpenBySeverity(context.Background())
	if err != nil {
		d.log.Error("stats could not count incidents", "error", err)
	}
	return metrics.Stats{OpenBySeverity: open}
}

// IngestResult says what a webhook did.
type IngestResult struct {
	Opened   []int64 `json:"opened"`
	Updated  []int64 `json:"updated"`
	Reopened []int64 `json:"reopened"`
	Resolved []int64 `json:"resolved"`
	// Notices counts the alerts that page nobody and went to the channels
	// as notices instead of incidents.
	Notices int `json:"notices"`
	// Dropped counts the alerts a route threw away.
	Dropped int `json:"dropped"`
}

// IngestAlertmanager folds an Alertmanager webhook's alerts in, as
// IngestAlerts does, for the producer that sent it.
func (d *Desk) IngestAlertmanager(ctx context.Context, p alertmanager.Payload, producer string) (IngestResult, error) {
	return d.IngestAlerts(ctx, p.Alerts, producer)
}

// sourceName is how a timeline names the producer an alert came from:
// Alertmanager for the shared secret, Azure Monitor for an action group's
// secure webhook, and a token's own name otherwise.
func sourceName(producer string) string {
	switch producer {
	case "alertmanager":
		return "Alertmanager"
	case "azure-monitor":
		return "Azure Monitor"
	}
	return producer
}

// IngestAlerts folds each alert that pages into the incident its group key
// names, opening one when none is open. An alert that pages nobody joins
// an unresolved incident of its group if there is one, and otherwise goes
// to the channels as a notice: it opens and reopens nothing. A repeat of
// an alert already in its state writes nothing, so repeat notifications
// leave no trace on the timeline.
func (d *Desk) IngestAlerts(ctx context.Context, alerts []alertmanager.Alert, producer string) (IngestResult, error) {
	res := IngestResult{Opened: []int64{}, Updated: []int64{}, Reopened: []int64{}, Resolved: []int64{}}
	settings, err := d.Store.Settings(ctx)
	if err != nil {
		return res, err
	}
	now := d.Now()
	for _, a := range alerts {
		routed := incident.ApplyRoutes(a.Labels, settings.Routes)
		if routed.Dropped {
			res.Dropped++
			d.metrics.Alert(ctx, producer, "dropped")
			d.log.InfoContext(ctx, "alert dropped by a route", "alertname", a.Name(), "route", routed.Route)
			continue
		}
		a.Labels = routed.Labels
		pages := routed.Pages
		asNotice := false
		service := incident.ServiceFor(a.Labels, settings.Rules)
		maintenance := false
		if pages {
			if maintenance, err = d.Store.InWindow(ctx, service, now); err != nil {
				return res, err
			}
			if maintenance {
				pages = false
				d.log.InfoContext(ctx, "alert quieted: its service is in a maintenance window", "alertname", a.Name(), "component", service)
			}
		}
		key := incident.GroupKey(a.Name(), service)
		// The catalog lookup is a network call, so it stays outside the transaction.
		workload := d.orrery.Workload(ctx, a.Labels["namespace"])
		var opened, reopened, resolved bool
		var before incident.State
		var inc incident.Incident
		// change is the timeline row of a state change, which keys its annals event.
		var change incident.Event
		err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			inc, err = d.Store.FindByGroup(ctx, tx, key, now.Add(-settings.GroupingWindow))
			switch {
			case errors.Is(err, store.ErrNotFound):
				if !pages {
					asNotice = true
					return nil
				}
				if !a.Firing() {
					// A resolution for something never opened, or long resolved: nothing to hold.
					return nil
				}
				sev := incident.ParseSeverity(a.Labels["severity"])
				inc, err = d.Store.Insert(ctx, tx, incident.Incident{
					Title: incident.TitleFor(a.Name(), a.Annotations), Severity: sev, State: incident.Open,
					Service: service, Workload: workload, GroupKey: key, Source: incident.SourceAlertmanager,
					AutoResolve: settings.AutoResolveInfo && sev == incident.Info, OpenedAt: now, UpdatedAt: now,
				})
				if err != nil {
					return err
				}
				opened = true
				if change, err = d.event(ctx, tx, inc.ID, now, incident.EventOpened, Machine, "Opened from "+sourceName(producer)+": "+a.Name()+" firing on "+service); err != nil {
					return err
				}
			case err != nil:
				return err
			case inc.State == incident.Resolved && a.Firing() && !pages:
				// An alert that pages nobody never reopens an incident.
				asNotice = true
				return nil
			}
			if inc.ID == 0 {
				return nil
			}
			stored, err := d.Store.GetAlert(ctx, tx, inc.ID, a.Fingerprint)
			known := err == nil
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if known && stored.Status == a.Status {
				return nil
			}
			if err := d.Store.UpsertAlert(ctx, tx, inc.ID, incident.Alert{
				Fingerprint: a.Fingerprint, Name: a.Name(), Status: a.Status, Labels: a.Labels, Annotations: a.Annotations,
				StartsAt: a.StartsAt, EndsAt: a.Ended(),
			}); err != nil {
				return err
			}
			if inc.Workload == "" && workload != "" {
				inc.Workload = workload
			}
			if a.Firing() {
				if !opened {
					if _, err := d.event(ctx, tx, inc.ID, now, incident.EventAlertFiring, Machine, a.Name()+" firing"); err != nil {
						return err
					}
				}
				if inc.State == incident.Resolved {
					before = inc.State
					inc.State = incident.Open
					inc.ResolvedAt = nil
					reopened = true
					if change, err = d.event(ctx, tx, inc.ID, now, incident.EventReopened, Machine, "Reopened: "+a.Name()+" fired again within the grouping window"); err != nil {
						return err
					}
				}
			} else {
				if _, err := d.event(ctx, tx, inc.ID, now, incident.EventAlertResolved, Machine, a.Name()+" resolved"); err != nil {
					return err
				}
				firing, err := d.Store.FiringCount(ctx, tx, inc.ID)
				if err != nil {
					return err
				}
				if incident.ShouldAutoResolve(inc, firing) {
					before = inc.State
					t := now
					inc.State = incident.Resolved
					inc.ResolvedAt = &t
					resolved = true
					if change, err = d.event(ctx, tx, inc.ID, now, incident.EventResolved, Machine, "Resolved by dispatch: every alert resolved and auto-resolve is on"); err != nil {
						return err
					}
				}
			}
			inc.UpdatedAt = now
			if err := d.Store.Update(ctx, tx, inc); err != nil {
				return err
			}
			inc, err = d.Store.Get(ctx, tx, inc.ID)
			return err
		})
		if err != nil {
			return res, err
		}
		if asNotice {
			res.Notices++
			if maintenance {
				d.metrics.Alert(ctx, producer, "quieted")
			} else {
				d.metrics.Alert(ctx, producer, "notice")
			}
			d.alertNotice(ctx, a, service, workload, producer, maintenance)
			continue
		}
		if inc.ID == 0 {
			d.log.InfoContext(ctx, "alert ignored: resolved and nothing open", "alertname", a.Name(), "component", service)
			continue
		}
		d.metrics.Alert(ctx, producer, "incident")
		switch {
		case opened:
			res.Opened = append(res.Opened, inc.ID)
			d.opened(ctx, inc, change, fromAlertmanager)
		case reopened:
			res.Reopened = append(res.Reopened, inc.ID)
			d.changed(ctx, inc, change, before, fromAlertmanager)
		case resolved:
			res.Resolved = append(res.Resolved, inc.ID)
			d.changed(ctx, inc, change, before, machine)
		default:
			res.Updated = append(res.Updated, inc.ID)
			d.orrery.Push(ctx, inc)
			if !a.Firing() && inc.Settling() {
				d.cleared(ctx, inc)
			}
		}
	}
	return res, nil
}

// OpenRequest opens an incident by hand or from another product.
type OpenRequest struct {
	Title       string            `json:"title"`
	Severity    incident.Severity `json:"severity"`
	Service     string            `json:"service"`
	Description string            `json:"description"`
	Public      bool              `json:"public"`
}

// Open creates an incident.
func (d *Desk) Open(ctx context.Context, req OpenRequest, source incident.Source, actor incident.Actor) (incident.Incident, error) {
	req.Title = strings.TrimSpace(req.Title)
	req.Service = strings.TrimSpace(req.Service)
	if req.Title == "" {
		return incident.Incident{}, fmt.Errorf("%w: a title is required", ErrInput)
	}
	if req.Service == "" {
		req.Service = "unknown"
	}
	sev := incident.ParseSeverity(string(req.Severity))
	now := d.Now()
	var inc incident.Incident
	var change incident.Event
	err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		inc, err = d.Store.Insert(ctx, tx, incident.Incident{
			Title: req.Title, Severity: sev, State: incident.Open, Service: req.Service, Source: source,
			Public: req.Public, OpenedAt: now, UpdatedAt: now,
		})
		if err != nil {
			return err
		}
		text := "Opened by " + actor.Name()
		if req.Description != "" {
			text += ": " + strings.TrimSpace(req.Description)
		}
		if change, err = d.event(ctx, tx, inc.ID, now, incident.EventOpened, actor.Name(), text); err != nil {
			return err
		}
		inc, err = d.Store.Get(ctx, tx, inc.ID)
		return err
	})
	if err != nil {
		return inc, err
	}
	d.opened(ctx, inc, change, actor)
	return inc, nil
}

// Change moves an incident to a state. Acknowledging makes the actor the
// owner when it has none.
func (d *Desk) Change(ctx context.Context, id int64, to incident.State, by incident.Actor) (incident.Incident, error) {
	return d.move(ctx, id, to, by, "")
}

// ResolveQuiet resolves, as dispatch, every incident whose alerts have all
// been quiet for the settings' QuietResolve window, and says so on its
// timeline. If the trouble comes back after the grouping window, a new
// incident opens.
func (d *Desk) ResolveQuiet(ctx context.Context) {
	st, err := d.Store.Settings(ctx)
	if err != nil {
		d.log.ErrorContext(ctx, "settings not read for quiet incidents", "error", err)
		return
	}
	if st.QuietResolve <= 0 {
		return
	}
	open, err := d.Store.List(ctx, store.Filter{States: []incident.State{incident.Open, incident.Acknowledged, incident.Mitigated}})
	if err != nil {
		d.log.ErrorContext(ctx, "unresolved incidents not read", "error", err)
		return
	}
	now := d.Now()
	text := fmt.Sprintf("Resolved by dispatch: no alert has fired for %g hours", st.QuietResolve.Hours())
	for _, inc := range open {
		if !incident.ShouldResolveQuiet(inc, st.QuietResolve, now) {
			continue
		}
		if _, err := d.move(ctx, inc.ID, incident.Resolved, machine, text); err != nil {
			d.log.ErrorContext(ctx, "quiet incident not resolved", "incident", inc.ID, "error", err)
			continue
		}
		d.log.InfoContext(ctx, "quiet incident resolved", "incident", inc.ID, "severity", inc.Severity, "component", inc.Service, "quiet_since", inc.QuietSince)
	}
}

// move is Change with the timeline's text; an empty text says who did it.
func (d *Desk) move(ctx context.Context, id int64, to incident.State, by incident.Actor, said string) (incident.Incident, error) {
	now := d.Now()
	actor := by.Name()
	var inc incident.Incident
	var change incident.Event
	var before incident.State
	err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		inc, err = d.Store.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := incident.Transition(inc.State, to); err != nil {
			return fmt.Errorf("%w: %w", ErrInput, err)
		}
		before = inc.State
		inc.State = to
		text := ""
		switch to {
		case incident.Acknowledged:
			if inc.AcknowledgedAt == nil {
				inc.AcknowledgedAt = &now
			}
			if inc.Owner == "" {
				inc.Owner = actor
			}
			text = "Acknowledged by " + actor
		case incident.Mitigated:
			inc.MitigatedAt = &now
			text = "Mitigated by " + actor
		case incident.Resolved:
			inc.ResolvedAt = &now
			text = "Resolved by " + actor
		case incident.Open:
			inc.ResolvedAt = nil
			text = "Reopened by " + actor
		}
		if said != "" {
			text = said
		}
		inc.UpdatedAt = now
		if err := d.Store.Update(ctx, tx, inc); err != nil {
			return err
		}
		if change, err = d.event(ctx, tx, inc.ID, now, incident.EventForState(to), actor, text); err != nil {
			return err
		}
		inc, err = d.Store.Get(ctx, tx, inc.ID)
		return err
	})
	if err != nil {
		return inc, err
	}
	d.changed(ctx, inc, change, before, by)
	return inc, nil
}

// Assign sets the owner.
func (d *Desk) Assign(ctx context.Context, id int64, owner, actor string) (incident.Incident, error) {
	owner = strings.TrimSpace(owner)
	text := "Assigned to " + owner + " by " + actor
	if owner == "" {
		text = "Owner cleared by " + actor
	}
	return d.edit(ctx, id, incident.EventAssigned, actor, text, func(_ *sql.Tx, i *incident.Incident) error {
		i.Owner = owner
		return nil
	})
}

// SetSeverity changes the severity.
func (d *Desk) SetSeverity(ctx context.Context, id int64, sev incident.Severity, actor string) (incident.Incident, error) {
	sev = incident.ParseSeverity(string(sev))
	return d.edit(ctx, id, incident.EventSeverity, actor, "Severity set to "+string(sev)+" by "+actor, func(_ *sql.Tx, i *incident.Incident) error {
		i.Severity = sev
		return nil
	})
}

// SetPublic puts the incident's title on the status page, or takes it off.
func (d *Desk) SetPublic(ctx context.Context, id int64, public bool, actor string) (incident.Incident, error) {
	text := "Shown on the status page by " + actor
	if !public {
		text = "Taken off the status page by " + actor
	}
	inc, err := d.edit(ctx, id, incident.EventPublic, actor, text, func(_ *sql.Tx, i *incident.Incident) error {
		i.Public = public
		return nil
	})
	if err == nil && public {
		d.tellFollowers(ctx, inc, "We are looking into it.", fmt.Sprintf("%d:public:%d", id, d.Now().UnixNano()))
	}
	return inc, err
}

// SetAutoResolve turns the auto-resolve rule on or off for an incident.
func (d *Desk) SetAutoResolve(ctx context.Context, id int64, on bool, actor string) (incident.Incident, error) {
	text := "Auto-resolve turned on by " + actor
	if !on {
		text = "Auto-resolve turned off by " + actor
	}
	return d.edit(ctx, id, incident.EventNote, actor, text, func(_ *sql.Tx, i *incident.Incident) error {
		i.AutoResolve = on
		return nil
	})
}

// Note adds a timeline note. A public note shows on the status page.
func (d *Desk) Note(ctx context.Context, id int64, text string, public bool, actor string) (incident.Incident, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return incident.Incident{}, fmt.Errorf("%w: a note needs text", ErrInput)
	}
	now := d.Now()
	var inc incident.Incident
	err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if inc, err = d.Store.Get(ctx, tx, id); err != nil {
			return err
		}
		if _, err := d.Store.AddEvent(ctx, tx, incident.Event{IncidentID: id, At: now, Kind: incident.EventNote, Actor: actor, Text: text, Public: public}); err != nil {
			return err
		}
		inc, err = d.Store.Get(ctx, tx, id)
		return err
	})
	if err == nil {
		d.orrery.Push(ctx, inc)
		d.postRoom(ctx, inc, "Note from "+actor, text)
		if public {
			d.tellFollowers(ctx, inc, text, fmt.Sprintf("%d:note:%d", inc.ID, now.UnixNano()))
		}
	}
	return inc, err
}

// SavePostmortem writes the postmortem and notes it on the timeline.
func (d *Desk) SavePostmortem(ctx context.Context, id int64, body, actor string) error {
	now := d.Now()
	return d.Store.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := d.Store.Get(ctx, tx, id); err != nil {
			return err
		}
		if err := d.Store.SavePostmortem(ctx, tx, incident.Postmortem{IncidentID: id, Body: body, UpdatedAt: now, UpdatedBy: actor}); err != nil {
			return err
		}
		_, err := d.event(ctx, tx, id, now, incident.EventPostmortem, actor, "Postmortem updated by "+actor)
		return err
	})
}

// AddAction adds an action item.
func (d *Desk) AddAction(ctx context.Context, id int64, text, owner, actor string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("%w: an action item needs text", ErrInput)
	}
	now := d.Now()
	var opened func()
	err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
		inc, err := d.Store.Get(ctx, tx, id)
		if err != nil {
			return err
		}
		item, err := d.Store.AddActionItem(ctx, tx, incident.ActionItem{IncidentID: id, Text: text, Owner: strings.TrimSpace(owner), CreatedAt: now})
		if err != nil {
			return err
		}
		if _, err := d.event(ctx, tx, id, now, incident.EventAction, actor, "Action item added by "+actor+": "+text); err != nil {
			return err
		}
		opened = func() { d.openIssue(ctx, inc, item) }
		return nil
	})
	if err == nil && opened != nil {
		opened()
	}
	return err
}

// SetActionDone ticks or unticks an action item.
func (d *Desk) SetActionDone(ctx context.Context, id, actionID int64, done bool, actor string) error {
	return d.Store.Tx(ctx, func(tx *sql.Tx) error {
		return d.Store.SetActionDone(ctx, tx, id, actionID, done)
	})
}

// Outcomes is the report over the window ending now.
func (d *Desk) Outcomes(ctx context.Context, window string) (outcomes.Report, error) {
	dur, name, err := outcomes.ParseWindow(window)
	if err != nil {
		return outcomes.Report{}, fmt.Errorf("%w: %w", ErrInput, err)
	}
	until := d.Now().UTC()
	since := until.Add(-dur)
	list, err := d.Store.List(ctx, store.Filter{Since: since})
	if err != nil {
		return outcomes.Report{}, err
	}
	r := outcomes.Build(list, name, since, until)
	settings, err := d.Store.Settings(ctx)
	if err != nil {
		return r, err
	}
	stats, err := d.Store.PageStats(ctx, since, until)
	if err != nil {
		return r, err
	}
	pages := make([]outcomes.Page, 0, len(stats))
	for _, p := range stats {
		pages = append(pages, outcomes.Page{Service: p.Service, StartedAt: p.StartedAt, Answered: p.State != notice.PageUnanswered, AckBy: p.AckBy, Told: p.Told})
	}
	schedules, err := d.Store.Schedules(ctx, since)
	if err != nil {
		return r, err
	}
	hours := map[string]float64{}
	for _, sc := range schedules {
		for _, sh := range sc.Shifts(since, until) {
			if sh.Person != "" {
				hours[sh.Person] += sh.To.Sub(sh.From).Hours()
			}
		}
	}
	outcomes.AddPages(&r, pages, hours, settings.Night)
	return r, nil
}

// Component is one row of the Components view: a service, what has been
// failing on it, and how fast it comes back.
type Component struct {
	outcomes.Row
	// WorstOpen is the worst severity open on it now, "" when nothing is.
	WorstOpen incident.Severity
	// LastOpened is when its newest incident opened.
	LastOpened time.Time
}

// Components lists every service with an incident in the window, most
// incidents first.
func (d *Desk) Components(ctx context.Context, window string) ([]Component, outcomes.Report, error) {
	report, err := d.Outcomes(ctx, window)
	if err != nil {
		return nil, report, err
	}
	list, err := d.Store.List(ctx, store.Filter{Since: report.Since})
	if err != nil {
		return nil, report, err
	}
	extra := map[string]*Component{}
	for _, i := range list {
		c := extra[i.Service]
		if c == nil {
			c = &Component{}
			extra[i.Service] = c
		}
		if i.OpenedAt.After(c.LastOpened) {
			c.LastOpened = i.OpenedAt
		}
		if i.State != incident.Resolved && (c.WorstOpen == "" || i.Severity.Rank() < c.WorstOpen.Rank()) {
			c.WorstOpen = i.Severity
		}
	}
	out := make([]Component, 0, len(report.Services))
	for _, row := range report.Services {
		c := Component{Row: row}
		if e := extra[row.Service]; e != nil {
			c.WorstOpen, c.LastOpened = e.WorstOpen, e.LastOpened
		}
		out = append(out, c)
	}
	return out, report, nil
}

func (d *Desk) edit(ctx context.Context, id int64, kind incident.EventKind, actor, text string, fn func(*sql.Tx, *incident.Incident) error) (incident.Incident, error) {
	now := d.Now()
	var inc incident.Incident
	err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if inc, err = d.Store.Get(ctx, tx, id); err != nil {
			return err
		}
		if err := fn(tx, &inc); err != nil {
			return err
		}
		inc.UpdatedAt = now
		if err := d.Store.Update(ctx, tx, inc); err != nil {
			return err
		}
		if _, err := d.event(ctx, tx, id, now, kind, actor, text); err != nil {
			return err
		}
		inc, err = d.Store.Get(ctx, tx, id)
		return err
	})
	if err == nil {
		d.orrery.Push(ctx, inc)
	}
	return inc, err
}

func (d *Desk) event(ctx context.Context, tx *sql.Tx, id int64, at time.Time, kind incident.EventKind, actor, text string) (incident.Event, error) {
	return d.Store.AddEvent(ctx, tx, incident.Event{IncidentID: id, At: at, Kind: kind, Actor: actor, Text: text})
}

func (d *Desk) url(i incident.Incident) string {
	return fmt.Sprintf("%s/incidents/%d", d.deskURL, i.ID)
}

func (d *Desk) opened(ctx context.Context, inc incident.Incident, change incident.Event, by incident.Actor) {
	d.log.InfoContext(ctx, "incident opened", "incident", inc.ID, "severity", inc.Severity, "component", inc.Service, "source", inc.Source)
	d.metrics.Opened(ctx, string(inc.Severity), inc.Service)
	d.notify.Send(ctx, notify.Message{Event: "incident.opened", At: inc.OpenedAt, Incident: inc, Actor: Machine, URL: d.url(inc)})
	d.orrery.Push(ctx, inc)
	d.annals.Opened(ctx, inc, change, by)
	d.announce(ctx, "incident.opened", inc, change, by, fmt.Sprintf("#%d %s", inc.ID, inc.Title))
	d.tellFollowers(ctx, inc, "We are looking into it.", fmt.Sprintf("%d:%d", inc.ID, change.ID))
	if d.rooms != nil && inc.Severity.Rank() <= d.roomsFrom.Rank() {
		ctx := context.WithoutCancel(ctx)
		go func() {
			if _, err := d.OpenRoom(ctx, inc.ID); err != nil {
				d.log.WarnContext(ctx, "incident room not opened", "incident", inc.ID, "error", err)
			}
		}()
	}
}

func (d *Desk) changed(ctx context.Context, inc incident.Incident, change incident.Event, from incident.State, by incident.Actor) {
	d.log.InfoContext(ctx, "incident state changed", "incident", inc.ID, "from", from, "to", inc.State, "actor", by.Name())
	if inc.State == incident.Acknowledged && inc.AcknowledgedAt != nil && from == incident.Open {
		d.metrics.Acknowledged(ctx, string(inc.Severity), inc.Service, inc.AcknowledgedAt.Sub(inc.OpenedAt).Seconds())
	}
	if inc.State == incident.Resolved && inc.ResolvedAt != nil {
		d.metrics.Restored(ctx, string(inc.Severity), inc.Service, inc.ResolvedAt.Sub(inc.OpenedAt).Seconds())
	}
	if from == incident.Open && inc.State != incident.Open && d.outbox != nil {
		// Someone has it: the chains paging people about it stop.
		if n, err := d.outbox.AcknowledgeFor(ctx, Machine, fmt.Sprintf("incident:%d:", inc.ID), by.Name()); err != nil {
			d.log.ErrorContext(ctx, "incident's pages not ended", "incident", inc.ID, "error", err)
		} else if n > 0 {
			d.log.InfoContext(ctx, "incident's pages ended", "incident", inc.ID, "pages", n)
		}
	}
	d.notify.Send(ctx, notify.Message{Event: "incident.state_changed", At: inc.UpdatedAt, Incident: inc, From: from, Actor: by.Name(), URL: d.url(inc)})
	d.orrery.Push(ctx, inc)
	d.annals.Changed(ctx, inc, change, by)
	d.announce(ctx, "incident.state_changed", inc, change, by, fmt.Sprintf("#%d %s is %s", inc.ID, inc.Title, inc.State))
	d.tellFollowers(ctx, inc, "It is "+string(inc.State)+" now.", fmt.Sprintf("%d:%d", inc.ID, change.ID))
	d.postRoom(ctx, inc, fmt.Sprintf("#%d is %s", inc.ID, inc.State), change.Text)
}

// alertNotice sends an alert that pages nobody to the channels as a
// notice, threaded by its fingerprint: firing opens the thread, resolved
// closes it. Repeats of the same firing are the same key, so they go
// nowhere.
func (d *Desk) alertNotice(ctx context.Context, a alertmanager.Alert, service, workload, producer string, maintenance bool) {
	d.log.InfoContext(ctx, "alert sent as a notice: it pages nobody", "alertname", a.Name(), "component", service, "status", a.Status)
	if d.outbox == nil {
		return
	}
	kind, move := "alert.firing", notice.Trigger
	if !a.Firing() {
		kind, move = "alert.resolved", notice.Resolve
	}
	labels := map[string]string{"service": service, "alertname": a.Name(), "severity": string(incident.ParseSeverity(a.Labels["severity"]))}
	if maintenance {
		labels["maintenance"] = "true"
	}
	for k, v := range a.Labels {
		if len(labels) >= 30 {
			break
		}
		if _, set := labels[k]; !set {
			labels[k] = v
		}
	}
	e := notice.Event{
		Producer: producer, Kind: kind, Key: fmt.Sprintf("%s:%s:%s", a.Fingerprint, a.Status, a.StartsAt.UTC().Format(time.RFC3339)),
		Title: incident.TitleFor(a.Name(), a.Annotations), Text: clipText(a.Annotations["description"], 4000),
		Severity: labels["severity"], Weight: notice.WeightNotice, Move: move, DedupKey: a.Fingerprint, At: d.Now(),
		Facts:  []notice.Fact{{Name: "Alert", Value: a.Name()}, {Name: "Service", Value: service}, {Name: "Status", Value: a.Status}},
		Labels: labels,
	}
	if strings.HasPrefix(a.GeneratorURL, "https://") || strings.HasPrefix(a.GeneratorURL, "http://") {
		e.Links = []notice.Link{{Title: "Source", URL: a.GeneratorURL}}
	}
	if workload != "" {
		e.Entity = &notice.Ref{Blueprint: "sw_workload", Key: workload}
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		if _, err := d.outbox.Accept(ctx, e); err != nil {
			d.log.ErrorContext(ctx, "alert notice not taken", "alertname", a.Name(), "error", err)
		}
	}()
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// announce gives the outbox an incident's change as an event, about the
// workload it is on when the catalog knows it. It runs on its own, so a
// slow catalog never holds the change up.
func (d *Desk) announce(ctx context.Context, kind string, inc incident.Incident, change incident.Event, by incident.Actor, title string) {
	if d.outbox == nil {
		return
	}
	e := notice.Event{
		Producer: Machine, Kind: kind, Key: fmt.Sprintf("incident:%d:%d", inc.ID, change.ID), Title: title,
		Severity: string(inc.Severity), At: change.At,
		Facts:  []notice.Fact{{Name: "Service", Value: inc.Service}, {Name: "State", Value: string(inc.State)}},
		Links:  []notice.Link{{Title: "Open the incident", URL: d.url(inc)}},
		Labels: map[string]string{"service": inc.Service, "severity": string(inc.Severity), "state": string(inc.State)},
	}
	if inc.Owner != "" {
		e.Facts = append(e.Facts, notice.Fact{Name: "Owner", Value: inc.Owner})
	}
	if by.Name() != Machine {
		e.Facts = append(e.Facts, notice.Fact{Name: "By", Value: by.Name()})
	}
	if inc.Workload != "" {
		e.Entity = &notice.Ref{Blueprint: "sw_workload", Key: inc.Workload}
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		d.labelFromAlerts(ctx, inc.ID, e.Labels)
		if _, err := d.outbox.Accept(ctx, e); err != nil {
			d.log.ErrorContext(ctx, "incident notice not taken", "incident", inc.ID, "kind", kind, "error", err)
		}
	}()
}

// cleared tells the incident's subscribers once that its last alert
// stopped and it now closes itself, so nobody works what has cleared. The
// key holds when it went quiet, so a repeat of the same resolve tells
// nobody again, and a later quiet spell tells them anew.
func (d *Desk) cleared(ctx context.Context, inc incident.Incident) {
	if d.outbox == nil || inc.QuietSince == nil || inc.ClosesAt == nil {
		return
	}
	at := inc.ClosesAt.UTC().Format("Jan 2 at 15:04 UTC")
	e := notice.Event{
		Producer: Machine, Kind: "incident.cleared", Key: fmt.Sprintf("incident:%d:cleared:%d", inc.ID, inc.QuietSince.Unix()),
		Title:    fmt.Sprintf("#%d %s cleared", inc.ID, inc.Title),
		Text:     "Every alert stopped. It closes itself on " + at + " unless one fires again; nothing needs working.",
		Severity: string(inc.Severity), At: d.Now(),
		Facts:  []notice.Fact{{Name: "Service", Value: inc.Service}, {Name: "Closes itself", Value: at}},
		Links:  []notice.Link{{Title: "Open the incident", URL: d.url(inc)}},
		Labels: map[string]string{"service": inc.Service, "severity": string(inc.Severity), "state": string(inc.State), "phase": inc.Phase()},
	}
	if inc.Workload != "" {
		e.Entity = &notice.Ref{Blueprint: "sw_workload", Key: inc.Workload}
	}
	d.log.InfoContext(ctx, "incident cleared", "incident", inc.ID, "closes_at", inc.ClosesAt)
	ctx = context.WithoutCancel(ctx)
	go func() {
		if _, err := d.outbox.Accept(ctx, e); err != nil {
			d.log.ErrorContext(ctx, "cleared notice not taken", "incident", inc.ID, "error", err)
		}
	}()
}

// pagingLabels are the alert labels that say how an incident should reach
// people, carried onto its events so a subscription can match them.
var pagingLabels = []string{"notify", "page"}

// labelFromAlerts copies the paging labels any of an incident's alerts
// carries onto its event's labels.
func (d *Desk) labelFromAlerts(ctx context.Context, id int64, labels map[string]string) {
	alerts, err := d.Store.Alerts(ctx, id)
	if err != nil {
		d.log.WarnContext(ctx, "incident's alerts not read for its event", "incident", id, "error", err)
		return
	}
	for _, a := range alerts {
		for _, k := range pagingLabels {
			if v := a.Labels[k]; v != "" && labels[k] == "" {
				labels[k] = v
			}
		}
	}
}
