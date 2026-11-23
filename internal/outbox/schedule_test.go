package outbox

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestAScheduleStepReachesWhoeverIsOnCallAndAChainRepeats(t *testing.T) {
	o, m, _, now, _ := peopleSetup(t)
	ctx := context.Background()
	layer, err := notice.ParseLayer("", "bo@example.com, stranger@example.com", now.Add(-time.Hour).Format("2006-01-02 15:04"), "1w", "", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.store.SaveSchedule(ctx, notice.Schedule{Slug: "platform", Name: "Platform", Zone: "UTC", Layers: []notice.Layer{layer}}); err != nil {
		t.Fatal(err)
	}
	if err := o.store.SaveChain(ctx, notice.Chain{Slug: "rota", Name: "Rota", Repeat: 1, Steps: []notice.Step{{Targets: []notice.Target{{Schedule: "platform"}}, Wait: 5}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Accept(ctx, notice.Event{Producer: "x", Kind: "a.b", Key: "k", Title: "T", To: []notice.Target{{Schedule: "nope"}}}); err == nil {
		t.Error("an unknown schedule was taken")
	}
	if _, err := o.Accept(ctx, notice.Event{Producer: "x", Kind: "a.b", Key: "p", Title: "Paged", To: []notice.Target{{Chain: "rota"}}}); err != nil {
		t.Fatal(err)
	}
	o.SendDue(ctx)
	if !slices.Equal(m.to(), []string{"bo@example.com"}) {
		t.Fatalf("the schedule's person: %v", m.to())
	}
	*now = now.Add(6 * time.Minute)
	o.SendDue(ctx)
	if got := m.to(); len(got) != 2 || got[1] != "bo@example.com" {
		t.Errorf("the repeat tells them again: %v", got)
	}
	*now = now.Add(6 * time.Minute)
	o.SendDue(ctx)
	if p, _ := o.store.Page(ctx, 1); p.State != notice.PageUnanswered || p.Round != 1 {
		t.Errorf("after its repeats the page ends: %+v", p)
	}
}
