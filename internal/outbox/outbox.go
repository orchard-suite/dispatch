// Package outbox takes events from any product, decides which channels
// each goes to by the subscriptions, and delivers them. Every delivery is a
// row before anything is sent, so a restart loses nothing: the sender picks
// up what is due, keeps the channel's answer as proof, and tries again on a
// growing wait until the channel takes it or the tries run out.
package outbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/discord"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/seal"
	"github.com/orchard-suite/dispatch/internal/store"
)

// Reach lists the catalog ids an entity reaches through its relations.
type Reach func(ctx context.Context, ref notice.Ref) []string

// BotSender posts an event to a bot channel, "team-id/channel-id" for
// Teams or a channel's id for Discord, and says what the far end answered.
type BotSender interface {
	Send(ctx context.Context, target string, e notice.Event) (int, error)
}

type Outbox struct {
	store   *store.Store
	seal    *seal.Sealer
	reach   Reach
	log     *slog.Logger
	client  *http.Client
	bot     BotSender
	discord BotSender
	outcome func(ctx context.Context, outcome string)
	wake    chan struct{}

	media                       map[notice.Medium]PersonSender
	deskURL                     string
	messageOutcome, pageOutcome func(ctx context.Context, outcome string)
	annals                      *annals.Poster
	acknowledged                []AckHook
	hush                        func(ctx context.Context, e notice.Event) bool
	// Now is replaced in tests.
	Now func() time.Time
}

// How often the sender looks for due deliveries when nothing wakes it, and
// how many it takes at a time.
const (
	every = 5 * time.Second
	batch = 25
)

func New(st *store.Store, sealer *seal.Sealer, reach Reach, log *slog.Logger, outcome func(ctx context.Context, outcome string)) *Outbox {
	return &Outbox{
		store: st, seal: sealer, reach: reach, log: log, outcome: outcome,
		client: telemetry.Client(20 * time.Second),
		wake:   make(chan struct{}, 1),
		media:  map[notice.Medium]PersonSender{},
		Now:    time.Now,
	}
}

// Accepted says what came of an event.
type Accepted struct {
	Event notice.Event `json:"event"`
	// Queued counts the channels it is on its way to.
	Queued int `json:"queued"`
	// Messages counts the messages on their way to people; Pages, the
	// chains it started.
	Messages int `json:"messages"`
	Pages    int `json:"pages"`
	// Repeat is true when the producer had already sent this key.
	Repeat bool `json:"repeat"`
	// Deduplicated is true when a trigger joined a thread already open:
	// channels have it, and nobody was told again.
	Deduplicated bool `json:"deduplicated"`
	// Dropped says why an acknowledge or resolve moved nothing, such as
	// no open thread under its dedup key.
	Dropped string `json:"dropped,omitempty"`
}

