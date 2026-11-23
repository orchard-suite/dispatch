// Package entra signs an Entra app in with the pod's federated workload
// token as its client assertion, for one scope, and keeps the token until
// five minutes before it ends. No app here holds a secret.
package entra

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
)

// Config is an app, where it signs in, and what it signs in for.
type Config struct {
	// Name says whose sign-in an error is about, such as "the mail app".
	Name      string
	AppID     string
	TenantID  string
	TokenFile string
	// Scope is the resource's default scope, such as
	// https://graph.microsoft.com/.default.
	Scope string
	// LoginURL is Microsoft's public cloud when empty; tests point it at
	// their own server.
	LoginURL string
}

// Signer holds one app's token.
type Signer struct {
	cfg    Config
	client *http.Client
	// Now is replaced in tests.
	Now func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

const assertType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

func New(cfg Config) (*Signer, error) {
	if cfg.AppID == "" || cfg.TenantID == "" || cfg.TokenFile == "" || cfg.Scope == "" {
		return nil, fmt.Errorf("%s needs an app id, a tenant, a federated token file, and a scope", cfg.Name)
	}
	if cfg.LoginURL == "" {
		cfg.LoginURL = "https://login.microsoftonline.com"
	}
	return &Signer{cfg: cfg, client: telemetry.Client(20 * time.Second), Now: time.Now}, nil
}

// UseClient signs in with c, for a test against its own TLS server.
func (s *Signer) UseClient(c *http.Client) { s.client = c }

// Forget drops the kept token, after the resource refused it.
func (s *Signer) Forget() {
	s.mu.Lock()
	s.token = ""
	s.mu.Unlock()
}

// Token is the app's token for its scope.
func (s *Signer) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.Now().Before(s.expires.Add(-5*time.Minute)) {
		return s.token, nil
	}
	assertion, err := os.ReadFile(s.cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("%s's workload token: %w", s.cfg.Name, err)
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {s.cfg.AppID},
		"scope":                 {s.cfg.Scope},
		"client_assertion_type": {assertType},
		"client_assertion":      {strings.TrimSpace(string(assertion))},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.LoginURL, "/")+"/"+url.PathEscape(s.cfg.TenantID)+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s's sign-in: %w", s.cfg.Name, Unwrap(err))
	}
	defer res.Body.Close()
	var got struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&got); err != nil || res.StatusCode != http.StatusOK || got.AccessToken == "" {
		return "", fmt.Errorf("%s's sign-in was refused (%d): %s", s.cfg.Name, res.StatusCode, FirstLine(got.Error))
	}
	s.token, s.expires = got.AccessToken, s.Now().Add(time.Duration(got.ExpiresIn)*time.Second)
	return s.token, nil
}

// Unwrap drops the URL from a client error, which may carry a secret.
func Unwrap(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// FirstLine is the first line of s, at most 300 characters.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
