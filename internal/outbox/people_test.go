package outbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

// fakeMail records every letter and refuses addresses on its list.
type fakeMail struct {
	mu      sync.Mutex
	sent    []string
	letters []notice.Letter
	refuse  map[string]int
}

func (f *fakeMail) Kind() string { return "fake" }

func (f *fakeMail) Send(_ context.Context, to string, l notice.Letter) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status, ok := f.refuse[to]; ok {
		return status, fmt.Errorf("refused %d", status)
	}
	f.sent = append(f.sent, to)
	f.letters = append(f.letters, l)
	return 202, nil
}

func (f *fakeMail) to() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string{}, f.sent...)
	slices.Sort(out)
	return out
}

// fakeTeams knows only the people in known.
type fakeTeams struct {
	mu    sync.Mutex
	known map[string]bool
	sent  []string
}

func (f *fakeTeams) SendPerson(_ context.Context, out notice.Outgoing) (int, error) {
	p := out.Person
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.known[p.Email] {
		return 0, fmt.Errorf("%w: not in Teams", notice.ErrUnreachable)
	}
	f.sent = append(f.sent, p.Email)
	return 201, nil
}

func peopleSetup(t *testing.T) (*Outbox, *fakeMail, *fakeTeams, *time.Time, map[string]int) {
	t.Helper()
	o, st, now := setup(t, &hook{status: 200})
	m := &fakeMail{refuse: map[string]int{}}
	tm := &fakeTeams{known: map[string]bool{}}
	o.UseMail(m)
	o.UseTeams(tm)
	o.UseDesk("https://dispatch.test")
	counts := map[string]int{}
	var mu sync.Mutex
	count := func(prefix string) func(context.Context, string) {
		return func(_ context.Context, outcome string) {
			mu.Lock()
			counts[prefix+outcome]++
			mu.Unlock()
		}
	}
	o.UseCounters(count("message."), count("page."))
	ctx := context.Background()
	for _, p := range []notice.Person{
		{Email: "ann@example.com", Name: "Ann"},
		{Email: "bo@example.com", Rules: notice.Rules{notice.WeightPage: {{Medium: notice.Email}}, notice.WeightNotify: {{Medium: notice.Email}}}},
		{Email: "cy@example.com", Rules: notice.Rules{notice.WeightPage: {{Medium: notice.TeamsChat}}, notice.WeightNotify: {{Medium: notice.TeamsChat}}}},
	} {
		if err := st.SavePerson(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveGroup(ctx, notice.Group{Slug: "platform", Name: "Platform", Members: []string{"ann@example.com", "bo@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveChain(ctx, notice.Chain{Slug: "oncall", Name: "On call", Steps: []notice.Step{
		{Targets: []notice.Target{{Person: "ann@example.com"}}, Wait: 10},
		{Targets: []notice.Target{{Group: "platform"}, {Person: "cy@example.com"}}, Wait: 20},
	}}); err != nil {
		t.Fatal(err)
	}
	return o, m, tm, now, counts
}

func event(key string, to ...notice.Target) notice.Event {
	return notice.Event{Producer: "vantage", Kind: "vantage.mention", Key: key, Title: "You were mentioned", To: to}
}

func TestAddressingReachesEachPersonOnceByTheirMedia(t *testing.T) {
	o, m, tm, _, counts := peopleSetup(t)
	ctx := context.Background()
	tm.known["ann@example.com"] = true
	got, err := o.Accept(ctx, event("k1", notice.Target{Person: "ANN@example.com"}, notice.Target{Group: "platform"}, notice.Target{Person: "stranger@example.com"}, notice.Target{Person: "cy@example.com"}))
	if err != nil {
		t.Fatal(err)
	}
	// Ann: email and Teams. Bo: email only. A stranger: email only. Cy: Teams only.
	if got.Messages != 5 || got.Pages != 0 {
		t.Fatalf("accepted %+v", got)
	}
	o.SendDue(ctx)
	if want := []string{"ann@example.com", "bo@example.com", "stranger@example.com"}; !slices.Equal(m.to(), want) {
		t.Errorf("mailed %v, want %v", m.to(), want)
	}
	if !slices.Equal(tm.sent, []string{"ann@example.com"}) {
		t.Errorf("Teams %v", tm.sent)
	}
	if counts["message.sent"] != 4 || counts["message.skipped"] != 1 {
		t.Errorf("counts %v", counts)
	}
	// A repeat of the key reaches nobody again.
	again, _ := o.Accept(ctx, event("k1", notice.Target{Person: "ann@example.com"}))
	if !again.Repeat || again.Messages != 0 {
		t.Errorf("repeat %+v", again)
	}
}

func TestAddressingRefusesAnUnknownGroupOrChain(t *testing.T) {
	o, _, _, _, _ := peopleSetup(t)
	for _, to := range []notice.Target{{Group: "nobody"}, {Chain: "nothing"}} {
		_, err := o.Accept(context.Background(), event("k-"+to.String(), to))
		if !errors.Is(err, notice.ErrInvalid) || !strings.Contains(err.Error(), to.String()) {
			t.Errorf("%s: %v", to, err)
		}
	}
	if _, err := o.Accept(context.Background(), event("bad", notice.Target{Person: "not an address"})); !errors.Is(err, notice.ErrInvalid) {
		t.Errorf("a bad address: %v", err)
	}
}

func TestASubscriptionSendsToAGroup(t *testing.T) {
	o, m, _, _, _ := peopleSetup(t)
	ctx := context.Background()
	if _, err := o.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Group: "nope"}, Topics: []string{"*"}}); err == nil {
		t.Fatal("a subscription to no group was kept")
	}
	if _, err := o.AddSubscription(ctx, notice.Subscription{Target: &notice.Target{Group: "platform"}, Topics: []string{"build.*"}}); err != nil {
		t.Fatal(err)
	}
	got, err := o.Accept(ctx, notice.Event{Producer: "ci", Kind: "build.failed", Key: "b1", Title: "Build failed", To: []notice.Target{{Person: "bo@example.com"}}})
	// Ann by email and Teams, Bo by email once though both routes reach him.
	if err != nil || got.Messages != 3 {
		t.Fatalf("accepted %+v %v", got, err)
	}
	o.SendDue(ctx)
	if want := []string{"ann@example.com", "bo@example.com"}; !slices.Equal(m.to(), want) {
		t.Errorf("mailed %v", m.to())
	}
}

