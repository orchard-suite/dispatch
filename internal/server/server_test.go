package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/metrics"
	"github.com/orchard-suite/dispatch/internal/notify"
	"github.com/orchard-suite/dispatch/internal/orrery"
	"github.com/orchard-suite/dispatch/internal/outbox"
	"github.com/orchard-suite/dispatch/internal/outcomes"
	"github.com/orchard-suite/dispatch/internal/seal"
	"github.com/orchard-suite/dispatch/internal/store"
)

const (
	publicURL = "https://dispatch.test"
	shared    = "shared-secret"
	admin     = "admin@example.com"
	member    = "member@example.com"
)

type harness struct {
	t       *testing.T
	srv     *Server
	desk    *desk.Desk
	api     http.Handler
	session http.Handler
	status  http.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, func(log *slog.Logger) *annals.Poster {
		return annals.New("", "", log, func(context.Context, string) {})
	})
}

// newHarnessWith runs the desk with the annals poster decisions makes.
func newHarnessWith(t *testing.T, decisions func(*slog.Logger) *annals.Poster) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "dispatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var d *desk.Desk
	mt, err := metrics.New(func() metrics.Stats { return d.Stats() })
	if err != nil {
		t.Fatal(err)
	}
	quiet := func(context.Context, string) {}
	sealer, _ := seal.New("test key")
	box := outbox.New(st, sealer, nil, log, quiet)
	poster := decisions(log)
	box.UseAnnals(poster)
	d = desk.New(st, mt, notify.New("", log, quiet), orrery.New(orrery.Config{}, log, quiet), poster, box, log, publicURL)
	srv, err := New(Config{PublicURL: publicURL, AlertmanagerToken: shared, Version: "test"}, d, mt, log)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, srv: srv, desk: d, api: srv.API(), session: srv.Session(), status: srv.Status()}
}

