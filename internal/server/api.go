package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

// API is the machine port. Every route but /healthz needs a token.
func (s *Server) API() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	for _, rt := range s.apiRoutes() {
		mux.Handle(rt.pattern, rt.handler)
	}
	return telemetry.Handler(mux, "api")
}

// apiRoute is one route of the machine port's API: a ServeMux pattern and
// its handler. openapi.yaml at the repository root describes every one;
// /healthz is liveness, not part of the API, and stays out of both.
type apiRoute struct {
	pattern string
	handler http.Handler
}

func (s *Server) apiRoutes() []apiRoute {
	return []apiRoute{
		{"POST /v1/alertmanager", s.token(s.apiAlertmanager)},
		{"POST /v1/azuremonitor", s.azureMonitorToken(s.apiAzureMonitor)},
		{"POST /v1/incidents", s.token(s.apiOpen)},
		{"GET /v1/incidents", s.token(s.apiList)},
		{"GET /v1/incidents/{id}", s.token(s.apiGet)},
		{"POST /v1/incidents/{id}/acknowledge", s.token(s.apiChange(incident.Acknowledged))},
		{"POST /v1/incidents/{id}/mitigate", s.token(s.apiChange(incident.Mitigated))},
		{"POST /v1/incidents/{id}/resolve", s.token(s.apiChange(incident.Resolved))},
		{"POST /v1/incidents/{id}/reopen", s.token(s.apiChange(incident.Open))},
		{"POST /v1/incidents/{id}/notes", s.token(s.apiNote)},
		{"POST /v1/incidents/{id}/roles", s.token(s.apiRole)},
		{"POST /v1/incidents/{id}/merge", s.token(s.apiMerge)},
		{"GET /v1/outcomes", s.token(s.apiOutcomes)},
		{"POST /v1/events", s.token(s.apiEvent)},
		{"PUT /v1/kinds", s.token(s.apiKinds)},
		{"POST /v1/sms", s.token(s.apiSMS)},
		{"POST /v2/enqueue", http.HandlerFunc(s.apiPagerDuty)},
		{"POST /v1/heartbeats/{name}", s.token(s.apiBeat)},
		{"POST /v1/changes", s.token(s.apiAddChange)},
		{"GET /v1/schedules", s.token(s.apiOnCall)},
		{"POST /v1/schedules/{slug}/overrides", s.token(s.apiCover)},
		{"GET /v1/calendar/{secret}", http.HandlerFunc(s.apiCalendar)},
		{"POST /mcp", s.token(s.apiMCP())},
		{"GET /v1/config", s.token(s.apiExport)},
		{"PUT /v1/config", s.token(s.apiApply)},
		{"POST /api/messages", http.HandlerFunc(s.apiBot)},
		{"POST /api/discord", http.HandlerFunc(s.apiDiscord)},
	}
}

// personKey holds, in a request's context, the person a personal token
// acts as.
type personKey struct{}

// actor is who a request acts as: the person a personal token stands for,
// or the token itself, on behalf of whoever the body names.
func actor(r *http.Request, by caller, onBehalfOf string) incident.Actor {
	if p, ok := r.Context().Value(personKey{}).(string); ok && p != "" {
		return incident.Person(p)
	}
	a := incident.System(string(by))
	a.For = strings.TrimSpace(onBehalfOf)
	return a
}

// caller is who a token stands for: its name, or "alertmanager" for the
// shared secret.
type caller string

type tokenHandler func(http.ResponseWriter, *http.Request, caller)

// token admits a request carrying the shared secret or a live issued
// token, and nothing else. Both branches are logged.
func (s *Server) token(next tokenHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		secret = strings.TrimSpace(secret)
		if !ok || secret == "" {
			s.logRequest(r.Context(), slog.LevelWarn, "api refused: no bearer token", r)
			writeError(w, http.StatusUnauthorized, "a bearer token is required")
			return
		}
		if s.cfg.AlertmanagerToken != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(s.cfg.AlertmanagerToken)) == 1 {
			s.logRequest(r.Context(), slog.LevelDebug, "api admitted", r, "token", "shared")
			next(w, r, "alertmanager")
			return
		}
		tok, ok := s.store.VerifyToken(r.Context(), secret)
		if !ok {
			s.logRequest(r.Context(), slog.LevelWarn, "api refused: unknown token", r)
			writeError(w, http.StatusUnauthorized, "this token is not valid")
			return
		}
		s.logRequest(r.Context(), slog.LevelDebug, "api admitted", r, "token", tok.Name, "personal", tok.Person != "")
		if tok.Person != "" {
			r = r.WithContext(context.WithValue(r.Context(), personKey{}, tok.Person))
		}
		next(w, r, caller(tok.Name))
	})
}