// Accept stores an event and queues it for every channel a subscription
// sends it to, and for the people, groups, and chains it is addressed to
// or a subscription sends it to. A repeat of a key the producer already
// sent queues nothing. An event with a dedup key moves its thread: a
// trigger on an open thread reaches channels only, an acknowledge ends the
// thread's pages, and a resolve closes it.
func (o *Outbox) Accept(ctx context.Context, e notice.Event) (Accepted, error) {
	move, err := notice.ParseMove(string(e.Move))
	if err != nil {
		return Accepted{}, err
	}
	e.Move = move
	var thread notice.Thread
	live := false
	if e.DedupKey != "" {
		thread, err = o.store.LiveThread(ctx, e.Producer, e.DedupKey)
		switch {
		case err == nil:
			live = true
			if strings.TrimSpace(e.Title) == "" {
				e.Title = thread.Title
			}
		case !errors.Is(err, store.ErrNotFound):
			return Accepted{}, err
		}
	}
	if move != notice.Trigger && !live {
		if err := e.Validate(); err != nil && strings.TrimSpace(e.Title) != "" {
			return Accepted{}, err
		}
		o.log.InfoContext(ctx, "event moved nothing", "producer", e.Producer, "move", move, "reason", "no open thread")
		o.decide(ctx, "notice.drop", e, incident.System(e.Producer), map[string]string{"reason": "no_open_thread", "move": string(move)})
		return Accepted{Event: e, Dropped: "no open thread under this dedup_key"}, nil
	}
	if e.Key == "" {
		e.Key = fmt.Sprintf("%s:%s:%d", e.DedupKey, move, o.Now().UnixNano())
	}
	if err := e.Validate(); err != nil {
		return Accepted{}, err
	}
	if err := o.checkTargets(ctx, e.To); err != nil {
		return Accepted{}, err
	}
	now := o.Now()
	if e.At.IsZero() {
		e.At = now
	}
	subs, err := o.store.Subscriptions(ctx)
	if err != nil {
		return Accepted{}, err
	}
	// The catalog is asked at most once an event, and only when a subscription's anchor needs it.
	var reached []string
	asked := false
	reach := func() []string {
		if !asked && e.Entity != nil && o.reach != nil {
			reached, asked = o.reach(ctx, *e.Entity), true
		}
		return reached
	}
	var channels []notice.Subscription
	targets := append([]notice.Target{}, e.To...)
	for _, sub := range subs {
		if !sub.Takes(e, reach) {
			continue
		}
		if sub.Target != nil {
			targets = append(targets, *sub.Target)
			continue
		}
		channels = append(channels, sub)
	}
	if e.Weight == "" {
		e.Weight = notice.WeightNotify
		for _, t := range targets {
			if t.Chain != "" {
				e.Weight = notice.WeightPage
			}
		}
	}
	if e.Weight != notice.WeightNotice && o.hush != nil && o.hush(ctx, e) {
		// A maintenance window quiets what it is about: channels still have
		// it, and it lands in people's lists without telling them.
		labels := map[string]string{"maintenance": "true"}
		for k, v := range e.Labels {
			labels[k] = v
		}
		e.Weight, e.Labels = notice.WeightNotice, labels
	}
	if live {
		e.Thread = thread.ID
	} else if e.DedupKey != "" {
		if thread, err = o.store.OpenThread(ctx, e.Producer, e.DedupKey, e.Title, now); err != nil {
			return Accepted{}, err
		}
		e.Thread = thread.ID
	}
	stored, fresh, err := o.store.InsertEvent(ctx, e, now)
	if err != nil {
		return Accepted{}, err
	}
	if !fresh {
		o.log.InfoContext(ctx, "event repeated, nothing queued", "producer", e.Producer, "kind", e.Kind, "event", stored.ID)
		o.decide(ctx, "notice.repeat", stored, incident.System(e.Producer), nil)
		return Accepted{Event: stored, Repeat: true}, nil
	}
	queued := 0
	for _, sub := range channels {
		added, err := o.store.AddDelivery(ctx, stored.ID, sub.ChannelID, sub.ID, now)
		if err != nil {
			return Accepted{}, err
		}
		if added {
			queued++
		}
	}
	res := Accepted{Event: stored, Queued: queued}
	chosen := 0
	switch {
	case move == notice.Trigger && live:
		res.Deduplicated = true
		if err := o.store.TouchThread(ctx, thread.ID); err != nil {
			return res, err
		}
	case move == notice.Trigger:
		if res.Messages, res.Pages, chosen, err = o.address(ctx, stored, targets, now); err != nil {
			return res, err
		}
	default:
		if err := o.moveThread(ctx, thread, move, e.Producer, now); err != nil {
			return res, err
		}
	}
	labels := map[string]string{"kind": stored.Kind, "weight": string(stored.Weight), "move": string(move),
		"channels": fmt.Sprint(queued), "messages": fmt.Sprint(res.Messages), "pages": fmt.Sprint(res.Pages)}
	if res.Deduplicated {
		labels["deduplicated"] = "true"
	}
	if chosen > 0 {
		// How many people their own choice for this kind told, in place of
		// their rules by weight.
		labels["chosen"] = fmt.Sprint(chosen)
	}
	o.decide(ctx, "notice.accept", stored, incident.System(e.Producer), labels)
	if queued+res.Messages > 0 {
		o.log.InfoContext(ctx, "event queued", "producer", e.Producer, "kind", e.Kind, "event", stored.ID, "weight", stored.Weight, "channels", queued, "messages", res.Messages, "pages", res.Pages, "deduplicated", res.Deduplicated)
		o.poke()
	} else {
		o.log.InfoContext(ctx, "event reached nobody", "producer", e.Producer, "kind", e.Kind, "event", stored.ID, "subscriptions", len(subs), "targets", len(targets), "deduplicated", res.Deduplicated, "move", move)
	}
	return res, nil
}

