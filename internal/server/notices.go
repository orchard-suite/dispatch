package server

import (
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/orchard-suite/platformkit/v3/door"

	"github.com/orchard-suite/dispatch/internal/discord"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// subscriptionLine is a subscription with its channel's name, as the page lists it.
type subscriptionLine struct {
	notice.Subscription
	Channel string
	Match   string
}

func (s *Server) pageNotifications(w http.ResponseWriter, r *http.Request) {
	s.renderNotifications(w, r, http.StatusOK, "", "")
}

// renderNotifications shows the channels, what each is sent, and the newest
// deliveries. bad is a form's refusal; done says what a form did.
func (s *Server) renderNotifications(w http.ResponseWriter, r *http.Request, status int, bad, done string) {
	box := s.desk.Notices()
	if box == nil {
		s.message(w, r, http.StatusServiceUnavailable, "Notifications", "This desk sends no notices.")
		return
	}
	ctx := r.Context()
	channels, err := s.store.Channels(ctx)
	if s.pageFailed(w, r, err, "Notifications") {
		return
	}
	subs, err := s.store.Subscriptions(ctx)
	if s.pageFailed(w, r, err, "Notifications") {
		return
	}
	deliveries, err := s.store.RecentDeliveries(ctx, 50)
	if s.pageFailed(w, r, err, "Notifications") {
		return
	}
	names := map[int64]string{}
	for _, c := range channels {
		names[c.ID] = c.Name
	}
	n := s.namer(r)
	lines := make([]subscriptionLine, 0, len(subs))
	for _, sub := range subs {
		to := names[sub.ChannelID]
		if sub.Target != nil {
			to = n.target(*sub.Target)
		}
		lines = append(lines, subscriptionLine{Subscription: sub, Channel: to, Match: formatMatch(sub.Match)})
	}
	messages, err := s.store.RecentMessages(ctx, 50)
	if s.pageFailed(w, r, err, "Notifications") {
		return
	}
	s.render(w, r, status, "notifications", map[string]any{
		"Channels": channels, "Subscriptions": lines, "Deliveries": deliveries, "Topics": notice.Topics, "Kinds": notice.Kinds,
		"Messages": messages, "Pages": s.recentPages(r), "Mails": box.Mails(),
		"Keeps": box.Keeps(), "Bad": bad, "Done": done, "Bot": s.bot != nil, "BotChannels": s.botChannels(ctx),
		"Discord": s.discord != nil, "DiscordChannels": s.discordChannels(r),
	})
}

func (s *Server) formAddChannel(w http.ResponseWriter, r *http.Request) {
	by := door.From(r.Context()).Email
	kind, target := notice.Kind(r.FormValue("kind")), r.FormValue("url")
	switch kind {
	case notice.Bot:
		if s.bot == nil {
			s.renderNotifications(w, r, http.StatusBadRequest, "this desk has no Teams bot", "")
			return
		}
		target = r.FormValue("bot_channel")
	case notice.DiscordBot:
		if s.discord == nil {
			s.renderNotifications(w, r, http.StatusBadRequest, "this desk has no Discord bot", "")
			return
		}
		target = r.FormValue("discord_channel")
	}
	ch, secret, err := s.desk.Notices().AddChannel(r.Context(), r.FormValue("name"), kind, target, by)
	if s.noticeRefused(w, r, err, "Notifications") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "channel added", r, "channel", ch.ID, "by", by)
	s.decideAdmin(r, "channel.add", "channel", strconv.FormatInt(ch.ID, 10), ch.Name)
	done := "Channel " + ch.Name + " added. Send it a test to check it."
	if secret != "" {
		done += " Its posts are signed with this secret, shown only now: " + secret
	}
	s.renderNotifications(w, r, http.StatusOK, "", done)
}

func (s *Server) formTestChannel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.message(w, r, http.StatusNotFound, "Notifications", "There is no such channel.")
		return
	}
	d, err := s.desk.Notices().Test(r.Context(), id, door.From(r.Context()).Email)
	if s.noticeRefused(w, r, err, "Notifications") {
		return
	}
	if d.State == notice.Sent {
		s.renderNotifications(w, r, http.StatusOK, "", "The test was delivered: the channel answered "+strconv.Itoa(d.Status)+".")
		return
	}
	s.renderNotifications(w, r, http.StatusOK, "The test was not delivered: "+d.LastError, "")
}

