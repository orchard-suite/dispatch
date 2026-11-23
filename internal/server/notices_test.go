package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

type channelHook struct {
	mu     sync.Mutex
	bodies []string
}

func (c *channelHook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies = append(c.bodies, string(b))
	c.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (c *channelHook) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.bodies...)
}

func TestNotificationsEndToEnd(t *testing.T) {
	h := newHarness(t)
	hook := &channelHook{}
	teams := httptest.NewTLSServer(hook)
	defer teams.Close()
	box := h.desk.Notices()
	box.UseClient(teams.Client())

	if rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/notifications", nil), member, "User")); rec.Code != http.StatusForbidden {
		t.Fatalf("a member opened notifications: %d", rec.Code)
	}
	rec := h.do(h.session, asPerson(form("/notifications/channels", url.Values{"name": {"Beacon devops"}, "kind": {"teams"}, "url": {teams.URL + "/hook?sig=s3cret"}}), admin, "Admin"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Beacon devops") {
		t.Fatalf("add channel: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatal("the page shows the channel's secret URL")
	}
	rec = h.do(h.session, asPerson(form("/notifications/channels/1/test", nil), admin, "Admin"))
	if !strings.Contains(rec.Body.String(), "The test was delivered") || len(hook.got()) != 1 {
		t.Fatalf("test: %s", rec.Body.String())
	}
	rec = h.do(h.session, asPerson(form("/notifications/subscriptions", url.Values{"channel": {"1"}, "topic": {"action.awaiting_approval", "incident.opened"}, "match": {"env=Prod"}}), admin, "Admin"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Subscription 1 added") {
		t.Fatalf("add subscription: %d %s", rec.Code, rec.Body.String())
	}

	send := func(e map[string]any) (int, notice.Event, int) {
		raw, _ := json.Marshal(e)
		req := bearer(httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(string(raw))), shared)
		rec := h.do(h.api, req)
		var got struct {
			Event  notice.Event `json:"event"`
			Queued int          `json:"queued"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return rec.Code, got.Event, got.Queued
	}
	code, e, queued := send(map[string]any{"producer": "someone-else", "kind": "action.awaiting_approval", "key": "run-1", "title": "Restart waits for approval", "labels": map[string]string{"env": "Prod"}})
	if code != http.StatusAccepted || queued != 1 || e.Producer != "alertmanager" {
		t.Fatalf("event: %d queued %d producer %q", code, queued, e.Producer)
	}
	if code, _, queued := send(map[string]any{"kind": "action.awaiting_approval", "key": "run-2", "title": "In test", "labels": map[string]string{"env": "Test"}}); code != http.StatusAccepted || queued != 0 {
		t.Fatalf("a test-environment event was queued: %d %d", code, queued)
	}
	if code, _, _ := send(map[string]any{"kind": "Bad Kind", "key": "x", "title": "x"}); code != http.StatusBadRequest {
		t.Fatalf("a bad event: %d", code)
	}
	box.SendDue(context.Background())
	if n := len(hook.got()); n != 2 {
		t.Fatalf("channel got %d messages, want the test and the approval", n)
	}

	// The desk's own incidents go through the same subscriptions.
	h.do(h.session, asPerson(form("/notifications/subscriptions", url.Values{"channel": {"1"}, "topic": {"incident.opened"}}), admin, "Admin"))
	open := bearer(httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(`{"title":"Checkout failing","severity":"critical","service":"shop"}`)), shared)
	if rec := h.do(h.api, open); rec.Code != http.StatusCreated {
		t.Fatalf("open: %d", rec.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(hook.got()) < 3 && time.Now().Before(deadline) {
		box.SendDue(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	bodies := hook.got()
	if len(bodies) != 3 || !strings.Contains(bodies[2], "Checkout failing") {
		t.Fatalf("the incident was not announced: %d messages", len(bodies))
	}
	page := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/notifications", nil), admin, "Admin")).Body.String()
	if !strings.Contains(page, "#1 Checkout failing") || !strings.Contains(page, "Delivered") || !strings.Contains(page, "code 202") {
		t.Errorf("the deliveries list is missing the incident: %s", page)
	}
}