// moveThread acknowledges or resolves a thread its producer moved, ending
// the pages its events started.
func (o *Outbox) moveThread(ctx context.Context, t notice.Thread, move notice.Move, producer string, now time.Time) error {
	to := notice.ThreadAcknowledged
	if move == notice.Resolve {
		to = notice.ThreadResolved
	}
	moved, err := o.store.MoveThread(ctx, t.ID, to, producer, now)
	if err != nil || !moved {
		return err
	}
	pages, err := o.store.ActivePagesOfThread(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, p := range pages {
		if _, err := o.endPage(ctx, p.ID, producer, incident.System(producer)); err != nil {
			return err
		}
	}
	o.log.InfoContext(ctx, "thread moved by its producer", "thread", t.ID, "to", to, "pages", len(pages))
	return nil
}

// decide posts one routing decision about an event to Annals.
func (o *Outbox) decide(ctx context.Context, verb string, e notice.Event, by incident.Actor, labels map[string]string) {
	if labels == nil {
		labels = map[string]string{}
	}
	labels["producer"] = e.Producer
	key := fmt.Sprintf("notice:%s:%s:%s", e.Producer, e.Key, verb)
	if e.Key == "" {
		key = fmt.Sprintf("notice:%s:%s:%d", e.Producer, verb, o.Now().UnixNano())
	}
	o.annals.Decide(ctx, annals.Decision{Verb: verb, Kind: "notice", ID: fmt.Sprint(e.ID), Title: e.Title, By: by, Key: key, Labels: labels, At: o.Now()})
}

// UseHush asks h of every event whether a maintenance window quiets it.
func (o *Outbox) UseHush(h func(ctx context.Context, e notice.Event) bool) { o.hush = h }

// UseAnnals posts every routing decision to l.
func (o *Outbox) UseAnnals(l *annals.Poster) { o.annals = l }

func (o *Outbox) poke() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Run sends what is due until ctx ends.
func (o *Outbox) Run(ctx context.Context) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		o.SendDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-o.wake:
		}
	}
}

// SendDue moves on every page whose wait ran out, and makes one attempt at
// every delivery and message whose time has come.
func (o *Outbox) SendDue(ctx context.Context) {
	o.escalate(ctx)
	o.sendMessages(ctx)
	for {
		due, err := o.store.Due(ctx, o.Now(), batch)
		if err != nil {
			o.log.ErrorContext(ctx, "due deliveries not read", "error", err)
			return
		}
		for _, d := range due {
			o.attempt(ctx, d)
		}
		if len(due) < batch || ctx.Err() != nil {
			return
		}
	}
}

// attempt sends one delivery once and writes what came of it.
func (o *Outbox) attempt(ctx context.Context, d notice.Delivery) notice.Delivery {
	status, err := o.post(ctx, d)
	now := o.Now()
	d.Attempts++
	d.Status = status
	switch {
	case err == nil:
		d.State, d.SentAt, d.LastError = notice.Sent, &now, ""
		o.outcome(ctx, "sent")
		o.log.InfoContext(ctx, "delivered", "delivery", d.ID, "event", d.EventID, "channel", d.ChannelID, "status", status, "attempt", d.Attempts)
	default:
		d.LastError = err.Error()
		next, again := notice.Retry(d.Attempts, now)
		if again && !notice.Permanent(status) {
			d.NextAt = next
			o.outcome(ctx, "retry")
			o.log.WarnContext(ctx, "delivery failed, trying again", "delivery", d.ID, "channel", d.ChannelID, "status", status, "attempt", d.Attempts, "next", next, "error", err)
		} else {
			d.State = notice.Failed
			o.outcome(ctx, "failed")
			o.log.ErrorContext(ctx, "delivery failed for good", "delivery", d.ID, "channel", d.ChannelID, "status", status, "attempt", d.Attempts, "error", err)
			o.annals.Decide(ctx, annals.Decision{Verb: "delivery.give_up", Kind: "delivery", ID: fmt.Sprint(d.ID), By: incident.System(Machine),
				Key: fmt.Sprintf("delivery:%d:give_up", d.ID), Labels: map[string]string{"channel": fmt.Sprint(d.ChannelID), "status": fmt.Sprint(status)}, At: now, Outcome: "failed", Reason: err.Error()})
		}
	}
	if err := o.store.SaveDelivery(ctx, d); err != nil {
		o.log.ErrorContext(ctx, "delivery outcome not saved", "delivery", d.ID, "error", err)
	}
	return d
}

