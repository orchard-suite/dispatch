package server

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
	"github.com/orchard-suite/dispatch/internal/pagerduty"
)

// apiPagerDuty is PagerDuty's Events API v2, so anything that pages
// PagerDuty pages the desk by changing only its URL. PagerDuty's protocol
// carries the credential in the body as routing_key, so this door takes an
// issued token or the shared secret there, and nothing else; it is checked
// as the bearer token is on every other route.
func (s *Server) apiPagerDuty(w http.ResponseWriter, r *http.Request) {
	e, err := pagerduty.Parse(http.MaxBytesReader(w, r.Body, pagerduty.MaxBody))
	if err != nil {
		s.metrics.Webhook(r.Context(), "pagerduty", "rejected")
		writeJSON(w, http.StatusBadRequest, pagerduty.Invalid(err))
		return
	}
	var by string
	switch {
	case s.cfg.AlertmanagerToken != "" && subtle.ConstantTimeCompare([]byte(e.RoutingKey), []byte(s.cfg.AlertmanagerToken)) == 1:
		by = "alertmanager"
	default:
		tok, ok := s.store.VerifyToken(r.Context(), e.RoutingKey)
		if !ok {
			s.logRequest(r.Context(), slog.LevelWarn, "api refused: unknown routing key", r)
			writeJSON(w, http.StatusBadRequest, pagerduty.Invalid(errInvalidKey))
			return
		}
		by = tok.Name
	}
	s.logRequest(r.Context(), slog.LevelDebug, "api admitted", r, "token", by)
	ctx := r.Context()
	switch e.EventAction {
	case "trigger":
		res, err := s.desk.IngestAlerts(ctx, []alertmanager.Alert{e.Alert(time.Now())}, by)
		if err != nil {
			s.metrics.Webhook(ctx, "pagerduty", "failed")
			s.logRequest(ctx, slog.LevelError, "webhook failed", r, "source", "pagerduty", "error", err)
			writeError(w, http.StatusInternalServerError, "the event could not be stored")
			return
		}
		s.logRequest(ctx, slog.LevelInfo, "webhook accepted", r, "source", "pagerduty", "by", by, "opened", len(res.Opened), "updated", len(res.Updated), "notices", res.Notices, "dropped", res.Dropped)
	case "acknowledge":
		if _, err := s.desk.AcknowledgeAtSource(ctx, e.DedupKey, by); err != nil {
			s.logRequest(ctx, slog.LevelError, "acknowledge at source failed", r, "error", err)
			writeError(w, http.StatusInternalServerError, "the event could not be stored")
			return
		}
	case "resolve":
		if _, err := s.desk.ResolveAtSource(ctx, e.DedupKey, by); err != nil {
			s.logRequest(ctx, slog.LevelError, "resolve at source failed", r, "error", err)
			writeError(w, http.StatusInternalServerError, "the event could not be stored")
			return
		}
	}
	s.metrics.Webhook(ctx, "pagerduty", "accepted")
	writeJSON(w, http.StatusAccepted, pagerduty.Accepted(e.DedupKey))
}

var errInvalidKey = errors.New("routing_key is not a token this desk issued")
