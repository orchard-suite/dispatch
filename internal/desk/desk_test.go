package desk

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/alertmanager"
	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/metrics"
	"github.com/orchard-suite/dispatch/internal/notify"
	"github.com/orchard-suite/dispatch/internal/orrery"
	"github.com/orchard-suite/dispatch/internal/outbox"
	"github.com/orchard-suite/dispatch/internal/store"
)

func newDesk(t *testing.T) *Desk {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "dispatch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var d *Desk
	mt, err := metrics.New(func() metrics.Stats { return d.Stats() })
	if err != nil {
		t.Fatal(err)
	}
	quiet := func(context.Context, string) {}
	d = New(st, mt, notify.New("", log, quiet), orrery.New(orrery.Config{}, log, quiet), annals.New("", "", log, quiet), nil, log, "https://dispatch.test")
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	d.Now = func() time.Time { return now }
	return d
}

// clock is a test's time, which the desk and its outbox both read, from
// their own goroutines too.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// clocks holds the clock of each desk a test made with one.
var clocks sync.Map

// shareClock makes the desk and the outbox read one clock from now.
func shareClock(d *Desk, box *outbox.Outbox, now time.Time) {
	c := &clock{t: now}
	clocks.Store(d, c)
	d.Now, box.Now = c.Now, c.Now
}

func advance(d *Desk, by time.Duration) {
	if c, ok := clocks.Load(d); ok {
		c := c.(*clock)
		c.mu.Lock()
		c.t = c.t.Add(by)
		c.mu.Unlock()
		return
	}
	t := d.Now().Add(by)
	d.Now = func() time.Time { return t }
}

func payload(t *testing.T, name string) alertmanager.Payload {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "server", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := alertmanager.Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAlertsBecomeIncidentsAndGroup(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	res, err := d.IngestAlertmanager(ctx, payload(t, "alertmanager_firing.json"), "alertmanager")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Opened) != 2 {
		t.Fatalf("two alert names open two incidents, got %+v", res)
	}
	// The same webhook again is Alertmanager repeating itself: no change.
	res, err = d.IngestAlertmanager(ctx, payload(t, "alertmanager_firing.json"), "alertmanager")
	if err != nil || len(res.Opened) != 0 || len(res.Updated) != 2 {
		t.Fatalf("repeat should update nothing new: %+v %v", res, err)
	}
	list, _ := d.Store.List(ctx, store.Filter{})
	if len(list) != 2 {
		t.Fatalf("incidents = %d", len(list))
	}
	crit := list[0]
	if crit.Severity != incident.Critical || crit.Service != "comet" || crit.Title == "" || crit.State != incident.Open || crit.FiringAlerts != 1 {
		t.Errorf("critical incident: %+v", crit)
	}
	events, _ := d.Store.Events(ctx, crit.ID)
	if len(events) != 1 || events[0].Kind != incident.EventOpened {
		t.Errorf("timeline after open: %+v", events)
	}

	// Resolving the alerts does not resolve the incidents: a person does.
	res, err = d.IngestAlertmanager(ctx, payload(t, "alertmanager_resolved.json"), "alertmanager")
	if err != nil || len(res.Resolved) != 0 {
		t.Fatalf("resolved alerts must not resolve warning or critical incidents: %+v %v", res, err)
	}
	crit, _ = d.Store.Get(ctx, d.Store.DB(), crit.ID)
	if crit.State != incident.Open || crit.FiringAlerts != 0 {
		t.Errorf("after alert resolved: %+v", crit)
	}
	events, _ = d.Store.Events(ctx, crit.ID)
	if len(events) != 2 || events[1].Kind != incident.EventAlertResolved {
		t.Errorf("timeline after alert resolved: %+v", events)
	}

	// A person resolves; the same alert firing again inside the window reopens it.
	if _, err := d.Change(ctx, crit.ID, incident.Resolved, incident.Person("oncall@example.com")); err != nil {
		t.Fatal(err)
	}
	advance(d, 5*time.Minute)
	res, _ = d.IngestAlertmanager(ctx, payload(t, "alertmanager_firing.json"), "alertmanager")
	if len(res.Reopened) != 2 && len(res.Reopened)+len(res.Updated) != 2 {
		t.Fatalf("inside the window the alert reopens: %+v", res)
	}
	crit, _ = d.Store.Get(ctx, d.Store.DB(), crit.ID)
	if crit.State != incident.Open || crit.ResolvedAt != nil {
		t.Errorf("reopened: %+v", crit)
	}

	// Outside the window, a new incident opens instead.
	d.IngestAlertmanager(ctx, payload(t, "alertmanager_resolved.json"), "alertmanager")
	d.Change(ctx, crit.ID, incident.Resolved, incident.Person("oncall@example.com"))
	advance(d, time.Hour)
	res, _ = d.IngestAlertmanager(ctx, payload(t, "alertmanager_firing.json"), "alertmanager")
	if len(res.Opened) == 0 {
		t.Fatalf("outside the window a new incident opens: %+v", res)
	}
	list, _ = d.Store.List(ctx, store.Filter{Service: "comet"})
	if len(list) < 3 {
		t.Errorf("incidents = %d", len(list))
	}
}

