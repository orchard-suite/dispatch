package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
)

func TestSummaryLeadsWithTheNewestWrittenOne(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	opened := incident.Event{At: at, Kind: incident.EventOpened, Actor: "a@example.com", Text: "Opened by a@example.com: the shop is down"}
	update := incident.Event{At: at.Add(time.Hour), Kind: incident.EventNote, Actor: "a@example.com", Text: "We are rolling back", Public: true}
	written := incident.Event{At: at.Add(2 * time.Hour), Kind: incident.EventNote, Actor: "b@example.com", Text: summaryPrefix + "The disk filled."}
	groups := []AlertGroup{{Name: "DiskFull", Summary: "Disk is 99% full"}}
	cases := []struct {
		events []incident.Event
		text   string
		from   string
	}{
		{[]incident.Event{opened, update, written}, "The disk filled.", "summary"},
		{[]incident.Event{opened, update}, "We are rolling back", "status update"},
		{[]incident.Event{opened}, "the shop is down", "description"},
		{nil, "Disk is 99% full", "alert"},
	}
	for _, c := range cases {
		got := summaryOf(incident.Incident{Title: "Shop down"}, c.events, groups)
		if got.Text != c.text || got.From != c.from {
			t.Errorf("summary = %+v, want %q from %s", got, c.text, c.from)
		}
	}
	if got := summaryOf(incident.Incident{Title: "Disk is 99% full"}, nil, groups); got.Text != "" {
		t.Errorf("an alert summary that repeats the title is not a lead: %+v", got)
	}
}

func TestActivityGroupsByDayMarksQuietAndHighlights(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	events := []incident.Event{
		{At: at, Kind: incident.EventOpened, Actor: "dispatch", Text: "Opened from Alertmanager"},
		{At: at.Add(time.Minute), Kind: incident.EventAlertFiring, Text: "DiskFull firing"},
		{At: at.Add(5 * time.Minute), Kind: incident.EventAlertResolved, Text: "DiskFull resolved"},
		{At: at.Add(6*time.Hour + 5*time.Minute), Kind: incident.EventNote, Actor: "a@example.com", Text: "Looking at it"},
		{At: at.Add(50 * time.Hour), Kind: incident.EventAcknowledged, Actor: "a@example.com", Text: "Acknowledged by a@example.com"},
	}
	items, hi := activityOf(foldTimeline(events))
	var lines []string
	for _, it := range items {
		switch {
		case it.Quiet != "":
			lines = append(lines, "gap "+it.Quiet)
		case it.Entry != nil:
			kind := string(it.Entry.Event.Kind)
			if it.Entry.Run != nil {
				kind = "run"
			}
			if it.Hi {
				kind += "*"
			}
			if it.Written {
				kind += " written"
			}
			lines = append(lines, kind)
		default:
			lines = append(lines, it.Day.Format("Jan 2")+" "+string(rune('0'+it.Count)))
		}
	}
	want := []string{"Nov 3 1", "acknowledged*", "gap 1d 19h quiet", "Nov 1 3", "note* written", "gap 6h quiet", "run", "opened*"}
	if strings.Join(lines, " | ") != strings.Join(want, " | ") {
		t.Errorf("activity:\n got %q\nwant %q", lines, want)
	}
	if hi != 3 {
		t.Errorf("highlights = %d, want 3", hi)
	}
}

func TestPhasesAndMoves(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ack := at.Add(time.Hour)
	inc := incident.Incident{State: incident.Mitigated, OpenedAt: at, AcknowledgedAt: &ack, Alerts: 1}
	p := phasesOf(inc)
	if p[0].Class != "done" || p[1].At == nil || p[2].Class != "now" || p[3].Class != "" {
		t.Errorf("phases: %+v", p)
	}
	skipped := phasesOf(incident.Incident{State: incident.Resolved, OpenedAt: at, ResolvedAt: &ack})
	if !skipped[1].Skipped || !skipped[2].Skipped || skipped[3].Class != "now" {
		t.Errorf("a straight resolve skips the middle: %+v", skipped)
	}
	m := movesOf(incident.Incident{State: incident.Open, FiringAlerts: 2, Alerts: 2})
	if len(m) != 3 || !m[0].Primary || m[0].Key != "A" || m[0].Label != "Acknowledge" {
		t.Errorf("open moves: %+v", m)
	}
	if m := movesOf(incident.Incident{State: incident.Resolved}); len(m) != 1 || m[0].Key != "O" || m[0].Primary {
		t.Errorf("resolved moves: %+v", m)
	}
}

