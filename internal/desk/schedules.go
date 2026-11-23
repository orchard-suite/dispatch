package desk

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// SaveSchedule checks and keeps a schedule.
func (d *Desk) SaveSchedule(ctx context.Context, sc notice.Schedule) error {
	if err := sc.Validate(); err != nil {
		return err
	}
	return d.Store.SaveSchedule(ctx, sc)
}

// Cover puts a person on call on a schedule for a while: a cover, or one
// half of a swap. Anyone on the desk may.
func (d *Desk) Cover(ctx context.Context, slug string, o notice.Override) (notice.Override, error) {
	email, err := notice.NormalEmail(o.Person)
	if err != nil {
		return o, err
	}
	o.Person = email
	switch {
	case !o.To.After(o.From):
		return o, fmt.Errorf("%w: a cover ends after it starts", notice.ErrInvalid)
	case o.To.Sub(o.From) > 31*24*time.Hour:
		return o, fmt.Errorf("%w: a cover is at most a month", notice.ErrInvalid)
	case o.To.Before(d.Now()):
		return o, fmt.Errorf("%w: a cover ends in the future", notice.ErrInvalid)
	case len(o.Reason) > 300:
		return o, fmt.Errorf("%w: a reason is at most 300 characters", notice.ErrInvalid)
	}
	if _, err := d.Store.Schedule(ctx, slug, d.Now()); err != nil {
		return o, err
	}
	o, err = d.Store.AddOverride(ctx, slug, o)
	if err == nil {
		d.log.InfoContext(ctx, "cover added", "schedule", slug, "override", o.ID)
	}
	return o, err
}

// WatchSchedules tells whoever comes on call that they are, and the
// schedule's subscribers too, and warns once a day of a gap in the next
// week. The first look at a schedule only notes who is on call.
func (d *Desk) WatchSchedules(ctx context.Context) {
	now := d.Now()
	schedules, err := d.Store.Schedules(ctx, now)
	if err != nil {
		d.log.ErrorContext(ctx, "schedules not read", "error", err)
		return
	}
	for _, sc := range schedules {
		who := sc.OnCall(now)
		seen, err := d.Store.Watched(ctx, sc.Slug)
		first := errors.Is(err, store.ErrNotFound)
		if err != nil && !first {
			d.log.ErrorContext(ctx, "schedule watch not read", "schedule", sc.Slug, "error", err)
			continue
		}
		next := seen
		if first || seen.Person != who {
			next.Person, next.Since = who, now
		}
		if !first && seen.Person != who {
			d.handoff(ctx, sc, seen.Person, who, now)
		}
		day := now.In(time.UTC).Format("2006-01-02")
		if gaps := sc.Gaps(now, now.Add(7*24*time.Hour)); len(gaps) > 0 && seen.GapTold != day {
			next.GapTold = day
			d.gapNotice(ctx, sc, gaps, now)
		}
		if err := d.Store.Watch(ctx, sc.Slug, next); err != nil {
			d.log.ErrorContext(ctx, "schedule watch not kept", "schedule", sc.Slug, "error", err)
		}
	}
}

// handoff tells the person coming on call, and channels that take
// schedule.handoff, who has it now.
func (d *Desk) handoff(ctx context.Context, sc notice.Schedule, from, to string, now time.Time) {
	d.log.InfoContext(ctx, "on call changed", "schedule", sc.Slug, "nobody_now", to == "")
	if d.outbox == nil {
		return
	}
	until := sc.Until(now)
	title := fmt.Sprintf("You are on call for %s until %s", sc.Name, until.In(zone(sc.Zone)).Format("Mon Jan 2 15:04 MST"))
	if to == "" {
		title = "Nobody is on call for " + sc.Name
	}
	e := notice.Event{Producer: Machine, Kind: "schedule.handoff", Key: fmt.Sprintf("handoff:%s:%d", sc.Slug, now.Unix()), Title: title, Weight: notice.WeightNotify, At: now,
		Facts:  []notice.Fact{{Name: "Schedule", Value: sc.Name}, {Name: "On call", Value: orNobody(to)}, {Name: "Before", Value: orNobody(from)}},
		Links:  []notice.Link{{Title: "The schedule", URL: d.deskURL + "/schedules#" + sc.Slug}},
		Labels: map[string]string{"schedule": sc.Slug}}
	if to != "" {
		e.To = []notice.Target{{Person: to}}
	}
	d.accept(ctx, e, "handoff")
}

func (d *Desk) gapNotice(ctx context.Context, sc notice.Schedule, gaps []notice.Shift, now time.Time) {
	d.log.WarnContext(ctx, "schedule has a gap", "schedule", sc.Slug, "gaps", len(gaps))
	if d.outbox == nil {
		return
	}
	loc := zone(sc.Zone)
	var parts []string
	for i, g := range gaps {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("and %d more", len(gaps)-3))
			break
		}
		parts = append(parts, g.From.In(loc).Format("Mon 15:04")+" to "+g.To.In(loc).Format("Mon 15:04"))
	}
	d.accept(ctx, notice.Event{Producer: Machine, Kind: "schedule.gap", Key: fmt.Sprintf("gap:%s:%s", sc.Slug, now.Format("2006-01-02")), Weight: notice.WeightNotice, At: now,
		Title: "Nobody is on call for " + sc.Name + " " + strings.Join(parts, ", "), Severity: "warning",
		Links:  []notice.Link{{Title: "The schedule", URL: d.deskURL + "/schedules#" + sc.Slug}},
		Labels: map[string]string{"schedule": sc.Slug}}, "gap")
}

