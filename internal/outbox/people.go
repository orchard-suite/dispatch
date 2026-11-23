package outbox

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/mail"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// Machine is the actor name for decisions dispatch makes itself.
const Machine = "dispatch"

// PersonSender sends one message to one person by one medium. It answers
// notice.ErrUnreachable when the medium cannot reach them, which is not
// tried again.
type PersonSender interface {
	SendPerson(ctx context.Context, out notice.Outgoing) (int, error)
}

// UseMedium sends a medium through s; nil sends none by it.
func (o *Outbox) UseMedium(m notice.Medium, s PersonSender) {
	if s == nil {
		delete(o.media, m)
		return
	}
	o.media[m] = s
}

// UseMail sends email to people through m; nil sends none.
func (o *Outbox) UseMail(m mail.Sender) {
	if m == nil {
		o.UseMedium(notice.Email, nil)
		return
	}
	o.UseMedium(notice.Email, mailPerson{m})
}

// mailPerson is email as a medium.
type mailPerson struct{ m mail.Sender }

func (mp mailPerson) SendPerson(ctx context.Context, out notice.Outgoing) (int, error) {
	return mp.m.Send(ctx, out.Person.Email, notice.Mail(out.Event, out.Ack))
}

// UseTeams sends Teams chats to people through t.
func (o *Outbox) UseTeams(t PersonSender) { o.UseMedium(notice.TeamsChat, t) }

// Sends says whether the desk can send by m.
func (o *Outbox) Sends(m notice.Medium) bool { return o.media[m] != nil }

// UseDesk is where acknowledge links point: the desk's public URL.
func (o *Outbox) UseDesk(publicURL string) { o.deskURL = publicURL }

// UseCounters counts messages to people and pages by outcome.
func (o *Outbox) UseCounters(message, page func(ctx context.Context, outcome string)) {
	o.messageOutcome, o.pageOutcome = message, page
}

// Mails says whether people can be sent email.
func (o *Outbox) Mails() bool { return o.Sends(notice.Email) }

// AckURL is the desk's page for acknowledging a chain's page.
func (o *Outbox) AckURL(pageID int64) string {
	if o.deskURL == "" {
		return ""
	}
	return o.deskURL + "/pages/" + strconv.FormatInt(pageID, 10)
}

