// Package teams speaks the Bot Connector protocol for dispatch's Teams bot.
// It checks that a request came from Microsoft's bot service, signs the bot
// in to answer, posts cards into channels, lists a team's channels, and asks
// the bot service's token store for a person's own token, so a click on a
// card calls a product as the person who clicked, never as the bot.
//
// The bot signs in with a federated workload token, never a secret: the
// token file the cluster projects is the client assertion.
package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/signed"
)

// Config is the bot's registration and where it signs in.
type Config struct {
	// AppID is the bot's single-tenant Entra app.
	AppID string
	// TenantID is the only tenant the bot answers.
	TenantID string
	// TokenFile is the projected workload token the bot signs in with.
	TokenFile string
	// Connection is the OAuth connection on the Azure Bot that gives a
	// person's token for the product the bot acts on.
	Connection string

	// The service addresses; empty means Microsoft's public cloud. Tests
	// point them at their own servers.
	OpenIDURL  string
	LoginURL   string
	TokenStore string
}

const (
	botIssuer  = "https://api.botframework.com"
	botScope   = "https://api.botframework.com/.default"
	assertType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

// Bot is one bot registration's client.
type Bot struct {
	cfg    Config
	client *http.Client
	keys   *signed.Keys
	// Now is replaced in tests.
	Now func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New makes the bot's client. It reaches nothing until it is used.
func New(cfg Config) (*Bot, error) {
	if cfg.AppID == "" || cfg.TenantID == "" {
		return nil, errors.New("the bot needs BOT_APP_ID and a tenant")
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("the bot needs a federated token file to sign in with")
	}
	if cfg.OpenIDURL == "" {
		cfg.OpenIDURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	}
	if cfg.LoginURL == "" {
		cfg.LoginURL = "https://login.microsoftonline.com"
	}
	if cfg.TokenStore == "" {
		cfg.TokenStore = "https://token.botframework.com"
	}
	b := &Bot{cfg: cfg, client: telemetry.Client(20 * time.Second), Now: time.Now}
	b.keys = signed.NewKeys(cfg.OpenIDURL, b.client, func() time.Time { return b.Now() })
	return b, nil
}

// UseClient sends with c, for a test against its own TLS servers.
func (b *Bot) UseClient(c *http.Client) {
	b.client = c
	b.keys.UseClient(c)
}

// AppID is the bot's own id, which Teams uses as its account id.
func (b *Bot) AppID() string { return b.cfg.AppID }

// TenantID is the tenant the bot answers.
func (b *Bot) TenantID() string { return b.cfg.TenantID }

// botToken is the bot's own token for the Bot Connector and the token
// store, kept until five minutes before it ends.
func (b *Bot) botToken(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && b.Now().Before(b.expires.Add(-5*time.Minute)) {
		return b.token, nil
	}
	assertion, err := os.ReadFile(b.cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("the bot's workload token: %w", err)
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {b.cfg.AppID},
		"scope":                 {botScope},
		"client_assertion_type": {assertType},
		"client_assertion":      {strings.TrimSpace(string(assertion))},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.cfg.LoginURL, "/")+"/"+url.PathEscape(b.cfg.TenantID)+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := b.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("the bot's sign-in: %w", unwrapURL(err))
	}
	defer res.Body.Close()
	var got struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&got); err != nil || res.StatusCode != http.StatusOK || got.AccessToken == "" {
		return "", fmt.Errorf("the bot's sign-in was refused (%d): %s", res.StatusCode, firstLine(got.Error))
	}
	b.token, b.expires = got.AccessToken, b.Now().Add(time.Duration(got.ExpiresIn)*time.Second)
	return b.token, nil
}

// call sends one request as the bot and decodes a JSON answer into out,
// when out is not nil. It returns the status the service answered.
func (b *Bot) call(ctx context.Context, method, target string, body, out any) (int, error) {
	token, err := b.botToken(ctx)
	if err != nil {
		return 0, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := b.client.Do(req)
	if err != nil {
		return 0, unwrapURL(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return res.StatusCode, fmt.Errorf("the bot service answered %d: %s", res.StatusCode, firstLine(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return res.StatusCode, fmt.Errorf("the bot service's answer: %w", err)
		}
	}
	return res.StatusCode, nil
}

// unwrapURL drops the request URL from a transport error.
func unwrapURL(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