func (h *harness) do(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func fixture(t *testing.T, name string) io.Reader {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReader(string(data))
}

func bearer(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func asPerson(req *http.Request, email, roles string) *http.Request {
	req.Header.Set("X-Forwarded-Email", email)
	req.Header.Set("X-Forwarded-Groups", roles)
	req.Header.Set("Origin", publicURL)
	return req
}

func form(path string, values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func TestTokenAuthBothBranches(t *testing.T) {
	h := newHarness(t)
	list := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
		if token != "" {
			bearer(req, token)
		}
		return h.do(h.api, req).Code
	}
	if code := list(""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code := list("nope"); code != http.StatusUnauthorized {
		t.Errorf("unknown token: %d", code)
	}
	if code := list(shared); code != http.StatusOK {
		t.Errorf("shared secret: %d", code)
	}
	// A token the desk issued.
	rec := h.do(h.session, asPerson(form("/tokens", url.Values{"name": {"board"}}), admin, "Admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	start := strings.Index(body, "dsp_")
	if start < 0 {
		t.Fatal("no secret shown")
	}
	secret := body[start : start+strings.IndexAny(body[start:], "<\n")]
	if code := list(secret); code != http.StatusOK {
		t.Errorf("issued token: %d", code)
	}
	if code := list(secret + "x"); code != http.StatusUnauthorized {
		t.Errorf("a near miss is not a token: %d", code)
	}
	// Headers never open the API port.
	req := httptest.NewRequest(http.MethodGet, "/v1/incidents", nil)
	asPerson(req, admin, "Admin")
	if code := h.do(h.api, req).Code; code != http.StatusUnauthorized {
		t.Errorf("identity headers on the API port: %d", code)
	}
	// A bearer token never opens the desk.
	req = bearer(httptest.NewRequest(http.MethodGet, "/", nil), shared)
	if code := h.do(h.session, req).Code; code != http.StatusUnauthorized {
		t.Errorf("bearer on the session port: %d", code)
	}
	// Health needs nothing.
	if code := h.do(h.api, httptest.NewRequest(http.MethodGet, "/healthz", nil)).Code; code != http.StatusOK {
		t.Errorf("healthz: %d", code)
	}
}

func TestDeskRoles(t *testing.T) {
	h := newHarness(t)
	get := func(path, email, roles string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if email != "" {
			asPerson(req, email, roles)
		}
		return h.do(h.session, req).Code
	}
	if code := get("/", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no identity: %d", code)
	}
	// Signed in with no desk role: Me is the one page open, and home leads there.
	if code := get("/", member, "Viewer"); code != http.StatusSeeOther {
		t.Errorf("home with no desk role: %d", code)
	}
	if code := get("/history", member, "Viewer"); code != http.StatusForbidden {
		t.Errorf("no desk role: %d", code)
	}
	if code := get("/", member, "User"); code != http.StatusOK {
		t.Errorf("desk role: %d", code)
	}
	if code := get("/settings", member, "User"); code != http.StatusForbidden {
		t.Errorf("settings as a member: %d", code)
	}
	if code := get("/settings", admin, "Admin"); code != http.StatusOK {
		t.Errorf("settings as admin: %d", code)
	}
	// A change from a foreign origin is refused.
	req := form("/incidents", url.Values{"title": {"x"}})
	asPerson(req, admin, "Admin")
	req.Header.Set("Origin", "https://evil.test")
	if code := h.do(h.session, req).Code; code != http.StatusForbidden {
		t.Errorf("foreign origin: %d", code)
	}
}

func TestWebhookThenDeskThenOutcomes(t *testing.T) {
	h := newHarness(t)
	rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/alertmanager", fixture(t, "alertmanager_firing.json")), shared))
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}
	var res desk.IngestResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Opened) != 2 {
		t.Fatalf("opened = %v", res)
	}
	id := res.Opened[0]
	// Bad bodies are refused, not stored.
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/alertmanager", strings.NewReader(`{"alerts":[]}`)), shared))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty payload: %d", rec.Code)
	}
	// Acknowledge and resolve through the API with an actor.
	path := "/v1/incidents/" + itoa(id)
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, path+"/acknowledge", strings.NewReader(`{"actor":"oncall@example.com"}`)), shared))
	if rec.Code != http.StatusOK {
		t.Fatalf("acknowledge: %d %s", rec.Code, rec.Body.String())
	}
	var inc incident.Incident
	json.Unmarshal(rec.Body.Bytes(), &inc)
	if inc.State != incident.Acknowledged || inc.Owner != "oncall@example.com" {
		t.Errorf("acknowledged: %+v", inc)
	}
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, path+"/acknowledge", nil), shared))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("acknowledge twice: %d", rec.Code)
	}
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, path+"/resolve", nil), shared))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/alertmanager", fixture(t, "alertmanager_resolved.json")), shared))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolved webhook: %d", rec.Code)
	}
	// The full view carries the timeline.
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, path, nil), shared))
	var view incidentView
	json.Unmarshal(rec.Body.Bytes(), &view)
	if len(view.Timeline) < 4 || len(view.Alerts) != 1 {
		t.Errorf("view: %d events, %d alerts", len(view.Timeline), len(view.Alerts))
	}
	// Filters.
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/incidents?state=resolved", nil), shared))
	var list []incident.Incident
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != id {
		t.Errorf("resolved filter: %+v", list)
	}
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/incidents?state=open&severity=critical", nil), shared))
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0].Severity != incident.Critical {
		t.Errorf("open critical filter: %+v", list)
	}
	// Outcomes.
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/outcomes?window=7d", nil), shared))
	if rec.Code != http.StatusOK {
		t.Fatalf("outcomes: %d %s", rec.Code, rec.Body.String())
	}
	var report outcomes.Report
	json.Unmarshal(rec.Body.Bytes(), &report)
	if report.Window != "7d" || report.Total.Incidents != 2 || report.Total.Resolved != 1 || report.Total.Acknowledged != 1 || len(report.Services) != 1 || report.Services[0].Service != "comet" {
		t.Errorf("report: %+v", report)
	}
	if rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/outcomes?window=2y", nil), shared)); rec.Code != http.StatusBadRequest {
		t.Errorf("bad window: %d", rec.Code)
	}
	// Every desk page renders for it.
	for _, p := range []string{"/", "/history?q=comet", "/components", "/incidents/" + itoa(id), "/incidents/" + itoa(id) + "/postmortem", "/settings"} {
		rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, p, nil), admin, "Admin"))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d", p, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "</html>") {
			t.Errorf("%s: page did not finish rendering", p)
		}
	}
	rec = h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/incidents/999", nil), admin, "Admin"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing incident page: %d", rec.Code)
	}
}