func TestResolvedAlertForNothingOpenIsIgnored(t *testing.T) {
	d := newDesk(t)
	res, err := d.IngestAlertmanager(context.Background(), payload(t, "alertmanager_resolved.json"), "alertmanager")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Opened)+len(res.Updated)+len(res.Resolved) != 0 {
		t.Errorf("nothing to hold: %+v", res)
	}
}

func TestAutoResolveInfoOnly(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	p := payload(t, "alertmanager_firing.json")
	p.Alerts = p.Alerts[:1]
	p.Alerts[0].Labels["severity"] = "info"
	p.Alerts[0].Labels["page"] = "true"
	if _, err := d.IngestAlertmanager(ctx, p, "alertmanager"); err != nil {
		t.Fatal(err)
	}
	r := payload(t, "alertmanager_resolved.json")
	r.Alerts = r.Alerts[:1]
	r.Alerts[0].Labels["severity"] = "info"
	r.Alerts[0].Labels["page"] = "true"
	res, err := d.IngestAlertmanager(ctx, r, "alertmanager")
	if err != nil || len(res.Resolved) != 1 {
		t.Fatalf("info with auto_resolve on resolves itself: %+v %v", res, err)
	}
	inc, _ := d.Store.Get(ctx, d.Store.DB(), res.Resolved[0])
	if inc.State != incident.Resolved || inc.ResolvedAt == nil {
		t.Errorf("auto-resolved: %+v", inc)
	}

	// With the setting off, an info incident waits for a person.
	settings, _ := d.Store.Settings(ctx)
	settings.AutoResolveInfo = false
	d.Store.SaveSettings(ctx, settings)
	advance(d, time.Hour)
	d.IngestAlertmanager(ctx, p, "alertmanager")
	res, _ = d.IngestAlertmanager(ctx, r, "alertmanager")
	if len(res.Resolved) != 0 {
		t.Errorf("auto_resolve off: %+v", res)
	}
}

