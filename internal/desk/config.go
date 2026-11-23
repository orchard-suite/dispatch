package desk

import (
	"context"
	"fmt"
	"path"
	"sort"
	"time"

	"github.com/orchard-suite/dispatch/internal/config"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// Applied counts what a config document changed.
type Applied struct {
	People        int  `json:"people"`
	Groups        int  `json:"groups"`
	Chains        int  `json:"chains"`
	Schedules     int  `json:"schedules"`
	Subscriptions int  `json:"subscriptions"`
	Heartbeats    int  `json:"heartbeats"`
	Removed       int  `json:"removed"`
	Settings      bool `json:"settings"`
}

// Apply makes the desk's routing what a document says. It checks the
// whole document before it changes anything; a section given replaces
// what the desk has, except people, who are only added or changed.
func (d *Desk) Apply(ctx context.Context, doc config.Document, by string) (Applied, error) {
	var a Applied
	people := make([]notice.Person, 0, len(doc.People))
	for _, p := range doc.People {
		person, err := p.ToPerson()
		if err != nil {
			return a, err
		}
		people = append(people, person)
	}
	for i, g := range doc.Groups {
		if !notice.ValidSlug(g.Slug) || g.Name == "" || len(g.Members) == 0 {
			return a, fmt.Errorf("%w: group %d needs a slug, a name, and members", notice.ErrInvalid, i+1)
		}
		for j, m := range g.Members {
			email, err := notice.NormalEmail(m)
			if err != nil {
				return a, err
			}
			doc.Groups[i].Members[j] = email
		}
	}
	chains := make([]notice.Chain, 0, len(doc.Chains))
	for _, c := range doc.Chains {
		chain, err := c.ToChain()
		if err != nil {
			return a, fmt.Errorf("chain %s: %w", c.Slug, err)
		}
		chains = append(chains, chain)
	}
	schedules := make([]notice.Schedule, 0, len(doc.Schedules))
	for _, s := range doc.Schedules {
		sc, err := s.ToSchedule()
		if err != nil {
			return a, err
		}
		schedules = append(schedules, sc)
	}
	beats := make([]incident.Heartbeat, 0, len(doc.Heartbeats))
	for _, h := range doc.Heartbeats {
		hb, err := h.ToHeartbeat()
		if err != nil {
			return a, err
		}
		hb.CreatedBy, hb.CreatedAt = by, d.Now()
		beats = append(beats, hb)
	}
	subs, err := d.subscriptions(ctx, doc.Subscriptions, by)
	if err != nil {
		return a, err
	}
	var settings *settingsChange
	if doc.Settings != nil {
		if settings, err = d.checkSettings(ctx, *doc.Settings); err != nil {
			return a, err
		}
	}

	for _, p := range people {
		if old, err := d.Store.Person(ctx, p.Email); err == nil {
			p.CreatedAt = old.CreatedAt
			p.KeepConsent(old)
		} else {
			p.CreatedAt = d.Now()
		}
		if err := d.Store.SavePerson(ctx, p); err != nil {
			return a, err
		}
		a.People++
	}
	if doc.Groups != nil {
		have, err := d.Store.Groups(ctx)
		if err != nil {
			return a, err
		}
		keep := map[string]bool{}
		for _, g := range doc.Groups {
			keep[g.Slug] = true
			if err := d.Store.SaveGroup(ctx, g); err != nil {
				return a, err
			}
			a.Groups++
		}
		for _, g := range have {
			if !keep[g.Slug] {
				if err := d.Store.DeleteGroup(ctx, g.Slug); err != nil {
					return a, err
				}
				a.Removed++
			}
		}
	}
	if doc.Schedules != nil {
		have, err := d.Store.Schedules(ctx, d.Now())
		if err != nil {
			return a, err
		}
		keep := map[string]bool{}
		for _, sc := range schedules {
			keep[sc.Slug] = true
			if err := d.Store.SaveSchedule(ctx, sc); err != nil {
				return a, err
			}
			a.Schedules++
		}
		for _, sc := range have {
			if !keep[sc.Slug] {
				if err := d.Store.DeleteSchedule(ctx, sc.Slug); err != nil {
					return a, err
				}
				a.Removed++
			}
		}
	}
	if doc.Chains != nil {
		have, err := d.Store.Chains(ctx)
		if err != nil {
			return a, err
		}
		keep := map[string]bool{}
		for _, c := range chains {
			keep[c.Slug] = true
			if err := d.Store.SaveChain(ctx, c); err != nil {
				return a, err
			}
			a.Chains++
		}
		for _, c := range have {
			if !keep[c.Slug] {
				if err := d.Store.DeleteChain(ctx, c.Slug); err != nil {
					return a, err
				}
				a.Removed++
			}
		}
	}
	if doc.Subscriptions != nil {
		have, err := d.Store.Subscriptions(ctx)
		if err != nil {
			return a, err
		}
		for _, s := range have {
			if err := d.Store.DeleteSubscription(ctx, s.ID); err != nil {
				return a, err
			}
		}
		for _, s := range subs {
			if _, err := d.outbox.AddSubscription(ctx, s); err != nil {
				return a, err
			}
			a.Subscriptions++
		}
		a.Removed += len(have)
	}
	if doc.Heartbeats != nil {
		have, err := d.Store.Heartbeats(ctx)
		if err != nil {
			return a, err
		}
		keep := map[string]bool{}
		for _, h := range beats {
			keep[h.Name] = true
			if err := d.Store.SaveHeartbeat(ctx, h); err != nil {
				return a, err
			}
			a.Heartbeats++
		}
		for _, h := range have {
			if !keep[h.Name] {
				if err := d.Store.DeleteHeartbeat(ctx, h.Name); err != nil {
					return a, err
				}
				a.Removed++
			}
		}
	}
	if settings != nil {
		if err := d.Store.SaveSettings(ctx, settings.apply); err != nil {
			return a, err
		}
		a.Settings = true
	}
	d.log.InfoContext(ctx, "config applied", "people", a.People, "groups", a.Groups, "chains", a.Chains, "schedules", a.Schedules, "subscriptions", a.Subscriptions, "heartbeats", a.Heartbeats, "removed", a.Removed)
	return a, nil
}

// subscriptions checks a document's subscriptions: a channel by its name,
// or a target.
func (d *Desk) subscriptions(ctx context.Context, in []config.Subscription, by string) ([]notice.Subscription, error) {
	if in == nil {
		return nil, nil
	}
	if d.outbox == nil {
		return nil, fmt.Errorf("%w: this desk sends no notices", notice.ErrInvalid)
	}
	channels, err := d.Store.Channels(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]int64{}
	for _, c := range channels {
		byName[c.Name] = c.ID
	}
	out := make([]notice.Subscription, 0, len(in))
	for i, s := range in {
		sub := notice.Subscription{Topics: s.Topics, Producers: s.Producers, Anchor: s.About, Match: s.Match, Paused: s.Paused, CreatedBy: by}
		switch {
		case s.Channel != "" && s.To != "":
			return nil, fmt.Errorf("%w: subscription %d names a channel and a target", notice.ErrInvalid, i+1)
		case s.Channel != "":
			id, ok := byName[s.Channel]
			if !ok {
				return nil, fmt.Errorf("%w: subscription %d: there is no channel %q; channels are added on Notifications", notice.ErrInvalid, i+1, s.Channel)
			}
			sub.ChannelID = id
		default:
			t, err := notice.ParseTarget(s.To)
			if err != nil {
				return nil, fmt.Errorf("subscription %d: %w", i+1, err)
			}
			sub.Target = &t
		}
		if len(sub.Topics) == 0 {
			return nil, fmt.Errorf("%w: subscription %d needs topics", notice.ErrInvalid, i+1)
		}
		for _, g := range append(append([]string{}, sub.Topics...), values(sub.Match)...) {
			if _, err := path.Match(g, ""); err != nil {
				return nil, fmt.Errorf("%w: subscription %d: %q is not a glob", notice.ErrInvalid, i+1, g)
			}
		}
		out = append(out, sub)
	}
	return out, nil
}

func values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

type settingsChange struct{ apply store.Settings }

// checkSettings reads a document's settings over the desk's own.
func (d *Desk) checkSettings(ctx context.Context, s config.Settings) (*settingsChange, error) {
	st, err := d.Store.Settings(ctx)
	if err != nil {
		return nil, err
	}
	rules, bad := incident.ParseRules(s.Rules)
	routes, badRoutes := incident.ParseRoutes(s.Routes)
	if bad = append(bad, badRoutes...); len(bad) > 0 {
		return nil, fmt.Errorf("%w: these rule and route lines cannot be read: %v", notice.ErrInvalid, bad)
	}
	if s.GroupingMinutes < 0 || s.GroupingMinutes > 7*24*60 {
		return nil, fmt.Errorf("%w: grouping_minutes is 0 to 10080", notice.ErrInvalid)
	}
	if s.QuietResolveHours < 0 || s.QuietResolveHours > 30*24 {
		return nil, fmt.Errorf("%w: quiet_resolve_hours is 0 to 720", notice.ErrInvalid)
	}
	if s.Zone != "" {
		if _, err := time.LoadLocation(s.Zone); err != nil {
			return nil, fmt.Errorf("%w: %q is not a time zone", notice.ErrInvalid, s.Zone)
		}
	}
	st.GroupingWindow = time.Duration(s.GroupingMinutes) * time.Minute
	st.AutoResolveInfo, st.Rules, st.Routes = s.AutoResolveInfo, rules, routes
	st.QuietResolve = time.Duration(s.QuietResolveHours) * time.Hour
	if s.StatusTitle != "" {
		st.StatusTitle = s.StatusTitle
	}
	if s.Public == nil {
		s.Public = map[string]string{}
	}
	st.PublicComponents, st.UpdateTemplates, st.IssueRepos = s.Public, s.Templates, s.IssueRepos
	st.Zone, st.PersonPages, st.ServicePages = s.Zone, s.PersonPages, s.ServicePages
	return &settingsChange{apply: st}, nil
}

// Export is the desk's routing as a document.
func (d *Desk) Export(ctx context.Context) (config.Document, error) {
	var doc config.Document
	st, err := d.Store.Settings(ctx)
	if err != nil {
		return doc, err
	}
	doc.Settings = &config.Settings{GroupingMinutes: int(st.GroupingWindow / time.Minute), AutoResolveInfo: st.AutoResolveInfo,
		QuietResolveHours: int(st.QuietResolve / time.Hour),
		Rules:             incident.FormatRules(st.Rules), Routes: incident.FormatRoutes(st.Routes), StatusTitle: st.StatusTitle, Public: st.PublicComponents,
		Templates: st.UpdateTemplates, IssueRepos: st.IssueRepos, Zone: st.Zone, PersonPages: st.PersonPages, ServicePages: st.ServicePages}
	people, err := d.Store.People(ctx)
	if err != nil {
		return doc, err
	}
	for _, p := range people {
		doc.People = append(doc.People, config.FromPerson(p))
	}
	if doc.Groups, err = d.Store.Groups(ctx); err != nil {
		return doc, err
	}
	chains, err := d.Store.Chains(ctx)
	if err != nil {
		return doc, err
	}
	for _, c := range chains {
		doc.Chains = append(doc.Chains, config.Chain{Slug: c.Slug, Name: c.Name, Steps: notice.FormatSteps(c.Steps), Repeat: c.Repeat})
	}
	schedules, err := d.Store.Schedules(ctx, d.Now())
	if err != nil {
		return doc, err
	}
	for _, s := range schedules {
		doc.Schedules = append(doc.Schedules, config.FromSchedule(s))
	}
	channels, err := d.Store.Channels(ctx)
	if err != nil {
		return doc, err
	}
	names := map[int64]string{}
	for _, c := range channels {
		names[c.ID] = c.Name
	}
	subs, err := d.Store.Subscriptions(ctx)
	if err != nil {
		return doc, err
	}
	for _, s := range subs {
		out := config.Subscription{Channel: names[s.ChannelID], Topics: s.Topics, Producers: s.Producers, About: s.Anchor, Match: s.Match, Paused: s.Paused}
		if s.Target != nil {
			out.Channel, out.To = "", s.Target.String()
		}
		doc.Subscriptions = append(doc.Subscriptions, out)
	}
	beats, err := d.Store.Heartbeats(ctx)
	if err != nil {
		return doc, err
	}
	for _, h := range beats {
		out := config.Heartbeat{Name: h.Name, Every: h.Interval.String(), Service: h.Service, Severity: string(h.Severity)}
		if h.Grace > 0 {
			out.Grace = h.Grace.String()
		}
		doc.Heartbeats = append(doc.Heartbeats, out)
	}
	sort.Slice(doc.People, func(i, j int) bool { return doc.People[i].Email < doc.People[j].Email })
	return doc, nil
}