func (o *Outbox) post(ctx context.Context, d notice.Delivery) (int, error) {
	ch, err := o.store.Channel(ctx, d.ChannelID)
	if err != nil {
		return 0, fmt.Errorf("channel: %w", err)
	}
	target, err := o.seal.Open(ch.Target)
	if err != nil {
		return 0, err
	}
	e, err := o.store.Event(ctx, d.EventID)
	if err != nil {
		return 0, fmt.Errorf("event: %w", err)
	}
	var body []byte
	switch ch.Kind {
	case notice.Bot:
		if o.bot == nil {
			return 0, errors.New("the Teams bot is not set up here")
		}
		return o.bot.Send(ctx, target, e)
	case notice.DiscordBot:
		if o.discord == nil {
			return 0, errors.New("the Discord bot is not set up here")
		}
		return o.discord.Send(ctx, target, e)
	case notice.DiscordWebhook:
		body, err = discord.Webhook(e)
	default:
		body, err = notice.Body(ch.Kind, e)
	}
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dispatch")
	if ch.Kind == notice.Webhook && ch.Signs() {
		secrets := []string{}
		for _, sealed := range []string{ch.Secret, ch.OldSecret} {
			if sealed == "" {
				continue
			}
			plain, err := o.seal.Open(sealed)
			if err != nil {
				return 0, fmt.Errorf("the channel's signing secret: %w", err)
			}
			secrets = append(secrets, plain)
		}
		req.Header.Set(notice.SignatureHeader, notice.Sign(secrets, o.Now(), body))
		req.Header.Set("X-Dispatch-Delivery", fmt.Sprint(d.ID))
	}
	res, err := o.client.Do(req)
	if err != nil {
		// The URL is the secret; the error is said without it.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		said, _ := io.ReadAll(io.LimitReader(res.Body, 300))
		return res.StatusCode, fmt.Errorf("the channel answered %d: %s", res.StatusCode, strings.TrimSpace(string(said)))
	}
	return res.StatusCode, nil
}

// AddChannel seals the channel's URL and stores it. A webhook channel gets
// a signing secret, sealed too; the plain secret is answered once, to be
// shown once.
func (o *Outbox) AddChannel(ctx context.Context, name string, kind notice.Kind, target, by string) (notice.Channel, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return notice.Channel{}, "", fmt.Errorf("%w: a channel needs a name of at most 80 characters", notice.ErrInvalid)
	}
	switch kind {
	case notice.Teams, notice.Webhook, notice.Bot, notice.DiscordWebhook, notice.DiscordBot:
	default:
		return notice.Channel{}, "", fmt.Errorf("%w: a channel is teams, discord, webhook, bot, or discord-bot", notice.ErrInvalid)
	}
	target = strings.TrimSpace(target)
	if err := notice.ValidTarget(kind, target); err != nil {
		return notice.Channel{}, "", err
	}
	sealed, err := o.seal.Seal(target)
	if err != nil {
		return notice.Channel{}, "", err
	}
	ch := notice.Channel{Name: name, Kind: kind, Target: sealed, CreatedAt: o.Now(), CreatedBy: by}
	secret := ""
	if kind == notice.Webhook {
		secret = notice.NewSecret()
		if ch.Secret, err = o.seal.Seal(secret); err != nil {
			return notice.Channel{}, "", err
		}
	}
	ch, err = o.store.AddChannel(ctx, ch)
	if err != nil {
		return ch, "", err
	}
	o.log.InfoContext(ctx, "channel added", "channel", ch.ID, "kind", kind, "signs", secret != "", "by", by)
	return ch, secret, nil
}

// RotateSecret gives a webhook channel a new signing secret and keeps the
// current one signing beside it until DropOldSecret, so its receiver can
// change over without a gap. It answers the new secret, to be shown once.
func (o *Outbox) RotateSecret(ctx context.Context, id int64) (string, error) {
	ch, err := o.store.Channel(ctx, id)
	if err != nil {
		return "", err
	}
	if ch.Kind != notice.Webhook {
		return "", fmt.Errorf("%w: only a webhook channel signs its posts", notice.ErrInvalid)
	}
	secret := notice.NewSecret()
	sealed, err := o.seal.Seal(secret)
	if err != nil {
		return "", err
	}
	if err := o.store.SetSecrets(ctx, id, sealed, ch.Secret); err != nil {
		return "", err
	}
	o.log.InfoContext(ctx, "channel secret rotated", "channel", id, "first", ch.Secret == "")
	return secret, nil
}

