package desk

import (
	"context"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

func TestALateHeartbeatPagesAndItsCheckInResolvesTheAlert(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	if err := d.SaveHeartbeat(ctx, incident.Heartbeat{Name: "nightly-backup", Interval: time.Hour, Grace: 10 * time.Minute, Severity: incident.Critical, CreatedBy: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveHeartbeat(ctx, incident.Heartbeat{Name: "Bad Name", Interval: time.Hour, Severity: incident.Critical}); err == nil {
		t.Error("a bad name was kept")
	}
	advance(d, 69*time.Minute)
	d.CheckHeartbeats(ctx)
	if list, _ := d.Store.List(ctx, store.Filter{}); len(list) != 0 {
		t.Fatal("raised before the grace ran out")
	}
	advance(d, 2*time.Minute)
	d.CheckHeartbeats(ctx)
	d.CheckHeartbeats(ctx)
	list, _ := d.Store.List(ctx, store.Filter{})
	if len(list) != 1 || list[0].Service != "nightly-backup" || list[0].Severity != incident.Critical || list[0].FiringAlerts != 1 {
		t.Fatalf("one incident for the late heartbeat: %+v", list)
	}
	h, err := d.Beat(ctx, "nightly-backup", 0, "backup-job")
	if err != nil || h.Late || h.LastAt == nil {
		t.Fatalf("beat %+v %v", h, err)
	}
	inc, _ := d.Store.Get(ctx, d.Store.DB(), list[0].ID)
	if inc.FiringAlerts != 0 || inc.State != incident.Open {
		t.Errorf("the alert resolves; a person resolves the incident: %+v", inc)
	}
	if _, err := d.Beat(ctx, "unknown", 0, "job"); err == nil {
		t.Error("an unknown heartbeat without an interval")
	}
	if h, err := d.Beat(ctx, "made-by-beat", 15*time.Minute, "job"); err != nil || h.Interval != 15*time.Minute || h.CreatedBy != "job" {
		t.Errorf("made by its first check-in: %+v %v", h, err)
	}
}

func TestAWindowQuietsAlertsAndEvents(t *testing.T) {
	d, box := newDeskWithBox(t)
	ctx := context.Background()
	if _, err := d.AddWindow(ctx, incident.Window{Service: "com*", StartsAt: d.Now().Add(-time.Minute), EndsAt: d.Now().Add(time.Hour), CreatedBy: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddWindow(ctx, incident.Window{Service: "x", StartsAt: d.Now(), EndsAt: d.Now().Add(-time.Hour)}); err == nil {
		t.Error("a window that ends before it starts")
	}
	p := only(payload(t, "alertmanager_firing.json"), "CometSilent")
	res, _ := d.IngestAlertmanager(ctx, p, "alertmanager")
	if len(res.Opened) != 0 || res.Notices != 1 {
		t.Fatalf("a critical alert in a window is a notice: %+v", res)
	}
	box.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Person: "ops@example.com"}, Topics: []string{"build.*"}})
	got, err := box.Accept(ctx, notice.Event{Producer: "ci", Kind: "build.failed", Key: "b", Title: "Build failed", Labels: map[string]string{"service": "comet"}})
	if err != nil || got.Event.Weight != notice.WeightNotice || got.Event.Labels["maintenance"] != "true" || got.Messages != 0 {
		t.Errorf("an event about a quieted service tells nobody: %+v %v", got, err)
	}
}

func TestChangesShowBesideIncidents(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	if _, _, err := d.AddChange(ctx, incident.Change{Producer: "argocd", Service: "comet", Title: "comet v1.2.0", URL: "https://example.org/r/1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.AddChange(ctx, incident.Change{Producer: "argocd", Service: "comet", Title: "x", Kind: "Bad Kind"}); err == nil {
		t.Error("a bad kind was kept")
	}
	advance(d, 30*time.Minute)
	p := only(payload(t, "alertmanager_firing.json"), "CometSilent")
	res, _ := d.IngestAlertmanager(ctx, p, "alertmanager")
	inc, _ := d.Store.Get(ctx, d.Store.DB(), res.Opened[0])
	changes, err := d.ChangesNear(ctx, inc)
	if err != nil || len(changes) != 1 || changes[0].Title != "comet v1.2.0" || changes[0].Kind != "deploy" {
		t.Errorf("changes %+v %v", changes, err)
	}
}
