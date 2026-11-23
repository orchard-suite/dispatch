package server

import (
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
)

func TestFoldTimelineRunsCloseAlertChanges(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ev := func(min int, kind incident.EventKind, text string) incident.Event {
		return incident.Event{At: at.Add(time.Duration(min) * time.Minute), Kind: kind, Text: text}
	}
	got := foldTimeline([]incident.Event{
		ev(0, incident.EventOpened, "Opened"),
		ev(1, incident.EventAlertFiring, "KubeJobFailed firing"),
		ev(10, incident.EventAlertResolved, "KubeJobFailed resolved"),
		ev(20, incident.EventAlertFiring, "KubeJobFailed firing"),
		ev(25, incident.EventNote, "looking"),
		ev(26, incident.EventAlertResolved, "KubeJobFailed resolved"),
		ev(300, incident.EventAlertFiring, "KubeJobFailed firing"),
	})
	if len(got) != 5 {
		t.Fatalf("got %d rows, want 5: %+v", len(got), got)
	}
	r := got[1].Run
	if r == nil || r.Fired != 2 || r.Cleared != 1 || r.Alert != "KubeJobFailed" || r.Span() != 19*time.Minute {
		t.Fatalf("first run = %+v", r)
	}
	if got[2].Run != nil || got[2].Event.Kind != incident.EventNote {
		t.Fatalf("a note breaks a run: %+v", got[2])
	}
	if got[3].Run == nil || got[3].Run.Changes() != 1 {
		t.Fatalf("a change after a note starts a run: %+v", got[3])
	}
	if got[4].Run == nil || got[4].Run.Fired != 1 {
		t.Fatalf("a change past the gap starts a run: %+v", got[4])
	}
}

func TestGroupAlertsFoldsByNameFiringFirst(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	ended := at.Add(time.Hour)
	got := groupAlerts([]incident.Alert{
		{Name: "KubeJobFailed", Status: "resolved", Labels: map[string]string{"alertname": "KubeJobFailed", "job": "a", "ns": "x"}, StartsAt: at, EndsAt: &ended},
		{Name: "KubeJobFailed", Status: "resolved", Labels: map[string]string{"alertname": "KubeJobFailed", "job": "b", "ns": "x"}, StartsAt: at},
		{Name: "CPUThrottlingHigh", Status: "firing", Labels: map[string]string{"alertname": "CPUThrottlingHigh"}, StartsAt: at, Annotations: map[string]string{"summary": "hot"}},
	})
	if len(got) != 2 || got[0].Name != "CPUThrottlingHigh" || got[0].Summary != "hot" {
		t.Fatalf("firing group first: %+v", got)
	}
	g := got[1]
	if g.Count() != 2 || g.Labels["ns"] != "x" || g.Labels["job"] != "" || len(g.Varying) != 1 || g.Varying[0] != "job" {
		t.Fatalf("shared and varying labels: %+v", g)
	}
	if _, ok := g.Labels["alertname"]; ok {
		t.Fatalf("alertname is the group's name, not a label: %+v", g.Labels)
	}
}

func TestIncidentRibbonDrawsBurstsAndMoves(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	resolved := at.Add(100 * time.Minute)
	i := incident.Incident{OpenedAt: at, ResolvedAt: &resolved}
	ev := func(min int, kind incident.EventKind) incident.Event {
		return incident.Event{At: at.Add(time.Duration(min) * time.Minute), Kind: kind, Text: "A firing"}
	}
	r := incidentRibbon(i, []incident.Event{
		ev(0, incident.EventOpened),
		ev(5, incident.EventAlertFiring),
		ev(10, incident.EventAlertResolved),
		ev(20, incident.EventAlertResolved),
		ev(30, incident.EventAcknowledged),
		ev(70, incident.EventAlertResolved),
		ev(100, incident.EventResolved),
	}, time.Now())
	if len(r.Bursts) != 2 || r.Bursts[0].Left != 5 || r.Bursts[0].Width != 15 || !r.Bursts[0].Fired || r.Bursts[1].Left != 70 || r.Bursts[1].Fired {
		t.Fatalf("bursts: %+v", r.Bursts)
	}
	if len(r.Marks) != 2 || r.Marks[0].Left != 30 || r.Marks[1].Kind != incident.EventResolved {
		t.Fatalf("marks: %+v", r.Marks)
	}
}

func TestRibbonPlacesChanges(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r := Ribbon{From: at, To: at.Add(100 * time.Minute)}.withChanges([]incident.Change{
		{Title: "deploy", At: at.Add(-30 * time.Minute)},
		{Title: "flag", At: at.Add(25 * time.Minute)},
	})
	if len(r.Changes) != 2 || r.Changes[0].Left != 0 || r.Changes[1].Left != 25 {
		t.Fatalf("changes: %+v", r.Changes)
	}
}

func TestFoldTimelineSaysWhenALoneClearFollowsTheOpening(t *testing.T) {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	got := foldTimeline([]incident.Event{
		{At: at, Kind: incident.EventOpened, Text: "Opened"},
		{At: at.Add(12 * time.Minute), Kind: incident.EventAlertResolved, Text: "A resolved"},
	})
	if len(got) != 2 || got[1].Run == nil || got[1].Run.After != 12*time.Minute {
		t.Fatalf("got %+v", got)
	}
}

func TestBylineAndSpan(t *testing.T) {
	if got := byline(incident.Event{Actor: "a@example.com", Text: "Resolved by a@example.com"}); got != "" {
		t.Fatalf("a line that names its actor needs no byline: %q", got)
	}
	if got := byline(incident.Event{Actor: "a@example.com", Text: "looking"}); got != "a@example.com" {
		t.Fatalf("got %q", got)
	}
	at := time.Date(time.Now().Year(), 11, 4, 14, 33, 0, 0, time.UTC)
	if got := span(at, at.Add(4*time.Hour+14*time.Minute)); got != "Nov 4, 14:33 to 18:47" {
		t.Fatalf("got %q", got)
	}
}
