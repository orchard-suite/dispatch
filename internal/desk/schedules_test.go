package desk

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestHandoffsAndGapsAreToldOnce(t *testing.T) {
	d, box := newDeskWithBox(t)
	ctx := context.Background()
	now := d.Now()
	layer, _ := notice.ParseLayer("", "ann@example.com, bo@example.com", now.Add(-time.Hour).Format("2006-01-02 15:04"), "12h", "", time.UTC)
	nights, _ := notice.ParseLayer("", "ann@example.com", now.Add(-time.Hour).Format("2006-01-02 15:04"), "1w", "09:00-17:00", time.UTC)
	nights.Members = []string{"cy@example.com"}
	if err := d.SaveSchedule(ctx, notice.Schedule{Slug: "s", Name: "Platform", Zone: "UTC", Layers: []notice.Layer{layer}}); err != nil {
		t.Fatal(err)
	}
	box.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Person: "lead@example.com"}, Topics: []string{"schedule.*"}})
	d.WatchSchedules(ctx)
	if list, _ := d.Store.Inbox(ctx, "ann@example.com", 10); len(list) != 0 {
		t.Fatal("the first look tells nobody")
	}
	advance(d, 12*time.Hour)
	d.WatchSchedules(ctx)
	d.WatchSchedules(ctx)
	list, _ := d.Store.Inbox(ctx, "bo@example.com", 10)
	if len(list) != 1 || list[0].Event.Kind != "schedule.handoff" || !strings.HasPrefix(list[0].Event.Title, "You are on call for Platform until") {
		t.Fatalf("Bo is told once: %+v", list)
	}
	// A schedule that is off at night has gaps, told once a day.
	d.SaveSchedule(ctx, notice.Schedule{Slug: "day", Name: "Days", Zone: "UTC", Layers: []notice.Layer{nights}})
	d.WatchSchedules(ctx)
	d.WatchSchedules(ctx)
	lead, _ := d.Store.Inbox(ctx, "lead@example.com", 10)
	gaps := 0
	for _, l := range lead {
		if l.Event.Kind == "schedule.gap" {
			gaps++
		}
	}
	if gaps != 1 {
		t.Errorf("the gap is told once: %+v", lead)
	}
	if _, err := d.Cover(ctx, "s", notice.Override{Person: "cy@example.com", From: d.Now(), To: d.Now().Add(-time.Hour)}); err == nil {
		t.Error("a cover that ends before it starts")
	}
	if _, err := d.Cover(ctx, "s", notice.Override{Person: "CY@example.com", From: d.Now(), To: d.Now().Add(time.Hour), By: "bo@example.com"}); err != nil {
		t.Fatal(err)
	}
	sc, _ := d.Store.Schedule(ctx, "s", d.Now())
	if sc.OnCall(d.Now().Add(time.Minute)) != "cy@example.com" {
		t.Error("the cover holds")
	}
}

func TestASnoozePagesAgainOnlyIfStillAcknowledged(t *testing.T) {
	d, _ := newDeskWithBox(t)
	ctx := context.Background()
	inc, _ := d.Open(ctx, OpenRequest{Title: "Checkout slow", Severity: incident.Critical, Service: "shop"}, incident.SourceDesk, incident.Person("ann@example.com"))
	if err := d.Snooze(ctx, inc.ID, 30, "ann@example.com"); err == nil {
		t.Fatal("an open incident is not snoozed")
	}
	d.Change(ctx, inc.ID, incident.Acknowledged, incident.Person("ann@example.com"))
	if err := d.Snooze(ctx, inc.ID, 30, "ann@example.com"); err != nil {
		t.Fatal(err)
	}
	advance(d, 31*time.Minute)
	d.Remind(ctx)
	d.Remind(ctx)
	list, _ := d.Store.Inbox(ctx, "ann@example.com", 10)
	if len(list) != 1 || list[0].Weight != notice.WeightPage || list[0].Event.Kind != "incident.snooze_ended" {
		t.Errorf("Ann is paged again once: %+v", list)
	}
}
