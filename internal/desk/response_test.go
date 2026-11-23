package desk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/github"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestRolesMergesAndLinks(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	p := only(payload(t, "alertmanager_firing.json"), "CometSilent")
	res, _ := d.IngestAlertmanager(ctx, p, "alertmanager")
	a, _ := d.Open(ctx, OpenRequest{Title: "Uploads fail", Severity: incident.Critical, Service: "comet"}, incident.SourceDesk, incident.Person("ann@example.com"))
	b := res.Opened[0]
	if _, err := d.SetRole(ctx, a.ID, "commander", "Ann@Example.com", "bo@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetRole(ctx, a.ID, "janitor", "x@example.com", "bo@example.com"); err == nil {
		t.Error("an unknown role")
	}
	if roles, _ := d.Store.Roles(ctx, a.ID); roles["commander"] != "ann@example.com" {
		t.Errorf("roles %v", roles)
	}
	merged, err := d.Merge(ctx, b, a.ID, incident.Person("bo@example.com"))
	if err != nil || merged.State != incident.Resolved {
		t.Fatalf("merge %+v %v", merged, err)
	}
	into, _ := d.Store.Get(ctx, d.Store.DB(), a.ID)
	if into.FiringAlerts != 1 {
		t.Errorf("the alert moved: %+v", into)
	}
	if links, _ := d.Store.Links(ctx, b); len(links) != 1 || !links[0].Into || links[0].ID != a.ID {
		t.Errorf("links of the merged %+v", links)
	}
	if _, err := d.Merge(ctx, a.ID, a.ID, incident.Person("bo@example.com")); err == nil {
		t.Error("merged into itself")
	}
	c, _ := d.Open(ctx, OpenRequest{Title: "Other", Service: "x"}, incident.SourceDesk, incident.Person("ann@example.com"))
	if err := d.Link(ctx, c.ID, a.ID, "bo@example.com"); err != nil {
		t.Fatal(err)
	}
	if links, _ := d.Store.Links(ctx, a.ID); len(links) != 2 {
		t.Errorf("a's links %+v", links)
	}
}

func TestFollowersHearOfPublicIncidentsOnly(t *testing.T) {
	d, _ := newDeskWithBox(t)
	ctx := context.Background()
	d.UseStatusURL("https://status.example.org")
	if err := d.Follow(ctx, "fan@example.org"); err != nil {
		t.Fatal(err)
	}
	list, _ := d.Store.Inbox(ctx, "fan@example.org", 10)
	if len(list) != 1 || list[0].Event.Kind != "status.confirm" || !strings.Contains(list[0].Event.Links[0].URL, "/confirm?token=") {
		t.Fatalf("a confirm link: %+v", list)
	}
	token := strings.SplitN(list[0].Event.Links[0].URL, "token=", 2)[1]
	d.Open(ctx, OpenRequest{Title: "Private", Service: "x"}, incident.SourceDesk, incident.Person("a@example.com"))
	if l, _ := d.Store.Inbox(ctx, "fan@example.org", 10); len(l) != 1 {
		t.Fatal("an unconfirmed follower was told")
	}
	if _, err := d.Store.Confirm(ctx, token); err != nil {
		t.Fatal(err)
	}
	inc, _ := d.Open(ctx, OpenRequest{Title: "Checkout slow", Service: "shop", Public: true}, incident.SourceDesk, incident.Person("a@example.com"))
	d.Note(ctx, inc.ID, "Fixed the cache", true, "a@example.com")
	d.Note(ctx, inc.ID, "internal", false, "a@example.com")
	l, _ := d.Store.Inbox(ctx, "fan@example.org", 10)
	if len(l) != 3 {
		t.Fatalf("the opening and the public note, not the private ones: %+v", l)
	}
	for _, x := range l[:2] {
		if x.Event.Kind != "status.updated" || !strings.Contains(x.Event.Links[1].URL, "/unsubscribe?token="+token) {
			t.Errorf("update %+v", x.Event)
		}
	}
}

func TestAnActionItemOpensAnIssueAndClosesWithIt(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	var mu sync.Mutex
	state := "open"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/example/shop/issues":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["title"] != "Add a cache alert" {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"html_url":"https://github.com/example/shop/issues/7"}`))
		case r.URL.Path == "/repos/example/shop/issues/7":
			w.Write([]byte(`{"state":"` + state + `"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	gh := github.New("t")
	gh.UseAPI(srv.URL, srv.Client())
	d.UseIssues(gh)
	settings, _ := d.Store.Settings(ctx)
	settings.IssueRepos = map[string]string{"shop": "example/shop"}
	d.Store.SaveSettings(ctx, settings)
	inc, _ := d.Open(ctx, OpenRequest{Title: "Checkout slow", Service: "shop"}, incident.SourceDesk, incident.Person("a@example.com"))
	if err := d.AddAction(ctx, inc.ID, "Add a cache alert", "a@example.com", "a@example.com"); err != nil {
		t.Fatal(err)
	}
	var items []incident.ActionItem
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if items, _ = d.Store.ActionItems(ctx, inc.ID); len(items) == 1 && items[0].IssueURL != "" {
			break
		}
	}
	if len(items) != 1 || items[0].IssueURL != "https://github.com/example/shop/issues/7" {
		t.Fatalf("items %+v", items)
	}
	d.SyncIssues(ctx)
	if items, _ = d.Store.ActionItems(ctx, inc.ID); items[0].Done {
		t.Fatal("done while the issue is open")
	}
	mu.Lock()
	state = "closed"
	mu.Unlock()
	d.SyncIssues(ctx)
	if items, _ = d.Store.ActionItems(ctx, inc.ID); !items[0].Done {
		t.Error("the closed issue ticks it off")
	}
}

// fakeRooms records rooms opened and posts made.
type fakeRooms struct {
	mu    sync.Mutex
	posts []string
}

func (f *fakeRooms) Open(_ context.Context, inc incident.Incident) (string, error) {
	return "team/room-1", nil
}

func (f *fakeRooms) Post(_ context.Context, room string, e notice.Event) error {
	f.mu.Lock()
	f.posts = append(f.posts, room+": "+e.Title)
	f.mu.Unlock()
	return nil
}

func TestACriticalIncidentGetsARoom(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	rooms := &fakeRooms{}
	d.UseRooms(rooms, incident.Critical)
	warn, _ := d.Open(ctx, OpenRequest{Title: "Slow", Severity: incident.Warning, Service: "x"}, incident.SourceDesk, incident.Person("a@example.com"))
	crit, _ := d.Open(ctx, OpenRequest{Title: "Down", Severity: incident.Critical, Service: "x"}, incident.SourceDesk, incident.Person("a@example.com"))
	var room string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && room == ""; time.Sleep(10 * time.Millisecond) {
		room, _ = d.Store.TeamsChannel(ctx, crit.ID)
	}
	if room != "team/room-1" {
		t.Fatalf("room %q", room)
	}
	if r, _ := d.Store.TeamsChannel(ctx, warn.ID); r != "" {
		t.Error("a warning got a room")
	}
	d.Change(ctx, crit.ID, incident.Acknowledged, incident.Person("a@example.com"))
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		rooms.mu.Lock()
		n := len(rooms.posts)
		rooms.mu.Unlock()
		if n >= 2 {
			break
		}
	}
	rooms.mu.Lock()
	defer rooms.mu.Unlock()
	if len(rooms.posts) < 2 || !strings.Contains(strings.Join(rooms.posts, "|"), "is acknowledged") {
		t.Errorf("posts %v", rooms.posts)
	}
}
