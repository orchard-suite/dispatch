package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/store"
)

const (
	// guest signs in at the front door and holds no desk role.
	guest      = "guest@example.com"
	guestRoles = "Viewer"
	twoKinds   = `{"kinds": [
  {"kind": "action.awaiting_approval", "title": "A request waits for your approval", "description": "Someone asked for a change that you can approve.", "weight": "notify", "producer": "someone_else"},
  {"kind": "action.failed", "title": "A run you asked for failed", "weight": "page"}
]}`
)

func putKinds(h *harness, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/v1/kinds", strings.NewReader(body))
	if token != "" {
		bearer(req, token)
	}
	return h.do(h.api, req)
}

// producerToken makes a token from Settings, whose name is the producer.
func producerToken(t *testing.T, h *harness, name string) string {
	t.Helper()
	_, secret, err := h.desk.Store.MintToken(context.Background(), name, admin)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestAProducerDeclaresItsKinds(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.desk.Store
	approvals := producerToken(t, h, "approvals")

	if rec := putKinds(h, "", twoKinds); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	// A header never sets identity on the API port.
	req := asPerson(httptest.NewRequest(http.MethodPut, "/v1/kinds", strings.NewReader(twoKinds)), admin, "Admin")
	if rec := h.do(h.api, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("identity headers: %d", rec.Code)
	}
	rec := putKinds(h, approvals, twoKinds)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"kinds":2}` {
		t.Fatalf("declare: %d %s", rec.Code, rec.Body.String())
	}
	kinds, err := st.DeclaredKinds(ctx)
	if err != nil || len(kinds) != 2 {
		t.Fatalf("kinds %+v %v", kinds, err)
	}
	// The producer is the token's name, never the body's word.
	want := notice.DeclaredKind{Producer: "approvals", Kind: "action.awaiting_approval", Title: "A request waits for your approval",
		Description: "Someone asked for a change that you can approve.", Weight: notice.WeightNotify}
	if kinds[0] != want || kinds[1].Producer != "approvals" {
		t.Errorf("kept %+v", kinds)
	}

	// A kind another producer declared is refused by name.
	other := producerToken(t, h, "catalog")
	rec = putKinds(h, other, `{"kinds": [{"kind": "catalog.changed", "title": "A thing changed", "weight": "notice"}, {"kind": "action.failed", "title": "Mine", "weight": "notice"}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "another producer declared this kind: action.failed") {
		t.Errorf("a taken kind: %d %s", rec.Code, rec.Body.String())
	}
	// A personal token cannot declare: anyone on the desk can make one.
	_, personal, err := st.MintPersonToken(ctx, "agent", member)
	if err != nil {
		t.Fatal(err)
	}
	if rec := putKinds(h, personal, `{"kinds": []}`); rec.Code != http.StatusForbidden {
		t.Errorf("a personal token: %d", rec.Code)
	}
	long := fmt.Sprintf(`{"kinds": [{"kind": "action.x", "title": %q, "weight": "notify"}]}`, strings.Repeat("a", 121))
	var many []string
	for i := range notice.MaxKinds + 1 {
		many = append(many, fmt.Sprintf(`{"kind": "action.k%d", "title": "A kind", "weight": "notice"}`, i))
	}
	for name, body := range map[string]string{
		"not JSON":           `kinds`,
		"no kinds field":     `{}`,
		"a null kinds field": `{"kinds": null}`,
		"a bad kind":         `{"kinds": [{"kind": "Approval", "title": "A", "weight": "notify"}]}`,
		"a bad weight":       `{"kinds": [{"kind": "action.x", "title": "A", "weight": "loud"}]}`,
		"no title":           `{"kinds": [{"kind": "action.x", "weight": "notify"}]}`,
		"a long title":       long,
		"too many kinds":     `{"kinds": [` + strings.Join(many, ",") + `]}`,
	} {
		if rec := putKinds(h, approvals, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if kinds, _ := st.DeclaredKinds(ctx); len(kinds) != 2 {
		t.Fatalf("a refused body changed the set: %+v", kinds)
	}

	// The body replaces the producer's whole set. A kept kind keeps what
	// people chose for it; a dropped kind takes its choices with it.
	if err := st.LearnPerson(ctx, member, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"action.awaiting_approval", "action.failed"} {
		if err := st.SetChoice(ctx, member, notice.Choice{Kind: kind}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	rec = putKinds(h, approvals, `{"kinds": [{"kind": "action.awaiting_approval", "title": "Your approval is needed", "weight": "notify"}]}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"kinds":1}` {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body.String())
	}
	kinds, _ = st.DeclaredKinds(ctx)
	if len(kinds) != 1 || kinds[0].Title != "Your approval is needed" || kinds[0].Description != "" {
		t.Errorf("replaced %+v", kinds)
	}
	if got, _ := st.Choices(ctx, member); len(got) != 1 || got["action.awaiting_approval"].Kind == "" {
		t.Errorf("choices after replacing: %+v", got)
	}
	// The freed kind is now anyone's to declare.
	if rec := putKinds(h, other, `{"kinds": [{"kind": "action.failed", "title": "Mine", "weight": "notice"}]}`); rec.Code != http.StatusOK {
		t.Errorf("a freed kind: %d %s", rec.Code, rec.Body.String())
	}
	// An empty list clears the producer's set.
	if rec := putKinds(h, approvals, `{"kinds": []}`); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"kinds":0}` {
		t.Errorf("empty: %d %s", rec.Code, rec.Body.String())
	}
	if kinds, _ := st.DeclaredKinds(ctx); len(kinds) != 1 || kinds[0].Producer != "catalog" {
		t.Errorf("after clearing: %+v", kinds)
	}
}

// meAs opens Me as a person.
func meAs(h *harness, email, roles string) *httptest.ResponseRecorder {
	return h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/me", nil), email, roles))
}

func choose(h *harness, email, roles string, values url.Values) *httptest.ResponseRecorder {
	return h.do(h.session, asPerson(form("/me/kinds", values), email, roles))
}

func TestAPersonChoosesHowTheyHearOfEachKind(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.desk.Store
	mail := &inbox{}
	box := h.desk.Notices()
	box.UseMail(mail)
	box.UseMedium(notice.SMS, &smsSent{})
	box.UseDesk(publicURL)
	approvals := producerToken(t, h, "approvals")
	if rec := putKinds(h, approvals, twoKinds); rec.Code != http.StatusOK {
		t.Fatalf("declare: %d %s", rec.Code, rec.Body.String())
	}
	if rec := putKinds(h, producerToken(t, h, "catalog"), `{"kinds": [{"kind": "catalog.changed", "title": "A thing you own changed", "weight": "notice"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("declare: %d", rec.Code)
	}

	// One row per kind, grouped by the product that sends it.
	page := meAs(h, member, "User")
	body := page.Body.String()
	if page.Code != http.StatusOK {
		t.Fatalf("me: %d", page.Code)
	}
	for _, want := range []string{
		"From approvals", "From catalog", "A request waits for your approval", "Someone asked for a change that you can approve.",
		"A thing you own changed", `name="list_action.awaiting_approval"`,
		// Email is set up. SMS is not, so it shows and cannot be ticked.
		`name="m_action.awaiting_approval" value="email" data-medium>`,
		`name="m_action.awaiting_approval" value="sms" data-medium disabled>`,
		"Add an SMS number below to use this",
		// A kind sent as a page is fixed, with the reason.
		"Fixed: a page always goes by your page rule",
		`name="m_action.failed" value="email" data-medium disabled>`,
		`name="list_action.failed" value="on" data-list disabled>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the grid lacks %q", want)
		}
	}
	if strings.Contains(body, `name="kind" value="action.failed"`) {
		t.Error("a kind sent as a page can be posted")
	}
	if strings.Index(body, "From approvals") > strings.Index(body, "From catalog") {
		t.Error("the groups are out of order")
	}

	event := func(key, weight string) {
		t.Helper()
		body := fmt.Sprintf(`{"kind": "action.awaiting_approval", "key": %q, "title": "Restart the shop waits for you", "weight": %q, "to": [{"person": %q}]}`, key, weight, member)
		rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(body)), approvals))
		if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
			t.Fatalf("event: %d %s", rec.Code, rec.Body.String())
		}
		box.SendDue(ctx)
	}
	// Left alone, the kind goes by the person's notify rule: email.
	event("k1", "notify")
	if len(mail.got()) != 1 {
		t.Fatalf("by the rules: %v", mail.got())
	}

	// List only: saved, shown ticked, sends nothing, still on the list.
	rec := choose(h, member, "User", url.Values{"kind": {"action.awaiting_approval", "catalog.changed"}, "list_action.awaiting_approval": {"on"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/me?saved" {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := st.Choices(ctx, member)
	if c, set := got["action.awaiting_approval"]; len(got) != 1 || !set || !c.ListOnly() {
		t.Fatalf("choices %+v", got)
	}
	if body := meAs(h, member, "User").Body.String(); !strings.Contains(body, `name="list_action.awaiting_approval" value="on" data-list checked>`) {
		t.Error("the saved row does not show ticked")
	}
	event("k2", "notify")
	if len(mail.got()) != 1 {
		t.Errorf("list only sent mail: %v", mail.got())
	}
	if lines, _ := st.Inbox(ctx, member, 50); len(lines) != 2 {
		t.Errorf("list only is still on the list: %d", len(lines))
	}
	// A page is never turned down, whatever the row says.
	event("k3", "page")
	if len(mail.got()) != 2 {
		t.Errorf("a page was held back by a choice: %v", mail.got())
	}

	// A medium, then both at once, then a medium the person has not set up.
	if rec := choose(h, member, "User", url.Values{"kind": {"action.awaiting_approval"}, "m_action.awaiting_approval": {"email"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d", rec.Code)
	}
	if got, _ := st.Choices(ctx, member); len(got["action.awaiting_approval"].Media) != 1 || got["action.awaiting_approval"].Media[0] != notice.Email {
		t.Errorf("choices %+v", got)
	}
	if body := meAs(h, member, "User").Body.String(); !strings.Contains(body, `name="m_action.awaiting_approval" value="email" data-medium checked>`) {
		t.Error("the chosen medium does not show ticked")
	}
	for name, values := range map[string]url.Values{
		"both list only and a medium": {"kind": {"action.awaiting_approval"}, "m_action.awaiting_approval": {"email"}, "list_action.awaiting_approval": {"on"}},
		"a medium not set up":         {"kind": {"action.awaiting_approval"}, "m_action.awaiting_approval": {"sms"}},
		"a medium that is not one":    {"kind": {"action.awaiting_approval"}, "m_action.awaiting_approval": {"pigeon"}},
	} {
		if rec := choose(h, member, "User", values); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	// A kind sent as a page, and a kind nobody declared, are never kept.
	if rec := choose(h, member, "User", url.Values{"kind": {"action.failed", "action.unknown", "action.awaiting_approval"}, "list_action.failed": {"on"}, "list_action.unknown": {"on"}, "m_action.awaiting_approval": {"email"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d", rec.Code)
	}
	if got, _ := st.Choices(ctx, member); len(got) != 1 {
		t.Errorf("a page kind was set: %+v", got)
	}
	// Nothing ticked goes back to the rules.
	if rec := choose(h, member, "User", url.Values{"kind": {"action.awaiting_approval"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear: %d", rec.Code)
	}
	if got, _ := st.Choices(ctx, member); len(got) != 0 {
		t.Errorf("cleared %+v", got)
	}

	// A change from a foreign origin, or with no identity, is refused.
	req := asPerson(form("/me/kinds", url.Values{"kind": {"action.awaiting_approval"}, "list_action.awaiting_approval": {"on"}}), member, "User")
	req.Header.Set("Origin", "https://evil.test")
	if rec := h.do(h.session, req); rec.Code != http.StatusForbidden {
		t.Errorf("foreign origin: %d", rec.Code)
	}
	if rec := h.do(h.session, form("/me/kinds", url.Values{"kind": {"action.awaiting_approval"}})); rec.Code != http.StatusUnauthorized {
		t.Errorf("no identity: %d", rec.Code)
	}
	if rec := h.do(h.session, bearer(form("/me/kinds", url.Values{"kind": {"action.awaiting_approval"}}), approvals)); rec.Code != http.StatusUnauthorized {
		t.Errorf("a bearer token on the session port: %d", rec.Code)
	}
	if got, _ := st.Choices(ctx, member); len(got) != 0 {
		t.Errorf("a refused change was kept: %+v", got)
	}
}

func TestMeIsOpenToEveryoneWhoSignsIn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	st := h.desk.Store
	box := h.desk.Notices()
	box.UseMail(&inbox{})
	if rec := putKinds(h, producerToken(t, h, "approvals"), twoKinds); rec.Code != http.StatusOK {
		t.Fatalf("declare: %d", rec.Code)
	}
	as := func(method, path string) int {
		return h.do(h.session, asPerson(httptest.NewRequest(method, path, nil), guest, guestRoles)).Code
	}

	// Me opens, with the grid and nothing of the desk around it.
	page := meAs(h, guest, guestRoles)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "A request waits for your approval") || !strings.Contains(body, "How you are reached") {
		t.Fatalf("me: %d", page.Code)
	}
	for _, desk := range []string{"data-open-menu", "data-stream", `href="/history"`, `href="/settings"`, "Your shifts", "Your agents", "/me/token", "/me/calendar", "Open incident", "on call"} {
		if strings.Contains(body, desk) {
			t.Errorf("someone with no desk role sees %q", desk)
		}
	}
	// Looking is not joining: the directory learns them when they save.
	if _, err := st.Person(ctx, guest); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a look at Me made a directory row: %v", err)
	}
	// Home sends them to Me; every other page and change stays shut.
	rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, "/", nil), guest, guestRoles))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/me" {
		t.Errorf("home: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	for _, path := range []string{"/history", "/live", "/people", "/schedules", "/settings", "/status", "/components", "/incidents/1", "/pages/1"} {
		if code := as(http.MethodGet, path); code != http.StatusForbidden {
			t.Errorf("GET %s with no desk role: %d", path, code)
		}
	}
	for _, path := range []string{"/me/token", "/me/calendar", "/incidents", "/people"} {
		if code := as(http.MethodPost, path); code != http.StatusForbidden {
			t.Errorf("POST %s with no desk role: %d", path, code)
		}
	}
	if code := as(http.MethodGet, "/desk.js"); code != http.StatusOK {
		t.Errorf("the page's script: %d", code)
	}

	// The first save of the grid makes their row, with no role and no rule.
	if rec := choose(h, guest, guestRoles, url.Values{"kind": {"action.awaiting_approval"}, "list_action.awaiting_approval": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("grid: %d %s", rec.Code, rec.Body.String())
	}
	p, err := st.Person(ctx, guest)
	if err != nil || p.Rules != nil || p.Phone != "" {
		t.Fatalf("after the first save: %+v %v", p, err)
	}
	if got, _ := st.Choices(ctx, guest); !got["action.awaiting_approval"].ListOnly() || len(got) != 1 {
		t.Errorf("choices %+v", got)
	}
	// So does a first save of how they are reached.
	other := "second-guest@example.com"
	if rec := h.do(h.session, asPerson(form("/me", url.Values{"phone": {"+1 555 555 0100"}, "sms_consent": {"yes"}}), other, "")); rec.Code != http.StatusSeeOther {
		t.Fatalf("contact details: %d %s", rec.Code, rec.Body.String())
	}
	if p, err := st.Person(ctx, other); err != nil || p.Phone != "+15555550100" {
		t.Errorf("saved %+v %v", p, err)
	}

	// The checks that guard every session route still guard Me.
	if code := h.do(h.session, httptest.NewRequest(http.MethodGet, "/me", nil)).Code; code != http.StatusUnauthorized {
		t.Errorf("no identity: %d", code)
	}
	if code := h.do(h.session, bearer(httptest.NewRequest(http.MethodGet, "/me", nil), shared)).Code; code != http.StatusUnauthorized {
		t.Errorf("a bearer token on the session port: %d", code)
	}
	req := asPerson(form("/me", url.Values{"phone": {"+1 555 555 0111"}}), guest, guestRoles)
	req.Header.Set("Origin", "https://evil.test")
	if code := h.do(h.session, req).Code; code != http.StatusForbidden {
		t.Errorf("foreign origin: %d", code)
	}

	// Someone on the desk still gets the whole page.
	full := meAs(h, member, "User").Body.String()
	for _, desk := range []string{"data-open-menu", "data-stream", `href="/history"`, "Your shifts", "Your agents"} {
		if !strings.Contains(full, desk) {
			t.Errorf("a desk member's Me lacks %q", desk)
		}
	}
}

func TestKindsAndChoicesReachTheAnnals(t *testing.T) {
	fake := newFakeAnnals(t)
	h := newHarnessWith(t, func(log *slog.Logger) *annals.Poster {
		return annals.New(fake.url, "annals-token", log, func(context.Context, string) {})
	})
	h.desk.Notices().UseMail(&inbox{})
	if rec := putKinds(h, producerToken(t, h, "approvals"), twoKinds); rec.Code != http.StatusOK {
		t.Fatalf("declare: %d", rec.Code)
	}
	ev := fake.next(t)
	if ev.Action.Verb != "kinds.declare" || ev.Action.Target.ID != "approvals" || ev.Actor.ID != "approvals" || ev.Labels["kinds"] != "2" || ev.Labels["dropped"] != "0" {
		t.Errorf("kinds declared: %+v", ev)
	}
	// Each row that changed is one decision; a row saved as it was is none.
	values := url.Values{"kind": {"action.awaiting_approval"}, "m_action.awaiting_approval": {"email"}}
	for range 2 {
		if rec := choose(h, guest, guestRoles, values); rec.Code != http.StatusSeeOther {
			t.Fatalf("choose: %d %s", rec.Code, rec.Body.String())
		}
	}
	ev = fake.next(t)
	if ev.Action.Verb != "person.choose" || ev.Action.Target.ID != guest || ev.Actor.ID != guest || ev.Labels["kind"] != "action.awaiting_approval" || ev.Labels["media"] != "email" {
		t.Errorf("chose: %+v", ev)
	}
	if rec := choose(h, guest, guestRoles, url.Values{"kind": {"action.awaiting_approval"}, "list_action.awaiting_approval": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("list only: %d", rec.Code)
	}
	if ev := fake.next(t); ev.Action.Verb != "person.choose" || ev.Labels["media"] != "list" {
		t.Errorf("list only, and the unchanged save posted nothing: %+v", ev)
	}
	if rec := choose(h, guest, guestRoles, url.Values{"kind": {"action.awaiting_approval"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear: %d", rec.Code)
	}
	if ev := fake.next(t); ev.Action.Verb != "person.choose" || ev.Labels["media"] != "rules" {
		t.Errorf("back to the rules: %+v", ev)
	}
}
