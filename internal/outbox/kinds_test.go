package outbox

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/notice"
)

// declare has vantage say it sends mentions, and sets what each person
// chose for them.
func declare(t *testing.T, o *Outbox, choices map[string][]notice.Medium) {
	t.Helper()
	ctx := context.Background()
	at := o.Now()
	kinds := []notice.DeclaredKind{{Kind: "vantage.mention", Title: "You were mentioned", Weight: notice.WeightNotify}}
	if _, err := o.store.DeclareKinds(ctx, "vantage", kinds, at); err != nil {
		t.Fatal(err)
	}
	for email, media := range choices {
		if err := o.store.SetChoice(ctx, email, notice.Choice{Kind: "vantage.mention", Media: media}, at); err != nil {
			t.Fatal(err)
		}
	}
}

func listed(t *testing.T, o *Outbox, email string) int {
	t.Helper()
	lines, err := o.store.Inbox(context.Background(), email, 50)
	if err != nil {
		t.Fatal(err)
	}
	return len(lines)
}

func TestAChoiceForAKindWinsOverTheRules(t *testing.T) {
	o, m, tm, _, _ := peopleSetup(t)
	ctx := context.Background()
	tm.known["ann@example.com"] = true
	// Ann's rules tell her of a notify by Teams and email. She chose email.
	// Bo's rules tell him by email. He chose his list only.
	declare(t, o, map[string][]notice.Medium{"ann@example.com": {notice.Email}, "bo@example.com": nil})
	got, err := o.Accept(ctx, event("k1", notice.Target{Person: "ann@example.com"}, notice.Target{Person: "bo@example.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages != 1 || got.Pages != 0 {
		t.Fatalf("accepted %+v", got)
	}
	o.SendDue(ctx)
	if !slices.Equal(m.to(), []string{"ann@example.com"}) || len(tm.sent) != 0 {
		t.Errorf("mailed %v, Teams %v", m.to(), tm.sent)
	}
	// List only sends nothing and still lands in the list.
	if listed(t, o, "bo@example.com") != 1 || listed(t, o, "ann@example.com") != 1 {
		t.Error("an event a person chose to only list is on their list")
	}
}

func TestAChoiceHoldsThroughAGroup(t *testing.T) {
	o, m, _, _, _ := peopleSetup(t)
	ctx := context.Background()
	declare(t, o, map[string][]notice.Medium{"bo@example.com": nil})
	got, err := o.Accept(ctx, event("k1", notice.Target{Group: "platform"}))
	if err != nil {
		t.Fatal(err)
	}
	// Ann set nothing: her rules give email, as Teams does not know her.
	o.SendDue(ctx)
	if !slices.Equal(m.to(), []string{"ann@example.com"}) {
		t.Errorf("mailed %v after %+v", m.to(), got)
	}
	if listed(t, o, "bo@example.com") != 1 {
		t.Error("the group's event is on the list of the member who chose list only")
	}
}

// A page is never turned down: the choice is not read at all.
func TestAChoiceNeverWeakensAPage(t *testing.T) {
	o, m, _, _, counts := peopleSetup(t)
	ctx := context.Background()
	declare(t, o, map[string][]notice.Medium{"ann@example.com": nil, "bo@example.com": nil})
	e := event("k1", notice.Target{Person: "bo@example.com"})
	e.Weight = notice.WeightPage
	if _, err := o.Accept(ctx, e); err != nil {
		t.Fatal(err)
	}
	// A chain makes the event a page, and its first step tells Ann.
	if got, err := o.Accept(ctx, event("k2", notice.Target{Chain: "oncall"})); err != nil || got.Pages != 1 {
		t.Fatalf("chain: %+v %v", got, err)
	}
	o.SendDue(ctx)
	if want := []string{"ann@example.com", "bo@example.com"}; !slices.Equal(m.to(), want) {
		t.Errorf("mailed %v, want %v", m.to(), want)
	}
	if counts["page.started"] != 1 {
		t.Errorf("counts %v", counts)
	}
}

// A page ignores quiet hours; a choice for a lighter event does not.
func TestAChoiceStillWaitsOutQuietHours(t *testing.T) {
	o, m, _, now, _ := peopleSetup(t)
	ctx := context.Background()
	quiet := &notice.Quiet{From: "00:00", To: "23:59", Zone: "UTC"}
	if err := o.store.SavePerson(ctx, notice.Person{Email: "ann@example.com", Quiet: quiet}); err != nil {
		t.Fatal(err)
	}
	declare(t, o, map[string][]notice.Medium{"ann@example.com": {notice.Email}})
	if got, err := o.Accept(ctx, event("k1", notice.Target{Person: "ann@example.com"})); err != nil || got.Messages != 1 {
		t.Fatalf("accepted %+v %v", got, err)
	}
	o.SendDue(ctx)
	if len(m.to()) != 0 {
		t.Fatalf("a chosen medium sent in quiet hours: %v", m.to())
	}
	*now = now.Add(24 * time.Hour)
	o.SendDue(ctx)
	if !slices.Equal(m.to(), []string{"ann@example.com"}) {
		t.Errorf("after quiet hours: %v", m.to())
	}
}

// A maintenance window keeps an event on the list. A choice does not send it.
func TestAChoiceNeverUndoesAMaintenanceWindow(t *testing.T) {
	o, m, _, _, _ := peopleSetup(t)
	ctx := context.Background()
	declare(t, o, map[string][]notice.Medium{"ann@example.com": {notice.Email}})
	o.UseHush(func(context.Context, notice.Event) bool { return true })
	got, err := o.Accept(ctx, event("k1", notice.Target{Person: "ann@example.com"}))
	if err != nil || got.Messages != 0 {
		t.Fatalf("accepted %+v %v", got, err)
	}
	o.SendDue(ctx)
	if len(m.to()) != 0 || listed(t, o, "ann@example.com") != 1 {
		t.Errorf("mailed %v", m.to())
	}
}

// Only the producer that declared a kind is heard: another product that
// sends the same word goes by the person's rules.
func TestAChoiceIsForTheProducerThatDeclaredTheKind(t *testing.T) {
	o, m, _, _, _ := peopleSetup(t)
	ctx := context.Background()
	declare(t, o, map[string][]notice.Medium{"bo@example.com": nil})
	e := event("k1", notice.Target{Person: "bo@example.com"})
	e.Producer = "someone_else"
	if got, err := o.Accept(ctx, e); err != nil || got.Messages != 1 {
		t.Fatalf("accepted %+v %v", got, err)
	}
	o.SendDue(ctx)
	if !slices.Equal(m.to(), []string{"bo@example.com"}) {
		t.Errorf("mailed %v", m.to())
	}
}
