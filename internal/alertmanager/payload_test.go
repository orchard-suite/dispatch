package alertmanager

import (
	"os"
	"strings"
	"testing"
)

func TestParseFiring(t *testing.T) {
	f, err := os.Open("../server/testdata/alertmanager_firing.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "4" || p.Status != "firing" || p.Receiver != "dispatch" {
		t.Errorf("header: %+v", p)
	}
	if len(p.Alerts) != 2 {
		t.Fatalf("alerts = %d", len(p.Alerts))
	}
	a := p.Alerts[0]
	if a.Name() != "CometErrors" || !a.Firing() || a.Resolved() || a.Ended() != nil {
		t.Errorf("first alert: %+v", a)
	}
	if a.Fingerprint != "4a1b2c3d4e5f6071" {
		t.Errorf("fingerprint kept: %q", a.Fingerprint)
	}
	if p.Alerts[1].Name() != "CometSilent" {
		t.Errorf("sorted by name: %q", p.Alerts[1].Name())
	}
}

func TestParseResolved(t *testing.T) {
	f, err := os.Open("../server/testdata/alertmanager_resolved.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "resolved" {
		t.Errorf("status = %q", p.Status)
	}
	for _, a := range p.Alerts {
		if !a.Resolved() || a.Ended() == nil {
			t.Errorf("%s should be resolved with an end time", a.Name())
		}
	}
}

func TestParseRefuses(t *testing.T) {
	cases := map[string]string{
		"not json":      `{`,
		"no alerts":     `{"version":"4","alerts":[]}`,
		"no alertname":  `{"version":"4","alerts":[{"status":"firing","labels":{"severity":"critical"}}]}`,
		"odd status":    `{"version":"4","alerts":[{"status":"pending","labels":{"alertname":"X"}}]}`,
		"alerts absent": `{"version":"4"}`,
	}
	for name, body := range cases {
		if _, err := Parse(strings.NewReader(body)); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
}

func TestFingerprintFallback(t *testing.T) {
	body := `{"version":"4","alerts":[{"status":"firing","labels":{"alertname":"X","b":"2","a":"1"}}]}`
	p, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if p.Alerts[0].Fingerprint != "a=1;alertname=X;b=2;" {
		t.Errorf("fingerprint = %q", p.Alerts[0].Fingerprint)
	}
}
