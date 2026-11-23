package outbox

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestADedupKeyThreadsTriggersAcknowledgesAndResolves(t *testing.T) {
	o, m, _, now, _ := peopleSetup(t)
	ctx := context.Background()
	trigger := func(key string) Accepted {
		t.Helper()
		got, err := o.Accept(ctx, notice.Event{Producer: "grafana", Kind: "alert.disk", Key: key, DedupKey: "disk-full", Title: "Disk full", To: []notice.Target{{Chain: "oncall"}}})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := trigger("t1")
	if first.Pages != 1 || first.Deduplicated || first.Event.Weight != notice.WeightPage {
		t.Fatalf("first trigger pages: %+v", first)
	}
	o.SendDue(ctx)
	again := trigger("t2")
	if !again.Deduplicated || again.Pages != 0 || again.Messages != 0 || again.Event.Thread != first.Event.Thread {
		t.Fatalf("a second trigger tells nobody again: %+v", again)
	}
	ack, err := o.Accept(ctx, notice.Event{Producer: "grafana", Kind: "alert.disk", DedupKey: "disk-full", Move: notice.Acknowledge})
	if err != nil || ack.Event.Title != "Disk full" {
		t.Fatalf("acknowledge takes the thread's title: %+v %v", ack, err)
	}
	if p, _ := o.store.Page(ctx, 1); p.State != notice.PageAcknowledged || p.AcknowledgedBy != "grafana" {
		t.Errorf("the producer's acknowledge ends the page: %+v", p)
	}
	*now = now.Add(time.Hour)
	o.SendDue(ctx)
	if got := m.to(); !slices.Equal(got, []string{"ann@example.com"}) {
		t.Errorf("only the first step was told before the acknowledge: %v", got)
	}
	if _, err := o.Accept(ctx, notice.Event{Producer: "grafana", Kind: "alert.disk", DedupKey: "disk-full", Move: notice.Resolve}); err != nil {
		t.Fatal(err)
	}
	if fresh := trigger("t3"); fresh.Deduplicated || fresh.Pages != 1 || fresh.Event.Thread == first.Event.Thread {
		t.Errorf("after resolve a trigger opens a new thread: %+v", fresh)
	}
	dropped, err := o.Accept(ctx, notice.Event{Producer: "other", Kind: "alert.disk", DedupKey: "disk-full", Move: notice.Resolve})
	if err != nil || dropped.Dropped == "" {
		t.Errorf("another producer's resolve moves nothing: %+v %v", dropped, err)
	}
}

func TestANoticeOnlyLandsInTheList(t *testing.T) {
	o, m, _, _, _ := peopleSetup(t)
	ctx := context.Background()
	got, err := o.Accept(ctx, notice.Event{Producer: "ci", Kind: "build.done", Key: "b1", Title: "Built", Weight: notice.WeightNotice, To: []notice.Target{{Group: "platform"}}})
	if err != nil || got.Messages != 0 {
		t.Fatalf("a notice sends nothing: %+v %v", got, err)
	}
	o.SendDue(ctx)
	if len(m.to()) != 0 {
		t.Errorf("mailed %v", m.to())
	}
	list, err := o.store.Inbox(ctx, "bo@example.com", 10)
	if err != nil || len(list) != 1 || list[0].Weight != notice.WeightNotice || list[0].Event.Title != "Built" {
		t.Errorf("Bo's list: %+v %v", list, err)
	}
}

func TestAPersonsAcknowledgeIsPassedBack(t *testing.T) {
	o, _, _, _, _ := peopleSetup(t)
	ctx := context.Background()
	var heard []string
	o.OnAcknowledge(func(_ context.Context, p notice.Page, e notice.Event, by string) {
		heard = append(heard, e.Key+" by "+by)
	})
	if _, err := o.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Person: "cy@example.com"}, Topics: []string{"page.acknowledged"}, Match: map[string]string{"producer": "orrery"}}); err != nil {
		t.Fatal(err)
	}
	got, err := o.Accept(ctx, notice.Event{Producer: "orrery", Kind: "action.awaiting_approval", Key: "run-9", DedupKey: "run-9", Title: "Approve run 9", To: []notice.Target{{Chain: "oncall"}}})
	if err != nil || got.Pages != 1 {
		t.Fatalf("accepted %+v %v", got, err)
	}
	if ended, err := o.Acknowledge(ctx, 1, "ann@example.com"); err != nil || !ended {
		t.Fatalf("acknowledge %v %v", ended, err)
	}
	if !slices.Equal(heard, []string{"run-9 by ann@example.com"}) {
		t.Errorf("hooks heard %v", heard)
	}
	th, _ := o.store.Thread(ctx, got.Event.Thread)
	if th.State != notice.ThreadAcknowledged || th.AcknowledgedBy != "ann@example.com" {
		t.Errorf("the thread is acknowledged by the person: %+v", th)
	}
	list, _ := o.store.Inbox(ctx, "cy@example.com", 10)
	if len(list) != 1 || list[0].Event.Kind != "page.acknowledged" || list[0].Event.Labels["producer"] != "orrery" || list[0].Event.Labels["dedup_key"] != "run-9" {
		t.Errorf("the producer's subscription got the acknowledge: %+v", list)
	}
	if ended, _ := o.Acknowledge(ctx, 1, "bo@example.com"); ended {
		t.Error("a page ends once")
	}
}

func TestEscalateNowTellsTheNextStep(t *testing.T) {
	o, m, _, _, _ := peopleSetup(t)
	ctx := context.Background()
	if _, err := o.Accept(ctx, event("e1", notice.Target{Chain: "oncall"})); err != nil {
		t.Fatal(err)
	}
	if moved, err := o.Escalate(ctx, 1, "ann@example.com"); err != nil || !moved {
		t.Fatalf("escalate %v %v", moved, err)
	}
	o.SendDue(ctx)
	if got := m.to(); !slices.Equal(got, []string{"ann@example.com", "bo@example.com"}) {
		t.Errorf("the second step was told at once: %v", got)
	}
	o.Escalate(ctx, 1, "ann@example.com")
	if p, _ := o.store.Page(ctx, 1); p.State != notice.PageUnanswered {
		t.Errorf("escalating past the last step ends the page: %+v", p)
	}
}
