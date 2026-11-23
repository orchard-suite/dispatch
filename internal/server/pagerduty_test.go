package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

func TestThePagerDutyDoorOpensAcknowledgesAndResolves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, key, err := h.desk.Store.MintToken(ctx, "grafana", admin)
	if err != nil {
		t.Fatal(err)
	}
	send := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := h.do(h.api, httptest.NewRequest(http.MethodPost, "/v2/enqueue", strings.NewReader(body)))
		var got map[string]any
		json.Unmarshal(rec.Body.Bytes(), &got)
		return rec, got
	}
	rec, got := send(`{"routing_key":"` + key + `","event_action":"trigger","dedup_key":"disk-1","payload":{"summary":"Disk 95% full on db-1","source":"db-1","severity":"error","component":"postgres","group":"shop","class":"DiskFull","custom_details":{"mount":"/data"}}}`)
	if rec.Code != http.StatusAccepted || got["status"] != "success" || got["dedup_key"] != "disk-1" {
		t.Fatalf("trigger %d %v", rec.Code, got)
	}
	list, _ := h.desk.Store.List(ctx, store.Filter{})
	if len(list) != 1 || list[0].Service != "shop" || list[0].Severity != incident.Critical || list[0].Title != "Disk 95% full on db-1" {
		t.Fatalf("an error pages and opens an incident: %+v", list)
	}
	if rec, got := send(`{"routing_key":"wrong","event_action":"trigger","payload":{"summary":"x","source":"y","severity":"info"}}`); rec.Code != 400 || got["status"] != "invalid event" {
		t.Errorf("a key this desk did not issue: %d %v", rec.Code, got)
	}
	if rec, _ := send(`{"routing_key":"` + key + `","event_action":"trigger","payload":{"summary":"x","source":"y","severity":"loud"}}`); rec.Code != 400 {
		t.Errorf("a bad severity: %d", rec.Code)
	}
	if rec, _ := send(`{"routing_key":"` + key + `","event_action":"acknowledge","dedup_key":"disk-1"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("acknowledge %d", rec.Code)
	}
	events, _ := h.desk.Store.Events(ctx, list[0].ID)
	if last := events[len(events)-1]; !strings.Contains(last.Text, "Acknowledged at its source by grafana") {
		t.Errorf("timeline %+v", last)
	}
	if rec, _ := send(`{"routing_key":"` + key + `","event_action":"resolve","dedup_key":"disk-1"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("resolve %d", rec.Code)
	}
	inc, _ := h.desk.Store.Get(ctx, h.desk.Store.DB(), list[0].ID)
	if inc.State != incident.Open || inc.FiringAlerts != 0 {
		t.Errorf("the alert resolves, not the incident: %+v", inc)
	}
	rec, got = send(`{"routing_key":"` + key + `","event_action":"trigger","payload":{"summary":"Queue slow","source":"worker","severity":"warning"}}`)
	if rec.Code != http.StatusAccepted || got["dedup_key"] == "" {
		t.Errorf("a trigger without a dedup key gets one: %v", got)
	}
	if list, _ := h.desk.Store.List(ctx, store.Filter{}); len(list) != 1 {
		t.Errorf("a warning opens nothing: %d incidents", len(list))
	}
}

func TestAHeartbeatChecksInOverTheAPI(t *testing.T) {
	h := newHarness(t)
	rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/heartbeats/nightly?interval=24h", nil), shared))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"nightly"`) {
		t.Fatalf("beat %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/heartbeats/other", nil), shared)); rec.Code != 404 {
		t.Errorf("unknown without an interval: %d", rec.Code)
	}
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/changes", strings.NewReader(`{"service":"shop","title":"shop v2"}`)), shared))
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"producer":"alertmanager"`) {
		t.Errorf("change %d %s", rec.Code, rec.Body.String())
	}
}
