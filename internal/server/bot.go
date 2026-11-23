package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
	"github.com/orchard-suite/dispatch/internal/teams"
)

// BotConfig is the Teams bot and the product its buttons act on.
type BotConfig struct {
	Bot *teams.Bot
	// ActsFor is the producer whose events' actions the bot offers: the name
	// of its token. Actions in any other producer's events are not shown.
	ActsFor string
	// ProductURL is that producer's API, which a click calls as the person.
	ProductURL string
}

// UseBot turns on the bot's endpoint and its channels.
func (s *Server) UseBot(cfg BotConfig) {
	cfg.ProductURL = strings.TrimRight(cfg.ProductURL, "/")
	s.bot = &cfg
	s.product = telemetry.Client(15 * time.Second)
}

// helpText is what the bot says when someone writes to it.
const helpText = "I'm the platform's voice in Teams. I post alerts, approvals, and deploys to the channels dispatch sends them to. I don't take commands yet."

// apiBot is the bot service's door: POST /api/messages on the machine port.
// It trusts only a token the bot service signed for this bot.
func (s *Server) apiBot(w http.ResponseWriter, r *http.Request) {
	if s.bot == nil {
		writeError(w, http.StatusNotFound, "this desk has no Teams bot")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the activity could not be read")
		return
	}
	var a teams.Activity
	if err := json.Unmarshal(raw, &a); err != nil {
		writeError(w, http.StatusBadRequest, "the activity is not JSON")
		return
	}
	ctx := r.Context()
	if err := s.bot.Bot.Verify(ctx, r.Header.Get("Authorization"), a.ServiceURL); err != nil {
		level, status := slog.LevelWarn, http.StatusUnauthorized
		if !errors.Is(err, teams.ErrRefused) {
			level, status = slog.LevelError, http.StatusServiceUnavailable
		}
		s.logRequest(ctx, level, "bot activity refused", r, "error", err)
		writeError(w, status, "not from the bot service")
		return
	}
	if tenant := a.Tenant(); tenant != "" && tenant != s.bot.Bot.TenantID() {
		s.logRequest(ctx, slog.LevelWarn, "bot activity refused: another tenant", r, "tenant", tenant)
		writeError(w, http.StatusForbidden, "this bot answers one tenant")
		return
	}
	switch a.Type {
	case "installationUpdate", "conversationUpdate":
		s.botMembership(ctx, r, a)
		w.WriteHeader(http.StatusOK)
	case "message":
		if err := s.bot.Bot.Reply(ctx, a, helpText); err != nil {
			s.logRequest(ctx, slog.LevelWarn, "bot reply failed", r, "error", err)
		}
		w.WriteHeader(http.StatusOK)
	case "invoke":
		if a.Name != "adaptiveCard/action" {
			s.logRequest(ctx, slog.LevelInfo, "bot invoke passed over", r, "name", a.Name)
			writeJSON(w, http.StatusOK, map[string]any{"status": 200})
			return
		}
		writeJSON(w, http.StatusOK, s.botAct(ctx, r, a))
	default:
		s.logRequest(ctx, slog.LevelDebug, "bot activity passed over", r, "type", a.Type)
		w.WriteHeader(http.StatusOK)
	}
}

