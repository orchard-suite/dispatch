package desk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"

	"github.com/orchard-suite/dispatch/internal/changetest"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/orrery"
	"github.com/orchard-suite/dispatch/internal/store"
)

var (
	shop   = change.Ref{Blueprint: "sw_workload", Key: "shop"}
	shopDB = change.Ref{Blueprint: "azure_resource", Key: "sub/shop/shop-db"}
)

func TestAStatedChangeIsKeyedToItsThingsAndTheRestAttach(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	c := changetest.New("orrery", "run-1", []change.Ref{shopDB}, changetest.Stops(shopDB))
	at := d.Now()
	kept, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Stated(at), c.Routed(change.RouteNoOne, at), c.Ended(change.OutcomeOK, at.Add(time.Minute))))
	if err != nil || kept.Kept != 3 || kept.Repeats != 0 {
		t.Fatalf("kept %+v %v", kept, err)
	}
	got, err := d.Changes(ctx, store.ChangeFilter{Entities: []change.Ref{shopDB}})
	if err != nil || len(got) != 1 {
		t.Fatalf("changes %+v %v", got, err)
	}
	ch := got[0]
	if ch.Record == nil || ch.Record.Key != "run-1.stated" || ch.Digest != c.Digest || len(ch.Effects) != 1 || ch.Effects[0].Kind != "stops" ||
		ch.Route != change.RouteNoOne || ch.Outcome != change.OutcomeOK || ch.EndedAt == nil || len(ch.Events) != 3 || ch.Kind != "terraform_plan" || ch.Producer != "orrery" {
		t.Errorf("the change carries its events: %+v", ch)
	}
	// The same events again are repeats and change nothing.
	kept, err = d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Stated(at), c.Ended(change.OutcomeOK, at.Add(time.Minute))))
	if err != nil || kept.Kept != 0 || kept.Repeats != 2 {
		t.Errorf("repeats %+v %v", kept, err)
	}
	if all, _ := d.Changes(ctx, store.ChangeFilter{}); len(all) != 1 {
		t.Errorf("one change, kept once: %d", len(all))
	}
}

func TestEventsBeforeTheirStatementWaitForIt(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	c := changetest.New("orrery", "run-2", []change.Ref{shopDB}, changetest.Stops(shopDB))
	at := d.Now()
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Ended(change.OutcomeFailed, at))); err != nil {
		t.Fatal(err)
	}
	if all, _ := d.Changes(ctx, store.ChangeFilter{}); len(all) != 0 {
		t.Fatalf("no change before its statement: %+v", all)
	}
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Stated(at))); err != nil {
		t.Fatal(err)
	}
	all, _ := d.Changes(ctx, store.ChangeFilter{})
	if len(all) != 1 || all[0].Outcome != change.OutcomeFailed || len(all[0].Events) != 2 {
		t.Errorf("the early end is the change's: %+v", all)
	}
}

func TestABatchIsKeptWholeOrRefusedAtItsFirstBadEvent(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	at := d.Now()
	good := changetest.New("orrery", "run-3", []change.Ref{shopDB}, changetest.Stops(shopDB))
	other := changetest.New("orrery", "run-4", []change.Ref{shopDB})
	unknownField := strings.Replace(string(good.Ended(change.OutcomeOK, at)), `"outcome"`, `"surprise":1,"outcome"`, 1)
	wrongDigest := changetest.Change{Sender: "orrery", Key: "run-3", Digest: other.Digest}.Ended(change.OutcomeOK, at)
	cases := []struct {
		name   string
		events [][]byte
		index  int
		reason string
	}{
		{"an unknown field", [][]byte{good.Stated(at), []byte(unknownField)}, 1, "bad_event"},
		{"another sender", [][]byte{good.Stated(at), changetest.New("trellis", "run-9", []change.Ref{shopDB}).Stated(at)}, 1, "sender_mismatch"},
		{"a digest that is not the statement's", [][]byte{good.Stated(at), wrongDigest}, 1, "digest_mismatch"},
		{"an unknown kind", [][]byte{changetest.New("orrery", "run-5", []change.Ref{shopDB}, change.Effect{Kind: "halts", Subject: shopDB, Path: "in-place", Undo: "yes", Claim: change.Claim{From: "writer"}}).Stated(at)}, 0, ""},
		{"not a change event", [][]byte{[]byte(`{"event":"deploy.done"}`)}, 0, "unknown_event"},
	}
	for _, tc := range cases {
		_, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(tc.events...))
		var refused *RefusedEvent
		if !errors.As(err, &refused) || refused.Index != tc.index || (tc.reason != "" && refused.Reason != tc.reason) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if all, _ := d.Changes(ctx, store.ChangeFilter{}); len(all) != 0 {
		t.Errorf("a refused batch keeps nothing: %+v", all)
	}
	// An estate's own kind, once in the settings, is taken.
	st, _ := d.Store.Settings(ctx)
	st.Kinds = []change.Kind{{Name: "halts", Family: change.FamilyDisruption, Means: "A line stops.", Watch: change.Duration(time.Hour)}}
	if err := d.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(cases[3].events...)); err != nil {
		t.Errorf("an estate kind: %v", err)
	}
}

