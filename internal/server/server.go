// Package server is the dispatch server. It answers on three ports, one per
// kind of caller, so no route can be reached with the wrong kind of
// identity:
//
//   - Session: the desk. Trusts the front door's identity headers, so
//     only Traefik may reach it.
//   - API: Alertmanager, other products, and an outcomes board. Trusts a
//     bearer token dispatch issued or the shared secret from the
//     environment, and nothing else. The Teams bot's door, /api/messages,
//     trusts only a token the bot service signed for the bot.
//   - Status: the public status page. Trusts nobody and names nothing
//     internal.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/orchard-suite/platformkit/v3/door"
	"github.com/orchard-suite/platformkit/v3/faro"

	"github.com/orchard-suite/dispatch/internal/azuremonitor"
	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/discord"
	"github.com/orchard-suite/dispatch/internal/metrics"
	"github.com/orchard-suite/dispatch/internal/sms"
	"github.com/orchard-suite/dispatch/internal/store"
)

type Config struct {
	// PublicURL is the desk, such as https://dispatch.example.org.
	PublicURL string
	// AlertmanagerToken is the shared secret Alertmanager's receiver sends.
	// Empty means only issued tokens are accepted.
	AlertmanagerToken string
	// DeskRole and AdminRole are the bare front-door roles that open the
	// desk and its settings.
	DeskRole  string
	AdminRole string
	// FaroURL and FaroKey relay browser telemetry; without a key batches
	// are accepted and dropped.
	FaroURL string
	FaroKey string
	Version string
	// SMSSenderName and SMSPolicyURL are who the SMS opt-in on Me says
	// texts, and the policy it links. Empty says "this desk" and links none.
	SMSSenderName string
	SMSPolicyURL  string
}

type Server struct {
	cfg          Config
	desk         *desk.Desk
	store        *store.Store
	metrics      *metrics.Metrics
	log          *slog.Logger
	publicOrigin string
	gate         *door.Gate
	// open admits everyone the front door signed in, whatever their roles.
	// Only Me and what its page loads sit behind it.
	open  *door.Gate
	relay http.Handler
	// bot is the Teams bot, when one is set up; product calls its producer.
	bot     *BotConfig
	product *http.Client
	// learned holds the addresses already kept as people.
	learned sync.Map
	// sms answers texts people send to the desk's number, when set.
	sms *sms.Client
	// discord is the Discord bot, when one is set up.
	discord *discord.Bot
	// azureMonitor checks the Entra token of an action group's secure
	// webhook, when the desk's app for it is set.
	azureMonitor *azuremonitor.Door
}

func New(cfg Config, d *desk.Desk, m *metrics.Metrics, log *slog.Logger) (*Server, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("PUBLIC_URL must be an absolute URL")
	}
	if cfg.DeskRole == "" {
		cfg.DeskRole = "User"
	}
	if cfg.AdminRole == "" {
		cfg.AdminRole = "Admin"
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	cfg.SMSSenderName = strings.TrimSpace(cfg.SMSSenderName)
	cfg.SMSPolicyURL = strings.TrimSpace(cfg.SMSPolicyURL)
	if cfg.SMSPolicyURL != "" {
		if p, err := url.Parse(cfg.SMSPolicyURL); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			return nil, errors.New("SMS_POLICY_URL must be an absolute http or https URL")
		}
	}
	gate, err := door.NewGate(log, cfg.DeskRole, cfg.AdminRole)
	if err != nil {
		return nil, err
	}
	open, err := door.NewGate(log)
	if err != nil {
		return nil, err
	}
	relay, err := faro.NewRelay(faro.Config{Service: metrics.Service, Version: cfg.Version, URL: cfg.FaroURL, Key: cfg.FaroKey}, log)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:          cfg,
		desk:         d,
		store:        d.Store,
		metrics:      m,
		log:          log,
		publicOrigin: u.Scheme + "://" + u.Host,
		gate:         gate,
		open:         open,
		relay:        relay,
	}, nil
}

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

func (s *Server) logRequest(ctx context.Context, level slog.Level, msg string, r *http.Request, attrs ...any) {
	s.log.Log(ctx, level, msg, append([]any{"method", r.Method, "route", r.Pattern}, attrs...)...)
}

// failed answers a desk error with the right status and reports whether
// there was one.
func (s *Server) failed(w http.ResponseWriter, r *http.Request, err error, what string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, desk.ErrNotFound):
		writeError(w, http.StatusNotFound, "no such incident")
	case errors.Is(err, desk.ErrInput):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.logRequest(r.Context(), slog.LevelError, what+" failed", r, "error", err)
		writeError(w, http.StatusInternalServerError, what+" failed")
	}
	return true
}

// decodeJSON reads a small JSON body into v.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v)
}
