package desk

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/github"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// Rooms opens a chat room for an incident and posts its changes there:
// with Teams, a channel the bot is in.
type Rooms interface {
	// Open makes a room named for the incident and answers where it is.
	Open(ctx context.Context, inc incident.Incident) (string, error)
	// Post puts an event in a room.
	Post(ctx context.Context, room string, e notice.Event) error
}

// UseRooms opens a room for every incident at least as bad as worst, and
// posts its changes there.
func (d *Desk) UseRooms(r Rooms, worst incident.Severity) { d.rooms, d.roomsFrom = r, worst }

// HasRooms says whether the desk opens incident rooms.
func (d *Desk) HasRooms() bool { return d.rooms != nil }

// UseIssues opens an action item's issue through c.
func (d *Desk) UseIssues(c *github.Client) { d.issues = c }

// UseStatusURL is the public status page's address, for followers' links.
func (d *Desk) UseStatusURL(u string) { d.statusURL = strings.TrimRight(u, "/") }

// SetRole gives a person a role on an incident: commander, comms, or
// scribe; an empty person takes the role away.
func (d *Desk) SetRole(ctx context.Context, id int64, role, person, actor string) (incident.Incident, error) {
	if !incident.ValidRole(role) {
		return incident.Incident{}, fmt.Errorf("%w: a role is commander, comms, or scribe", ErrInput)
	}
	person = strings.ToLower(strings.TrimSpace(person))
	if person != "" {
		if _, err := notice.NormalEmail(person); err != nil {
			return incident.Incident{}, fmt.Errorf("%w: a role goes to a person by address", ErrInput)
		}
	}
	text := strings.ToUpper(role[:1]) + role[1:] + ": " + person + ", set by " + actor
	if person == "" {
		text = "No " + role + " now, set by " + actor
	}
	return d.edit(ctx, id, incident.EventAssigned, actor, text, func(tx *sql.Tx, i *incident.Incident) error {
		return d.Store.SetRole(ctx, tx, id, role, person)
	})
}

// Merge folds one incident into another: its alerts move, the two are
// linked, and the merged one resolves, as the person who merged it.
func (d *Desk) Merge(ctx context.Context, from, into int64, by incident.Actor) (incident.Incident, error) {
	if from == into {
		return incident.Incident{}, fmt.Errorf("%w: an incident merges into another", ErrInput)
	}
	now := d.Now()
	var merged incident.Incident
	var change incident.Event
	var before incident.State
	err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
		target, err := d.Store.Get(ctx, tx, into)
		if err != nil {
			return err
		}
		if merged, err = d.Store.Get(ctx, tx, from); err != nil {
			return err
		}
		if err := d.Store.MoveAlerts(ctx, tx, from, into); err != nil {
			return err
		}
		if err := d.Store.AddLink(ctx, tx, from, into, "merged", by.Name(), now); err != nil {
			return err
		}
		if _, err := d.event(ctx, tx, into, now, incident.EventNote, by.Name(), fmt.Sprintf("#%d %s merged in by %s", merged.ID, merged.Title, by.Name())); err != nil {
			return err
		}
		before = merged.State
		if merged.State != incident.Resolved {
			merged.State, merged.ResolvedAt, merged.UpdatedAt = incident.Resolved, &now, now
			if err := d.Store.Update(ctx, tx, merged); err != nil {
				return err
			}
		}
		if change, err = d.event(ctx, tx, from, now, incident.EventResolved, by.Name(), fmt.Sprintf("Merged into #%d %s by %s", target.ID, target.Title, by.Name())); err != nil {
			return err
		}
		merged, err = d.Store.Get(ctx, tx, from)
		return err
	})
	if err != nil {
		return merged, err
	}
	if before != incident.Resolved {
		d.changed(ctx, merged, change, before, by)
	}
	return merged, nil
}