func TestOpenByHandAndForms(t *testing.T) {
	h := newHarness(t)
	rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(`{"title":"Checkout failing","severity":"critical","service":"shop"}`)), shared))
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") == "" {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	var inc incident.Incident
	json.Unmarshal(rec.Body.Bytes(), &inc)
	if inc.Source != incident.SourceAPI || inc.Severity != incident.Critical {
		t.Errorf("opened: %+v", inc)
	}
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(`{"service":"shop"}`)), shared))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no title: %d", rec.Code)
	}
	id := itoa(inc.ID)
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		return h.do(h.session, asPerson(form(path, v), member, "User"))
	}
	if rec := post("/incidents/"+id+"/state", url.Values{"to": {"acknowledged"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("acknowledge form: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/incidents/"+id+"/note", url.Values{"text": {"Rolling back"}, "public": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("note form: %d", rec.Code)
	}
	if rec := post("/incidents/"+id+"/public", url.Values{"public": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("public form: %d", rec.Code)
	}
	if rec := post("/incidents/"+id+"/severity", url.Values{"severity": {"warning"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("severity form: %d", rec.Code)
	}
	if rec := post("/incidents/"+id+"/actions", url.Values{"text": {"Add a canary"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("action form: %d", rec.Code)
	}
	if rec := post("/incidents/"+id+"/postmortem", url.Values{"body": {"## What happened\n\nIt broke."}}); rec.Code != http.StatusSeeOther {
		t.Errorf("postmortem form: %d", rec.Code)
	}
	got, _ := h.desk.Store.Get(context.Background(), h.desk.Store.DB(), inc.ID)
	if got.State != incident.Acknowledged || got.Owner != member || !got.Public || got.Severity != incident.Warning {
		t.Errorf("after forms: %+v", got)
	}
	page := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/incidents/"+id, nil), member, "User")).Body.String()
	for _, want := range []string{"Rolling back", "public"} {
		if !strings.Contains(page, want) {
			t.Errorf("incident page lacks %q", want)
		}
	}
	pm := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/incidents/"+id+"/postmortem", nil), member, "User")).Body.String()
	for _, want := range []string{"Add a canary", "It broke.", `name="body"`} {
		if !strings.Contains(pm, want) {
			t.Errorf("postmortem tab lacks %q", want)
		}
	}
}

func TestStatusPageRedacts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	settings, _ := h.desk.Store.Settings(ctx)
	settings.PublicComponents = map[string]string{"shop": "Online shop", "api": "API"}
	settings.StatusTitle = "Example status"
	h.desk.Store.SaveSettings(ctx, settings)

	secret, _ := h.desk.Open(ctx, desk.OpenRequest{Title: "Internal: database host db-7 on fire", Severity: "critical", Service: "db"}, incident.SourceAPI, incident.System("board"))
	h.desk.Change(ctx, secret.ID, incident.Acknowledged, incident.Person("oncall@example.com"))
	h.desk.Note(ctx, secret.ID, "root password rotated", false, "oncall@example.com")

	shown, _ := h.desk.Open(ctx, desk.OpenRequest{Title: "Checkout is slow", Severity: "warning", Service: "shop", Public: true}, incident.SourceAPI, incident.System("board"))
	h.desk.Change(ctx, shown.ID, incident.Acknowledged, incident.Person("oncall@example.com"))
	h.desk.Note(ctx, shown.ID, "We are scaling the checkout service.", true, "oncall@example.com")
	h.desk.Note(ctx, shown.ID, "pod shop-7f9 OOMKilled, bumping limits", false, "oncall@example.com")

	rec := h.do(h.status, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Example status", "Online shop", "Degraded", "API", "Operational", "Checkout is slow", "We are scaling the checkout service.", "Some systems are degraded"} {
		if !strings.Contains(body, want) {
			t.Errorf("status page lacks %q", want)
		}
	}
	for _, leak := range []string{"db-7", "Internal", "root password", "oncall", "example.com", "OOMKilled", "shop-7f9", `"db"`, ">db<", "board"} {
		if strings.Contains(body, leak) {
			t.Errorf("status page leaks %q", leak)
		}
	}
	rec = h.do(h.status, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	var page StatusPage
	json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Incidents) != 1 || len(page.Incidents[0].Updates) != 1 || len(page.Components) != 2 || page.Overall != "degraded" {
		t.Errorf("status json: %+v", page)
	}
	js := rec.Body.String()
	for _, leak := range []string{"oncall", "db-7", "OOMKilled", "\"owner\"", "\"service\""} {
		if strings.Contains(js, leak) {
			t.Errorf("status json leaks %q", leak)
		}
	}
	// A critical on a public component takes the whole page down; one on
	// a component not listed changes nothing in public.
	h.desk.SetSeverity(ctx, shown.ID, incident.Critical, "oncall@example.com")
	rec = h.do(h.status, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Overall != "down" {
		t.Errorf("overall after critical: %s", page.Overall)
	}
	h.desk.Change(ctx, shown.ID, incident.Resolved, incident.Person("oncall@example.com"))
	rec = h.do(h.status, httptest.NewRequest(http.MethodGet, "/status.json", nil))
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Overall != "operational" || len(page.Incidents) != 0 || len(page.Resolved) != 1 {
		t.Errorf("after resolve: %+v", page)
	}
}

func TestTelemetryRelayDropsWithoutKey(t *testing.T) {
	h := newHarness(t)
	req := asPerson(httptest.NewRequest(http.MethodPost, "/api/telemetry", strings.NewReader(`{"meta":{"app":{"name":"x"}},"logs":[]}`)), member, "User")
	if rec := h.do(h.session, req); rec.Code != http.StatusAccepted {
		t.Errorf("relay without key: %d", rec.Code)
	}
	req = asPerson(httptest.NewRequest(http.MethodPost, "/api/telemetry", strings.NewReader(`nope`)), member, "User")
	if rec := h.do(h.session, req); rec.Code != http.StatusBadRequest {
		t.Errorf("relay bad body: %d", rec.Code)
	}
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.Replace(time.Duration(n).String(), "ns", "", 1))
}
