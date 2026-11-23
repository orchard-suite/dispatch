// Package notice is the notification model: an event any product sends,
// the channels events go to, the subscriptions that decide which events go
// to which channel, and a delivery of one event to one channel. It has no
// storage and no transport.
package notice

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Ref names one entity of the catalog.
type Ref struct {
	Blueprint string `json:"blueprint"`
	Key       string `json:"key"`
	Title     string `json:"title,omitempty"`
}

// ID is the catalog's id for the entity, "blueprint:key".
func (r Ref) ID() string { return r.Blueprint + ":" + r.Key }

// ParseRef reads "blueprint:key".
func ParseRef(id string) (Ref, bool) {
	bp, key, ok := strings.Cut(strings.TrimSpace(id), ":")
	if !ok || bp == "" || key == "" {
		return Ref{}, false
	}
	return Ref{Blueprint: bp, Key: key}, true
}

// Fact is one line of detail a card shows: a name and its value.
type Fact struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Link is a button on a card.
type Link struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Action is a call on the producer's own API that a channel able to act,
// the Teams bot, offers as a button. Whoever clicks it signs in and makes
// the call as themselves; the producer decides whether they may. Done is
// the past tense the card shows after, such as "approved".
type Action struct {
	Title string `json:"title"`
	Path  string `json:"path"`
	Style string `json:"style,omitempty"`
	Done  string `json:"done"`
}

// Event is one thing a product says happened. Producer is the name of the
// token that sent it, never the body's word. Key is the producer's own id
// for it, so a retried send is stored once. Weight says how hard it pushes
// on people. DedupKey ties it to a thread its Move moves: a later trigger
// on an open thread reaches channels but tells no person again.
type Event struct {
	ID       int64  `json:"id,omitempty"`
	Producer string `json:"producer"`
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Title    string `json:"title"`
	Text     string `json:"text,omitempty"`
	// Page is the event drawn by its producer as a whole HTML page, for
	// mail alone: a letter's body is the page, and no other medium or
	// channel is given it.
	Page     string            `json:"page,omitempty"`
	Severity string            `json:"severity,omitempty"`
	Entity   *Ref              `json:"entity,omitempty"`
	Facts    []Fact            `json:"facts,omitempty"`
	Links    []Link            `json:"links,omitempty"`
	Actions  []Action          `json:"actions,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	// To addresses people, groups, and chains besides the channels whose
	// subscriptions take the event.
	To       []Target `json:"to,omitempty"`
	Weight   Weight   `json:"weight,omitempty"`
	Move     Move     `json:"action,omitempty"`
	DedupKey string   `json:"dedup_key,omitempty"`
	// Thread is the thread the dedup key named, set by dispatch.
	Thread int64     `json:"thread,omitempty"`
	At     time.Time `json:"at"`
}

// MaxPage is the most bytes of HTML an event's page may hold.
const MaxPage = 256 << 10

var (
	ErrInvalid = errors.New("invalid")
	kindRe     = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)+$`)
)

// Severities an event may carry; empty is none.
var Severities = []string{"critical", "warning", "info"}