func (s *Server) apiAlertmanager(w http.ResponseWriter, r *http.Request, by caller) {
	p, err := alertmanager.Parse(http.MaxBytesReader(w, r.Body, alertmanager.MaxBody))
	if err != nil {
		s.metrics.Webhook(r.Context(), "alertmanager", "rejected")
		s.logRequest(r.Context(), slog.LevelWarn, "webhook rejected", r, "source", "alertmanager", "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.desk.IngestAlertmanager(r.Context(), p, string(by))
	if err != nil {
		s.metrics.Webhook(r.Context(), "alertmanager", "failed")
		s.logRequest(r.Context(), slog.LevelError, "webhook failed", r, "source", "alertmanager", "error", err)
		writeError(w, http.StatusInternalServerError, "the alerts could not be stored")
		return
	}
	s.metrics.Webhook(r.Context(), "alertmanager", "accepted")
	s.logRequest(r.Context(), slog.LevelInfo, "webhook accepted", r, "source", "alertmanager", "by", string(by),
		"alerts", len(p.Alerts), "opened", len(res.Opened), "updated", len(res.Updated), "reopened", len(res.Reopened), "resolved", len(res.Resolved), "notices", res.Notices)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiOpen(w http.ResponseWriter, r *http.Request, by caller) {
	var req desk.OpenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "the body must be JSON with title, severity, and service")
		return
	}
	inc, err := s.desk.Open(r.Context(), req, incident.SourceAPI, incident.System(string(by)))
	if s.failed(w, r, err, "open") {
		return
	}
	w.Header().Set("Location", s.cfg.PublicURL+"/incidents/"+strconv.FormatInt(inc.ID, 10))
	writeJSON(w, http.StatusCreated, inc)
}

// apiList answers GET /v1/incidents?state=open,acknowledged&service=x&severity=critical&q=text&limit=n.
func (s *Server) apiList(w http.ResponseWriter, r *http.Request, _ caller) {
	q := r.URL.Query()
	f := store.Filter{Service: q.Get("service"), Severity: incident.Severity(q.Get("severity")), Search: q.Get("q"), Limit: 500}
	if v := q.Get("state"); v != "" {
		for _, st := range strings.Split(v, ",") {
			f.States = append(f.States, incident.State(strings.TrimSpace(st)))
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 5000 {
			writeError(w, http.StatusBadRequest, "limit must be a number from 1 to 5000")
			return
		}
		f.Limit = n
	}
	list, err := s.store.List(r.Context(), f)
	if s.failed(w, r, err, "list") {
		return
	}
	if list == nil {
		list = []incident.Incident{}
	}
	writeJSON(w, http.StatusOK, list)
}

// incidentView is an incident with its timeline, for GET /v1/incidents/{id}.
type incidentView struct {
	incident.Incident
	Alerts     []incident.Alert      `json:"alerts"`
	Timeline   []incident.Event      `json:"timeline"`
	Postmortem *incident.Postmortem  `json:"postmortem,omitempty"`
	Actions    []incident.ActionItem `json:"action_items"`
}

func (s *Server) apiGet(w http.ResponseWriter, r *http.Request, _ caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	v, err := s.view(r.Context(), id)
	if s.failed(w, r, err, "read") {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) view(ctx context.Context, id int64) (incidentView, error) {
	inc, err := s.store.Get(ctx, s.store.DB(), id)
	if err != nil {
		return incidentView{}, err
	}
	v := incidentView{Incident: inc}
	if v.Alerts, err = s.store.Alerts(ctx, id); err != nil {
		return v, err
	}
	if v.Timeline, err = s.store.Events(ctx, id); err != nil {
		return v, err
	}
	if v.Actions, err = s.store.ActionItems(ctx, id); err != nil {
		return v, err
	}
	pm, err := s.store.Postmortem(ctx, id)
	if err == nil {
		v.Postmortem = &pm
	} else if !errors.Is(err, store.ErrNotFound) {
		return v, err
	}
	if v.Alerts == nil {
		v.Alerts = []incident.Alert{}
	}
	if v.Actions == nil {
		v.Actions = []incident.ActionItem{}
	}
	return v, nil
}

func (s *Server) apiChange(to incident.State) tokenHandler {
	return func(w http.ResponseWriter, r *http.Request, by caller) {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var body struct {
			Actor string `json:"actor"`
		}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "the body, when sent, must be JSON")
				return
			}
		}
		inc, err := s.desk.Change(r.Context(), id, to, actor(r, by, body.Actor))
		if s.failed(w, r, err, "change") {
			return
		}
		writeJSON(w, http.StatusOK, inc)
	}
}

func (s *Server) apiNote(w http.ResponseWriter, r *http.Request, by caller) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Text   string `json:"text"`
		Public bool   `json:"public"`
		Actor  string `json:"actor"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "the body must be JSON with text")
		return
	}
	actor := string(by)
	if strings.TrimSpace(body.Actor) != "" {
		actor = strings.TrimSpace(body.Actor)
	}
	inc, err := s.desk.Note(r.Context(), id, body.Text, body.Public, actor)
	if s.failed(w, r, err, "note") {
		return
	}
	writeJSON(w, http.StatusOK, inc)
}

func (s *Server) apiOutcomes(w http.ResponseWriter, r *http.Request, _ caller) {
	report, err := s.desk.Outcomes(r.Context(), r.URL.Query().Get("window"))
	if s.failed(w, r, err, "outcomes") {
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "no such incident")
		return 0, false
	}
	return id, true
}

// apiEvent takes one event from any product and queues it for every
// channel a subscription sends it to. The producer is the token's name,
// whatever the body says, so one product cannot speak as another.
func (s *Server) apiEvent(w http.ResponseWriter, r *http.Request, by caller) {
	box := s.desk.Notices()
	if box == nil {
		writeError(w, http.StatusServiceUnavailable, "this desk sends no notices")
		return
	}
	var e notice.Event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10+2*notice.MaxPage)).Decode(&e); err != nil {
		writeError(w, http.StatusBadRequest, "the body must be one event as JSON")
		return
	}
	e.ID, e.Producer = 0, string(by)
	got, err := box.Accept(r.Context(), e)
	if errors.Is(err, notice.ErrInvalid) {
		s.logRequest(r.Context(), slog.LevelWarn, "event refused", r, "producer", string(by), "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.failed(w, r, err, "event") {
		return
	}
	got.Event.Page = ""
	writeJSON(w, http.StatusAccepted, got)
}
