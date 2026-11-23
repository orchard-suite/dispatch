package server

import (
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

func TestStripsShowFiringsAndQuietOpenIncidents(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	resolved := now.Add(-46 * time.Hour)
	list := []incident.Incident{
		{ID: 1, Service: "noisy", Severity: incident.Warning, State: incident.Resolved, OpenedAt: now.Add(-48 * time.Hour), ResolvedAt: &resolved},
		{ID: 2, Service: "noisy", Severity: incident.Info, State: incident.Resolved, OpenedAt: now.Add(-48 * time.Hour), ResolvedAt: &resolved},
		{ID: 3, Service: "down", Severity: incident.Critical, State: incident.Open, OpenedAt: now.Add(-9 * time.Hour)},
		{ID: 4, Service: "quietly", Severity: incident.Info, State: incident.Resolved, OpenedAt: now.Add(-24 * time.Hour), ResolvedAt: &resolved},
		{ID: 5, Service: "burning", Severity: incident.Warning, State: incident.Open, OpenedAt: now.Add(-2 * time.Hour), FiringAlerts: 1},
	}
	firings := []store.Firing{
		{IncidentID: 1, At: now.Add(-48 * time.Hour)},
		{IncidentID: 2, At: now.Add(-47*time.Hour - 30*time.Minute)},
		{IncidentID: 1, At: now.Add(-47 * time.Hour)},
		{IncidentID: 3, At: now.Add(-9 * time.Hour)},
		{IncidentID: 5, At: now.Add(-time.Hour)},
		{IncidentID: 9, At: now.Add(-time.Hour)},
	}
	lanes, lives := strips(list, firings, now)
	if len(lanes) != 4 || lanes[0].Service != "down" || lanes[1].Service != "burning" || lanes[2].Service != "noisy" || lanes[3].Service != "quietly" {
		t.Fatalf("open first, worst first, then busiest: %+v", names(lanes))
	}
	down := lanes[0].Cells
	if down[stripCells-3].Class() != "critical" || down[stripCells-2].Class() != "quiet" || down[stripCells-1].Class() != "quiet" || down[stripCells-4].Class() != "" {
		t.Fatalf("an open incident fires once, then sits quiet: %v %v %v %v", down[stripCells-4], down[stripCells-3], down[stripCells-2], down[stripCells-1])
	}
	noisy := lanes[2]
	if noisy.Firings != 3 || noisy.Restore != 2*time.Hour {
		t.Fatalf("noisy: %+v", noisy)
	}
	hit := false
	for _, c := range noisy.Cells {
		if c.Fired == incident.Warning && c.Count == 3 {
			hit = true
		}
		if c.Quiet {
			t.Fatalf("a resolved incident leaves no quiet cells: %+v", c)
		}
	}
	if !hit {
		t.Fatalf("three firings in a stretch, the worst colors it: %+v", noisy.Cells)
	}
	for _, c := range lanes[3].Cells {
		if c.Class() != "" {
			t.Fatalf("an incident that never fired in the week leaves its lane empty: %+v", c)
		}
	}
	if len(lives) != 2 || lives[0].ID != 5 || lives[1].ID != 3 || lives[0].Firings != 1 {
		t.Fatalf("still firing first: %+v", lives)
	}
}

func names(lanes []Lane) []string {
	out := []string{}
	for _, l := range lanes {
		out = append(out, l.Service)
	}
	return out
}

func TestQueueSplitsByWhoIsOnIt(t *testing.T) {
	q := queue([]incident.Incident{
		{ID: 1, State: incident.Open},
		{ID: 2, State: incident.Acknowledged, Owner: "a@example.com"},
		{ID: 3, State: incident.Mitigated, Owner: "a@example.com"},
		{ID: 4, State: incident.Open, Owner: "b@example.com"},
	})
	if len(q.Nobody) != 1 || len(q.Working) != 2 || len(q.Watching) != 1 {
		t.Fatalf("queue: %+v", q)
	}
}

func TestDayBarsKeepEmptyDays(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	acked := now
	bars := dayBars([]incident.Incident{
		{Severity: incident.Critical, OpenedAt: now.Add(-49 * time.Hour), AcknowledgedAt: &acked},
		{Severity: incident.Info, OpenedAt: now.Add(-49 * time.Hour)},
		{Severity: incident.Warning, OpenedAt: now},
		{Severity: incident.Warning, OpenedAt: now.Add(-30 * 24 * time.Hour)},
	}, now.Add(-3*24*time.Hour), now)
	if len(bars) != 4 || bars[1].Total() != 2 || bars[1].Critical != 1 || bars[1].Acknowledged != 1 || bars[2].Total() != 0 || bars[3].Warning != 1 {
		t.Fatalf("bars: %+v", bars)
	}
}

func TestByDayFoldsRepeats(t *testing.T) {
	now := time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)
	r1, r2 := now.Add(-9*time.Hour), now.Add(-7*time.Hour)
	days := byDay([]incident.Incident{
		{ID: 1, Title: "CPU throttling", Service: "kubelet", Severity: incident.Info, OpenedAt: now.Add(-10 * time.Hour), ResolvedAt: &r1},
		{ID: 2, Title: "CPU throttling", Service: "kubelet", Severity: incident.Info, OpenedAt: now.Add(-8 * time.Hour), ResolvedAt: &r2},
		{ID: 3, Title: "CPU throttling", Service: "other", Severity: incident.Warning, OpenedAt: now.Add(-6 * time.Hour)},
		{ID: 4, Title: "Disk full", Service: "nas", Severity: incident.Critical, OpenedAt: now.Add(-30 * time.Hour)},
	}, now)
	if len(days) != 2 || days[0].Label != "Today" || days[0].Count != 3 || len(days[0].Groups) != 2 || days[0].Info != 2 || days[0].Warning != 1 {
		t.Fatalf("days: %+v", days)
	}
	g := days[0].Groups[1]
	if g.Latest.ID != 2 || g.Count != 2 || g.Restored != time.Hour || !g.First.Equal(now.Add(-10*time.Hour)) {
		t.Fatalf("folded repeat: %+v", g)
	}
	if days[1].Label != "Yesterday" || days[1].Groups[0].Latest.ID != 4 {
		t.Fatalf("yesterday: %+v", days[1])
	}
}

