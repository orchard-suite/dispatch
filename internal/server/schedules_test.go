package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSchedulesCoversAndTheCalendarFeed(t *testing.T) {
	h := newHarness(t)
	start := time.Now().UTC().Add(-time.Hour).Format("2006-01-02 15:04")
	rec := h.do(h.session, asPerson(form("/schedules", url.Values{"slug": {"platform"}, "name": {"Platform"}, "zone": {"UTC"},
		"layers": {"weekly | " + member + ", " + admin + " | " + start + " | 1w |"}}), admin, "Admin"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/schedules?done=saved#platform" {
		t.Fatalf("save %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if page := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/schedules?done=saved", nil), admin, "Admin")).Body.String(); !strings.Contains(page, "Schedule saved.") || !strings.Contains(page, "data-flash") {
		t.Errorf("the page does not say it saved")
	}
	if rec := h.do(h.session, asPerson(form("/schedules", url.Values{"slug": {"x"}, "name": {"X"}, "zone": {"UTC"}, "layers": {"a | " + member + " | " + start + " | 1w |"}}), member, "User")); rec.Code != http.StatusForbidden {
		t.Errorf("a member changed a schedule: %d", rec.Code)
	}
	if rec := h.do(h.session, asPerson(form("/schedules", url.Values{"slug": {"bad"}, "name": {"Bad"}, "zone": {"UTC"}, "layers": {"a | " + member + " | soon | 1w |"}}), admin, "Admin")); rec.Code != 400 {
		t.Errorf("a bad start: %d", rec.Code)
	}
	var oncall []map[string]any
	json.Unmarshal(h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/schedules", nil), shared)).Body.Bytes(), &oncall)
	if len(oncall) != 1 || oncall[0]["person"] != member {
		t.Fatalf("on call %v", oncall)
	}
	from, to := time.Now().UTC().Format("2006-01-02T15:04"), time.Now().UTC().Add(2*time.Hour).Format("2006-01-02T15:04")
	if rec := h.do(h.session, asPerson(form("/schedules/platform/covers", url.Values{"person": {admin}, "from": {from}, "to": {to}, "tz_offset": {"0"}}), member, "User")); rec.Code != http.StatusSeeOther {
		t.Fatalf("cover %d %s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/schedules", nil), shared)).Body.Bytes(), &oncall)
	if oncall[0]["person"] != admin {
		t.Errorf("the cover holds: %v", oncall)
	}
	page := h.do(h.session, asPerson(form("/me/calendar", url.Values{}), admin, "Admin"))
	link := regexp.MustCompile(`/v1/calendar/(cal_[0-9a-f]+)\.ics`).FindStringSubmatch(page.Body.String())
	if link == nil {
		t.Fatalf("no calendar link: %s", page.Body.String())
	}
	feed := h.do(h.api, httptest.NewRequest(http.MethodGet, "/v1/calendar/"+link[1]+".ics", nil))
	if feed.Code != 200 || !strings.Contains(feed.Body.String(), "SUMMARY:On call: Platform") || !strings.HasPrefix(feed.Body.String(), "BEGIN:VCALENDAR") {
		t.Errorf("feed %d %s", feed.Code, feed.Body.String())
	}
	if rec := h.do(h.api, httptest.NewRequest(http.MethodGet, "/v1/calendar/cal_nope.ics", nil)); rec.Code != 404 {
		t.Errorf("a made-up link: %d", rec.Code)
	}
}

func TestSchedulesSaveLayersAsRows(t *testing.T) {
	h := newHarness(t)
	start := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04")
	save := func(v url.Values) *httptest.ResponseRecorder {
		v.Set("rows", "1")
		return h.do(h.session, asPerson(form("/schedules", v), admin, "Admin"))
	}
	// Two layers, the first with two members in turn order, and a blank row.
	rec := save(url.Values{"slug": {"platform"}, "name": {"Platform"}, "zone": {"UTC"},
		"l0_name": {"weekly"}, "l0_member": {member, admin, ""}, "l0_start": {start}, "l0_turn": {"1w"},
		"l1_name": {"nights"}, "l1_member": {""}, "l1_new": {"night@example.com"}, "l1_start": {start}, "l1_turn": {"1d"}, "l1_from": {"22:00"}, "l1_to": {"06:00"}, "l1_day": {"1", "2"},
		"l2_name": {""}, "l2_member": {""}, "l2_start": {start}, "l2_turn": {"1w"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save %d %s", rec.Code, rec.Body.String())
	}
	sc, err := h.desk.Store.Schedule(context.Background(), "platform", time.Now())
	if err != nil || len(sc.Layers) != 2 || strings.Join(sc.Layers[0].Members, ",") != member+","+admin || sc.Layers[1].Members[0] != "night@example.com" || sc.Layers[1].Hours.String() != "22:00-06:00 mon,tue" {
		t.Fatalf("saved %+v %v", sc, err)
	}
	// The page shows the rows with names in the picks, never an address.
	page := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/schedules", nil), admin, "Admin")).Body.String()
	for _, want := range []string{`name="l0_kept"`, `name="l1_drop"`, `name="l2_name"`, "only 22:00 to 06:00 on Mon, Tue", ">" + personName(member) + "<"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	if strings.Contains(page, ">"+member+"<") {
		t.Errorf("the page shows an address")
	}
	// Removing a layer keeps the rest; a kept layer left with nobody is refused.
	if rec := save(url.Values{"slug": {"platform"}, "name": {"Platform"}, "zone": {"UTC"},
		"l0_kept": {"1"}, "l0_name": {"weekly"}, "l0_member": {member}, "l0_start": {start}, "l0_turn": {"1w"},
		"l1_kept": {"1"}, "l1_drop": {"on"}, "l1_name": {"nights"}, "l1_member": {"night@example.com"}, "l1_start": {start}, "l1_turn": {"1d"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("drop %d %s", rec.Code, rec.Body.String())
	}
	if sc, _ := h.desk.Store.Schedule(context.Background(), "platform", time.Now()); len(sc.Layers) != 1 || len(sc.Layers[0].Members) != 1 {
		t.Errorf("after the drop %+v", sc.Layers)
	}
	// A new schedule takes its slug from its name, and a slug is never a label.
	if rec := save(url.Values{"name": {"Night Watch"}, "zone": {"UTC"}, "l0_name": {"nights"}, "l0_member": {member}, "l0_start": {start}, "l0_turn": {"1d"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("new %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.desk.Store.Schedule(context.Background(), "night-watch", time.Now()); err != nil {
		t.Errorf("slug from the name: %v", err)
	}
	if page := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/schedules", nil), admin, "Admin")).Body.String(); strings.Contains(page, "schedule:night-watch") || strings.Contains(page, `name="zone" required value`) {
		t.Errorf("the page shows a slug or a bare zone box")
	}
	if rec := save(url.Values{"slug": {"platform"}, "name": {"Platform"}, "zone": {"UTC"},
		"l0_kept": {"1"}, "l0_name": {"weekly"}, "l0_member": {""}, "l0_start": {start}, "l0_turn": {"1w"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "data-message") {
		t.Errorf("an empty kept layer: %d", rec.Code)
	}
}
