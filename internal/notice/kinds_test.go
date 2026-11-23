package notice

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestValidateKinds(t *testing.T) {
	good := []DeclaredKind{
		{Kind: "action.awaiting_approval", Title: "  A request waits for your approval  ", Description: " One sentence. ", Weight: WeightNotify},
		{Kind: "action.finished", Title: "A run ended", Weight: WeightNotice},
	}
	if err := ValidateKinds(good); err != nil {
		t.Fatal(err)
	}
	if good[0].Title != "A request waits for your approval" || good[0].Description != "One sentence." {
		t.Errorf("titles and descriptions are trimmed: %+v", good[0])
	}
	kind := func(change func(*DeclaredKind)) []DeclaredKind {
		k := DeclaredKind{Kind: "action.finished", Title: "A run ended", Weight: WeightNotify}
		change(&k)
		return []DeclaredKind{k}
	}
	many := make([]DeclaredKind, MaxKinds+1)
	for i := range many {
		many[i] = DeclaredKind{Kind: fmt.Sprintf("action.k%d", i), Title: "A kind", Weight: WeightNotice}
	}
	for name, kinds := range map[string][]DeclaredKind{
		"a kind with no dot":        kind(func(k *DeclaredKind) { k.Kind = "finished" }),
		"a kind in capitals":        kind(func(k *DeclaredKind) { k.Kind = "Action.Finished" }),
		"no title":                  kind(func(k *DeclaredKind) { k.Title = "   " }),
		"a title too long":          kind(func(k *DeclaredKind) { k.Title = strings.Repeat("a", 121) }),
		"a description too long":    kind(func(k *DeclaredKind) { k.Description = strings.Repeat("a", 301) }),
		"no weight":                 kind(func(k *DeclaredKind) { k.Weight = "" }),
		"a weight that is not one":  kind(func(k *DeclaredKind) { k.Weight = "loud" }),
		"a kind listed twice":       append(kind(func(*DeclaredKind) {}), kind(func(*DeclaredKind) {})...),
		"more kinds than the limit": many,
	} {
		if err := ValidateKinds(kinds); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The limits themselves pass, and count characters, not bytes.
	if err := ValidateKinds(many[:MaxKinds]); err != nil {
		t.Errorf("%d kinds: %v", MaxKinds, err)
	}
	edge := kind(func(k *DeclaredKind) { k.Title, k.Description = strings.Repeat("é", 120), strings.Repeat("é", 300) })
	if err := ValidateKinds(edge); err != nil {
		t.Errorf("the longest title and description: %v", err)
	}
}

func TestChoosingReplacesOneWeightsRule(t *testing.T) {
	p := Person{Email: "a@example.com", Phone: "+15555550100", Rules: Rules{
		WeightPage:   {{Medium: SMS}, {Medium: Email, After: 5}},
		WeightNotify: {{Medium: Email}},
	}}
	all := func(Medium) bool { return true }
	now := time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC)
	chose := p.Choosing(WeightNotify, []Medium{SMS})
	if plan := chose.Plan(WeightNotify, all, now); len(plan) != 1 || plan[0].Medium != SMS || !plan[0].At.Equal(now) {
		t.Errorf("the chosen medium, at once: %+v", plan)
	}
	if plan := chose.Plan(WeightPage, all, now); len(plan) != 2 {
		t.Errorf("the page rule is untouched: %+v", plan)
	}
	if plan := p.Choosing(WeightNotify, nil).Plan(WeightNotify, all, now); len(plan) != 0 {
		t.Errorf("list only sends nothing: %+v", plan)
	}
	if len(p.Rules[WeightNotify]) != 1 || p.Rules[WeightNotify][0].Medium != Email {
		t.Errorf("the person's own rules are not changed: %+v", p.Rules)
	}
	if !(Choice{Kind: "a.b"}).ListOnly() || (Choice{Kind: "a.b", Media: []Medium{Email}}).ListOnly() {
		t.Error("a choice with no media is list only")
	}
}