func (d *Desk) accept(ctx context.Context, e notice.Event, what string) {
	if _, err := d.outbox.Accept(ctx, e); err != nil {
		d.log.ErrorContext(ctx, what+" notice not taken", "error", err)
	}
}

func orNobody(s string) string {
	if s == "" {
		return "nobody"
	}
	return s
}

func zone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Snooze has the desk page a person again about an acknowledged incident
// after a while, unless it is mitigated or resolved by then.
func (d *Desk) Snooze(ctx context.Context, id int64, minutes int, by string) error {
	if minutes < 5 || minutes > 24*60 {
		return fmt.Errorf("%w: a snooze is 5 minutes to a day", ErrInput)
	}
	inc, err := d.Store.Get(ctx, d.Store.DB(), id)
	if err != nil {
		return err
	}
	if inc.State != incident.Acknowledged {
		return fmt.Errorf("%w: only an acknowledged incident is snoozed", ErrInput)
	}
	due := d.Now().Add(time.Duration(minutes) * time.Minute)
	if err := d.Store.AddReminder(ctx, store.Reminder{IncidentID: id, Person: by, DueAt: due}); err != nil {
		return err
	}
	_, err = d.Note(ctx, id, fmt.Sprintf("Snoozed by %s until %s", by, due.UTC().Format("15:04 UTC")), false, by)
	return err
}

// Remind pages the person a snooze named about every incident whose
// snooze ran out while it was still acknowledged.
func (d *Desk) Remind(ctx context.Context) {
	due, err := d.Store.DueReminders(ctx, d.Now())
	if err != nil {
		d.log.ErrorContext(ctx, "reminders not read", "error", err)
		return
	}
	for _, r := range due {
		inc, err := d.Store.Get(ctx, d.Store.DB(), r.IncidentID)
		if err != nil || inc.State != incident.Acknowledged || d.outbox == nil {
			continue
		}
		d.log.InfoContext(ctx, "snooze ran out", "incident", inc.ID)
		d.accept(ctx, notice.Event{Producer: Machine, Kind: "incident.snooze_ended", Key: fmt.Sprintf("reminder:%d", r.ID), Weight: notice.WeightPage, At: d.Now(),
			Title: fmt.Sprintf("#%d %s is still %s", inc.ID, inc.Title, inc.State), Severity: string(inc.Severity),
			Links:  []notice.Link{{Title: "Open the incident", URL: d.url(inc)}},
			Labels: map[string]string{"service": inc.Service, "severity": string(inc.Severity), "state": string(inc.State)},
			To:     []notice.Target{{Person: r.Person}}}, "reminder")
	}
}

// EscalateIncident tells the next step of every page about an incident
// now, and says how many it moved.
func (d *Desk) EscalateIncident(ctx context.Context, id int64, by string) (int, error) {
	if d.outbox == nil {
		return 0, nil
	}
	pages, err := d.Store.ActivePagesFor(ctx, Machine, fmt.Sprintf("incident:%d:", id))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pages {
		moved, err := d.outbox.Escalate(ctx, p.ID, by)
		if err != nil {
			return n, err
		}
		if moved {
			n++
		}
	}
	return n, nil
}

// CheckBudgets tells the channels, once a week each, of every service
// that caused more pages in seven days than the settings allow.
func (d *Desk) CheckBudgets(ctx context.Context) {
	settings, err := d.Store.Settings(ctx)
	if err != nil || settings.ServicePages <= 0 || d.outbox == nil {
		return
	}
	now := d.Now()
	stats, err := d.Store.PageStats(ctx, now.Add(-7*24*time.Hour), now)
	if err != nil {
		d.log.ErrorContext(ctx, "page stats not read", "error", err)
		return
	}
	per := map[string]int{}
	for _, p := range stats {
		if p.Service != "" {
			per[p.Service]++
		}
	}
	year, week := now.ISOWeek()
	for service, n := range per {
		if n <= settings.ServicePages {
			continue
		}
		d.log.WarnContext(ctx, "service over its page budget", "component", service, "pages", n)
		d.accept(ctx, notice.Event{Producer: Machine, Kind: "budget.exceeded", Key: fmt.Sprintf("budget:%s:%d-%d", service, year, week), Weight: notice.WeightNotice, At: now,
			Title:  fmt.Sprintf("%s paged %d times in seven days, over its budget of %d", service, n, settings.ServicePages),
			Links:  []notice.Link{{Title: "Components", URL: d.deskURL + "/components?window=7d"}},
			Labels: map[string]string{"service": service}, Severity: "warning"}, "budget")
	}
}