func TestIncidentPageFrameAndTabs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inc, err := h.desk.Open(ctx, desk.OpenRequest{Title: "Checkout failing", Severity: "critical", Service: "shop", Description: "Payments time out"}, incident.SourceDesk, incident.Person(member))
	if err != nil {
		t.Fatal(err)
	}
	id := itoa(inc.ID)
	get := func(path string) string {
		rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, path, nil), member, "User"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		return h.do(h.session, asPerson(form(path, v), member, "User"))
	}
	page := get("/incidents/" + id)
	for _, want := range []string{"Payments time out", `data-filter="tl"`, `id="tl-body" data-live`, `id="props" data-live`, `data-key="M"`, `data-cmd="More actions"`, "Next move"} {
		if !strings.Contains(page, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	// An address may ride in a picker's value, never in what a person reads.
	if strings.Contains(regexp.MustCompile(`value="[^"]*"`).ReplaceAllString(page, ""), member) {
		t.Errorf("overview prints an address")
	}
	// A summary written with Edit leads the page.
	if rec := post("/incidents/"+id+"/summary", url.Values{"text": {"The card processor is slow."}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("summary: %d", rec.Code)
	}
	if rec := post("/incidents/"+id+"/summary", url.Values{"text": {" "}}); rec.Code != http.StatusBadRequest {
		t.Errorf("an empty summary: %d", rec.Code)
	}
	if page := get("/incidents/" + id); !strings.Contains(page, "The card processor is slow.") || !strings.Contains(page, ">by Member") {
		t.Errorf("the written summary does not lead")
	}
	// The postmortem tab keeps its editor and action items, and its forms come back to it.
	pm := get("/incidents/" + id + "/postmortem")
	for _, want := range []string{`aria-current="page">Postmortem`, `name="body"`, `action="/incidents/` + id + `/actions"`, `name="back" value="postmortem"`} {
		if !strings.Contains(pm, want) {
			t.Errorf("postmortem tab lacks %q", want)
		}
	}
	if rec := post("/incidents/"+id+"/actions", url.Values{"text": {"Add a canary"}, "back": {"postmortem"}}); rec.Header().Get("Location") != "/incidents/"+id+"/postmortem" {
		t.Errorf("an action item from the tab goes to %q", rec.Header().Get("Location"))
	}
	if rec := post("/incidents/"+id+"/postmortem", url.Values{"body": {"## What happened"}}); rec.Header().Get("Location") != "/incidents/"+id+"/postmortem?saved" {
		t.Errorf("a saved postmortem goes to %q", rec.Header().Get("Location"))
	}
}

func TestStripShowsFiringFromTheOpeningToNow(t *testing.T) {
	at := time.Date(2026, 11, 8, 22, 0, 0, 0, time.UTC)
	now := at.Add(22 * time.Minute)
	inc := incident.Incident{State: incident.Open, OpenedAt: at, FiringAlerts: 1, Alerts: 1}
	events := []incident.Event{{At: at, Kind: incident.EventOpened, Text: "Opened from Alertmanager"}}
	alerts := []incident.Alert{{Name: "DiskFull", Status: "firing", StartsAt: at.Add(-time.Minute)}}
	r := stripOf(inc, events, alerts, nil, now)
	if len(r.Bursts) != 1 || !r.Bursts[0].Fired || r.Bursts[0].Left != 0 || r.Bursts[0].Width != 100 {
		t.Errorf("a fresh firing incident fires across the strip: %+v", r.Bursts)
	}
	if ticks := ribbonTicks(inc, now); len(ticks) < 2 {
		t.Errorf("a 22 minute strip has minute ticks: %+v", ticks)
	}
	// The alert that opened it fired until its first clear.
	events = append(events, incident.Event{At: at.Add(10 * time.Minute), Kind: incident.EventAlertResolved, Text: "DiskFull resolved"})
	inc.FiringAlerts = 0
	r = stripOf(inc, events, nil, nil, now)
	if len(r.Bursts) != 1 || !r.Bursts[0].Fired || r.Bursts[0].Left != 0 || r.Bursts[0].Width < 45 {
		t.Errorf("the opening firing runs to its clear: %+v", r.Bursts)
	}
}

func TestBreakableBreaksOnlyBetweenWords(t *testing.T) {
	got := string(breakable("AlertmanagerClusterFailed|monitoring-kube_state.x<y"))
	want := "Alertmanager<wbr>Cluster<wbr>Failed|<wbr>monitoring-<wbr>kube_<wbr>state.<wbr>x&lt;y"
	if got != want {
		t.Errorf("breakable = %q, want %q", got, want)
	}
}

func TestAlertsDifferNamesTheLabelThatVaries(t *testing.T) {
	job := func(name, j string) incident.Alert {
		return incident.Alert{Name: name, Status: "firing", Labels: map[string]string{"alertname": name, "namespace": "orrery", "job_name": j}}
	}
	alerts := []incident.Alert{job("KubeJobFailed", "push-4"), job("KubeJobFailed", "push-1"), job("KubeJobFailed", "push-3"), job("KubeJobFailed", "push-2"), job("KubeJobFailed", "push-1")}
	if got, want := alertsDiffer(alerts), "5 KubeJobFailed alerts, differing by job_name: push-1, push-2, push-3 and 1 more"; got != want {
		t.Errorf("alertsDiffer = %q, want %q", got, want)
	}
	if got := alertsDiffer(alerts[:1]); got != "" {
		t.Errorf("one alert differs from nothing: %q", got)
	}
	if got, want := alertsDiffer(append(alerts[:2], job("DiskFull", "x"))), "2 KubeJobFailed alerts, differing by job_name: push-1, push-4; and DiskFull"; got != want {
		t.Errorf("alertsDiffer = %q, want %q", got, want)
	}
}

func TestSummaryFallsBackToTheNewestNote(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	events := []incident.Event{
		{At: at, Kind: incident.EventOpened, Actor: "dispatch", Text: "Opened from Alertmanager: X firing on y"},
		{At: at.Add(time.Hour), Kind: incident.EventNote, Actor: "a@example.com", Text: "The pod is stuck"},
	}
	if got := summaryOf(incident.Incident{}, events, nil); got.Text != "The pod is stuck" || got.From != "note" {
		t.Errorf("summary = %+v", got)
	}
}

func TestNextMoveMatchesTheActionItems(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	done := at.Add(time.Hour)
	inc := incident.Incident{State: incident.Resolved, OpenedAt: at, ResolvedAt: &done}
	for _, c := range []struct {
		written bool
		actions []incident.ActionItem
		want    string
	}{
		{false, nil, "Write the postmortem"},
		{true, nil, "add the action items"},
		{true, []incident.ActionItem{{Done: true}, {}}, "last open action item"},
		{true, []incident.ActionItem{{Done: true}}, "every action item are done"},
	} {
		if got := nextMoveWith(inc, c.written, c.actions, done).Say; !strings.Contains(got, c.want) {
			t.Errorf("written %v, %d items: %q lacks %q", c.written, len(c.actions), got, c.want)
		}
	}
}

func TestStripLanesPerDifferingValueFoldPastEight(t *testing.T) {
	at := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
	now := at.Add(time.Hour)
	inc := incident.Incident{State: incident.Open, OpenedAt: at, FiringAlerts: 14, Alerts: 14}
	var alerts []incident.Alert
	for n := range 14 {
		alerts = append(alerts, incident.Alert{Name: "KubeJobFailed", Status: "firing", StartsAt: at.Add(time.Duration(n) * time.Minute),
			Labels: map[string]string{"alertname": "KubeJobFailed", "job_name": fmt.Sprintf("push-%02d", n)}})
	}
	v := incidentStrip(inc, nil, alerts, nil, now)
	if len(v.Lanes) != 8 || v.Lanes[0].Label != "push-00" || v.Lanes[7].Label != "+7 more" || v.Lanes[7].More != 7 || len(v.Lanes[7].Spans) != 7 {
		t.Fatalf("lanes: %+v", v.Lanes)
	}
	if v.Single {
		t.Error("fourteen alerts are not one line")
	}
}

func TestStripOneAlertFiringThroughoutIsOneLine(t *testing.T) {
	at := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
	inc := incident.Incident{State: incident.Open, OpenedAt: at, FiringAlerts: 1, Alerts: 1}
	alerts := []incident.Alert{{Name: "AnnalsStoreDown", Status: "firing", StartsAt: at}}
	v := incidentStrip(inc, []incident.Event{{At: at, Kind: incident.EventOpened}}, alerts, nil, at.Add(84*time.Minute))
	if !v.Single || !v.Since.Equal(at) || v.For != 84*time.Minute {
		t.Errorf("one line: %+v", v)
	}
	// One alert that flapped keeps its runs in its own lane.
	done := at.Add(3 * time.Hour)
	inc = incident.Incident{State: incident.Resolved, OpenedAt: at, ResolvedAt: &done, Alerts: 1}
	ended := at.Add(2 * time.Hour)
	events := []incident.Event{
		{At: at, Kind: incident.EventOpened, Text: "Opened from Alertmanager: X firing on y"},
		{At: at.Add(10 * time.Minute), Kind: incident.EventAlertResolved, Text: "X resolved"},
		{At: at.Add(time.Hour), Kind: incident.EventAlertFiring, Text: "X firing"},
		{At: at.Add(2 * time.Hour), Kind: incident.EventAlertResolved, Text: "X resolved"},
	}
	v = incidentStrip(inc, events, []incident.Alert{{Name: "X", Status: "resolved", StartsAt: at.Add(time.Hour), EndsAt: &ended}}, nil, done)
	if v.Single || len(v.Lanes) != 1 || len(v.Lanes[0].Spans) != 2 || v.Lanes[0].Spans[0].Left != 0 || v.Lanes[0].Spans[1].Width < 30 {
		t.Errorf("flapping lane: %+v", v.Lanes)
	}
}
