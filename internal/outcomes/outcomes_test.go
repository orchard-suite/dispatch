package outcomes

import (
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
)

func at(base time.Time, m int) *time.Time {
	t := base.Add(time.Duration(m) * time.Minute)
	return &t
}

func TestBuild(t *testing.T) {
	until := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	since := until.Add(-30 * 24 * time.Hour)
	early := since.Add(24 * time.Hour)
	late := until.Add(-24 * time.Hour)
	incidents := []incident.Incident{
		// web: two resolved, one acknowledged late in the window; one open.
		{Service: "web", Severity: incident.Critical, State: incident.Resolved, OpenedAt: early, AcknowledgedAt: at(early, 5), ResolvedAt: at(early, 60)},
		{Service: "web", Severity: incident.Warning, State: incident.Resolved, OpenedAt: early.Add(time.Hour), AcknowledgedAt: at(early.Add(time.Hour), 15), ResolvedAt: at(early.Add(time.Hour), 120)},
		{Service: "web", Severity: incident.Warning, State: incident.Resolved, OpenedAt: late, AcknowledgedAt: at(late, 10), ResolvedAt: at(late, 20)},
		{Service: "web", Severity: incident.Warning, State: incident.Resolved, OpenedAt: late.Add(time.Hour), ResolvedAt: at(late.Add(time.Hour), 40)},
		{Service: "web", Severity: incident.Info, State: incident.Open, OpenedAt: late.Add(2 * time.Hour)},
		// db: one, never acknowledged, still open.
		{Service: "db", Severity: incident.Critical, State: incident.Acknowledged, OpenedAt: late, AcknowledgedAt: at(late, 2)},
		// outside the window, both sides.
		{Service: "web", Severity: incident.Critical, State: incident.Resolved, OpenedAt: since.Add(-time.Minute), ResolvedAt: at(since, 10)},
		{Service: "web", Severity: incident.Critical, State: incident.Open, OpenedAt: until},
	}
	r := Build(incidents, "30d", since, until)
	if r.Total.Incidents != 6 || r.Total.Open != 2 {
		t.Errorf("total = %+v", r.Total)
	}
	if len(r.Services) != 2 || r.Services[0].Service != "web" || r.Services[1].Service != "db" {
		t.Fatalf("services = %+v", r.Services)
	}
	web := r.Services[0]
	if web.Incidents != 5 || web.Open != 1 || web.Acknowledged != 3 || web.Resolved != 4 {
		t.Errorf("web counts = %+v", web)
	}
	if web.BySeverity["critical"] != 1 || web.BySeverity["warning"] != 3 || web.BySeverity["info"] != 1 {
		t.Errorf("web by severity = %v", web.BySeverity)
	}
	if *web.MTTASeconds != 600 {
		t.Errorf("web MTTA = %v, want 600 (mean of 5, 15, 10 minutes)", *web.MTTASeconds)
	}
	if *web.MTTRSeconds != 3600 {
		t.Errorf("web MTTR = %v, want 3600 (mean of 60, 120, 20, 40 minutes)", *web.MTTRSeconds)
	}
	if web.TTRTrend != "improving" {
		t.Errorf("web trend = %q; 90 minutes early, 30 late", web.TTRTrend)
	}
	db := r.Services[1]
	if db.MTTRSeconds != nil || *db.MTTASeconds != 120 || db.TTRTrend != "" {
		t.Errorf("db = %+v", db)
	}
}

func TestParseWindow(t *testing.T) {
	if d, s, err := ParseWindow(""); err != nil || s != "30d" || d != 30*24*time.Hour {
		t.Errorf("default: %v %s %v", d, s, err)
	}
	if d, _, err := ParseWindow("24h"); err != nil || d != 24*time.Hour {
		t.Errorf("24h: %v %v", d, err)
	}
	for _, bad := range []string{"0d", "x", "30", "400d", "-1d", "1w"} {
		if _, _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestDescribe(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := map[*float64]string{nil: "no data", f(30): "30s", f(600): "10m", f(5400): "1h 30m", f(7200): "2h", f(3 * 86400): "3d", f(2.6 * 86400): "2d 14h"}
	for in, want := range cases {
		if got := Describe(in); got != want {
			t.Errorf("Describe = %q, want %q", got, want)
		}
	}
}