// Link ties two incidents together as related.
func (d *Desk) Link(ctx context.Context, a, b int64, actor string) error {
	if a == b {
		return fmt.Errorf("%w: an incident links to another", ErrInput)
	}
	now := d.Now()
	return d.Store.Tx(ctx, func(tx *sql.Tx) error {
		other, err := d.Store.Get(ctx, tx, b)
		if err != nil {
			return err
		}
		if _, err := d.Store.Get(ctx, tx, a); err != nil {
			return err
		}
		if err := d.Store.AddLink(ctx, tx, a, b, "related", actor, now); err != nil {
			return err
		}
		_, err = d.event(ctx, tx, a, now, incident.EventNote, actor, fmt.Sprintf("Linked to #%d %s by %s", other.ID, other.Title, actor))
		return err
	})
}

// UpdateTemplate is the start of a status update for an incident, from
// the settings' template for its severity.
func (d *Desk) UpdateTemplate(ctx context.Context, inc incident.Incident) string {
	settings, err := d.Store.Settings(ctx)
	if err != nil {
		return ""
	}
	t := settings.UpdateTemplates[string(inc.Severity)]
	if t == "" {
		t = "We are looking into {title}. {service} is {state}. Next update within the hour."
	}
	// Titles are often sentences; the template brings its own stop.
	title := strings.TrimRight(strings.TrimSpace(inc.Title), ".")
	return strings.NewReplacer("{title}", title, "{service}", inc.Service, "{state}", string(inc.State)).Replace(t)
}

// tellFollowers emails the status page's followers about a public
// incident, each with their own way to stop.
func (d *Desk) tellFollowers(ctx context.Context, inc incident.Incident, what, key string) {
	if d.outbox == nil || !inc.Public || d.statusURL == "" {
		return
	}
	followers, err := d.Store.Followers(ctx)
	if err != nil {
		d.log.ErrorContext(ctx, "followers not read", "error", err)
		return
	}
	for _, f := range followers {
		d.accept(ctx, notice.Event{Producer: Machine, Kind: "status.updated", Key: fmt.Sprintf("status:%s:%s", key, f.Token[:12]), Weight: notice.WeightNotify, At: d.Now(),
			Title: inc.Title + ": " + string(inc.State), Text: what,
			Links: []notice.Link{{Title: "The status page", URL: d.statusURL}, {Title: "Stop these emails", URL: d.statusURL + "/unsubscribe?token=" + f.Token}},
			To:    []notice.Target{{Person: f.Email}}}, "follower")
	}
	if len(followers) > 0 {
		d.log.InfoContext(ctx, "status followers told", "incident", inc.ID, "followers", len(followers))
	}
}

// openIssue opens the issue that tracks an action item, in the repository
// the settings give its incident's service.
func (d *Desk) openIssue(ctx context.Context, inc incident.Incident, item incident.ActionItem) {
	if d.issues == nil {
		return
	}
	settings, err := d.Store.Settings(ctx)
	if err != nil {
		return
	}
	repo := settings.IssueRepos[inc.Service]
	if repo == "" {
		repo = settings.IssueRepos["*"]
	}
	if repo == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		body := fmt.Sprintf("From incident #%d, %s: %s\n\nOwner: %s", inc.ID, inc.Title, d.url(inc), item.Owner)
		url, err := d.issues.Open(ctx, repo, item.Text, body)
		if err != nil {
			d.log.WarnContext(ctx, "action item's issue not opened", "incident", inc.ID, "action", item.ID, "error", err)
			return
		}
		if err := d.Store.SetIssue(ctx, item.ID, url); err != nil {
			d.log.ErrorContext(ctx, "action item's issue not kept", "action", item.ID, "error", err)
		}
		d.log.InfoContext(ctx, "action item's issue opened", "incident", inc.ID, "action", item.ID)
	}()
}