// botMembership keeps the teams the bot is in, from its install, its
// removal, and a team's rename.
func (s *Server) botMembership(ctx context.Context, r *http.Request, a teams.Activity) {
	team := a.ChannelData.Team
	if team == nil || team.ID == "" {
		s.logRequest(ctx, slog.LevelDebug, "bot membership change outside a team passed over", r, "type", a.Type)
		return
	}
	removed := (a.Type == "installationUpdate" && a.Action == "remove") ||
		a.ChannelData.EventType == "teamDeleted" ||
		(a.ChannelData.EventType == "teamMemberRemoved" && s.bot.Bot.Includes(a.MembersRemoved, a.Recipient))
	if removed {
		if err := s.store.RemoveBotTeam(ctx, team.ID); err != nil {
			s.logRequest(ctx, slog.LevelError, "bot team not forgotten", r, "team", team.ID, "error", err)
			return
		}
		s.logRequest(ctx, slog.LevelInfo, "bot removed from a team", r, "team", team.ID)
		return
	}
	added := (a.Type == "installationUpdate" && a.Action == "add") ||
		a.ChannelData.EventType == "teamRenamed" ||
		(a.ChannelData.EventType == "teamMemberAdded" && s.bot.Bot.Includes(a.MembersAdded, a.Recipient))
	if !added {
		s.logRequest(ctx, slog.LevelDebug, "bot membership change passed over", r, "event", a.ChannelData.EventType)
		return
	}
	err := s.store.SaveBotTeam(ctx, store.BotTeam{ID: team.ID, Name: team.Name, TenantID: a.Tenant(), ServiceURL: a.ServiceURL, AddedAt: time.Now()})
	if err != nil {
		s.logRequest(ctx, slog.LevelError, "bot team not kept", r, "team", team.ID, "error", err)
		return
	}
	s.logRequest(ctx, slog.LevelInfo, "bot is in a team", r, "team", team.ID, "name", team.Name)
}

