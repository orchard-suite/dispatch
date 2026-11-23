package incident

import (
	"errors"
	"testing"
	"time"
)

func TestTransition(t *testing.T) {
	allowed := [][2]State{
		{Open, Acknowledged}, {Open, Mitigated}, {Open, Resolved},
		{Acknowledged, Mitigated}, {Acknowledged, Resolved},
		{Mitigated, Resolved}, {Resolved, Open},
	}
	for _, c := range allowed {
		if err := Transition(c[0], c[1]); err != nil {
			t.Errorf("%s -> %s should be allowed: %v", c[0], c[1], err)
		}
	}
	refused := [][2]State{
		{Open, Open}, {Acknowledged, Open}, {Mitigated, Acknowledged},
		{Resolved, Acknowledged}, {Resolved, Mitigated}, {Resolved, Resolved},
	}
	for _, c := range refused {
		if err := Transition(c[0], c[1]); !errors.Is(err, ErrTransition) {
			t.Errorf("%s -> %s should be refused, got %v", c[0], c[1], err)
		}
	}
}

func TestParseSeverity(t *testing.T) {
	cases := map[string]Severity{"critical": Critical, "CRITICAL": Critical, "warning": Warning, "info": Info, "": Warning, "page": Warning}
	for in, want := range cases {
		if got := ParseSeverity(in); got != want {
			t.Errorf("ParseSeverity(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestShouldAutoResolve(t *testing.T) {
	base := Incident{Severity: Info, AutoResolve: true, State: Open}
	if !ShouldAutoResolve(base, 0) {
		t.Error("info, auto_resolve on, nothing firing: should resolve")
	}
	if ShouldAutoResolve(base, 1) {
		t.Error("still firing: must not resolve")
	}
	off := base
	off.AutoResolve = false
	if ShouldAutoResolve(off, 0) {
		t.Error("auto_resolve off: must not resolve")
	}
	warn := base
	warn.Severity = Warning
	if ShouldAutoResolve(warn, 0) {
		t.Error("warning: a person resolves, never the machine")
	}
	done := base
	done.State = Resolved
	if ShouldAutoResolve(done, 0) {
		t.Error("already resolved: nothing to do")
	}
}

func TestServiceFor(t *testing.T) {
	rules, bad := ParseRules(`
# comments are fine
namespace=kube-* -> cluster
alertname=Comet*, namespace=comet -> comet
broken line
`)
	if len(bad) != 1 || bad[0] != "broken line" {
		t.Fatalf("bad lines = %v", bad)
	}
	if len(rules) != 2 {
		t.Fatalf("rules = %d", len(rules))
	}
	cases := []struct {
		labels map[string]string
		want   string
	}{
		{map[string]string{"namespace": "kube-system", "app": "coredns"}, "cluster"},
		{map[string]string{"alertname": "CometSilent", "namespace": "comet"}, "comet"},
		{map[string]string{"alertname": "CometSilent", "namespace": "other", "app": "x"}, "x"},
		{map[string]string{"namespace": "orrery", "job": "orrery"}, "orrery"},
		{map[string]string{"job": "node-exporter"}, "node-exporter"},
		{map[string]string{}, "unknown"},
	}
	for _, c := range cases {
		if got := ServiceFor(c.labels, rules); got != c.want {
			t.Errorf("ServiceFor(%v) = %q, want %q", c.labels, got, c.want)
		}
	}
	if FormatRules(rules) != "namespace=kube-* -> cluster\nalertname=Comet*, namespace=comet -> comet\n" {
		t.Errorf("FormatRules = %q", FormatRules(rules))
	}
}

func TestTitleAndAge(t *testing.T) {
	if TitleFor("X", map[string]string{"summary": " the summary "}) != "the summary" {
		t.Error("summary should win")
	}
	if TitleFor("X", nil) != "X" {
		t.Error("alertname is the fallback")
	}
	now := time.Now()
	opened := now.Add(-time.Hour)
	i := Incident{OpenedAt: opened}
	if i.Age(now) != time.Hour {
		t.Error("age of an open incident runs to now")
	}
	r := opened.Add(10 * time.Minute)
	i.ResolvedAt = &r
	if i.Age(now) != 10*time.Minute {
		t.Error("age of a resolved incident stops at resolution")
	}
}