// Validate checks an event the way the API takes it.
func (e Event) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	switch {
	case !kindRe.MatchString(e.Kind):
		return bad("kind must be dotted lowercase words, such as action.finished")
	case e.Key == "" || len(e.Key) > 200:
		return bad("key must be 1 to 200 characters")
	case strings.TrimSpace(e.Title) == "" || len(e.Title) > 300:
		return bad("title must be 1 to 300 characters")
	case len(e.Text) > 4000:
		return bad("text must be at most 4000 characters")
	case len(e.Page) > MaxPage:
		return bad("page must be at most %d bytes", MaxPage)
	case e.Severity != "" && !oneOf(e.Severity, Severities):
		return bad("severity must be critical, warning, or info")
	case len(e.Facts) > 20:
		return bad("at most 20 facts")
	case len(e.Links) > 5:
		return bad("at most 5 links")
	case len(e.Actions) > 3:
		return bad("at most 3 actions")
	case len(e.Labels) > 30:
		return bad("at most 30 labels")
	case e.Entity != nil && (e.Entity.Blueprint == "" || e.Entity.Key == ""):
		return bad("entity needs a blueprint and a key")
	case len(e.To) > 20:
		return bad("at most 20 targets in to")
	case len(e.DedupKey) > 255:
		return bad("dedup_key must be at most 255 characters")
	case e.Move != "" && e.Move != Trigger && e.DedupKey == "":
		return bad("acknowledge and resolve need the dedup_key of what they move")
	}
	if _, err := ParseWeight(string(e.Weight)); err != nil {
		return err
	}
	if _, err := ParseMove(string(e.Move)); err != nil {
		return err
	}
	for i, t := range e.To {
		n, err := t.Normal()
		if err != nil {
			return err
		}
		e.To[i] = n
	}
	for _, f := range e.Facts {
		if f.Name == "" || len(f.Name) > 80 || len(f.Value) > 500 {
			return bad("a fact needs a name of at most 80 characters and a value of at most 500")
		}
	}
	for _, l := range e.Links {
		u, err := url.Parse(l.URL)
		if l.Title == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return bad("a link needs a title and an absolute http or https URL")
		}
	}
	for _, a := range e.Actions {
		if a.Title == "" || len(a.Title) > 40 || a.Done == "" || len(a.Done) > 40 || !strings.HasPrefix(a.Path, "/") || strings.HasPrefix(a.Path, "//") || len(a.Path) > 300 || strings.ContainsAny(a.Path, "?# ") {
			return bad("an action needs a title, a done word, and a path on the producer's API, such as /v1/runs/1/approve")
		}
		if a.Style != "" && a.Style != "positive" && a.Style != "destructive" {
			return bad("an action's style is positive or destructive")
		}
	}
	return nil
}

// Kind is how a channel is spoken to.
type Kind string

const (
	// Teams posts an Adaptive Card to a Teams Workflows webhook.
	Teams Kind = "teams"
	// Webhook posts the event as JSON.
	Webhook Kind = "webhook"
	// Bot posts an Adaptive Card as dispatch's Teams bot, into a channel of a
	// team the bot is in, with the event's actions as buttons.
	Bot Kind = "bot"
	// DiscordWebhook posts a card to a Discord channel's own webhook.
	DiscordWebhook Kind = "discord"
	// DiscordBot posts a card as dispatch's Discord bot into a server
	// channel it can see, by the channel's id.
	DiscordBot Kind = "discord-bot"
)

// Kinds are the channel kinds added by URL; a bot channel is picked from
// the teams or servers the bot is in.
var Kinds = []Kind{Teams, DiscordWebhook, Webhook}

// Channel is one place events go. Target is the URL it posts to, which is
// a secret: whoever holds it can post there; for a bot channel it is the
// team and channel, "team-id/channel-id". It is kept sealed and never shown
// again once saved.
type Channel struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Kind   Kind   `json:"kind"`
	Target string `json:"-"`
	// Secret signs a webhook channel's posts and OldSecret, while set, signs
	// them too; both are sealed like the target.
	Secret    string    `json:"-"`
	OldSecret string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// Signs says whether the channel's posts carry a signature.
func (c Channel) Signs() bool { return c.Secret != "" }

// Retiring says whether an old secret still signs too.
func (c Channel) Retiring() bool { return c.OldSecret != "" }

// ValidTarget checks a channel's target for its kind.
func ValidTarget(kind Kind, target string) error {
	if kind == DiscordBot {
		if !discordRe.MatchString(target) {
			return fmt.Errorf("%w: pick a channel of a server the Discord bot is in", ErrInvalid)
		}
		return nil
	}
	if kind == Bot {
		team, channel, ok := strings.Cut(target, "/")
		if !ok || team == "" || channel == "" || strings.Contains(channel, "/") {
			return fmt.Errorf("%w: pick a channel of a team the bot is in", ErrInvalid)
		}
		return nil
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: the channel's URL must be an absolute https URL", ErrInvalid)
	}
	if kind == DiscordWebhook && ((u.Host != "discord.com" && u.Host != "discordapp.com") || !strings.HasPrefix(u.Path, "/api/webhooks/")) {
		return fmt.Errorf("%w: a Discord channel's URL is its webhook, https://discord.com/api/webhooks/...", ErrInvalid)
	}
	return nil
}

