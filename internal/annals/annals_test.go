package annals

import (
	"strings"
	"testing"
	"time"

	kit "github.com/orchard-suite/platformkit/v3/annals"

	"github.com/orchard-suite/dispatch/internal/incident"
)

func TestEventPassesTheContract(t *testing.T) {
	inc := incident.Incident{ID: 7, Title: "Disk full\non " + strings.Repeat("é", 600), Severity: incident.Critical, Service: strings.Repeat("shop-", 30)}
	change := incident.Event{ID: 42, At: time.Date(2026, 11, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))}
	by := incident.Actor{Kind: incident.ActorSystem, ID: "board", For: strings.Repeat("a", 300) + "\n"}
	ev := Event("resolve", inc, change, by)
	if err := ev.Validate(Source); err != nil {
		t.Fatalf("an oversized incident must still pass: %v", err)
	}
	if ev.EventKey != "incident:7:resolve:42" || ev.Action.Verb != "incident.resolve" || !ev.At.Equal(change.At) || ev.At.Location() != time.UTC {
		t.Errorf("event: %+v", ev)
	}
	if person := Event("acknowledge", inc, change, incident.Person("someone@example.com")); person.Actor.OnBehalfOf != "" || person.Actor.Kind != "person" {
		t.Errorf("a person acts for nobody: %+v", person.Actor)
	}
}

// A person is posted with their email in its own field, so Annals's
// directory finds them whichever id they were posted as.
func TestAPersonCarriesTheirEmail(t *testing.T) {
	inc := incident.Incident{ID: 7, Title: "Disk full", Severity: incident.Critical}
	change := incident.Event{ID: 42, At: time.Now()}
	ev := Event("acknowledge", inc, change, incident.Person("Someone@Example.com"))
	if ev.Actor.Email != "someone@example.com" {
		t.Errorf("the email: %+v", ev.Actor)
	}
	if err := ev.Validate(Source); err != nil {
		t.Fatal(err)
	}
	if sys := Event("acknowledge", inc, change, incident.System("board")); sys.Actor.Email != "" {
		t.Errorf("a system has no email: %+v", sys.Actor)
	}
}

// A person in a decision is a party, never a title, so Annals takes
// the event and masks the person for a reader.
func TestAPersonInADecisionIsAParty(t *testing.T) {
	ev := DecisionEvent(Decision{Verb: "schedule.cover", Kind: "schedule", ID: "platform", By: incident.Person("admin@example.com"),
		Key: "schedule:platform:cover:1", At: time.Now(), Parties: []kit.Party{PersonParty("cover", "Cover@Example.com")}})
	if err := ev.Validate(Source); err != nil {
		t.Fatal(err)
	}
	if p := ev.Parties[0]; p.Kind != "person" || p.Email != "cover@example.com" || p.Role != "cover" {
		t.Errorf("the person covering: %+v", p)
	}
}

// A notice's title can quote a person's email; Annals takes the event
// with the address replaced, rather than refusing it.
func TestAnEmailInANoticeTitleIsReplaced(t *testing.T) {
	ev := DecisionEvent(Decision{Verb: "notice.accept", Kind: "notice", ID: "1274447", Title: "pat@example.com changed blueprint web",
		By: incident.System("orrery"), Key: "notice:orrery:change:1274447:notice.accept", At: time.Now()})
	if err := ev.Validate(Source); err != nil {
		t.Fatal(err)
	}
	if ev.Action.Target.Title != "[email] changed blueprint web" {
		t.Errorf("the title: %q", ev.Action.Target.Title)
	}
}

// A send given up is a failed outcome, which Annals takes only with
// the reason it failed.
func TestAFailedDecisionSaysWhy(t *testing.T) {
	ev := DecisionEvent(Decision{Verb: "message.give_up", Kind: "message", ID: "183", By: incident.System("dispatch"),
		Key: "message:183:give_up", At: time.Now(), Outcome: "failed", Reason: "the SMS service answered 401 Denied"})
	if err := ev.Validate(Source); err != nil {
		t.Fatal(err)
	}
	ev.Action.Reason = ""
	if err := ev.Validate(Source); err == nil {
		t.Error("Annals takes a failure that does not say why")
	}
}
