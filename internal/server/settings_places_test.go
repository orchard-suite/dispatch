package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
)

// fireNow sends the fixture's two alerts as if they started just now, so
// they count in the last week.
func fireNow(t *testing.T, h *harness) {
	t.Helper()
	raw, _ := io.ReadAll(fixture(t, "alertmanager_firing.json"))
	now := time.Now().UTC().Format(time.RFC3339)
	body := strings.NewReplacer("2026-10-31T14:02:11.000Z", now, "2026-10-31T14:05:41.000Z", now).Replace(string(raw))
	if rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/alertmanager", strings.NewReader(body)), shared)); rec.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSettingsPlacesSaveOnlyTheirOwnFields(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	get := func(path string) *httptest.ResponseRecorder {
		return h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, path, nil), admin, "Admin"))
	}
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		return h.do(h.session, asPerson(form(path, v), admin, "Admin"))
	}
	for _, p := range []string{"/settings", "/settings/routing", "/settings/status", "/settings/machines"} {
		if rec := get(p); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "</html>") {
			t.Fatalf("%s: %d, rendered whole: %v", p, rec.Code, strings.Contains(rec.Body.String(), "</html>"))
		}
	}
	if rec := get("/settings/nowhere"); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown place: %d", rec.Code)
	}
	if rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/settings/routing", nil), member, "User")); rec.Code != http.StatusForbidden {
		t.Errorf("routing as a member: %d", rec.Code)
	}

	st, _ := h.desk.Store.Settings(ctx)
	st.Routes = []incident.Route{{Match: map[string]string{"alertname": "Watchdog"}, Action: incident.Drop}}
	st.StatusTitle = "Acme status"
	h.desk.Store.SaveSettings(ctx, st)

	rec := post("/settings", url.Values{"place": {"general"}, "window_minutes": {"30"}, "quiet_resolve_hours": {"6"}, "person_pages": {"4"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?saved=general" {
		t.Fatalf("general: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	st, _ = h.desk.Store.Settings(ctx)
	if st.GroupingWindow != 30*time.Minute || st.QuietResolve != 6*time.Hour || st.PersonPages != 4 {
		t.Errorf("general fields: %+v", st)
	}
	if len(st.Routes) != 1 || st.StatusTitle != "Acme status" {
		t.Errorf("general wiped another place: %+v", st)
	}
	if rec := post("/settings", url.Values{"place": {"status"}, "status_title": {"Example status"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("status: %d", rec.Code)
	}
	st, _ = h.desk.Store.Settings(ctx)
	if st.StatusTitle != "Example status" || st.GroupingWindow != 30*time.Minute || len(st.Routes) != 1 {
		t.Errorf("status saved more than its title: %+v", st)
	}
	if rec := post("/settings", url.Values{"window_minutes": {"0"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("a form naming no place: %d", rec.Code)
	}
	if rec := post("/settings", url.Values{"place": {"general"}, "window_minutes": {"x"}, "quiet_resolve_hours": {"0"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "data-message") {
		t.Errorf("a bad window: %d", rec.Code)
	}
}

func TestRoutesAreRowsWithCounts(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	fireNow(t, h)
	post := func(v url.Values) *httptest.ResponseRecorder {
		return h.do(h.session, asPerson(form("/settings/lists/routes", v), admin, "Admin"))
	}
	routing := func(q string) string {
		return h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/settings/routing"+q, nil), admin, "Admin")).Body.String()
	}

	rec := post(url.Values{"op": {"add"}, "label": {"alertname"}, "value": {"CometErrors"}, "action": {"notice"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/routing?saved=routes" {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	post(url.Values{"op": {"add"}, "label": {"namespace"}, "value": {"com*"}, "match": {"job=comet"}, "action": {"severity"}, "severity": {"info"}})
	st, _ := h.desk.Store.Settings(ctx)
	if len(st.Routes) != 2 || st.Routes[1].Action != incident.Rate || st.Routes[1].Severity != incident.Info || st.Routes[1].Match["job"] != "comet" {
		t.Fatalf("routes %+v", st.Routes)
	}

	// Each route reads as a sentence, with what it caught: the first takes
	// CometErrors, the second the alert left.
	page := routing("")
	for _, want := range []string{"make it a notice", "rate it info", "<b>1</b> caught", "<b>0</b> fell through"} {
		if !strings.Contains(page, want) {
			t.Errorf("routing page lacks %q", want)
		}
	}

	if rec := post(url.Values{"op": {"down"}, "i": {"0"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("down: %d", rec.Code)
	}
	st, _ = h.desk.Store.Settings(ctx)
	if st.Routes[0].Action != incident.Rate {
		t.Errorf("moved %+v", st.Routes)
	}
	post(url.Values{"op": {"replace"}, "i": {"1"}, "label": {"alertname"}, "value": {"CometErrors"}, "action": {"drop"}})
	post(url.Values{"op": {"remove"}, "i": {"0"}})
	st, _ = h.desk.Store.Settings(ctx)
	if len(st.Routes) != 1 || st.Routes[0].Action != incident.Drop {
		t.Errorf("replaced and removed %+v", st.Routes)
	}
	if rec := post(url.Values{"op": {"add"}, "label": {"alertname"}, "value": {""}, "action": {"notice"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("a route with no value: %d", rec.Code)
	}
	if rec := post(url.Values{"op": {"up"}, "i": {"0"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("the first route moved up: %d", rec.Code)
	}

	// Quiet it on the Noise panel opens a prefilled row, counted as if last.
	page = routing("?route=" + url.QueryEscape("alertname=CometSilent => notice"))
	if !strings.Contains(page, "From Quiet it") || !strings.Contains(page, `value="CometSilent"`) || !strings.Contains(page, "caught <b>1</b> alert ") {
		t.Errorf("the suggested route is not a ready row")
	}
	old := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/settings?route="+url.QueryEscape("alertname=X => notice"), nil), admin, "Admin"))
	if old.Code != http.StatusSeeOther || !strings.HasPrefix(old.Header().Get("Location"), "/settings/routing?route=") {
		t.Errorf("the old Quiet it link: %d %s", old.Code, old.Header().Get("Location"))
	}
}

func TestKeyedListsAndMappingRules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	fireNow(t, h)
	post := func(list string, v url.Values) *httptest.ResponseRecorder {
		return h.do(h.session, asPerson(form("/settings/lists/"+list, v), admin, "Admin"))
	}
	if rec := post("rules", url.Values{"op": {"add"}, "label": {"namespace"}, "value": {"com*"}, "service": {"comet-web"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("rule: %d %s", rec.Code, rec.Body.String())
	}
	page := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/settings/routing", nil), admin, "Admin")).Body.String()
	if !strings.Contains(page, "the service is <b>comet-web</b>") || !strings.Contains(page, "<b>2</b> mapped") {
		t.Errorf("the rule row and its count")
	}
	post("components", url.Values{"op": {"add"}, "key": {"shop"}, "name": {"Online shop"}})
	post("templates", url.Values{"op": {"add"}, "key": {"critical"}, "text": {"We know {service} is down."}})
	post("repos", url.Values{"op": {"add"}, "key": {"*"}, "repo": {"example/platform"}})
	if rec := post("repos", url.Values{"op": {"add"}, "key": {"shop"}, "repo": {"not a repo"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("a bad repository: %d", rec.Code)
	}
	st, _ := h.desk.Store.Settings(ctx)
	if st.PublicComponents["shop"] != "Online shop" || st.UpdateTemplates["critical"] == "" || st.IssueRepos["*"] != "example/platform" {
		t.Errorf("keyed lists %+v", st)
	}
	machines := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/settings/machines", nil), admin, "Admin")).Body.String()
	if !strings.Contains(machines, "Every other service") || !strings.Contains(machines, "example/platform") {
		t.Errorf("machines lacks the repository row")
	}
}