// formRotateSecret gives a webhook channel a new signing secret, shown
// once, while the old one keeps signing until it is dropped.
func (s *Server) formRotateSecret(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.message(w, r, http.StatusNotFound, "Notifications", "There is no such channel.")
		return
	}
	secret, err := s.desk.Notices().RotateSecret(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, r, http.StatusNotFound, "Notifications", "There is no such channel.")
		return
	}
	if s.noticeRefused(w, r, err, "Notifications") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, "channel secret rotated", r, "channel", id, "by", door.From(r.Context()).Email)
	s.decideAdmin(r, "channel.rotate_secret", "channel", strconv.FormatInt(id, 10), "")
	s.renderNotifications(w, r, http.StatusOK, "", "The new signing secret, shown only now: "+secret+". Posts carry both signatures until you drop the old one.")
}

func (s *Server) formDropOldSecret(w http.ResponseWriter, r *http.Request) {
	s.removed(w, r, "channel", "drop_old_secret", func(id int64) error { return s.desk.Notices().DropOldSecret(r.Context(), id) })
}

func (s *Server) formDeleteChannel(w http.ResponseWriter, r *http.Request) {
	s.removed(w, r, "channel", "remove", func(id int64) error { return s.store.DeleteChannel(r.Context(), id) })
}

func (s *Server) formAddSubscription(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderNotifications(w, r, http.StatusBadRequest, "the form could not be read", "")
		return
	}
	channel, _ := strconv.ParseInt(r.FormValue("channel"), 10, 64)
	var target *notice.Target
	if to := strings.TrimSpace(r.FormValue("to")); to != "" {
		t, err := notice.ParseTarget(to)
		if s.noticeRefused(w, r, err, "Notifications") {
			return
		}
		target, channel = &t, 0
	}
	topics := r.Form["topic"]
	topics = append(topics, splitList(r.FormValue("other_topics"))...)
	match, err := parseMatch(r.FormValue("match"))
	if err != nil {
		s.renderNotifications(w, r, http.StatusBadRequest, err.Error(), "")
		return
	}
	sub, err := s.desk.Notices().AddSubscription(r.Context(), notice.Subscription{
		ChannelID: channel, Target: target, Topics: topics, Producers: splitList(r.FormValue("producers")),
		Anchor: strings.TrimSpace(r.FormValue("anchor")), Match: match, CreatedBy: door.From(r.Context()).Email,
	})
	if s.noticeRefused(w, r, err, "Notifications") {
		return
	}
	s.decideAdmin(r, "subscription.add", "subscription", strconv.FormatInt(sub.ID, 10), strings.Join(sub.Topics, ", "))
	s.renderNotifications(w, r, http.StatusOK, "", "Subscription "+strconv.FormatInt(sub.ID, 10)+" added.")
}

func (s *Server) formPauseSubscription(w http.ResponseWriter, r *http.Request) {
	paused := r.FormValue("paused") == "1"
	verb := "resume"
	if paused {
		verb = "pause"
	}
	s.removed(w, r, "subscription", verb, func(id int64) error { return s.store.SetSubscriptionPaused(r.Context(), id, paused) })
}

func (s *Server) formDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	s.removed(w, r, "subscription", "remove", func(id int64) error { return s.store.DeleteSubscription(r.Context(), id) })
}

// removed runs a change on the path's id and goes back to the page.
func (s *Server) removed(w http.ResponseWriter, r *http.Request, what, verb string, fn func(int64) error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = fn(id)
	}
	if err != nil && (errors.Is(err, store.ErrNotFound) || errors.Is(err, strconv.ErrSyntax)) {
		s.message(w, r, http.StatusNotFound, "Notifications", "There is no such "+what+".")
		return
	}
	if s.pageFailed(w, r, err, "Notifications") {
		return
	}
	s.logRequest(r.Context(), slog.LevelInfo, what+" changed", r, "id", id, "verb", verb, "by", door.From(r.Context()).Email)
	s.decideAdmin(r, what+"."+verb, what, strconv.FormatInt(id, 10), "")
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

// noticeRefused shows a refusal of a notice form on the page, and anything
// else as a failure.
func (s *Server) noticeRefused(w http.ResponseWriter, r *http.Request, err error, what string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, notice.ErrInvalid) || errors.Is(err, store.ErrNotFound) || strings.Contains(err.Error(), "SECRET_KEY") {
		s.renderNotifications(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), notice.ErrInvalid.Error()+": "), "")
		return true
	}
	return s.pageFailed(w, r, err, what)
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseMatch reads "label=glob, label=glob".
func parseMatch(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, errors.New("labels are written label=glob, separated by commas")
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func formatMatch(m map[string]string) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

// discordChannels lists the text channels the Discord bot can post in, or
// none when it has no bot or Discord will not list them.
func (s *Server) discordChannels(r *http.Request) []discord.Channel {
	if s.discord == nil {
		return nil
	}
	chans, err := s.discord.Channels(r.Context())
	if err != nil {
		s.logRequest(r.Context(), slog.LevelWarn, "discord channels not listed", r, "error", err)
		return nil
	}
	return chans
}
