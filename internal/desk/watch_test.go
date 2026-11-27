package desk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/changetest"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/metrics"
	"github.com/orchard-suite/dispatch/internal/notify"
	"github.com/orchard-suite/dispatch/internal/orrery"
	"github.com/orchard-suite/dispatch/internal/outbox"
	"github.com/orchard-suite/dispatch/internal/seal"
	"github.com/orchard-suite/dispatch/internal/signals"
	"github.com/orchard-suite/dispatch/internal/store"
)

var shopPods = change.Ref{Blueprint: "k8s_workload", Key: "home/shop/shop"}

// restarts is the query the tests watch stops with, and what it renders
// to for the shop's pods over a 30 minute window.
const (
	restarts         = `increase(restarts{namespace="{namespace.title}", pod=~"{name}-.*"}[{window}])`
	shopRestarts     = `increase(restarts{namespace="shop", pod=~"shop-.*"}[1800s])`
	shopWorkloadDown = `down{workload="{key}"}`
)

// hubFor answers the catalog for the watch: relations from edges, and the
// shop's pods with their fields.
func hubFor(t *testing.T, edges ...[3]string) *httptest.Server {
	t.Helper()
	rel := catalog(t, edges...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/related"):
			res, err := http.Get(rel.URL + r.URL.Path)
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			defer res.Body.Close()
			_, _ = io.Copy(w, res.Body)
		case r.URL.Path == "/v1/entities/k8s_workload/home/shop/shop":
			_, _ = w.Write([]byte(`{"title":"shop","fields":{"name":"shop","labels":{"app":"shop"}},"relations":{"namespace":[{"blueprint":"k8s_namespace","key":"home/shop","title":"shop"}]},"incomingRefs":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// watchDesk is a desk on a store at path, with the outbox, a memory store
// named prom, and the catalog at hub.
func watchDesk(t *testing.T, path, hub string, mem *signals.Memory, poster *annals.Poster) *Desk {
	t.Helper()
	st, err := store.Open(path)
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
	if poster == nil {
		poster = annals.New("", "", log, quiet)
	}
	cat := orrery.New(orrery.Config{URL: hub, Token: "read", PushToken: "push"}, log, quiet)
	d = New(st, mt, notify.New("", log, quiet), cat, poster, box, log, "https://dispatch.test")
	d.Now = func() time.Time { return now }
	o := signals.NewOpener(nil)
	o.Use("prom", mem)
	d.UseSignals(o)
	d.UseVersion("1.2.0")
	return d
}

func watchSettings(t *testing.T, d *Desk, queries ...signals.Signal) {
	t.Helper()
	ctx := context.Background()
	st, _ := d.Store.Settings(ctx)
	st.Leaning = incident.Leaning{Relations: []string{"<-k8s_workload:workload"}, Depth: 1}
	st.Watch = signals.Settings{Stores: map[string]signals.Store{"prom": {Kind: signals.StorePrometheus, URL: "http://prometheus.test"}}, Queries: queries}
	if err := d.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
}

func alertsRaised(t *testing.T, d *Desk) []string {
	t.Helper()
	rows, err := d.Store.DB().Query(`SELECT json_extract(body, '$.labels.alertname') FROM notice_events WHERE kind = 'alert.firing' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func watchOf(t *testing.T, d *Desk, key string) incident.Change {
	t.Helper()
	c, err := d.Store.ChangeByRecord(context.Background(), d.Store.DB(), change.EventRef{Sender: "orrery", Key: key + ".stated"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTheWatchSeesHarmOnWhatAChangeStopsAndWhatLeansOnIt(t *testing.T) {
	ctx := context.Background()
	hub := hubFor(t, [3]string{"k8s_workload:home/shop/shop", "workload", "sw_workload:shop"})
	mem := signals.NewMemory()
	mem.Set(shopRestarts, 2)
	d := watchDesk(t, filepath.Join(t.TempDir(), "d.db"), hub.URL, mem, nil)
	watchSettings(t, d,
		signals.Signal{Blueprint: "k8s_workload", Signal: "restarts", Kinds: []string{"stops"}, Store: "prom", Query: restarts},
	)
	c := changetest.New("orrery", "run-1", []change.Ref{shopPods}, changetest.Stops(shopPods), changetest.Grants(shopPods))
	at := d.Now()
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Stated(at), c.Routed(change.RouteNoOne, at), c.Ended(change.OutcomeOK, at))); err != nil {
		t.Fatal(err)
	}
	got := watchOf(t, d, "run-1")
	if got.Watch == nil || got.Watch.State != incident.WatchWatching || !got.Watch.Due.Equal(at.Add(30*time.Minute)) {
		t.Fatalf("a watch for the stops window: %+v", got.Watch)
	}
	advance(d, 29*time.Minute)
	d.WatchChanges(ctx)
	if got := watchOf(t, d, "run-1"); got.Watch.State != incident.WatchWatching {
		t.Fatal("closed before its window")
	}
	advance(d, time.Minute)
	d.WatchChanges(ctx)
	got = watchOf(t, d, "run-1")
	w := got.Watch
	if w.State != incident.WatchDone || w.Verdict != change.VerdictHarm || len(w.Sights) != 2 {
		t.Fatalf("closed with harm on the pods and a blind workload: %+v", w)
	}
	byKey := map[string]change.Sight{}
	for _, s := range w.Sights {
		byKey[s.Subject.Key] = s
	}
	if s := byKey["home/shop/shop"]; s.Verdict != change.VerdictHarm || s.Path != change.PathInPlace || len(s.Signals) != 1 || s.Signals[0] != "restarts" {
		t.Errorf("the subject: %+v", s)
	}
	if s := byKey["shop"]; s.Verdict != change.VerdictBlind || s.Kind != "stops" {
		t.Errorf("what leans on it, with no query for its blueprint, is blind: %+v", s)
	}
	if alerts := alertsRaised(t, d); len(alerts) != 1 || alerts[0] != ChangeMissedAlert {
		t.Errorf("a change routed to no one that did harm is a miss: %v", alerts)
	}
	var observed int
	for _, e := range got.Events {
		if e.Event == change.EventObserved && e.Sender == Machine {
			observed++
		}
	}
	if observed != 1 {
		t.Errorf("the observation is kept with the change: %+v", got.Events)
	}
}

func TestBlindIsNeverClean(t *testing.T) {
	ctx := context.Background()
	hub := hubFor(t)
	mem := signals.NewMemory()
	mem.Fail(`down{workload="home/shop/shop"}`, errors.New("connection refused"))
	d := watchDesk(t, filepath.Join(t.TempDir(), "d.db"), hub.URL, mem, nil)
	watchSettings(t, d,
		signals.Signal{Blueprint: "k8s_workload", Signal: "restarts", Kinds: []string{"stops"}, Store: "prom", Query: restarts},
		signals.Signal{Blueprint: "k8s_workload", Signal: "down", Kinds: []string{"stops"}, Store: "prom", Query: shopWorkloadDown},
		signals.Signal{Blueprint: "k8s_workload", Signal: "refused", Kinds: []string{"cuts"}, Store: "nowhere", Query: "x"},
	)
	c := changetest.New("orrery", "run-2", []change.Ref{shopPods}, changetest.Stops(shopPods), changetest.Cuts(shopPods))
	at := d.Now()
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Stated(at), c.Routed(change.RoutePeople, at), c.Ended(change.OutcomeOK, at))); err != nil {
		t.Fatal(err)
	}
	advance(d, 2*time.Hour)
	d.WatchChanges(ctx)
	w := watchOf(t, d, "run-2").Watch
	if w.State != incident.WatchDone || w.Verdict != change.VerdictBlind || len(w.Sights) != 2 {
		t.Fatalf("no data, a failed store, and no adapter are all blind: %+v", w)
	}
	for _, s := range w.Sights {
		if s.Verdict != change.VerdictBlind {
			t.Errorf("%s: %s", s.Kind, s.Verdict)
		}
	}
	if alerts := alertsRaised(t, d); len(alerts) != 1 || alerts[0] != ChangeUnwatchedAlert {
		t.Errorf("a change nothing could watch: %v", alerts)
	}
}

func TestAnIncidentOnWhatAChangeTouchedJoinsItAndIsAMiss(t *testing.T) {
	ctx := context.Background()
	hub := hubFor(t, [3]string{"k8s_workload:home/shop/shop", "workload", "sw_workload:shop"})
	mem := signals.NewMemory()
	mem.Set(shopRestarts, 0)
	d := watchDesk(t, filepath.Join(t.TempDir(), "d.db"), hub.URL, mem, nil)
	watchSettings(t, d, signals.Signal{Blueprint: "k8s_workload", Signal: "restarts", Kinds: []string{"stops"}, Store: "prom", Query: restarts})
	c := changetest.New("orrery", "run-3", []change.Ref{shopPods}, changetest.Stops(shopPods))
	at := d.Now()
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Stated(at), c.Routed(change.RoutePeople, at), c.Ended(change.OutcomeOK, at))); err != nil {
		t.Fatal(err)
	}
	advance(d, 10*time.Minute)
	on, err := d.Open(ctx, OpenRequest{Title: "shop is slow", Service: "shop", Severity: incident.Critical}, incident.SourceAPI, incident.System("ci"))
	if err != nil {
		t.Fatal(err)
	}
	on.Workload = "shop"
	if err := d.Store.Update(ctx, d.Store.DB(), on); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Open(ctx, OpenRequest{Title: "blog is down", Service: "blog", Severity: incident.Critical}, incident.SourceAPI, incident.System("ci")); err != nil {
		t.Fatal(err)
	}
	advance(d, 20*time.Minute)
	d.WatchChanges(ctx)
	w := watchOf(t, d, "run-3").Watch
	if len(w.Incidents) != 1 || w.Incidents[0].ID != "1" || w.Verdict != change.VerdictBlind {
		t.Errorf("the incident on the workload that leans on the pods joins: %+v", w)
	}
	if alerts := alertsRaised(t, d); len(alerts) != 1 || alerts[0] != ChangeMissedAlert {
		t.Errorf("a change an incident joined is a miss: %v", alerts)
	}
}

func TestAWatchSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	hub := hubFor(t)
	mem := signals.NewMemory()
	mem.Set(shopRestarts, 0)
	path := filepath.Join(t.TempDir(), "d.db")
	first := watchDesk(t, path, hub.URL, mem, nil)
	watchSettings(t, first, signals.Signal{Blueprint: "k8s_workload", Signal: "restarts", Kinds: []string{"stops"}, Store: "prom", Query: restarts})
	c := changetest.New("orrery", "run-4", []change.Ref{shopPods}, changetest.Stops(shopPods))
	at := first.Now()
	if _, err := first.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Stated(at), c.Ended(change.OutcomeOK, at))); err != nil {
		t.Fatal(err)
	}
	first.Store.Close()
	second := watchDesk(t, path, hub.URL, mem, nil)
	advance(second, time.Hour)
	second.WatchChanges(ctx)
	w := watchOf(t, second, "run-4").Watch
	if w.State != incident.WatchDone || w.Verdict != change.VerdictClean {
		t.Errorf("the pending watch is a row a new desk closes: %+v", w)
	}
	if alerts := alertsRaised(t, second); len(alerts) != 0 {
		t.Errorf("a clean change routed to nobody known raises nothing: %v", alerts)
	}
}

