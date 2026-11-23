package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
	"github.com/orchard-suite/dispatch/internal/teams"
	"github.com/orchard-suite/dispatch/internal/teams/teamstest"
)

type inbox struct {
	mu   sync.Mutex
	sent []string
}

func (i *inbox) Kind() string { return "fake" }

func (i *inbox) Send(_ context.Context, to string, l notice.Letter) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.sent = append(i.sent, to+" "+l.Subject)
	return 202, nil
}

func (i *inbox) got() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string{}, i.sent...)
}

func event(t *testing.T, h *harness, body string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body)), shared))
}

func TestPeopleGroupsChainsAndTheAcknowledgeLink(t *testing.T) {
	h := newHarness(t)
	mail := &inbox{}
	box := h.desk.Notices()
	box.UseMail(mail)
	box.UseDesk(publicURL)
	ctx := context.Background()

	// Signing in keeps the person; only an admin opens People.
	if rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/", nil), member, "User")); rec.Code != http.StatusOK {
		t.Fatalf("desk: %d", rec.Code)
	}
	if rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/people", nil), member, "User")); rec.Code != http.StatusForbidden {
		t.Fatalf("a member opened People: %d", rec.Code)
	}
	rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/people", nil), admin, "Admin"))
	if !strings.Contains(rec.Body.String(), member) || !strings.Contains(rec.Body.String(), "</html>") {
		t.Fatalf("the signed-in member is not kept: %s", rec.Body.String())
	}
	post := func(path string, v url.Values) *httptest.ResponseRecorder {
		return h.do(h.session, asPerson(form(path, v), admin, "Admin"))
	}
	if rec := post("/people", url.Values{"email": {"Lead@Example.com"}, "name": {"Lead"}, "via": {"email"}}); rec.Code != http.StatusOK {
		t.Fatalf("person: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/groups", url.Values{"slug": {"platform"}, "name": {"Platform"}, "members": {member + ", lead@example.com"}}); rec.Code != http.StatusOK {
		t.Fatalf("group: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("/chains", url.Values{"slug": {"oncall"}, "name": {"On call"}, "steps": {"5 group:nobody"}}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "There is no group nobody") {
		t.Fatalf("a chain naming no group: %d", rec.Code)
	}
	if rec := post("/chains", url.Values{"slug": {"oncall"}, "name": {"On call"}, "steps": {"5 person:lead@example.com\n15 group:platform"}}); rec.Code != http.StatusOK {
		t.Fatalf("chain: %d %s", rec.Code, rec.Body.String())
	}

	// The API: an unknown group is refused by name; a chain starts a page.
	if rec := event(t, h, `{"kind":"vantage.drift","key":"d1","title":"Drift","to":[{"group":"nobody"}]}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "group:nobody") {
		t.Fatalf("unknown group: %d %s", rec.Code, rec.Body.String())
	}
	rec = event(t, h, `{"kind":"vantage.drift","key":"d2","title":"Drift in Shop","to":[{"chain":"oncall"},{"person":"outside@example.com"}]}`)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"messages":2`) || !strings.Contains(rec.Body.String(), `"pages":1`) {
		t.Fatalf("chain event: %d %s", rec.Code, rec.Body.String())
	}
	box.SendDue(ctx)
	if got := mail.got(); len(got) != 2 {
		t.Fatalf("mailed %v", got)
	}

	// The link every chain message carries opens the page; acknowledging it
	// needs the desk's own origin.
	rec = h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/pages/1", nil), member, "User"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Drift in Shop") || !strings.Contains(rec.Body.String(), "Acknowledge") {
		t.Fatalf("page: %d %s", rec.Code, rec.Body.String())
	}
	foreign := asPerson(form("/pages/1/acknowledge", nil), member, "User")
	foreign.Header.Set("Origin", "https://evil.test")
	if rec := h.do(h.session, foreign); rec.Code != http.StatusForbidden {
		t.Fatalf("a foreign origin acknowledged: %d", rec.Code)
	}
	rec = h.do(h.session, asPerson(form("/pages/1/acknowledge", nil), member, "User"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "You have it") || !strings.Contains(rec.Body.String(), "Acknowledged by "+personName(member)) {
		t.Fatalf("acknowledge: %d %s", rec.Code, rec.Body.String())
	}
	p, _ := h.desk.Store.Page(ctx, 1)
	if p.State != notice.PageAcknowledged || p.AcknowledgedBy != member {
		t.Errorf("page %+v", p)
	}
	rec = h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/notifications", nil), admin, "Admin"))
	if !strings.Contains(rec.Body.String(), "Drift in Shop") || !strings.Contains(rec.Body.String(), "to Outside by Email") {
		t.Errorf("notifications does not list the page and messages")
	}
}

func TestAcknowledgingAnIncidentEndsItsPages(t *testing.T) {
	h := newHarness(t)
	box := h.desk.Notices()
	box.UseMail(&inbox{})
	ctx := context.Background()
	if err := h.desk.Store.SaveChain(ctx, notice.Chain{Slug: "oncall", Name: "On call", Steps: []notice.Step{{Targets: []notice.Target{{Person: "lead@example.com"}}, Wait: 5}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := box.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Chain: "oncall"}, Topics: []string{"incident.opened"}}); err != nil {
		t.Fatal(err)
	}
	rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(`{"title":"Checkout failing","severity":"critical","service":"shop"}`)), shared))
	if rec.Code != http.StatusCreated {
		t.Fatalf("open: %d", rec.Code)
	}
	// The desk announces the incident on its own goroutine.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p, err := h.desk.Store.Page(ctx, 1); err == nil && p.State == notice.PageActive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no page started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/incidents/1/acknowledge", strings.NewReader(`{}`)), shared)); rec.Code != http.StatusOK {
		t.Fatalf("acknowledge: %d %s", rec.Code, rec.Body.String())
	}
	p, _ := h.desk.Store.Page(ctx, 1)
	if p.State != notice.PageAcknowledged {
		t.Errorf("page %+v", p)
	}
}

