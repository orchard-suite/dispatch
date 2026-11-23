package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/entra"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/teams/teamstest"
)

func TestFollowingTheStatusPage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.desk.UseStatusURL("https://status.example.org")
	follow := func(email string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/follow", strings.NewReader(url.Values{"email": {email}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return h.do(h.status, req)
	}
	if rec := follow("not an address"); rec.Code != 400 {
		t.Errorf("a bad address: %d", rec.Code)
	}
	rec := follow("fan@example.org")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "has a link to confirm") {
		t.Fatalf("follow %d %s", rec.Code, rec.Body.String())
	}
	list, _ := h.desk.Store.Inbox(ctx, "fan@example.org", 5)
	if len(list) != 1 {
		t.Fatalf("confirm mail %+v", list)
	}
	confirm, _ := url.Parse(list[0].Event.Links[0].URL)
	if rec := h.do(h.status, httptest.NewRequest(http.MethodGet, "/confirm?"+confirm.RawQuery, nil)); !strings.Contains(rec.Body.String(), "Confirmed") {
		t.Errorf("confirm %s", rec.Body.String())
	}
	if rec := h.do(h.status, httptest.NewRequest(http.MethodGet, "/unsubscribe?token=nope", nil)); rec.Code != 404 {
		t.Errorf("a made-up token: %d", rec.Code)
	}
	token := confirm.Query().Get("token")
	if rec := h.do(h.status, httptest.NewRequest(http.MethodGet, "/unsubscribe?token="+token, nil)); !strings.Contains(rec.Body.String(), "Stopped") {
		t.Errorf("unsubscribe %s", rec.Body.String())
	}
}

func TestTheInsideStatusPageShowsEveryOpenIncident(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.desk.Open(ctx, desk.OpenRequest{Title: "Private trouble", Service: "shop"}, incident.SourceDesk, incident.Person(member))
	public := h.do(h.status, httptest.NewRequest(http.MethodGet, "/", nil)).Body.String()
	inside := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/status", nil), member, "User")).Body.String()
	if strings.Contains(public, "Private trouble") || !strings.Contains(inside, "Private trouble") {
		t.Errorf("public has it %v, inside has it %v", strings.Contains(public, "Private trouble"), strings.Contains(inside, "Private trouble"))
	}
}

func TestTeamsRoomsOpenAChannelThroughGraph(t *testing.T) {
	h := newHarness(t)
	svc := teamstest.New(t)
	h.srv.UseBot(BotConfig{Bot: svc.Bot(t), ActsFor: "orrery"})
	if err := h.desk.Store.SaveBotTeam(context.Background(), storeTeam(svc.URL)); err != nil {
		t.Fatal(err)
	}
	var made map[string]any
	graph := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tenant/oauth2/v2.0/token":
			w.Write([]byte(`{"access_token":"g","expires_in":3600}`))
		case "/v1.0/teams/team-1/channels":
			if r.Header.Get("Authorization") != "Bearer g" {
				w.WriteHeader(401)
				return
			}
			json.NewDecoder(r.Body).Decode(&made)
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"19:inc7"}`))
		}
	}))
	defer graph.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("w"), 0o600)
	signer, _ := entra.New(entra.Config{Name: "the bot", AppID: "bot", TenantID: "tenant", TokenFile: file, Scope: "https://graph.microsoft.com/.default", LoginURL: graph.URL})
	signer.UseClient(graph.Client())
	rooms, err := h.srv.TeamsRooms("team-1", signer)
	if err != nil {
		t.Fatal(err)
	}
	rooms.UseGraph(graph.URL, graph.Client())
	room, err := rooms.Open(context.Background(), incident.Incident{ID: 7, Title: "Checkout: down / again", Service: "shop"})
	if err != nil || room != "team-1/19:inc7" {
		t.Fatalf("open %q %v", room, err)
	}
	if name := made["displayName"].(string); strings.ContainsAny(name, ":/") || !strings.HasPrefix(name, "inc-7 ") {
		t.Errorf("channel name %q", name)
	}
	if err := rooms.Post(context.Background(), room, notice.Event{Producer: "dispatch", Title: "#7 is acknowledged"}); err != nil {
		t.Fatal(err)
	}
	if posts := svc.Sent(); len(posts) != 1 || !strings.Contains(stringOf(posts[0]), "19:inc7") {
		t.Errorf("posts %v", posts)
	}
}
