package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
)

// fakeAnnals is Annals's POST /v1/events: it takes the token "annals-token"
// and hands every event it stores to the test.
type fakeAnnals struct {
	url    string
	events chan kit.Event
	// others holds the routing decisions nextIncident stepped over.
	others []kit.Event
}

func newFakeAnnals(t *testing.T) *fakeAnnals {
	t.Helper()
	f := &fakeAnnals{events: make(chan kit.Event, 128)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" || r.Header.Get("Authorization") != "Bearer annals-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var ev kit.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.events <- ev
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"status":"stored","id":"01JBX7Y8Z9ABCDEFGHJKMNPQRS"}`))
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// next waits for the next event Annals received.
func (f *fakeAnnals) next(t *testing.T) kit.Event {
	t.Helper()
	select {
	case ev := <-f.events:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event reached Annals")
		return kit.Event{}
	}
}

// nextIncident waits for the next incident decision, keeping the routing
// decisions it passes.
func (f *fakeAnnals) nextIncident(t *testing.T) kit.Event {
	t.Helper()
	for {
		ev := f.next(t)
		if strings.HasPrefix(ev.Action.Verb, "incident.") {
			return ev
		}
		f.others = append(f.others, ev)
	}
}

func TestDecisionsReachTheAnnals(t *testing.T) {
	fake := newFakeAnnals(t)
	var posted atomic.Int64
	h := newHarnessWith(t, func(log *slog.Logger) *annals.Poster {
		return annals.New(fake.url, "annals-token", log, func(_ context.Context, outcome string) {
			if outcome == "posted" {
				posted.Add(1)
			}
		})
	})

	// Alertmanager opens two incidents.
	rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/alertmanager", fixture(t, "alertmanager_firing.json")), shared))
	var res desk.IngestResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Opened) != 2 {
		t.Fatalf("opened = %v", res)
	}
	for range res.Opened {
		ev := fake.nextIncident(t)
		if ev.Action.Verb != "incident.open" || ev.Actor != (kit.Actor{Kind: "system", ID: "alertmanager"}) {
			t.Errorf("open: %+v", ev)
		}
	}
	id := res.Opened[0]
	inc, _ := h.desk.Store.Get(context.Background(), h.desk.Store.DB(), id)

	// A person acknowledges it on the desk.
	if rec := h.do(h.session, asPerson(form("/incidents/"+itoa(id)+"/state", url.Values{"to": {"acknowledged"}}), member, "User")); rec.Code != http.StatusSeeOther {
		t.Fatalf("acknowledge form: %d", rec.Code)
	}
	ack := fake.nextIncident(t)
	want := kit.Event{
		At:       ack.At,
		Source:   "dispatch",
		EventKey: ack.EventKey,
		Actor:    kit.Actor{Kind: "person", ID: member, Email: member},
		Action: kit.Action{
			Verb:    "incident.acknowledge",
			Target:  kit.Target{System: "dispatch", Kind: "incident", ID: itoa(id), Title: inc.Title},
			Outcome: "ok",
		},
		Labels: map[string]string{"severity": string(inc.Severity), "service": inc.Service},
	}
	if got, _ := json.Marshal(ack); string(got) != string(mustJSON(t, want)) {
		t.Errorf("acknowledge event:\n got %s\nwant %s", got, mustJSON(t, want))
	}
	if !strings.HasPrefix(ack.EventKey, "incident:"+itoa(id)+":acknowledge:") || ack.At.IsZero() {
		t.Errorf("acknowledge key and time: %q %v", ack.EventKey, ack.At)
	}

	// A token resolves it on the API, saying who it acts for.
	_, secret, err := h.desk.Store.MintToken(context.Background(), "board", admin)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/incidents/" + itoa(id)
	if rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, path+"/resolve", strings.NewReader(`{"actor":"oncall@example.com"}`)), secret)); rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body.String())
	}
	resolved := fake.nextIncident(t)
	if resolved.Action.Verb != "incident.resolve" || resolved.Actor != (kit.Actor{Kind: "system", ID: "board", OnBehalfOf: "oncall@example.com"}) {
		t.Errorf("resolve: %+v", resolved)
	}

	// A reopen and a second resolve are new decisions, each stored.
	for _, step := range []string{"reopen", "resolve"} {
		if rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, path+"/"+step, nil), secret)); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", step, rec.Code)
		}
		ev := fake.nextIncident(t)
		if ev.Action.Verb != "incident."+step || ev.Actor != (kit.Actor{Kind: "system", ID: "board"}) {
			t.Errorf("%s: %+v", step, ev)
		}
		if ev.EventKey == resolved.EventKey {
			t.Errorf("%s reused the key %q", step, ev.EventKey)
		}
	}
	// The count lands after Annals answers: six incident decisions and
	// the routing decision for each incident event the desk announced.
	for deadline := time.Now().Add(5 * time.Second); posted.Load() < 12 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if n := posted.Load(); n != 12 {
		t.Errorf("posted outcomes = %d, want 12", n)
	}
	for len(fake.others) < 6 {
		fake.others = append(fake.others, fake.next(t))
	}
	for _, ev := range fake.others {
		if ev.Action.Verb != "notice.accept" || ev.Labels["producer"] != "dispatch" || ev.Actor != (kit.Actor{Kind: "system", ID: "dispatch"}) {
			t.Errorf("routing decision: %+v", ev)
		}
	}
}

func TestNoAnnalsWithoutSettings(t *testing.T) {
	fake := newFakeAnnals(t)
	var outcomes atomic.Int64
	h := newHarnessWith(t, func(log *slog.Logger) *annals.Poster {
		// A URL without a token is no annals.
		return annals.New(fake.url, "", log, func(context.Context, string) { outcomes.Add(1) })
	})
	rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(`{"title":"Checkout failing","severity":"critical","service":"shop"}`)), shared))
	var inc incident.Incident
	json.Unmarshal(rec.Body.Bytes(), &inc)
	if rec := h.do(h.session, asPerson(form("/incidents/"+itoa(inc.ID)+"/state", url.Values{"to": {"acknowledged"}}), member, "User")); rec.Code != http.StatusSeeOther {
		t.Fatalf("acknowledge form: %d", rec.Code)
	}
	if rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/incidents/"+itoa(inc.ID)+"/resolve", nil), shared)); rec.Code != http.StatusOK {
		t.Fatalf("resolve: %d", rec.Code)
	}
	select {
	case ev := <-fake.events:
		t.Fatalf("posted without settings: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}
	if outcomes.Load() != 0 {
		t.Errorf("counted %d posts without a annals", outcomes.Load())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAdminChangesReachTheAnnals(t *testing.T) {
	fake := newFakeAnnals(t)
	h := newHarnessWith(t, func(log *slog.Logger) *annals.Poster {
		return annals.New(fake.url, "annals-token", log, func(context.Context, string) {})
	})
	if rec := h.do(h.session, asPerson(form("/groups", url.Values{"slug": {"platform"}, "name": {"Platform"}, "members": {"a@example.com"}}), admin, "Admin")); rec.Code != http.StatusOK {
		t.Fatalf("group: %d", rec.Code)
	}
	ev := fake.next(t)
	if ev.Action.Verb != "group.save" || ev.Action.Target.ID != "platform" || ev.Actor != (kit.Actor{Kind: "person", ID: admin, Email: admin}) {
		t.Errorf("group saved: %+v", ev)
	}
	if rec := h.do(h.session, asPerson(form("/me", url.Values{"phone": {"+1 555 555 0100"}, "sms_consent": {"yes"}, "rule_page": {"sms, email+5"}}), member, "User")); rec.Code != http.StatusSeeOther {
		t.Fatalf("me: %d", rec.Code)
	}
	if ev := fake.next(t); ev.Action.Verb != "person.save" || ev.Action.Target.ID != member || ev.Actor.ID != member {
		t.Errorf("own settings saved: %+v", ev)
	}
}