func TestTheBotFindsAPersonAndChatsWithThem(t *testing.T) {
	h := newHarness(t)
	svc := teamstest.New(t)
	svc.Roster = []teams.Member{{ID: "29:lead", AadObjectID: "aad-lead", UserPrincipalName: "Lead@example.com"}}
	h.srv.UseBot(BotConfig{Bot: svc.Bot(t), ActsFor: "orrery"})
	sender := h.srv.BotSender()
	ctx := context.Background()
	if _, err := sender.SendPerson(ctx, notice.Outgoing{Person: notice.Person{Email: "lead@example.com"}, Event: notice.Event{Title: "x"}, Ack: ""}); !errors.Is(err, notice.ErrUnreachable) {
		t.Fatalf("with the bot in no team: %v", err)
	}
	if err := h.desk.Store.SaveBotTeam(ctx, storeTeam(svc.URL)); err != nil {
		t.Fatal(err)
	}
	if err := h.desk.Store.SavePerson(ctx, notice.Person{Email: "lead@example.com"}); err != nil {
		t.Fatal(err)
	}
	status, err := sender.SendPerson(ctx, notice.Outgoing{Person: notice.Person{Email: "lead@example.com"}, Event: notice.Event{Producer: "vantage", Title: "Drift in Shop"}, Ack: publicURL + "/pages/1"})
	if err != nil || status >= 300 {
		t.Fatalf("send: %d %v", status, err)
	}
	opened := svc.Sent()
	if len(opened) != 1 || opened[0]["isGroup"] != false || !strings.Contains(stringOf(opened[0]["members"]), "29:lead") {
		t.Fatalf("chat opened %v", opened)
	}
	said := svc.Said()
	if len(said) != 1 || !strings.Contains(stringOf(said[0]), "Drift in Shop") || !strings.Contains(stringOf(said[0]), publicURL+"/pages/1") {
		t.Fatalf("posted %v", said)
	}
	if p, _ := h.desk.Store.Person(ctx, "lead@example.com"); p.TeamsID != "29:lead" {
		t.Errorf("Teams id not kept: %+v", p)
	}
	if _, err := sender.SendPerson(ctx, notice.Outgoing{Person: notice.Person{Email: "stranger@example.com"}, Event: notice.Event{Title: "x"}, Ack: ""}); !errors.Is(err, notice.ErrUnreachable) {
		t.Errorf("someone in no team: %v", err)
	}
}

func storeTeam(serviceURL string) store.BotTeam {
	return store.BotTeam{ID: "team-1", Name: "Platform", TenantID: teamstest.TenantID, ServiceURL: serviceURL, AddedAt: time.Now()}
}

func stringOf(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestAPersonSetsHowTheyAreReached(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rec := h.do(h.session, asPerson(form("/me", url.Values{
		"email": {"someone-else@example.com"}, "name": {"Member"}, "phone": {"+1 (555) 555-0100"}, "sms_consent": {"yes"}, "discord_id": {"123456789012345678"},
		"rule_page": {"sms, email+5"}, "quiet_from": {"22:00"}, "quiet_to": {"07:00"}, "quiet_zone": {"America/New_York"},
	}), member, "User"))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := h.desk.Store.Person(ctx, "someone-else@example.com"); err == nil {
		t.Error("the form's address was used instead of the front door's")
	}
	p, err := h.desk.Store.Person(ctx, member)
	if err != nil || p.Phone != "+15555550100" || p.DiscordID != "123456789012345678" || p.Quiet == nil || p.Quiet.To != "07:00" {
		t.Fatalf("saved %+v %v", p, err)
	}
	if steps := p.Rules[notice.WeightPage]; len(steps) != 2 || steps[1].After != 5 {
		t.Errorf("page rule %+v", p.Rules)
	}
	if _, set := p.Rules[notice.WeightNotify]; set {
		t.Error("a blank rule keeps the default")
	}
	// A row posts only its own setting; the rest stay.
	h.do(h.session, asPerson(form("/me", url.Values{"quiet_from": {""}, "quiet_to": {""}}), member, "User"))
	if p, _ := h.desk.Store.Person(ctx, member); p.Quiet != nil || p.Phone != "+15555550100" || len(p.Rules[notice.WeightPage]) != 2 {
		t.Errorf("one row changed more than its setting: %+v", p)
	}
	h.do(h.session, asPerson(form("/me", url.Values{"rule_page": {""}}), member, "User"))
	if p, _ := h.desk.Store.Person(ctx, member); p.Rules != nil || p.Phone == "" {
		t.Errorf("a blank rule goes back to the default and keeps the rest: %+v", p)
	}
	if rec := h.do(h.session, asPerson(form("/me", url.Values{"phone": {"555-0100"}}), member, "User")); rec.Code != http.StatusBadRequest {
		t.Errorf("a number without its country: %d", rec.Code)
	}
	page := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/me", nil), member, "User"))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "+15555550100") {
		t.Errorf("the page shows the number: %d", page.Code)
	}
}