// SyncIssues ticks off every action item whose issue was closed.
func (d *Desk) SyncIssues(ctx context.Context) {
	if d.issues == nil {
		return
	}
	items, err := d.Store.OpenIssues(ctx)
	if err != nil {
		d.log.ErrorContext(ctx, "action items not read", "error", err)
		return
	}
	for _, a := range items {
		closed, err := d.issues.Closed(ctx, a.IssueURL)
		if err != nil {
			d.log.WarnContext(ctx, "issue not read", "action", a.ID, "error", err)
			continue
		}
		if closed {
			if err := d.SetActionDone(ctx, a.IncidentID, a.ID, true, Machine); err == nil {
				d.log.InfoContext(ctx, "action item done: its issue closed", "action", a.ID)
			}
		}
	}
}

// OpenRoom opens a room for an incident now, if it has none.
func (d *Desk) OpenRoom(ctx context.Context, id int64) (string, error) {
	if d.rooms == nil {
		return "", fmt.Errorf("%w: this desk opens no rooms", ErrInput)
	}
	room, err := d.Store.TeamsChannel(ctx, id)
	if err != nil || room != "" {
		return room, err
	}
	inc, err := d.Store.Get(ctx, d.Store.DB(), id)
	if err != nil {
		return "", err
	}
	if room, err = d.rooms.Open(ctx, inc); err != nil {
		return "", err
	}
	if err := d.Store.SetTeamsChannel(ctx, id, room); err != nil {
		return room, err
	}
	d.log.InfoContext(ctx, "incident room opened", "incident", id)
	d.postRoom(ctx, inc, fmt.Sprintf("#%d %s", inc.ID, inc.Title), "The room for this incident. Its changes post here.")
	return room, nil
}

// postRoom posts a line to an incident's room, when it has one.
func (d *Desk) postRoom(ctx context.Context, inc incident.Incident, title, text string) {
	if d.rooms == nil {
		return
	}
	room, err := d.Store.TeamsChannel(ctx, inc.ID)
	if err != nil || room == "" {
		return
	}
	e := notice.Event{Producer: Machine, Kind: "incident.room", Title: title, Text: text, Severity: string(inc.Severity), At: d.Now(),
		Facts: []notice.Fact{{Name: "State", Value: string(inc.State)}, {Name: "Service", Value: inc.Service}},
		Links: []notice.Link{{Title: "Open the incident", URL: d.url(inc)}}}
	ctx = context.WithoutCancel(ctx)
	go func() {
		if err := d.rooms.Post(ctx, room, e); err != nil {
			d.log.WarnContext(ctx, "incident room post failed", "incident", inc.ID, "error", err)
		}
	}()
}

// About is what the catalog knows of the workload an incident is on: its
// runbook, docs, owner, and repository.
func (d *Desk) About(ctx context.Context, inc incident.Incident) map[string]string {
	if inc.Workload == "" {
		return nil
	}
	return d.orrery.About(ctx, inc.Workload)
}

// Follow mails an address a link that confirms it wants the status page's
// updates. An address already confirmed is not mailed again.
func (d *Desk) Follow(ctx context.Context, email string) error {
	if d.outbox == nil || d.statusURL == "" {
		return fmt.Errorf("%w: this desk mails no status updates", ErrInput)
	}
	token, confirmed, err := d.Store.Subscribe(ctx, email, d.Now())
	if err != nil || confirmed {
		return err
	}
	_, err = d.outbox.Accept(ctx, notice.Event{Producer: Machine, Kind: "status.confirm", Key: "follow:" + token[:12] + fmt.Sprint(":", d.Now().Unix()/3600),
		Title: "Confirm you want status updates", Text: "Someone asked for updates on public incidents to come to this address. If it was you, confirm; if not, do nothing and none will come.",
		Weight: notice.WeightNotify, At: d.Now(), To: []notice.Target{{Person: email}},
		Links: []notice.Link{{Title: "Confirm", URL: d.statusURL + "/confirm?token=" + token}}})
	return err
}