// botAct answers a button on a bot card: it signs the person in, calls the
// product as them, and shows the card with what they did. Every answer is
// an invoke response; a refusal shows only to the person who clicked.
func (s *Server) botAct(ctx context.Context, r *http.Request, a teams.Activity) map[string]any {
	say := func(text string) map[string]any {
		return map[string]any{"statusCode": 200, "type": "application/vnd.microsoft.activity.message", "value": text}
	}
	var v struct {
		Action struct {
			Verb string         `json:"verb"`
			Data notice.ActData `json:"data"`
		} `json:"action"`
		Authentication *struct {
			Token string `json:"token"`
		} `json:"authentication"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(a.Value, &v); err != nil || v.Action.Verb != notice.ActVerb {
		s.logRequest(ctx, slog.LevelWarn, "bot card action not understood", r)
		return say("I don't know that button.")
	}
	e, err := s.store.Event(ctx, v.Action.Data.Event)
	if err != nil || v.Action.Data.Action < 0 || v.Action.Data.Action >= len(e.Actions) || e.Producer != s.bot.ActsFor || s.bot.ProductURL == "" {
		s.logRequest(ctx, slog.LevelWarn, "bot card action refused: no such action", r, "event", v.Action.Data.Event, "action", v.Action.Data.Action)
		return say("That button no longer does anything.")
	}
	act := e.Actions[v.Action.Data.Action]

	token := ""
	if v.Authentication != nil && v.Authentication.Token != "" {
		token, err = s.bot.Bot.Exchange(ctx, a, v.Authentication.Token)
	} else {
		token, err = s.bot.Bot.UserToken(ctx, a, v.State)
	}
	if err != nil {
		s.logRequest(ctx, slog.LevelError, "bot sign-in failed", r, "event", e.ID, "error", err)
		return say("I couldn't sign you in just now. Try again in a minute.")
	}
	if token == "" {
		login, err := s.bot.Bot.LoginRequest(ctx, a, "Sign in to "+act.Title+" as yourself")
		if err != nil {
			s.logRequest(ctx, slog.LevelError, "bot sign-in link not made", r, "event", e.ID, "error", err)
			return say("I couldn't start your sign-in just now. Try again in a minute.")
		}
		s.logRequest(ctx, slog.LevelInfo, "bot card action asked the person to sign in", r, "event", e.ID, "sso", v.Authentication != nil)
		return login
	}

	status, answer, err := s.callProduct(ctx, act.Path, token)
	switch {
	case err != nil:
		s.logRequest(ctx, slog.LevelError, "bot card action failed", r, "event", e.ID, "path", act.Path, "error", err)
		return say("I couldn't reach " + e.Producer + ". Try again in a minute.")
	case status >= 200 && status < 300:
		who := a.From.Name
		if who == "" {
			who = "Someone"
		}
		s.logRequest(ctx, slog.LevelInfo, "bot card action done", r, "event", e.ID, "path", act.Path, "status", status)
		return map[string]any{"statusCode": 200, "type": "application/vnd.microsoft.card.adaptive",
			"value": notice.BotCard(e, fmt.Sprintf("%s %s this in Teams.", who, act.Done))}
	default:
		s.logRequest(ctx, slog.LevelInfo, "bot card action refused by the product", r, "event", e.ID, "path", act.Path, "status", status)
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return say(e.Producer + " says you may not do that: " + answer)
		}
		return say(e.Producer + " said no: " + answer)
	}
}

// callProduct posts to the producer's API as the person, and returns its
// status and, on a refusal, its error message.
func (s *Server) callProduct(ctx context.Context, path, token string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.bot.ProductURL+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := s.product.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var said struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &said)
	if said.Error == "" {
		said.Error = http.StatusText(res.StatusCode)
	}
	return res.StatusCode, said.Error, nil
}

// botSender posts bot channels' cards through the bot.
type botSender struct {
	store *store.Store
	bot   *BotConfig
}

// BotSender is what the outbox sends bot channels with, or nil without a bot.
func (s *Server) BotSender() *botSender {
	if s.bot == nil {
		return nil
	}
	return &botSender{store: s.store, bot: s.bot}
}

func (b *botSender) Send(ctx context.Context, target string, e notice.Event) (int, error) {
	teamID, channelID, _ := strings.Cut(target, "/")
	team, err := b.store.BotTeam(ctx, teamID)
	if err != nil {
		return 0, errors.New("the bot is no longer in that channel's team")
	}
	if e.Producer != b.bot.ActsFor {
		e.Actions = nil
	}
	return b.bot.Bot.PostToChannel(ctx, team.ServiceURL, team.TenantID, channelID, notice.BotCard(e, ""))
}

// botChannel is one channel of a team the bot is in, as the page offers it.
type botChannel struct {
	Target string
	Name   string
}

// botChannels lists every channel of every team the bot is in. A team the
// bot service will not list is logged and left out.
func (s *Server) botChannels(ctx context.Context) []botChannel {
	if s.bot == nil {
		return nil
	}
	teamsIn, err := s.store.BotTeams(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "bot teams not read", "error", err)
		return nil
	}
	var out []botChannel
	for _, t := range teamsIn {
		chans, err := s.bot.Bot.Channels(ctx, t.ServiceURL, t.ID)
		if err != nil {
			s.log.WarnContext(ctx, "bot team's channels not listed", "team", t.ID, "error", err)
			continue
		}
		for _, c := range chans {
			out = append(out, botChannel{Target: t.ID + "/" + c.ID, Name: t.Name + " / " + c.Name})
		}
	}
	return out
}

// SendPerson posts the event to one person's chat with the bot. Their
// Teams account is found once in the rosters of the teams the bot is in, by
// email or sign-in name, and kept. Someone in none of them is unreachable.
func (b *botSender) SendPerson(ctx context.Context, out notice.Outgoing) (int, error) {
	p, e := out.Person, notice.WithAck(out.Event, out.Ack)
	teamsIn, err := b.store.BotTeams(ctx)
	if err != nil {
		return 0, err
	}
	if len(teamsIn) == 0 {
		return 0, fmt.Errorf("%w: the Teams bot is in no team", notice.ErrUnreachable)
	}
	id := p.TeamsID
	if id == "" {
		for _, t := range teamsIn {
			members, err := b.bot.Bot.Members(ctx, t.ServiceURL, t.ID)
			if err != nil {
				return 0, fmt.Errorf("the roster of a team the bot is in: %w", err)
			}
			for _, m := range members {
				if strings.EqualFold(m.Email, p.Email) || strings.EqualFold(m.UserPrincipalName, p.Email) {
					id = m.ID
					break
				}
			}
			if id != "" {
				break
			}
		}
		if id == "" {
			return 0, fmt.Errorf("%w: no team the bot is in has this person", notice.ErrUnreachable)
		}
		if err := b.store.SetTeamsID(ctx, p.Email, id); err != nil {
			return 0, err
		}
	}
	if e.Producer != b.bot.ActsFor {
		e.Actions = nil
	}
	return b.bot.Bot.PostToPerson(ctx, teamsIn[0].ServiceURL, teamsIn[0].TenantID, id, notice.BotCard(e, ""))
}