func TestTextsNeedThePersonsOwnOptIn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	me := func(v url.Values) *httptest.ResponseRecorder {
		return h.do(h.session, asPerson(form("/me", v), member, "User"))
	}

	rec := me(url.Values{"phone": {"+15555550100"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Text me alerts at this number") {
		t.Fatalf("a number without the box: %d", rec.Code)
	}
	if p, err := h.desk.Store.Person(ctx, member); err == nil && p.Phone != "" {
		t.Fatalf("a number was kept without consent: %+v", p)
	}

	before := time.Now().Add(-time.Second)
	if rec := me(url.Values{"phone": {"+15555550100"}, "sms_consent": {"yes"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("a number with the box: %d %s", rec.Code, rec.Body.String())
	}
	p, err := h.desk.Store.Person(ctx, member)
	if err != nil || p.Phone != "+15555550100" || p.SMSConsentAt == nil || p.SMSConsentAt.Before(before) {
		t.Fatalf("consent not kept: %+v %v", p, err)
	}
	page := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/me", nil), member, "User")).Body.String()
	if !strings.Contains(page, "you opted in on <time") || !strings.Contains(page, ">"+p.SMSConsentAt.Format("January 2, 2006")+"</time>") {
		t.Error("Me does not say when they opted in")
	}

	// Another row keeps the number and its consent.
	me(url.Values{"name": {"Member"}})
	if p, _ := h.desk.Store.Person(ctx, member); p.SMSConsentAt == nil {
		t.Error("saving another row dropped consent")
	}

	// An admin who changes the number on People drops the consent with it.
	if rec := h.do(h.session, asPerson(form("/people", url.Values{"email": {member}, "phone": {"+15555550199"}}), admin, "User,Admin")); rec.Code != http.StatusOK {
		t.Fatalf("admin save: %d", rec.Code)
	}
	if p, _ := h.desk.Store.Person(ctx, member); p.Phone != "+15555550199" || p.SMSConsentAt != nil {
		t.Errorf("a new number kept the old consent: %+v", p)
	}

	me(url.Values{"phone": {"+15555550100"}, "sms_consent": {"yes"}})
	if rec := me(url.Values{"phone": {""}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("clearing needs no box: %d", rec.Code)
	}
	if p, _ := h.desk.Store.Person(ctx, member); p.Phone != "" || p.SMSConsentAt != nil {
		t.Errorf("clearing the number kept consent: %+v", p)
	}
}

func TestTheOptInNamesTheSenderAndPolicy(t *testing.T) {
	h := newHarness(t)
	words := "By checking this box, you agree to receive operational alert text messages from %s at this number. Message frequency varies with on-call schedules and incidents. Message and data rates may apply. Reply STOP to opt out or HELP for help."
	page := func() string {
		return h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/me", nil), member, "User")).Body.String()
	}

	got := page()
	if !strings.Contains(got, fmt.Sprintf(words, "this desk")) {
		t.Error("with no sender the wording does not say this desk")
	}
	if strings.Contains(got, "SMS policy") {
		t.Error("a policy link with no policy set")
	}

	h.srv.cfg.SMSSenderName = "Example Ops"
	h.srv.cfg.SMSPolicyURL = "https://example.com/sms"
	got = page()
	if !strings.Contains(got, fmt.Sprintf(words, "Example Ops")) {
		t.Error("the wording does not name the sender")
	}
	if !strings.Contains(got, `href="https://example.com/sms"`) {
		t.Error("the wording does not link the policy")
	}
}

func TestAnSMSPolicyIsAWebAddress(t *testing.T) {
	h := newHarness(t)
	if _, err := New(Config{PublicURL: publicURL, SMSPolicyURL: "not a url"}, h.desk, h.srv.metrics, h.srv.log); err == nil {
		t.Error("a policy that is not a URL was taken")
	}
}
