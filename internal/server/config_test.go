package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/config"
)

const document = `settings:
  grouping_minutes: 20
  auto_resolve_info: true
  routes: |
    alertname=Watchdog => drop
  zone: America/New_York
  person_pages: 10
people:
  - email: Ann@Example.com
    name: Ann
    phone: +1 555 555 0100
    rules: {page: "sms, email+5"}
groups:
  - slug: platform
    name: Platform
    members: [ann@example.com, bo@example.com]
schedules:
  - slug: platform
    name: Platform on call
    zone: America/New_York
    layers:
      - members: [ann@example.com, bo@example.com]
        start: "2026-11-02 09:00"
        every: 1w
chains:
  - slug: oncall
    name: On call
    steps: |
      10 schedule:platform
      20 group:platform
    repeat: 1
subscriptions:
  - to: chain:oncall
    topics: [incident.opened]
heartbeats:
  - name: nightly-backup
    every: 24h
    grace: 30m
`

func TestConfigAppliesAndExports(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	put := func(body, token string) *httptest.ResponseRecorder {
		return h.do(h.api, bearer(httptest.NewRequest(http.MethodPut, "/v1/config", strings.NewReader(body)), token))
	}
	if rec := put(document, shared); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"schedules":1`) {
		t.Fatalf("apply %d %s", rec.Code, rec.Body.String())
	}
	if p, _ := h.desk.Store.Person(ctx, "ann@example.com"); p.Phone != "+15555550100" || len(p.Rules["page"]) != 2 {
		t.Errorf("ann %+v", p)
	}
	if st, _ := h.desk.Store.Settings(ctx); len(st.Routes) != 1 || st.Zone != "America/New_York" || st.PersonPages != 10 {
		t.Errorf("settings %+v", st)
	}
	if rec := put("subscriptions:\n  - channel: nowhere\n    topics: ['*']\n", shared); rec.Code != 400 || !strings.Contains(rec.Body.String(), "no channel") {
		t.Errorf("an unknown channel: %d %s", rec.Code, rec.Body.String())
	}
	if rec := put("nonsense: 1\n", shared); rec.Code != 400 {
		t.Errorf("an unknown field: %d", rec.Code)
	}
	if subs, _ := h.desk.Store.Subscriptions(ctx); len(subs) != 1 {
		t.Errorf("a refused document changed nothing: %d", len(subs))
	}
	_, personal, _ := h.desk.Store.MintPersonToken(ctx, "agent", member)
	if rec := put(document, personal); rec.Code != http.StatusForbidden {
		t.Errorf("a personal token applied: %d", rec.Code)
	}
	exported := h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/config", nil), shared))
	doc, err := config.Read(strings.NewReader(exported.Body.String()))
	if err != nil || len(doc.Schedules) != 1 || doc.Schedules[0].Layers[0].Start != "2026-11-02 09:00" || doc.Chains[0].Repeat != 1 {
		t.Fatalf("export %v %s", err, exported.Body.String())
	}
	// Applying the export again changes nothing that matters, and dropping a
	// section's item removes it.
	if rec := put(exported.Body.String(), shared); rec.Code != 200 {
		t.Fatalf("reapply %d %s", rec.Code, rec.Body.String())
	}
	if rec := put("heartbeats: []\n", shared); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if beats, _ := h.desk.Store.Heartbeats(ctx); len(beats) != 0 {
		t.Errorf("an empty section empties it: %+v", beats)
	}
}

func TestConfigCarriesHowChangesAreRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	put := func(body string) *httptest.ResponseRecorder {
		return h.do(h.api, bearer(httptest.NewRequest(http.MethodPut, "/v1/config", strings.NewReader(body)), shared))
	}
	doc := `settings:
  grouping_minutes: 15
  changes:
    leans_on: [depends_on, "<-azure_resource:service"]
    depth: 2
    kinds:
      - {name: stops, family: disruption, means: It stops or restarts., unit: copies, watch: 45m}
      - {name: halts, family: disruption, means: A line stops., watch: 1h}
`
	if rec := put(doc); rec.Code != 200 {
		t.Fatalf("apply %d %s", rec.Code, rec.Body.String())
	}
	st, _ := h.desk.Store.Settings(ctx)
	if len(st.Leaning.Relations) != 2 || st.Leaning.Depth != 2 || len(st.Kinds) != 2 {
		t.Fatalf("settings %+v", st)
	}
	kinds, err := h.desk.Kinds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := kinds.Get("stops"); time.Duration(k.Watch) != 45*time.Minute {
		t.Errorf("a base kind's watch changed: %+v", k)
	}
	if _, ok := kinds.Get("halts"); !ok {
		t.Error("the estate's own kind")
	}
	for name, bad := range map[string]string{
		"a bad relation":          "settings:\n  changes:\n    leans_on: [Depends On]\n",
		"a base kind's family":    "settings:\n  changes:\n    kinds:\n      - {name: stops, family: exposure, means: x}\n",
		"a depth past four":       "settings:\n  changes:\n    leans_on: [depends_on]\n    depth: 9\n",
		"a watch that is no time": "settings:\n  changes:\n    kinds:\n      - {name: halts, family: disruption, means: x, watch: soon}\n",
	} {
		if rec := put(bad); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	exported := h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/config", nil), shared))
	back, err := config.Read(strings.NewReader(exported.Body.String()))
	if err != nil || back.Settings.Changes == nil || back.Settings.Changes.Kinds[0].Watch != "45m0s" || back.Settings.Changes.LeansOn[1] != "<-azure_resource:service" {
		t.Fatalf("export %v %s", err, exported.Body.String())
	}
	if rec := put(exported.Body.String()); rec.Code != 200 {
		t.Errorf("reapply %d %s", rec.Code, rec.Body.String())
	}
}

func TestConfigCarriesTheWatchAndChecksItWhole(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	put := func(body string) *httptest.ResponseRecorder {
		return h.do(h.api, bearer(httptest.NewRequest(http.MethodPut, "/v1/config", strings.NewReader(body)), shared))
	}
	doc := `settings:
  grouping_minutes: 15
  changes:
    watch:
      stores:
        prom: {kind: prometheus, url: "http://prometheus.example.org:9090"}
      queries:
        - blueprint: k8s_workload
          signal: restarts
          kinds: [stops]
          store: prom
          query: 'sum(increase(restarts{namespace="{namespace.title}"}[{window}]))'
          above: 0
`
	if rec := put(doc); rec.Code != 200 {
		t.Fatalf("apply %d %s", rec.Code, rec.Body.String())
	}
	if st, _ := h.desk.Store.Settings(ctx); len(st.Watch.Queries) != 1 || st.Watch.Stores["prom"].URL != "http://prometheus.example.org:9090" {
		t.Fatalf("settings %+v", st.Watch)
	}
	bad := strings.Replace(doc, "store: prom", "store: loki", 1)
	if rec := put(bad); rec.Code != 400 || !strings.Contains(rec.Body.String(), "no store") {
		t.Errorf("a query on a store that is not there: %d %s", rec.Code, rec.Body.String())
	}
	if st, _ := h.desk.Store.Settings(ctx); st.Watch.Queries[0].Store != "prom" {
		t.Error("a refused document changed the watch")
	}
	exported := h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/config", nil), shared))
	back, err := config.Read(strings.NewReader(exported.Body.String()))
	if err != nil || back.Settings.Changes == nil || back.Settings.Changes.Watch == nil || back.Settings.Changes.Watch.Queries[0].Signal != "restarts" {
		t.Fatalf("export %v %s", err, exported.Body.String())
	}
	if rec := put(exported.Body.String()); rec.Code != 200 {
		t.Errorf("reapply %d %s", rec.Code, rec.Body.String())
	}
}