func TestNoiseRowsCountFiringsPerAlert(t *testing.T) {
	at := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	rows := noiseRows([]store.Noise{{Alert: "KubeJobFailed", Fired: 6, Paged: 4, Acted: 1}, {Alert: "Quiet", Fired: 2, Paged: 2}}, []store.Firing{
		{Kind: incident.EventOpened, Text: "Opened from Alertmanager: KubeJobFailed firing on jobs", At: at},
		{Kind: incident.EventAlertFiring, Text: "KubeJobFailed firing", At: at},
		{Kind: incident.EventAlertFiring, Text: "KubeJobFailed firing", At: at},
		{Kind: incident.EventAlertFiring, Text: "Fresh firing", At: at},
		{Kind: incident.EventReopened, Text: "Reopened by a person", At: at},
	})
	if len(rows) != 3 || rows[0].Alert != "KubeJobFailed" || rows[0].Firings != 4 || rows[0].Incidents != 4 || rows[0].Acted != 1 {
		t.Fatalf("rows: %+v", rows)
	}
	if rows[1].Alert != "Quiet" || rows[1].Firings != 2 {
		t.Fatalf("an alert's incidents are at least its firings: %+v", rows[1])
	}
}

func TestDayTicksSitOnDayMiddles(t *testing.T) {
	first := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	ticks := dayTicks(first, 8)
	if len(ticks) != 5 || ticks[0].Label != "Oct 31" || ticks[0].Left != 6.25 || ticks[4].Label != "today" || ticks[4].Left != 93.75 {
		t.Fatalf("ticks: %+v", ticks)
	}
	if got := dayTicks(first, 2); len(got) != 2 {
		t.Fatalf("two days, two ticks: %+v", got)
	}
}

func TestOpenMixNamesSeverity(t *testing.T) {
	crit := incident.Incident{Severity: incident.Critical}
	warn := incident.Incident{Severity: incident.Warning}
	if got := openMix([]incident.Incident{crit, crit}); got != "2 critical" {
		t.Fatalf("got %q", got)
	}
	if got := openMix([]incident.Incident{crit, warn, crit}); got != "3 (2 critical, 1 warning)" {
		t.Fatalf("got %q", got)
	}
}