// DropOldSecret stops the retired secret signing a channel's posts.
func (o *Outbox) DropOldSecret(ctx context.Context, id int64) error {
	ch, err := o.store.Channel(ctx, id)
	if err != nil {
		return err
	}
	return o.store.SetSecrets(ctx, id, ch.Secret, "")
}

// Test sends a channel a test message now and says what came of it.
func (o *Outbox) Test(ctx context.Context, channelID int64, by string) (notice.Delivery, error) {
	ch, err := o.store.Channel(ctx, channelID)
	if err != nil {
		return notice.Delivery{}, err
	}
	now := o.Now()
	e, _, err := o.store.InsertEvent(ctx, notice.Event{
		Producer: "dispatch", Kind: "notice.test", Key: fmt.Sprintf("test-%d-%d", channelID, now.UnixNano()),
		Title: "A test from dispatch", Text: by + " sent this to check that " + ch.Name + " gets dispatch's messages.", At: now,
	}, now)
	if err != nil {
		return notice.Delivery{}, err
	}
	if _, err := o.store.AddDelivery(ctx, e.ID, channelID, 0, now); err != nil {
		return notice.Delivery{}, err
	}
	due, err := o.store.Due(ctx, now, batch)
	if err != nil {
		return notice.Delivery{}, err
	}
	for _, d := range due {
		if d.EventID != e.ID {
			continue
		}
		d = o.attempt(ctx, d)
		if d.State == notice.Pending {
			// A test is said once: a failure is shown now, not tried again later.
			d.State = notice.Failed
			if err := o.store.SaveDelivery(ctx, d); err != nil {
				return d, err
			}
		}
		return d, nil
	}
	return notice.Delivery{}, errors.New("the test was not queued")
}

// Keeps says whether channels can be added: their URLs need SECRET_KEY.
func (o *Outbox) Keeps() bool { return o.seal.Keeps() }

// AddSubscription checks a subscription and stores it.
func (o *Outbox) AddSubscription(ctx context.Context, sub notice.Subscription) (notice.Subscription, error) {
	switch {
	case sub.Target != nil && sub.ChannelID != 0:
		return sub, fmt.Errorf("%w: a subscription sends to a channel or to a person, group, or chain, not both", notice.ErrInvalid)
	case sub.Target != nil:
		t, err := sub.Target.Normal()
		if err != nil {
			return sub, err
		}
		if err := o.checkTargets(ctx, []notice.Target{t}); err != nil {
			return sub, err
		}
		sub.Target = &t
	default:
		if _, err := o.store.Channel(ctx, sub.ChannelID); err != nil {
			return sub, fmt.Errorf("%w: pick a channel, or a person, group, or chain", notice.ErrInvalid)
		}
	}
	if len(sub.Topics) == 0 {
		return sub, fmt.Errorf("%w: pick at least one topic", notice.ErrInvalid)
	}
	for _, t := range append(append([]string{}, sub.Topics...), mapValues(sub.Match)...) {
		if _, err := path.Match(t, ""); err != nil {
			return sub, fmt.Errorf("%w: %q is not a glob", notice.ErrInvalid, t)
		}
	}
	if sub.Anchor != "" {
		ref, ok := notice.ParseRef(sub.Anchor)
		if !ok {
			return sub, fmt.Errorf("%w: the anchor is a catalog id, such as program:4242", notice.ErrInvalid)
		}
		sub.Anchor = ref.ID()
	}
	sub.CreatedAt = o.Now()
	sub, err := o.store.AddSubscription(ctx, sub)
	if err != nil {
		return sub, err
	}
	o.log.InfoContext(ctx, "subscription added", "subscription", sub.ID, "channel", sub.ChannelID, "target", targetName(sub.Target), "topics", sub.Topics, "anchor", sub.Anchor, "by", sub.CreatedBy)
	return sub, nil
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// UseBot sends bot channels through the Teams bot.
func (o *Outbox) UseBot(b BotSender) { o.bot = b }

// UseDiscord sends Discord bot channels through d.
func (o *Outbox) UseDiscord(d BotSender) { o.discord = d }

// HasBot says whether bot channels can be sent.
func (o *Outbox) HasBot() bool { return o.bot != nil }

// UseClient sends with c, for a test against its own TLS server.
func (o *Outbox) UseClient(c *http.Client) { o.client = c }

func targetName(t *notice.Target) string {
	if t == nil {
		return ""
	}
	return t.String()
}
