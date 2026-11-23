package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/orchard-suite/dispatch/internal/teams/teamstest"
)

// product stands in for the producer the bot acts on: it records who
// called and refuses one path.
type product struct {
	mu    sync.Mutex
	calls []string
}

func (p *product) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.calls = append(p.calls, r.URL.Path+" "+r.Header.Get("Authorization"))
	p.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/decline") {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"You may not decide this run"}`)
		return
	}
	_, _ = io.WriteString(w, `{}`)
}

func TestBotPostsCardsAndActsAsThePersonWhoClicks(t *testing.T) {
	h := newHarness(t)
	svc := teamstest.New(t)
	prod := &product{}
	productSrv := httptest.NewServer(prod)
	defer productSrv.Close()
	h.srv.UseBot(BotConfig{Bot: svc.Bot(t), ActsFor: "alertmanager", ProductURL: productSrv.URL})
	h.desk.Notices().UseBot(h.srv.BotSender())

	activity := func(a map[string]any, token string) *httptest.ResponseRecorder {
		a["serviceUrl"] = svc.URL
		a["channelId"] = "msteams"
		a["recipient"] = map[string]string{"id": "28:" + teamstest.AppID}
		a["from"] = map[string]string{"id": "29:ada", "name": "Ada"}
		raw, _ := json.Marshal(a)
		req := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader(string(raw)))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return h.do(h.api, req)
	}
	signed := svc.Sign(svc.Claims())
	team := map[string]any{"team": map[string]string{"id": "team-1", "name": "Platform Engineering"}, "tenant": map[string]string{"id": teamstest.TenantID}}

	if rec := activity(map[string]any{"type": "installationUpdate", "action": "add", "channelData": team}, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unsigned activity was taken: %d", rec.Code)
	}
	other := map[string]any{"team": map[string]string{"id": "team-x"}, "tenant": map[string]string{"id": "another-tenant"}}
	if rec := activity(map[string]any{"type": "installationUpdate", "action": "add", "channelData": other}, signed); rec.Code != http.StatusForbidden {
		t.Fatalf("another tenant was taken: %d", rec.Code)
	}
	if rec := activity(map[string]any{"type": "installationUpdate", "action": "add", "channelData": team}, signed); rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body.String())
	}

	// The page offers the team's channels; one becomes a bot channel with a subscription.
	rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/notifications", nil), admin, "Admin"))
	if !strings.Contains(rec.Body.String(), "Platform Engineering / Approvals") || !strings.Contains(rec.Body.String(), "Platform Engineering / General") {
		t.Fatalf("the page does not offer the bot's channels: %s", rec.Body.String())
	}
	rec = h.do(h.session, asPerson(form("/notifications/channels", url.Values{"name": {"Approvals"}, "kind": {"bot"}, "bot_channel": {"team-1/19:approvals"}}), admin, "Admin"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Channel Approvals added") {
		t.Fatalf("add bot channel: %d %s", rec.Code, rec.Body.String())
	}
	h.do(h.session, asPerson(form("/notifications/subscriptions", url.Values{"channel": {"1"}, "topic": {"action.awaiting_approval"}}), admin, "Admin"))

	raw := `{"kind":"action.awaiting_approval","key":"run-1","title":"Restart beacon-api-prod waits for approval",
		"actions":[{"title":"Approve","path":"/v1/action-runs/r1/approve","style":"positive","done":"approved"},{"title":"Decline","path":"/v1/action-runs/r1/decline","style":"destructive","done":"declined"}]}`
	if rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(raw)), shared)); rec.Code != http.StatusAccepted {
		t.Fatalf("event: %d %s", rec.Code, rec.Body.String())
	}
	h.desk.Notices().SendDue(t.Context())
	posts := svc.Sent()
	if len(posts) != 1 {
		t.Fatalf("posted %d cards", len(posts))
	}
	card, _ := json.Marshal(posts[0])
	for _, want := range []string{`"channel":{"id":"19:approvals"}`, `"Action.Execute"`, `"verb":"act"`, `"title":"Approve"`} {
		if !strings.Contains(string(card), want) {
			t.Fatalf("the card lacks %s: %s", want, card)
		}
	}

	click := func(action int, extra map[string]any) map[string]any {
		value := map[string]any{"action": map[string]any{"type": "Action.Execute", "verb": "act", "data": map[string]any{"event": 1, "action": action}}}
		for k, v := range extra {
			value[k] = v
		}
		rec := activity(map[string]any{"type": "invoke", "name": "adaptiveCard/action", "id": "inv-1", "conversation": map[string]string{"id": "conv-1"}, "channelData": team, "value": value}, signed)
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("invoke: %d %s", rec.Code, rec.Body.String())
		}
		return got
	}
	// Not signed in yet: the card asks the person to sign in, with Teams' silent sign-in offered.
	got := click(0, nil)
	if got["statusCode"] != float64(401) || got["type"] != "application/vnd.microsoft.activity.loginRequest" || !strings.Contains(asJSON(got), "tokenExchangeResource") {
		t.Fatalf("not asked to sign in: %v", got)
	}
	if len(prod.calls) != 0 {
		t.Fatalf("the product was called before the person signed in: %v", prod.calls)
	}
	// Teams signs them in silently: the call is made with their own token and the card says who did it.
	got = click(0, map[string]any{"authentication": map[string]string{"token": teamstest.SSO}})
	if got["type"] != "application/vnd.microsoft.card.adaptive" || !strings.Contains(asJSON(got), "Ada approved this in Teams.") || strings.Contains(asJSON(got), "Action.Execute") {
		t.Fatalf("the card was not updated: %v", got)
	}
	if len(prod.calls) != 1 || prod.calls[0] != "/v1/action-runs/r1/approve Bearer "+teamstest.PersonToken {
		t.Fatalf("the product was not called as the person: %v", prod.calls)
	}
	// The product refuses the other button: only the person who clicked hears why.
	got = click(1, nil)
	if got["type"] != "application/vnd.microsoft.activity.message" || !strings.Contains(asJSON(got), "You may not decide this run") {
		t.Fatalf("the refusal was not said: %v", got)
	}
	// A button the event never had does nothing.
	if got = click(5, nil); !strings.Contains(asJSON(got), "no longer does anything") {
		t.Fatalf("a made-up button acted: %v", got)
	}

	// Someone writes to the bot: it says what it does.
	if rec := activity(map[string]any{"type": "message", "id": "m1", "text": "hi", "conversation": map[string]string{"id": "conv-1"}, "channelData": team}, signed); rec.Code != http.StatusOK || len(svc.Said()) != 1 {
		t.Fatalf("no reply: %d %d", rec.Code, len(svc.Said()))
	}
	// Taken out of the team: its channels are no longer offered.
	activity(map[string]any{"type": "installationUpdate", "action": "remove", "channelData": team}, signed)
	if rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/notifications", nil), admin, "Admin")); strings.Contains(rec.Body.String(), "Platform Engineering / Approvals") {
		t.Fatal("a team the bot left is still offered")
	}
}

func asJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
