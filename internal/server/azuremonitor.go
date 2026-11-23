package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
	"github.com/orchard-suite/dispatch/internal/azuremonitor"
)

// azureMonitorProducer is who an alert from an action group's secure
// webhook comes from.
const azureMonitorProducer = "azure-monitor"

// UseAzureMonitor admits the Entra token an action group's secure webhook
// carries on POST /v1/azuremonitor.
func (s *Server) UseAzureMonitor(d *azuremonitor.Door) { s.azureMonitor = d }

// azureMonitorToken admits a secure webhook's Entra token, checked by the
// door, and otherwise an issued token or the shared secret, as on every
// other route. A token that looks signed is only ever checked as Entra's,
// and both branches are logged.
func (s *Server) azureMonitorToken(next tokenHandler) http.Handler {
	byToken := s.token(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !azuremonitor.LooksSigned(auth) {
			byToken.ServeHTTP(w, r)
			return
		}
		if s.azureMonitor == nil {
			s.logRequest(r.Context(), slog.LevelWarn, "api refused: no Azure Monitor app is set", r)
			writeError(w, http.StatusUnauthorized, "this desk takes no Azure Monitor token")
			return
		}
		if err := s.azureMonitor.Check(r.Context(), auth); err != nil {
			level, status := slog.LevelWarn, http.StatusUnauthorized
			if !errors.Is(err, azuremonitor.ErrRefused) {
				level, status = slog.LevelError, http.StatusServiceUnavailable
			}
			s.logRequest(r.Context(), level, "api refused: Azure Monitor token", r, "error", err)
			writeError(w, status, "not an Azure Monitor token for this desk")
			return
		}
		s.logRequest(r.Context(), slog.LevelDebug, "api admitted", r, "token", azureMonitorProducer)
		next(w, r, azureMonitorProducer)
	})
}

// apiAzureMonitor folds an action group's alert, in the common alert
// schema, into incidents and notices exactly as Alertmanager's are.
func (s *Server) apiAzureMonitor(w http.ResponseWriter, r *http.Request, by caller) {
	p, err := azuremonitor.Parse(http.MaxBytesReader(w, r.Body, azuremonitor.MaxBody))
	if err != nil {
		s.metrics.Webhook(r.Context(), "azure_monitor", "rejected")
		s.logRequest(r.Context(), slog.LevelWarn, "webhook rejected", r, "source", "azure_monitor", "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	a := p.Alert()
	res, err := s.desk.IngestAlerts(r.Context(), []alertmanager.Alert{a}, string(by))
	if err != nil {
		s.metrics.Webhook(r.Context(), "azure_monitor", "failed")
		s.logRequest(r.Context(), slog.LevelError, "webhook failed", r, "source", "azure_monitor", "error", err)
		writeError(w, http.StatusInternalServerError, "the alert could not be stored")
		return
	}
	s.metrics.Webhook(r.Context(), "azure_monitor", "accepted")
	s.logRequest(r.Context(), slog.LevelInfo, "webhook accepted", r, "source", "azure_monitor", "by", string(by),
		"alertname", a.Name(), "status", a.Status, "opened", len(res.Opened), "updated", len(res.Updated), "reopened", len(res.Reopened), "resolved", len(res.Resolved), "notices", res.Notices, "dropped", res.Dropped)
	writeJSON(w, http.StatusOK, res)
}
