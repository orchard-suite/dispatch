package desk

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/metrics"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/notify"
	"github.com/orchard-suite/dispatch/internal/orrery"
	"github.com/orchard-suite/dispatch/internal/outbox"
	"github.com/orchard-suite/dispatch/internal/seal"
	"github.com/orchard-suite/dispatch/internal/store"
)

// newDeskWithBox is a desk whose incidents go through an outbox.
func newDeskWithBox(t *testing.T) (*Desk, *outbox.Outbox) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "dispatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	quiet := func(context.Context, string) {}
	sealer, _ := seal.New("k")
	box := outbox.New(st, sealer, nil, log, quiet)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	box.Now = func() time.Time { return now }
	var d *Desk
	mt, err := metrics.New(func() metrics.Stats { return d.Stats() })
	if err != nil {
		t.Fatal(err)
	}
	d = New(st, mt, notify.New("", log, quiet), orrery.New(orrery.Config{}, log, quiet), annals.New("", "", log, quiet), box, log, "https://dispatch.test")
	d.Now = func() time.Time { return now }
	return d, box
}

// only keeps the payload's alert of one name.
func only(p alertmanager.Payload, name string) alertmanager.Payload {
	var keep []alertmanager.Alert
	for _, a := range p.Alerts {
		if a.Name() == name {
			keep = append(keep, a)
		}
	}
	p.Alerts = keep
	return p
}

// settle waits for the notices the desk hands the outbox on their own.
func settle(t *testing.T, st *store.Store, person string, want int) []store.InboxLine {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		list, err := st.Inbox(context.Background(), person, 50)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) >= want || time.Now().After(deadline) {
			return list
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAnAlertThatPagesNobodyIsANotice(t *testing.T) {
	d, box := newDeskWithBox(t)
	ctx := context.Background()
	if _, err := box.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Person: "ops@example.com"}, Topics: []string{"alert.*"}}); err != nil {
		t.Fatal(err)
	}
	p := only(payload(t, "alertmanager_firing.json"), "CometErrors")
	delete(p.Alerts[0].Labels, "page")
	res, err := d.IngestAlertmanager(ctx, p, "alertmanager")
	if err != nil || len(res.Opened) != 0 || res.Notices != 1 {
		t.Fatalf("a warning opens nothing: %+v %v", res, err)
	}
	if list, _ := d.Store.List(ctx, store.Filter{}); len(list) != 0 {
		t.Fatalf("incidents %d", len(list))
	}
	list := settle(t, d.Store, "ops@example.com", 1)
	if len(list) != 1 || list[0].Event.Kind != "alert.firing" || list[0].Weight != notice.WeightNotice || list[0].Event.DedupKey != p.Alerts[0].Fingerprint {
		t.Fatalf("the notice: %+v", list)
	}
	r := only(payload(t, "alertmanager_resolved.json"), "CometErrors")
	delete(r.Alerts[0].Labels, "page")
	if res, _ := d.IngestAlertmanager(ctx, r, "alertmanager"); res.Notices != 1 {
		t.Fatalf("resolved %+v", res)
	}
	// A resolve closes the thread and tells nobody.
	var th notice.Thread
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if th, err = d.Store.Thread(ctx, list[0].Event.Thread); err == nil && th.State == notice.ThreadResolved {
			break
		}
	}
	if th.State != notice.ThreadResolved {
		t.Errorf("the resolve closes the notice's thread: %+v %v", th, err)
	}
}

func TestAWarningJoinsAnOpenIncidentButNeverReopensOne(t *testing.T) {
	d, _ := newDeskWithBox(t)
	ctx := context.Background()
	crit := only(payload(t, "alertmanager_firing.json"), "CometSilent")
	res, _ := d.IngestAlertmanager(ctx, crit, "alertmanager")
	if len(res.Opened) != 1 {
		t.Fatalf("critical opens: %+v", res)
	}
	warn := only(payload(t, "alertmanager_firing.json"), "CometSilent")
	warn.Alerts[0].Labels["severity"] = "warning"
	warn.Alerts[0].Fingerprint = "warn-variant"
	if res, _ := d.IngestAlertmanager(ctx, warn, "alertmanager"); len(res.Updated) != 1 || res.Notices != 0 {
		t.Fatalf("a warning of the same group joins the open incident: %+v", res)
	}
	d.Change(ctx, res.Opened[0], incident.Resolved, incident.Person("oncall@example.com"))
	advance(d, time.Minute)
	warn.Alerts[0].Fingerprint = "warn-again"
	if res, _ := d.IngestAlertmanager(ctx, warn, "alertmanager"); len(res.Reopened) != 0 || res.Notices != 1 {
		t.Errorf("a warning never reopens: %+v", res)
	}
}