func TestAChainEscalatesUntilSomeoneAcknowledges(t *testing.T) {
	o, m, tm, now, counts := peopleSetup(t)
	ctx := context.Background()
	tm.known["cy@example.com"] = true
	got, err := o.Accept(ctx, event("p1", notice.Target{Chain: "oncall"}, notice.Target{Person: "ann@example.com"}))
	if err != nil || got.Pages != 1 {
		t.Fatalf("accepted %+v %v", got, err)
	}
	o.SendDue(ctx)
	if !slices.Equal(m.to(), []string{"ann@example.com"}) {
		t.Fatalf("first step mailed %v, want Ann once", m.to())
	}
	if !strings.Contains(m.letters[0].Text, "https://dispatch.test/pages/1") {
		t.Errorf("no acknowledge link: %s", m.letters[0].Text)
	}
	*now = now.Add(9 * time.Minute)
	o.SendDue(ctx)
	if len(m.to()) != 1 {
		t.Fatal("escalated before the wait ran out")
	}
	*now = now.Add(2 * time.Minute)
	o.SendDue(ctx)
	// The second step: the group by email, but not Ann again, and Cy in Teams.
	if want := []string{"ann@example.com", "bo@example.com"}; !slices.Equal(m.to(), want) {
		t.Errorf("second step mailed %v", m.to())
	}
	if !slices.Equal(tm.sent, []string{"cy@example.com"}) {
		t.Errorf("second step in Teams %v", tm.sent)
	}
	ended, err := o.Acknowledge(ctx, 1, "bo@example.com")
	if err != nil || !ended {
		t.Fatalf("acknowledge: %v %v", ended, err)
	}
	if again, _ := o.Acknowledge(ctx, 1, "ann@example.com"); again {
		t.Error("acknowledged twice")
	}
	*now = now.Add(time.Hour)
	o.SendDue(ctx)
	p, _ := o.store.Page(ctx, 1)
	if p.State != notice.PageAcknowledged || p.AcknowledgedBy != "bo@example.com" {
		t.Errorf("page %+v", p)
	}
	if counts["page.started"] != 1 || counts["page.escalated"] != 1 || counts["page.acknowledged"] != 1 || counts["page.unanswered"] != 0 {
		t.Errorf("counts %v", counts)
	}
}

func TestAnUnansweredChainEndsAfterItsLastWait(t *testing.T) {
	o, _, _, now, counts := peopleSetup(t)
	ctx := context.Background()
	if _, err := o.Accept(ctx, notice.Event{Producer: "dispatch", Kind: "incident.opened", Key: "incident:7:1", Title: "#7 down", To: []notice.Target{{Chain: "oncall"}}}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(10 * time.Minute)
	o.SendDue(ctx)
	*now = now.Add(20 * time.Minute)
	o.SendDue(ctx)
	p, _ := o.store.Page(ctx, 1)
	if p.State != notice.PageUnanswered || counts["page.unanswered"] != 1 {
		t.Errorf("page %+v counts %v", p, counts)
	}
}

func TestAcknowledgeForEndsAnIncidentsPages(t *testing.T) {
	o, m, _, now, _ := peopleSetup(t)
	ctx := context.Background()
	for _, key := range []string{"incident:7:1", "incident:70:1"} {
		if _, err := o.Accept(ctx, notice.Event{Producer: "dispatch", Kind: "incident.opened", Key: key, Title: key, To: []notice.Target{{Chain: "oncall"}}}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := o.AcknowledgeFor(ctx, "dispatch", "incident:7:", "ann@example.com")
	if err != nil || n != 1 {
		t.Fatalf("ended %d %v", n, err)
	}
	// A message queued before the page ended is not sent.
	o.SendDue(ctx)
	if len(m.to()) != 1 {
		t.Errorf("mailed %v, want only the other incident's page", m.to())
	}
	*now = now.Add(11 * time.Minute)
	o.SendDue(ctx)
	if p, _ := o.store.Page(ctx, 2); p.Step != 1 {
		t.Errorf("the other incident's page did not escalate: %+v", p)
	}
}

func TestAPermanentRefusalFailsAtOnce(t *testing.T) {
	o, m, _, _, counts := peopleSetup(t)
	ctx := context.Background()
	m.refuse["bo@example.com"] = 400
	m.refuse["ann@example.com"] = 0
	if _, err := o.Accept(ctx, event("r1", notice.Target{Group: "platform"})); err != nil {
		t.Fatal(err)
	}
	o.SendDue(ctx)
	if counts["message.failed"] != 1 || counts["message.retry"] != 1 {
		t.Errorf("counts %v", counts)
	}
}