func TestStateChangesThroughTheDesk(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	inc, err := d.Open(ctx, OpenRequest{Title: "Database slow", Severity: "critical", Service: "db"}, incident.SourceAPI, incident.System("board"))
	if err != nil {
		t.Fatal(err)
	}
	advance(d, 4*time.Minute)
	inc, err = d.Change(ctx, inc.ID, incident.Acknowledged, incident.Person("oncall@example.com"))
	if err != nil || inc.Owner != "oncall@example.com" || inc.AcknowledgedAt == nil {
		t.Fatalf("acknowledge: %+v %v", inc, err)
	}
	if _, err := d.Change(ctx, inc.ID, incident.Open, incident.Person("oncall@example.com")); !errors.Is(err, ErrInput) {
		t.Errorf("acknowledged does not reopen: %v", err)
	}
	advance(d, 30*time.Minute)
	inc, err = d.Change(ctx, inc.ID, incident.Resolved, incident.Person("oncall@example.com"))
	if err != nil || inc.State != incident.Resolved {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := d.Change(ctx, inc.ID, incident.Mitigated, incident.Person("x")); !errors.Is(err, ErrInput) {
		t.Errorf("resolved does not mitigate: %v", err)
	}
	report, err := d.Outcomes(ctx, "7d")
	if err != nil {
		t.Fatal(err)
	}
	if report.Total.Incidents != 1 || *report.Total.MTTASeconds != 240 || *report.Total.MTTRSeconds != 2040 {
		t.Errorf("outcomes: %+v", report.Total)
	}
	if _, err := d.Outcomes(ctx, "bogus"); !errors.Is(err, ErrInput) {
		t.Errorf("bad window: %v", err)
	}
	if _, err := d.Note(ctx, inc.ID, "  ", false, "x"); !errors.Is(err, ErrInput) {
		t.Errorf("empty note: %v", err)
	}
	if err := d.SavePostmortem(ctx, inc.ID, "## What happened\nIt broke.", "oncall@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := d.AddAction(ctx, inc.ID, "Add an index", "dba", "oncall@example.com"); err != nil {
		t.Fatal(err)
	}
	items, _ := d.Store.ActionItems(ctx, inc.ID)
	if len(items) != 1 || items[0].Done {
		t.Fatalf("items = %+v", items)
	}
	if err := d.SetActionDone(ctx, inc.ID, items[0].ID, true, "x"); err != nil {
		t.Fatal(err)
	}
	comps, _, err := d.Components(ctx, "30d")
	if err != nil || len(comps) != 1 || comps[0].Service != "db" || comps[0].WorstOpen != "" {
		t.Errorf("components: %+v %v", comps, err)
	}
	if _, err := d.Change(ctx, 999, incident.Acknowledged, incident.Person("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing incident: %v", err)
	}
}

func TestAQuietIncidentResolvesAfterTheWindow(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	p := only(payload(t, "alertmanager_firing.json"), "CometSilent")
	res, err := d.IngestAlertmanager(ctx, p, "alertmanager")
	if err != nil || len(res.Opened) != 1 {
		t.Fatalf("critical opens: %+v %v", res, err)
	}
	id := res.Opened[0]
	byHand, err := d.Open(ctx, OpenRequest{Title: "Garage door stuck", Severity: "critical", Service: "garage"}, incident.SourceDesk, incident.Person("ann@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Change(ctx, id, incident.Acknowledged, incident.Person("ann@example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.IngestAlertmanager(ctx, only(payload(t, "alertmanager_resolved.json"), "CometSilent"), "alertmanager"); err != nil {
		t.Fatal(err)
	}
	inc, _ := d.Store.Get(ctx, d.Store.DB(), id)
	if inc.State != incident.Acknowledged || inc.QuietSince == nil {
		t.Fatalf("a critical stays open when its alert resolves: %+v", inc)
	}
	quiet := *inc.QuietSince
	d.Now = func() time.Time { return quiet.Add(24*time.Hour - time.Minute) }
	d.ResolveQuiet(ctx)
	if inc, _ = d.Store.Get(ctx, d.Store.DB(), id); inc.State != incident.Acknowledged {
		t.Fatalf("not before the window: %+v", inc)
	}
	d.Now = func() time.Time { return quiet.Add(24 * time.Hour) }
	d.ResolveQuiet(ctx)
	if inc, _ = d.Store.Get(ctx, d.Store.DB(), id); inc.State != incident.Resolved || inc.ResolvedAt == nil {
		t.Fatalf("resolved at the window: %+v", inc)
	}
	events, err := d.Store.Events(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Actor != Machine || last.Text != "Resolved by dispatch: no alert has fired for 24 hours" {
		t.Errorf("the timeline says why: %+v", last)
	}
	if inc, _ := d.Store.Get(ctx, d.Store.DB(), byHand.ID); inc.State != incident.Open {
		t.Errorf("an incident a person opened holds no alerts and waits for a person: %+v", inc)
	}

	// With the window at zero, nothing resolves itself.
	settings, _ := d.Store.Settings(ctx)
	settings.QuietResolve = 0
	d.Store.SaveSettings(ctx, settings)
	advance(d, 15*time.Minute+time.Second)
	res, _ = d.IngestAlertmanager(ctx, p, "alertmanager")
	d.IngestAlertmanager(ctx, only(payload(t, "alertmanager_resolved.json"), "CometSilent"), "alertmanager")
	advance(d, 48*time.Hour)
	d.ResolveQuiet(ctx)
	for _, opened := range append(res.Opened, res.Reopened...) {
		if inc, _ := d.Store.Get(ctx, d.Store.DB(), opened); inc.State == incident.Resolved {
			t.Errorf("window off: %+v", inc)
		}
	}
}