// checkTargets refuses an event addressed to a group or chain that does not
// exist, naming it.
func (o *Outbox) checkTargets(ctx context.Context, to []notice.Target) error {
	for _, t := range to {
		var err error
		switch {
		case t.Group != "":
			_, err = o.store.Group(ctx, t.Group)
		case t.Chain != "":
			_, err = o.store.Chain(ctx, t.Chain)
		case t.Schedule != "":
			_, err = o.store.Schedule(ctx, t.Schedule, o.Now())
		}
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: no %s", notice.ErrInvalid, t.String())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// address queues the event for the people, groups, and chains it reaches,
// by its weight. A page starts a page on each chain and tells its first
// step; a lighter event tells everyone a chain names once. Everyone is
// told once, unless a chain's first step already tells them, and every
// person reached finds it in their list. chosen counts the people whose
// own choice for the event's kind said how they are told.
func (o *Outbox) address(ctx context.Context, e notice.Event, targets []notice.Target, now time.Time) (messages, pages, chosen int, err error) {
	var direct []string
	var chains []string
	seen := map[string]bool{}
	for _, t := range targets {
		if t.Chain != "" && e.Weight == notice.WeightPage {
			if !seen["chain:"+t.Chain] {
				seen["chain:"+t.Chain] = true
				chains = append(chains, t.Chain)
			}
			continue
		}
		emails, err := o.expand(ctx, t, now)
		if err != nil {
			return 0, 0, 0, err
		}
		direct = append(direct, emails...)
	}
	told := map[string]bool{}
	for _, slug := range chains {
		c, err := o.store.Chain(ctx, slug)
		if errors.Is(err, store.ErrNotFound) {
			o.log.WarnContext(ctx, "event's chain no longer exists", "event", e.ID, "chain", slug)
			continue
		}
		if err != nil {
			return messages, pages, chosen, err
		}
		p, err := o.store.AddPage(ctx, notice.Page{EventID: e.ID, Chain: c.Slug, Steps: c.Steps, Repeat: c.Repeat, State: notice.PageActive,
			NextAt: now.Add(time.Duration(c.Steps[0].Wait) * time.Minute), StartedAt: now})
		if err != nil {
			return messages, pages, chosen, err
		}
		pages++
		o.countPage(ctx, "started")
		o.log.InfoContext(ctx, "page started", "page", p.ID, "event", e.ID, "chain", c.Slug, "steps", len(c.Steps))
		o.decidePage(ctx, "page.start", p, e, incident.System(e.Producer), map[string]string{"chain": c.Slug})
		n, emails, err := o.tellStep(ctx, e, p, 0, 0, now)
		if err != nil {
			return messages, pages, chosen, err
		}
		messages += n
		for _, m := range emails {
			told[m] = true
		}
	}
	for _, email := range direct {
		if told[email] {
			continue
		}
		told[email] = true
		n, chose, err := o.queuePerson(ctx, e, email, 0, 0, now)
		if err != nil {
			return messages, pages, chosen, err
		}
		messages += n
		if chose {
			chosen++
		}
	}
	return messages, pages, chosen, nil
}

// expand is the addresses a target reaches: a person, a group's members,
// whoever a schedule has on call now, or everyone a chain's steps name.
func (o *Outbox) expand(ctx context.Context, t notice.Target, now time.Time) ([]string, error) {
	if t.Person != "" {
		return []string{t.Person}, nil
	}
	if t.Schedule != "" {
		sc, err := o.store.Schedule(ctx, t.Schedule, now)
		if errors.Is(err, store.ErrNotFound) {
			o.log.WarnContext(ctx, "a schedule no longer exists", "schedule", t.Schedule)
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		who := sc.OnCall(now)
		if who == "" {
			o.log.WarnContext(ctx, "a schedule has nobody on call", "schedule", t.Schedule)
			return nil, nil
		}
		return []string{who}, nil
	}
	if t.Chain != "" {
		c, err := o.store.Chain(ctx, t.Chain)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var out []string
		for _, step := range c.Steps {
			for _, st := range step.Targets {
				got, err := o.expand(ctx, st, now)
				if err != nil {
					return nil, err
				}
				out = append(out, got...)
			}
		}
		return out, nil
	}
	g, err := o.store.Group(ctx, t.Group)
	if errors.Is(err, store.ErrNotFound) {
		o.log.WarnContext(ctx, "a group no longer exists", "group", t.Group)
		return nil, nil
	}
	return g.Members, err
}

// tellStep queues a page's step for everyone it names.
func (o *Outbox) tellStep(ctx context.Context, e notice.Event, p notice.Page, step, round int, now time.Time) (int, []string, error) {
	var emails []string
	seen := map[string]bool{}
	for _, t := range p.Steps[step].Targets {
		got, err := o.expand(ctx, t, now)
		if err != nil {
			return 0, nil, err
		}
		for _, m := range got {
			if !seen[m] {
				seen[m] = true
				emails = append(emails, m)
			}
		}
	}
	queued := 0
	for _, m := range emails {
		n, _, err := o.queuePerson(ctx, e, m, p.ID, round, now)
		if err != nil {
			return queued, emails, err
		}
		queued += n
	}
	return queued, emails, nil
}

// person is someone in the directory, or an address that is not, who is
// reached by email only.
func (o *Outbox) person(ctx context.Context, email string) (notice.Person, bool, error) {
	p, err := o.store.Person(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return notice.Person{Email: email, Rules: notice.Rules{
			notice.WeightPage: {{Medium: notice.Email}}, notice.WeightNotify: {{Medium: notice.Email}},
		}}, false, nil
	}
	return p, err == nil, err
}

// chose is the media a person set for the event's kind, when their choice
// decides how they are told. It never decides a page, which goes as their
// page rules say, nor an event a maintenance window quieted, which stays
// on their list. Only the producer that declared the kind is listened to.
func (o *Outbox) chose(ctx context.Context, e notice.Event, email string) (notice.Choice, bool, error) {
	if e.Weight == notice.WeightPage || e.Labels["maintenance"] == "true" {
		return notice.Choice{}, false, nil
	}
	c, err := o.store.Choice(ctx, email, e.Producer, e.Kind)
	if errors.Is(err, store.ErrNotFound) {
		return c, false, nil
	}
	return c, err == nil, err
}

// queuePerson puts the event in the person's list and queues it by each
// medium their rules give its weight, when each is due. A person who
// chose how to hear of the event's kind is told that way instead, and
// chose is true.
func (o *Outbox) queuePerson(ctx context.Context, e notice.Event, email string, pageID int64, round int, now time.Time) (queued int, chose bool, err error) {
	p, known, err := o.person(ctx, email)
	if err != nil {
		return 0, false, err
	}
	if err := o.store.AddInbox(ctx, e.ID, email, e.Weight, now); err != nil {
		return 0, false, err
	}
	can := func(m notice.Medium) bool {
		return o.Sends(m) && (m != notice.TeamsChat || known)
	}
	choice, chose, err := o.chose(ctx, e, email)
	if err != nil {
		return 0, false, err
	}
	if chose {
		p = p.Choosing(e.Weight, choice.Media)
		o.log.InfoContext(ctx, "a person's own choice for this kind says how they are told", "event", e.ID, "kind", e.Kind, "weight", e.Weight, "media", choice.Media)
	}
	plan := p.Plan(e.Weight, can, now)
	if e.Weight == notice.WeightNotify && len(plan) > 0 {
		if morning, over := o.overBudget(ctx, email, now); over {
			for i := range plan {
				if plan[i].At.Before(morning) {
					plan[i].At = morning
				}
			}
			o.log.InfoContext(ctx, "a person is over their page budget: notify waits for the morning", "event", e.ID, "until", morning)
		}
	}
	for _, send := range plan {
		added, err := o.store.AddMessage(ctx, e.ID, email, send.Medium, pageID, round, send.At, now)
		if err != nil {
			return queued, chose, err
		}
		if added {
			queued++
		}
	}
	switch {
	case chose && len(choice.Media) == 0:
	case len(plan) == 0 && e.Weight != notice.WeightNotice:
		o.log.InfoContext(ctx, "a person has no medium the desk can send for this weight", "event", e.ID, "page", pageID, "weight", e.Weight, "known", known)
	case queued == 0 && len(plan) > 0:
		o.log.InfoContext(ctx, "a person was already told", "event", e.ID, "page", pageID)
	}
	return queued, chose, nil
}

// sendMessages makes one attempt at every message whose time has come.
func (o *Outbox) sendMessages(ctx context.Context) {
	for {
		due, err := o.store.DueMessages(ctx, o.Now(), batch)
		if err != nil {
			o.log.ErrorContext(ctx, "due messages not read", "error", err)
			return
		}
		for _, m := range due {
			o.attemptMessage(ctx, m)
		}
		if len(due) < batch || ctx.Err() != nil {
			return
		}
	}
}

// attemptMessage sends one message once and writes what came of it.
func (o *Outbox) attemptMessage(ctx context.Context, m notice.Message) {
	status, err := o.postPerson(ctx, m)
	now := o.Now()
	m.Attempts++
	m.Status = status
	switch {
	case err == nil:
		m.State, m.SentAt, m.LastError = notice.Sent, &now, ""
		o.countMessage(ctx, "sent")
		o.log.InfoContext(ctx, "person told", "message", m.ID, "event", m.EventID, "medium", m.Medium, "page", m.PageID, "status", status, "attempt", m.Attempts)
	case errors.Is(err, notice.ErrUnreachable):
		m.State, m.LastError = notice.Skipped, err.Error()
		o.countMessage(ctx, "skipped")
		o.log.InfoContext(ctx, "message skipped", "message", m.ID, "event", m.EventID, "medium", m.Medium, "reason", err)
	default:
		m.LastError = err.Error()
		next, again := notice.Retry(m.Attempts, now)
		if again && !notice.Permanent(status) {
			m.NextAt = next
			o.countMessage(ctx, "retry")
			o.log.WarnContext(ctx, "message failed, trying again", "message", m.ID, "medium", m.Medium, "status", status, "attempt", m.Attempts, "next", next, "error", err)
		} else {
			m.State = notice.Failed
			o.countMessage(ctx, "failed")
			o.log.ErrorContext(ctx, "message failed for good", "message", m.ID, "medium", m.Medium, "status", status, "attempt", m.Attempts, "error", err)
			o.annals.Decide(ctx, annals.Decision{Verb: "message.give_up", Kind: "message", ID: fmt.Sprint(m.ID), By: incident.System(Machine),
				Key: fmt.Sprintf("message:%d:give_up", m.ID), Labels: map[string]string{"medium": string(m.Medium), "status": fmt.Sprint(status)}, At: now, Outcome: "failed", Reason: err.Error()})
		}
	}
	if err := o.store.SaveMessage(ctx, m); err != nil {
		o.log.ErrorContext(ctx, "message outcome not saved", "message", m.ID, "error", err)
	}
}

func (o *Outbox) postPerson(ctx context.Context, m notice.Message) (int, error) {
	e, err := o.store.Event(ctx, m.EventID)
	if err != nil {
		return 0, fmt.Errorf("event: %w", err)
	}
	ack := ""
	if m.PageID != 0 {
		p, err := o.store.Page(ctx, m.PageID)
		if err != nil {
			return 0, fmt.Errorf("page: %w", err)
		}
		if p.State != notice.PageActive {
			return 0, fmt.Errorf("%w: the page ended before this was sent", notice.ErrUnreachable)
		}
		ack = o.AckURL(p.ID)
	}
	p, _, err := o.person(ctx, m.Person)
	if err != nil {
		return 0, err
	}
	sender := o.media[m.Medium]
	if sender == nil {
		return 0, fmt.Errorf("%w: this desk does not send by %s", notice.ErrUnreachable, m.Medium)
	}
	return sender.SendPerson(ctx, notice.Outgoing{Person: p, Event: e, Ack: ack, PageID: m.PageID, MessageID: m.ID})
}

// escalate tells the next step of every page whose wait ran out, and ends
// a page whose last step went unanswered.
func (o *Outbox) escalate(ctx context.Context) {
	now := o.Now()
	due, err := o.store.DuePages(ctx, now)
	if err != nil {
		o.log.ErrorContext(ctx, "due pages not read", "error", err)
		return
	}
	for _, p := range due {
		if err := o.nextStep(ctx, p, now, incident.System(Machine)); err != nil {
			o.log.ErrorContext(ctx, "page not moved on", "page", p.ID, "error", err)
		}
	}
}

// nextStep tells a page's next step now, or ends it unanswered after its
// last.
func (o *Outbox) nextStep(ctx context.Context, p notice.Page, now time.Time, by incident.Actor) error {
	e, err := o.store.Event(ctx, p.EventID)
	if err != nil {
		return err
	}
	next, round := p.Step+1, p.Round
	if next >= len(p.Steps) && round < p.Repeat {
		next, round = 0, round+1
	}
	if next >= len(p.Steps) {
		ended, err := o.store.EndPage(ctx, p.ID, notice.PageUnanswered, "", now)
		if err != nil || !ended {
			return err
		}
		o.countPage(ctx, "unanswered")
		o.log.WarnContext(ctx, "page went unanswered", "page", p.ID, "event", p.EventID, "chain", p.Chain, "steps", len(p.Steps))
		o.decidePage(ctx, "page.unanswered", p, e, by, map[string]string{"chain": p.Chain})
		return nil
	}
	moved, err := o.store.StepPage(ctx, p.ID, next, round, now.Add(time.Duration(p.Steps[next].Wait)*time.Minute))
	if err != nil || !moved {
		return err
	}
	n, _, err := o.tellStep(ctx, e, p, next, round, now)
	if err != nil {
		return fmt.Errorf("step %d not queued: %w", next+1, err)
	}
	o.countPage(ctx, "escalated")
	o.log.InfoContext(ctx, "page escalated", "page", p.ID, "event", p.EventID, "chain", p.Chain, "step", next+1, "round", round, "messages", n)
	o.decidePage(ctx, "page.escalate", p, e, by, map[string]string{"chain": p.Chain, "step": fmt.Sprintf("%d.%d", round, next+1)})
	if n > 0 {
		o.poke()
	}
	return nil
}

// Escalate tells an active page's next step now, without waiting out the
// current one. It says false when the page had already ended.
func (o *Outbox) Escalate(ctx context.Context, pageID int64, by string) (bool, error) {
	p, err := o.store.Page(ctx, pageID)
	if err != nil {
		return false, err
	}
	if p.State != notice.PageActive {
		return false, nil
	}
	o.log.InfoContext(ctx, "page escalated by hand", "page", pageID, "by", by)
	return true, o.nextStep(ctx, p, o.Now(), incident.Person(by))
}

// AckHook hears a person acknowledge a page, with the event that started
// it, so the source learns: the desk acknowledges its incident.
type AckHook func(ctx context.Context, p notice.Page, e notice.Event, by string)

// OnAcknowledge adds a hook a person's acknowledge runs.
func (o *Outbox) OnAcknowledge(h AckHook) { o.acknowledged = append(o.acknowledged, h) }

// Acknowledge ends an active page because a person has it, and tells the
// source: the page's thread is acknowledged, the hooks run, and a
// page.acknowledged event goes out from dispatch for the producer to
// subscribe to. It says false when the page had already ended.
func (o *Outbox) Acknowledge(ctx context.Context, pageID int64, by string) (bool, error) {
	ended, err := o.endPage(ctx, pageID, by, incident.Person(by))
	if err != nil || !ended {
		return ended, err
	}
	p, err := o.store.Page(ctx, pageID)
	if err != nil {
		return true, err
	}
	e, err := o.store.Event(ctx, p.EventID)
	if err != nil {
		return true, err
	}
	now := o.Now()
	if e.Thread != 0 {
		if _, err := o.store.MoveThread(ctx, e.Thread, notice.ThreadAcknowledged, by, now); err != nil {
			return true, err
		}
	}
	for _, h := range o.acknowledged {
		h(ctx, p, e, by)
	}
	if e.Producer != Machine {
		back := notice.Event{
			Producer: Machine, Kind: "page.acknowledged", Key: fmt.Sprintf("page:%d:acknowledged", p.ID), Weight: notice.WeightNotice,
			Title: "Acknowledged: " + e.Title, Severity: e.Severity, Entity: e.Entity, At: now,
			Facts:  []notice.Fact{{Name: "By", Value: by}, {Name: "Chain", Value: p.Chain}, {Name: "Event", Value: e.Key}},
			Labels: map[string]string{"producer": e.Producer, "kind": e.Kind, "event_key": clipLabel(e.Key), "dedup_key": clipLabel(e.DedupKey)},
		}
		if _, err := o.Accept(ctx, back); err != nil {
			o.log.ErrorContext(ctx, "acknowledge not passed back", "page", p.ID, "error", err)
		}
	}
	return true, nil
}

func clipLabel(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// endPage ends an active page as acknowledged without telling the source,
// for a page its source already moved.
func (o *Outbox) endPage(ctx context.Context, pageID int64, by string, actor incident.Actor) (bool, error) {
	ended, err := o.store.EndPage(ctx, pageID, notice.PageAcknowledged, by, o.Now())
	if err != nil || !ended {
		return ended, err
	}
	o.countPage(ctx, "acknowledged")
	o.log.InfoContext(ctx, "page acknowledged", "page", pageID, "by_kind", actor.Kind)
	if p, err := o.store.Page(ctx, pageID); err == nil {
		if e, err := o.store.Event(ctx, p.EventID); err == nil {
			o.decidePage(ctx, "page.acknowledge", p, e, actor, map[string]string{"chain": p.Chain})
		}
	}
	return true, nil
}

// AcknowledgeFor ends the active pages started for a producer's events
// whose keys start with prefix, such as every event of one incident. The
// source moved them, so it is not told again.
func (o *Outbox) AcknowledgeFor(ctx context.Context, producer, prefix, by string) (int, error) {
	pages, err := o.store.ActivePagesFor(ctx, producer, prefix)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pages {
		ended, err := o.endPage(ctx, p.ID, by, incident.Person(by))
		if err != nil {
			return n, err
		}
		if ended {
			n++
		}
	}
	return n, nil
}

// decidePage posts a page's decision to Annals.
func (o *Outbox) decidePage(ctx context.Context, verb string, p notice.Page, e notice.Event, by incident.Actor, labels map[string]string) {
	labels["producer"] = e.Producer
	labels["kind"] = e.Kind
	key := fmt.Sprintf("page:%d:%s", p.ID, verb)
	if verb == "page.escalate" {
		key += ":" + labels["step"]
	}
	o.annals.Decide(ctx, annals.Decision{Verb: verb, Kind: "page", ID: fmt.Sprint(p.ID), Title: e.Title, By: by, Key: key, Labels: labels, At: o.Now()})
}

func (o *Outbox) countMessage(ctx context.Context, outcome string) {
	if o.messageOutcome != nil {
		o.messageOutcome(ctx, outcome)
	}
}

func (o *Outbox) countPage(ctx context.Context, outcome string) {
	if o.pageOutcome != nil {
		o.pageOutcome(ctx, outcome)
	}
}

// overBudget says whether a person has taken their settings' share of
// pages in seven days, and when their morning is: 07:00 next in the
// desk's zone.
func (o *Outbox) overBudget(ctx context.Context, email string, now time.Time) (time.Time, bool) {
	settings, err := o.store.Settings(ctx)
	if err != nil || settings.PersonPages <= 0 {
		return now, false
	}
	n, err := o.store.PagesOf(ctx, email, now.Add(-7*24*time.Hour))
	if err != nil || n < settings.PersonPages {
		return now, false
	}
	loc, err := time.LoadLocation(settings.Zone)
	if err != nil || settings.Zone == "" {
		loc = time.UTC
	}
	local := now.In(loc)
	morning := time.Date(local.Year(), local.Month(), local.Day(), 7, 0, 0, 0, loc)
	if !morning.After(local) {
		morning = morning.AddDate(0, 0, 1)
	}
	return morning, true
}
