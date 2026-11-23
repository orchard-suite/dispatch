package incident

import "testing"

func TestRoutesDropQuietRaiseAndRate(t *testing.T) {
	routes, bad := ParseRoutes("alertname=Watchdog => drop\nnamespace=sandbox-* => notice\nalertname=DiskFull, env=prod => page\nalertname=Noisy => severity info\nnonsense\nx=y => severity loud\n# comment")
	if len(routes) != 4 || len(bad) != 2 {
		t.Fatalf("routes %+v bad %v", routes, bad)
	}
	if again, _ := ParseRoutes(FormatRoutes(routes)); len(again) != 4 || again[3].Severity != Info {
		t.Errorf("round trip %+v", again)
	}
	cases := []struct {
		labels  map[string]string
		dropped bool
		pages   bool
	}{
		{map[string]string{"alertname": "Watchdog", "severity": "critical"}, true, false},
		{map[string]string{"alertname": "PodDown", "namespace": "sandbox-1", "severity": "critical"}, false, false},
		{map[string]string{"alertname": "DiskFull", "env": "prod", "severity": "warning"}, false, true},
		{map[string]string{"alertname": "DiskFull", "env": "dev", "severity": "warning"}, false, false},
		{map[string]string{"alertname": "Noisy", "severity": "critical"}, false, false},
		{map[string]string{"alertname": "Other", "severity": "critical"}, false, true},
	}
	for _, c := range cases {
		got := ApplyRoutes(c.labels, routes)
		if got.Dropped != c.dropped || got.Pages != c.pages {
			t.Errorf("%v: %+v", c.labels, got)
		}
	}
	rated := ApplyRoutes(map[string]string{"alertname": "Noisy", "severity": "critical"}, routes)
	if rated.Labels["severity"] != "info" {
		t.Errorf("rated %v", rated.Labels)
	}
}