func TestAPostedChangeIsKeptOncePerRecordAndTakesItsStatementLater(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	c := changetest.New("orrery", "run-6", []change.Ref{shopDB}, changetest.Stops(shopDB))
	posted := incident.Change{Producer: "orrery", Title: "apply", Things: []change.Ref{shopDB}, Run: "https://hub.example.org/runs/6", Record: c.Of()}
	first, kept, err := d.AddChange(ctx, posted)
	if err != nil || !kept || first.Run != "https://hub.example.org/runs/6" || first.Name() != "azure_resource:sub/shop/shop-db" {
		t.Fatalf("first %+v %v %v", first, kept, err)
	}
	again, kept, err := d.AddChange(ctx, posted)
	if err != nil || kept || again.ID != first.ID {
		t.Errorf("a repeat of its record: %+v %v %v", again, kept, err)
	}
	if _, _, err := d.AddChange(ctx, incident.Change{Producer: "ci", Title: "x"}); !errors.Is(err, ErrInput) {
		t.Errorf("no service and no things: %v", err)
	}
	if _, _, err := d.AddChange(ctx, incident.Change{Producer: "ci", Title: "x", Things: []change.Ref{{Blueprint: "Bad Blueprint", Key: "k"}}}); !errors.Is(err, ErrInput) {
		t.Errorf("a bad thing: %v", err)
	}
	if _, err := d.TakeChangeEvents(ctx, "orrery", changetest.Raws(c.Stated(d.Now()))); err != nil {
		t.Fatal(err)
	}
	all, _ := d.Changes(ctx, store.ChangeFilter{})
	if len(all) != 1 || all[0].Digest != c.Digest || len(all[0].Effects) != 1 {
		t.Errorf("the posted change takes its statement: %+v", all)
	}
}

func TestChangesReadByEntityServiceAndTime(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	t0 := d.Now()
	for i, c := range []incident.Change{
		{Producer: "ci", Service: "shop", Title: "a", At: t0},
		{Producer: "ci", Title: "b", Things: []change.Ref{shopDB}, At: t0.Add(time.Hour)},
		{Producer: "ci", Service: "blog", Title: "c", At: t0.Add(2 * time.Hour)},
	} {
		if _, _, err := d.AddChange(ctx, c); err != nil {
			t.Fatal(i, err)
		}
	}
	titles := func(f store.ChangeFilter) string {
		got, err := d.Changes(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range got {
			out = append(out, c.Title)
		}
		return strings.Join(out, ",")
	}
	if got := titles(store.ChangeFilter{}); got != "c,b,a" {
		t.Errorf("newest first: %s", got)
	}
	if got := titles(store.ChangeFilter{Service: "shop", Entities: []change.Ref{shopDB}}); got != "b,a" {
		t.Errorf("by service or entity: %s", got)
	}
	if got := titles(store.ChangeFilter{Since: t0.Add(30 * time.Minute), Until: t0.Add(90 * time.Minute)}); got != "b" {
		t.Errorf("by time: %s", got)
	}
	if _, err := d.Changes(ctx, store.ChangeFilter{Since: t0, Until: t0}); !errors.Is(err, ErrInput) {
		t.Errorf("an empty span: %v", err)
	}
}

// catalog is a hub that answers each entity's relations from edges.
func catalog(t *testing.T, edges ...[3]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/related") {
			http.NotFound(w, r)
			return
		}
		parts := strings.Split(strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/related"), "/v1/entities/"), "/")
		id := parts[0] + ":" + strings.Join(parts[1:], "/")
		type ref struct {
			Blueprint string `json:"blueprint"`
			Key       string `json:"key"`
		}
		split := func(s string) ref {
			b, k, _ := strings.Cut(s, ":")
			return ref{b, k}
		}
		var out []map[string]any
		for _, e := range edges {
			if e[0] == id || e[2] == id {
				out = append(out, map[string]any{"from": split(e[0]), "relation": e[1], "to": split(e[2])})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"edges": out})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func useCatalog(d *Desk, url string) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d.orrery = orrery.New(orrery.Config{URL: url, Token: "read"}, log, func(context.Context, string) {})
}

func TestAnIncidentJoinsChangesToWhatItLeansOn(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	hub := catalog(t,
		[3]string{"sw_workload:shop", "services", "sw_service:shop-api"},
		[3]string{"azure_resource:sub/shop/shop-db", "service", "sw_service:shop-api"},
		[3]string{"plat_finding:x", "sw_workload", "sw_workload:shop"},
	)
	useCatalog(d, hub.URL)
	st, _ := d.Store.Settings(ctx)
	st.Leaning = incident.Leaning{Relations: []string{"services", "<-service"}, Depth: 2}
	if err := d.Store.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.AddChange(ctx, incident.Change{Producer: "orrery", Title: "the database moved", Things: []change.Ref{shopDB}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.AddChange(ctx, incident.Change{Producer: "orrery", Title: "a finding's record", Things: []change.Ref{{Blueprint: "plat_finding", Key: "x"}}}); err != nil {
		t.Fatal(err)
	}
	advance(d, 10*time.Minute)
	inc := incident.Incident{ID: 1, Service: "checkout", Workload: "shop", OpenedAt: d.Now()}
	got, err := d.ChangesNear(ctx, inc)
	if err != nil || len(got) != 1 || got[0].Title != "the database moved" {
		t.Errorf("the workload leans on its service, which leans on the database it runs on: %+v %v", got, err)
	}
	leaners, err := d.orrery.Leaners(ctx, shopDB, st.Leaning)
	if err != nil || len(leaners) != 2 || leaners[0].Key != "shop-api" || leaners[1] != shop {
		t.Errorf("leaners of the database: %+v %v", leaners, err)
	}
	// With the hub down, the service and the workload still join.
	hub.Close()
	useCatalog(d, hub.URL)
	if _, _, err := d.AddChange(ctx, incident.Change{Producer: "ci", Title: "shop deploy", Things: []change.Ref{shop}}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.ChangesNear(ctx, inc); err != nil || len(got) != 1 || got[0].Title != "shop deploy" {
		t.Errorf("hub down: %+v %v", got, err)
	}
}
