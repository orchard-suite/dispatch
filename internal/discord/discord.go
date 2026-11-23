// Package discord speaks to Discord for dispatch: as a bot it opens a
// direct message with a person and posts a card there, or into a server's
// channel; as a plain webhook it posts a card into one channel. It checks
// the signature on every interaction Discord sends back, such as a press
// of a card's Acknowledge button.
package discord

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/entra"
	"github.com/orchard-suite/dispatch/internal/notice"
)

// API is Discord's REST API.
const API = "https://discord.com/api/v10"

// Bot is dispatch's Discord application: its bot token posts, its public
// key checks what Discord sends back.
type Bot struct {
	token  string
	key    ed25519.PublicKey
	api    string
	client *http.Client
}

// New makes the bot from its token and its application's public key, as
// hex. The token is a secret and never appears in an error.
func New(token, publicKey string) (*Bot, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(publicKey))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("discord: DISCORD_PUBLIC_KEY must be the application's public key in hex")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("discord: DISCORD_BOT_TOKEN is required")
	}
	return &Bot{token: strings.TrimSpace(token), key: raw, api: API, client: telemetry.Client(20 * time.Second)}, nil
}

// UseAPI points the bot at another API, for a test's own server.
func (b *Bot) UseAPI(url string, c *http.Client) { b.api, b.client = strings.TrimRight(url, "/"), c }

// Verify checks Discord's Ed25519 signature over the timestamp and body.
func (b *Bot) Verify(signature, timestamp string, body []byte) bool {
	sig, err := hex.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize || timestamp == "" {
		return false
	}
	return ed25519.Verify(b.key, append([]byte(timestamp), body...), sig)
}

func (b *Bot) call(ctx context.Context, method, path string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.api+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bot "+b.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/orchard-suite/dispatch, 1)")
	res, err := b.client.Do(req)
	if err != nil {
		return 0, entra.Unwrap(err)
	}
	defer res.Body.Close()
	said, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if res.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		}
		_ = json.Unmarshal(said, &e)
		if res.StatusCode == http.StatusForbidden && e.Code == 50007 {
			return res.StatusCode, fmt.Errorf("%w: Discord will not let the bot message this person; they share no server with it or block its messages", notice.ErrUnreachable)
		}
		return res.StatusCode, fmt.Errorf("Discord answered %d: %s", res.StatusCode, entra.FirstLine(e.Message))
	}
	if out != nil {
		if err := json.Unmarshal(said, out); err != nil {
			return res.StatusCode, errors.New("Discord's answer could not be read")
		}
	}
	return res.StatusCode, nil
}

// SendPerson posts the message into the person's direct messages with the
// bot, opening them first. A page gets Acknowledge and Pass it on buttons.
func (b *Bot) SendPerson(ctx context.Context, out notice.Outgoing) (int, error) {
	if out.Person.DiscordID == "" {
		return 0, fmt.Errorf("%w: no Discord account", notice.ErrUnreachable)
	}
	var dm struct {
		ID string `json:"id"`
	}
	if status, err := b.call(ctx, http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": out.Person.DiscordID}, &dm); err != nil {
		return status, err
	}
	return b.call(ctx, http.MethodPost, "/channels/"+dm.ID+"/messages", Message(out.Event, out.Ack, out.PageID), nil)
}

// Send posts an event into a server channel the bot is in, by its id.
func (b *Bot) Send(ctx context.Context, channelID string, e notice.Event) (int, error) {
	return b.call(ctx, http.MethodPost, "/channels/"+channelID+"/messages", Message(e, "", 0), nil)
}

// Colors of a card's edge by severity.
var colors = map[string]int{"critical": 0xD93F0B, "warning": 0xE4A11B, "info": 0x1F6FEB}

// Message is an event as Discord's message body: an embed with its title,
// text, facts, and links; for a page, buttons that acknowledge it or pass
// it to its next step, and a link to its page on the desk.
func Message(e notice.Event, ack string, pageID int64) map[string]any {
	embed := map[string]any{"title": clip(e.Title, 256), "timestamp": e.At.UTC().Format(time.RFC3339)}
	if e.Text != "" {
		embed["description"] = clip(e.Text, 4000)
	}
	if c, ok := colors[e.Severity]; ok {
		embed["color"] = c
	}
	var fields []map[string]any
	for _, f := range e.Facts {
		fields = append(fields, map[string]any{"name": clip(f.Name, 256), "value": clip(f.Value, 1024), "inline": true})
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}
	embed["footer"] = map[string]string{"text": e.Producer + " · " + e.Kind}
	var buttons []map[string]any
	if ack != "" && pageID != 0 {
		buttons = append(buttons,
			map[string]any{"type": 2, "style": 3, "label": "Acknowledge", "custom_id": fmt.Sprintf("ack:%d", pageID)},
			map[string]any{"type": 2, "style": 2, "label": "Pass it on", "custom_id": fmt.Sprintf("esc:%d", pageID)},
			map[string]any{"type": 2, "style": 5, "label": "Open the page", "url": ack})
	}
	for _, l := range e.Links {
		if len(buttons) >= 5 {
			break
		}
		buttons = append(buttons, map[string]any{"type": 2, "style": 5, "label": clip(l.Title, 80), "url": l.URL})
	}
	msg := map[string]any{"embeds": []any{embed}, "allowed_mentions": map[string]any{"parse": []string{}}}
	if len(buttons) > 0 {
		msg["components"] = []any{map[string]any{"type": 1, "components": buttons}}
	}
	return msg
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Interaction is what Discord sends when someone presses a button.
type Interaction struct {
	Type int `json:"type"`
	Data struct {
		CustomID string `json:"custom_id"`
	} `json:"data"`
	Member *struct {
		User User `json:"user"`
	} `json:"member"`
	User *User `json:"user"`
}

// User is a Discord account.
type User struct {
	ID string `json:"id"`
}

// Interaction types and response types this package uses.
const (
	Ping            = 1
	Component       = 3
	Pong            = 1
	ChannelMessage  = 4
	EphemeralFlag   = 64
	MaxInteraction  = 1 << 16
	SignatureHeader = "X-Signature-Ed25519"
	TimestampHeader = "X-Signature-Timestamp"
)

// Who is the account that pressed the button, in a server or a DM.
func (i Interaction) Who() string {
	if i.Member != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// Reply is an answer only the person who pressed sees.
func Reply(text string) map[string]any {
	return map[string]any{"type": ChannelMessage, "data": map[string]any{"content": text, "flags": EphemeralFlag}}
}

// Webhook is an event as the body of a post to a channel's own webhook
// URL. A plain webhook may carry no buttons, so links stay out.
func Webhook(e notice.Event) ([]byte, error) {
	msg := Message(e, "", 0)
	delete(msg, "components")
	return json.Marshal(msg)
}

// Channel is a text channel of a server the bot is in.
type Channel struct {
	ID     string
	Server string
	Name   string
}

// Channels lists the text channels of every server the bot is in.
func (b *Bot) Channels(ctx context.Context) ([]Channel, error) {
	var guilds []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if _, err := b.call(ctx, http.MethodGet, "/users/@me/guilds", nil, &guilds); err != nil {
		return nil, err
	}
	var out []Channel
	for _, g := range guilds {
		var chans []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type int    `json:"type"`
		}
		if _, err := b.call(ctx, http.MethodGet, "/guilds/"+g.ID+"/channels", nil, &chans); err != nil {
			return nil, err
		}
		for _, c := range chans {
			if c.Type == 0 {
				out = append(out, Channel{ID: c.ID, Server: g.Name, Name: c.Name})
			}
		}
	}
	return out, nil
}
