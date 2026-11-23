package notice

import (
	"errors"
	"testing"
)

func TestTargetsAndSteps(t *testing.T) {
	for in, want := range map[string]string{"Ann@Example.com": "person:ann@example.com", "group:platform": "group:platform", "chain:on-call": "chain:on-call"} {
		got, err := ParseTarget(in)
		if err != nil || got.String() != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"team:x", "group:Bad Slug", "person:nope", "chain:-x"} {
		if _, err := ParseTarget(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s was taken", bad)
		}
	}
	if _, err := (Target{Person: "a@example.com", Group: "g"}).Normal(); err == nil {
		t.Error("two kinds in one target")
	}
	steps, err := ParseSteps("5 person:a@example.com, group:platform\n\n30 group:leads")
	if err != nil || len(steps) != 2 || steps[0].Wait != 5 || len(steps[0].Targets) != 2 || steps[1].Targets[0].Group != "leads" {
		t.Fatalf("steps %+v %v", steps, err)
	}
	if FormatSteps(steps) != "5 person:a@example.com group:platform\n30 group:leads" {
		t.Errorf("format %q", FormatSteps(steps))
	}
	if _, err := ParseSteps("group:platform"); err == nil {
		t.Error("a step with no wait")
	}
	c := Chain{Slug: "oncall", Name: "On call", Steps: []Step{{Targets: []Target{{Chain: "x"}}}}}
	if err := c.Validate(); err == nil {
		t.Error("a chain inside a chain")
	}
	e := Event{Kind: "a.b", Key: "k", Title: "t", To: []Target{{Person: "B@Example.com"}}}
	if err := e.Validate(); err != nil || e.To[0].Person != "b@example.com" {
		t.Errorf("event to: %v %v", e.To, err)
	}
	e.To = make([]Target, 21)
	if err := e.Validate(); err == nil {
		t.Error("21 targets taken")
	}
}