func TestOnlyAChangeThatRanWithAWatchedKindIsWatched(t *testing.T) {
	ctx := context.Background()
	d := watchDesk(t, filepath.Join(t.TempDir(), "d.db"), hubFor(t).URL, signals.NewMemory(), nil)
	at := d.Now()
	refused := changetest.New("orrery", "run-5", []change.Ref{shopPods}, changetest.Stops(shopPods))
	grants := changetest.New("orrery", "run-6", []change.Ref{shopPods}, changetest.Grants(shopPods))
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(refused.Stated(at), refused.Ended(change.OutcomeRefused, at), grants.Stated(at), grants.Ended(change.OutcomeOK, at))); err != nil {
		t.Fatal(err)
	}
	if w := watchOf(t, d, "run-5").Watch; w != nil {
		t.Errorf("a change that never ran: %+v", w)
	}
	if w := watchOf(t, d, "run-6").Watch; w != nil {
		t.Errorf("a change with only an exposure effect: %+v", w)
	}
}

func TestAnEndWithNoStatementIsAChangeWithNoWatch(t *testing.T) {
	ctx := context.Background()
	d := watchDesk(t, filepath.Join(t.TempDir(), "d.db"), hubFor(t).URL, signals.NewMemory(), nil)
	c := changetest.New("orrery", "run-7", []change.Ref{shopPods}, changetest.Stops(shopPods))
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Ended(change.OutcomeOK, d.Now()))); err != nil {
		t.Fatal(err)
	}
	d.WatchChanges(ctx)
	if alerts := alertsRaised(t, d); len(alerts) != 0 {
		t.Fatalf("its statement may still come: %v", alerts)
	}
	advance(d, 11*time.Minute)
	d.WatchChanges(ctx)
	d.WatchChanges(ctx)
	if alerts := alertsRaised(t, d); len(alerts) != 1 || alerts[0] != ChangeUnwatchedAlert {
		t.Errorf("told once: %v", alerts)
	}
}