func TestAcknowledgingAPageAcknowledgesItsIncident(t *testing.T) {
	d, box := newDeskWithBox(t)
	ctx := context.Background()
	if err := d.Store.SaveChain(ctx, notice.Chain{Slug: "oncall", Name: "On call", Steps: []notice.Step{{Targets: []notice.Target{{Person: "ann@example.com"}}, Wait: 5}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := box.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Chain: "oncall"}, Topics: []string{"incident.opened"}}); err != nil {
		t.Fatal(err)
	}
	p := only(payload(t, "alertmanager_firing.json"), "CometSilent")
	res, _ := d.IngestAlertmanager(ctx, p, "alertmanager")
	settle(t, d.Store, "ann@example.com", 1)
	if ended, err := box.Acknowledge(ctx, 1, "ann@example.com"); err != nil || !ended {
		t.Fatalf("acknowledge %v %v", ended, err)
	}
	inc, _ := d.Store.Get(ctx, d.Store.DB(), res.Opened[0])
	if inc.State != incident.Acknowledged || inc.Owner != "ann@example.com" {
		t.Errorf("the incident follows its page: %+v", inc)
	}
}

func TestAnIncidentEventCarriesItsAlertsPagingLabels(t *testing.T) {
	d, box := newDeskWithBox(t)
	ctx := context.Background()
	if _, err := box.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Person: "texts@example.com"}, Topics: []string{"incident.opened"}, Match: map[string]string{"notify": "sms"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := box.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Person: "all@example.com"}, Topics: []string{"incident.opened"}}); err != nil {
		t.Fatal(err)
	}
	plain := only(payload(t, "alertmanager_firing.json"), "CometSilent")
	if res, _ := d.IngestAlertmanager(ctx, plain, "alertmanager"); len(res.Opened) != 1 {
		t.Fatalf("opens: %+v", res)
	}
	texted := only(payload(t, "alertmanager_firing.json"), "CometSilent")
	texted.Alerts[0].Labels["alertname"] = "CometDown"
	texted.Alerts[0].Labels["notify"] = "sms"
	texted.Alerts[0].Fingerprint = "texted"
	if res, _ := d.IngestAlertmanager(ctx, texted, "alertmanager"); len(res.Opened) != 1 {
		t.Fatalf("opens: %+v", res)
	}
	if all := settle(t, d.Store, "all@example.com", 2); len(all) != 2 {
		t.Fatalf("every incident reaches the plain subscription: %+v", all)
	}
	list := settle(t, d.Store, "texts@example.com", 1)
	if len(list) != 1 || list[0].Event.Labels["notify"] != "sms" {
		t.Fatalf("only the notify=sms incident reaches the matching subscription: %+v", list)
	}
	time.Sleep(50 * time.Millisecond)
	if again, _ := d.Store.Inbox(ctx, "texts@example.com", 50); len(again) != 1 {
		t.Errorf("the plain incident must not match notify=sms: %+v", again)
	}
}

func TestACriticalWhoseAlertsStoppedIsSettlingAndSaysSoOnce(t *testing.T) {
	d, box := newDeskWithBox(t)
	ctx := context.Background()
	if _, err := box.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Person: "oncall@example.com"}, Topics: []string{"incident.cleared"}}); err != nil {
		t.Fatal(err)
	}
	res, _ := d.IngestAlertmanager(ctx, only(payload(t, "alertmanager_firing.json"), "CometSilent"), "alertmanager")
	if len(res.Opened) != 1 {
		t.Fatalf("opens: %+v", res)
	}
	firing, _ := d.Store.Get(ctx, d.Store.DB(), res.Opened[0])
	if firing.Settling() || firing.Phase() != "open" {
		t.Fatalf("a firing incident is worked, not settling: %+v", firing)
	}
	resolved := only(payload(t, "alertmanager_resolved.json"), "CometSilent")
	d.IngestAlertmanager(ctx, resolved, "alertmanager")
	inc, _ := d.Store.Get(ctx, d.Store.DB(), res.Opened[0])
	if !inc.Settling() || inc.Phase() != "settling" || inc.ClosesAt == nil || !inc.ClosesAt.Equal(inc.QuietSince.Add(24*time.Hour)) {
		t.Fatalf("settling, closing a day after it went quiet: %+v", inc)
	}
	if list := settle(t, d.Store, "oncall@example.com", 1); len(list) != 1 || list[0].Event.Kind != "incident.cleared" {
		t.Fatalf("one cleared notice: %+v", list)
	}
	d.IngestAlertmanager(ctx, resolved, "alertmanager")
	time.Sleep(50 * time.Millisecond)
	if list, _ := d.Store.Inbox(ctx, "oncall@example.com", 50); len(list) != 1 {
		t.Errorf("a repeat of the same resolve tells nobody again: %+v", list)
	}

	// With the quiet window off, nothing settles: a person resolves it.
	settings, _ := d.Store.Settings(ctx)
	settings.QuietResolve = 0
	d.Store.SaveSettings(ctx, settings)
	if inc, _ := d.Store.Get(ctx, d.Store.DB(), res.Opened[0]); inc.Settling() {
		t.Errorf("window off: %+v", inc)
	}
}