// Subscription sends a channel, or a person, group, or chain, the events
// it takes. Topics are globs on the
// event kind ("action.*", "*"). Producers, when any are given, are the only
// products it takes events from. Anchor, when set, takes only events about
// that catalog entity or about something that reaches it through its
// relations, such as every app service of a program. Match holds label
// globs that must all hold.
type Subscription struct {
	ID        int64             `json:"id"`
	ChannelID int64             `json:"channel_id,omitempty"`
	Target    *Target           `json:"target,omitempty"`
	Topics    []string          `json:"topics"`
	Producers []string          `json:"producers,omitempty"`
	Anchor    string            `json:"anchor,omitempty"`
	Match     map[string]string `json:"match,omitempty"`
	Paused    bool              `json:"paused"`
	CreatedAt time.Time         `json:"created_at"`
	CreatedBy string            `json:"created_by"`
}

// Takes says whether the subscription takes the event. reach is asked only
// when the anchor is not the event's own entity: it lists the ids the
// event's entity reaches through its relations.
func (s Subscription) Takes(e Event, reach func() []string) bool {
	if s.Paused {
		return false
	}
	if len(s.Producers) > 0 && !oneOf(e.Producer, s.Producers) {
		return false
	}
	if !globAny(s.Topics, e.Kind) {
		return false
	}
	for label, glob := range s.Match {
		if ok, _ := path.Match(glob, e.Labels[label]); !ok {
			return false
		}
	}
	if s.Anchor == "" {
		return true
	}
	if e.Entity == nil {
		return false
	}
	if e.Entity.ID() == s.Anchor {
		return true
	}
	return oneOf(s.Anchor, reach())
}

// Topics are the event kinds dispatch and the suite's products send today,
// offered as switches on a subscription. A subscription may name any other.
var Topics = []struct{ Kind, Means string }{
	{"incident.opened", "An incident opened"},
	{"incident.state_changed", "An incident was acknowledged, mitigated, resolved, or reopened"},
	{"incident.cleared", "Every alert of an incident stopped; it closes itself unless one fires again"},
	{"action.requested", "Someone ran a self-service action"},
	{"action.awaiting_approval", "A run waits for someone to approve it"},
	{"action.approved", "A waiting run was approved"},
	{"action.declined", "A waiting run was declined"},
	{"action.finished", "A run succeeded or failed"},
}

func globAny(globs []string, s string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, s); ok {
			return true
		}
	}
	return false
}

func oneOf(s string, list []string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// DeliveryState is where one delivery is.
type DeliveryState string

const (
	Pending DeliveryState = "pending"
	Sent    DeliveryState = "sent"
	Failed  DeliveryState = "failed"
)

// Delivery is one event on its way to one channel. Status is the answer the
// channel gave, the proof it took the message.
type Delivery struct {
	ID             int64         `json:"id"`
	EventID        int64         `json:"event_id"`
	ChannelID      int64         `json:"channel_id"`
	SubscriptionID int64         `json:"subscription_id"`
	State          DeliveryState `json:"state"`
	Attempts       int           `json:"attempts"`
	NextAt         time.Time     `json:"next_at"`
	Status         int           `json:"status,omitempty"`
	LastError      string        `json:"last_error,omitempty"`
	SentAt         *time.Time    `json:"sent_at,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
}

// Waits are how long a delivery waits after each failed attempt. After the
// last it has failed for good.
var Waits = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute, time.Hour}

// Retry says when a delivery that failed its attempt-th try goes again, and
// false once it has had every try.
func Retry(attempt int, now time.Time) (time.Time, bool) {
	if attempt < 1 || attempt > len(Waits) {
		return time.Time{}, false
	}
	return now.Add(Waits[attempt-1]), true
}

// Permanent says whether an answer means trying again will not help.
func Permanent(status int) bool {
	return status >= 400 && status < 500 && status != 408 && status != 429
}