// takers counts posts and refuses the first refuse of them.
type takers struct {
	mu     sync.Mutex
	refuse int
	posts  map[string]int
	bodies map[string][]byte
}

func (k *takers) handler(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	k.posts[r.URL.Path]++
	k.bodies[r.URL.Path] = body
	if k.posts[r.URL.Path] <= k.refuse {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such route"}`))
		return
	}
	_, _ = w.Write([]byte(`{"status":"stored","id":"01J0000000000000000000000"}`))
}

func TestTheObservationIsHandedOnUntilTaken(t *testing.T) {
	ctx := context.Background()
	k := &takers{refuse: 2, posts: map[string]int{}, bodies: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(k.handler))
	defer srv.Close()
	hub := hubFor(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	poster := annals.New(srv.URL, "annals-token", log, func(context.Context, string) {})
	mem := signals.NewMemory()
	mem.Set(shopRestarts, 0)
	d := watchDesk(t, filepath.Join(t.TempDir(), "d.db"), hub.URL, mem, poster)
	d.orrery = orrery.New(orrery.Config{URL: srv.URL, Token: "read", PushToken: "push"}, log, func(context.Context, string) {})
	watchSettings(t, d, signals.Signal{Blueprint: "k8s_workload", Signal: "restarts", Kinds: []string{"stops"}, Store: "prom", Query: restarts})
	c := changetest.New("orrery", "run-8", []change.Ref{shopPods}, changetest.Stops(shopPods))
	at := d.Now()
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Stated(at), c.Ended(change.OutcomeOK, at))); err != nil {
		t.Fatal(err)
	}
	advance(d, time.Hour)
	d.WatchChanges(ctx)
	sends := watchOf(t, d, "run-8").Watch.Sends
	if len(sends) != 2 || sends[0].State != "pending" || sends[0].LastError == "" {
		t.Fatalf("a refusal is kept and tried again: %+v", sends)
	}
	for range 3 {
		advance(d, time.Hour)
		d.WatchChanges(ctx)
	}
	sends = watchOf(t, d, "run-8").Watch.Sends
	for _, s := range sends {
		if s.State != "sent" {
			t.Errorf("taken in the end: %+v", s)
		}
	}
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(k.bodies["/v1/ingest/change-events"], &body); err != nil || len(body.Events) != 1 || body.Events[0]["event"] != change.EventObserved {
		t.Errorf("the catalog is sent the event: %s %v", k.bodies["/v1/ingest/change-events"], err)
	}
	var ann map[string]any
	if err := json.Unmarshal(k.bodies["/v1/events"], &ann); err != nil {
		t.Fatal(err)
	}
	action, _ := ann["action"].(map[string]any)
	effects, _ := ann["effects"].([]any)
	if action["verb"] != change.EventObserved || ann["cause"] != "run-8.stated" || ann["cause_source"] != "orrery" || len(effects) != 1 {
		t.Errorf("annals is sent the record with its effects: %s", k.bodies["/v1/events"])
	}
}
