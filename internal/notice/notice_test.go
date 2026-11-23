package notice

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func event() Event {
	return Event{
		Producer: "orrery", Kind: "action.awaiting_approval", Key: "run-1", Title: "Restart beacon-api-prod waits for approval",
		Entity: &Ref{Blueprint: "azure_app_service", Key: "beacon-api-prod", Title: "beacon-api-prod"},
		Facts:  []Fact{{"Environment", "Prod"}}, Links: []Link{{"Approve in Orrery", "https://orrery.example.org/runs/1"}},
		Labels: map[string]string{"env": "Prod"}, At: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
	}
}

func TestValidate(t *testing.T) {
	if err := event().Validate(); err != nil {
		t.Fatalf("a good event: %v", err)
	}
	for name, bad := range map[string]func(*Event){
		"kind":     func(e *Event) { e.Kind = "Approval" },
		"key":      func(e *Event) { e.Key = "" },
		"title":    func(e *Event) { e.Title = " " },
		"severity": func(e *Event) { e.Severity = "huge" },
		"link":     func(e *Event) { e.Links = []Link{{"x", "javascript:alert(1)"}} },
		"entity":   func(e *Event) { e.Entity = &Ref{Blueprint: "x"} },
	} {
		e := event()
		bad(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s: a bad event was taken", name)
		}
	}
}

func TestTakes(t *testing.T) {
	e := event()
	reached := false
	reach := func() []string { reached = true; return []string{"program:4242"} }
	cases := []struct {
		name string
		sub  Subscription
		want bool
	}{
		{"every action", Subscription{Topics: []string{"action.*"}}, true},
		{"another topic", Subscription{Topics: []string{"incident.*"}}, false},
		{"paused", Subscription{Topics: []string{"*"}, Paused: true}, false},
		{"its producer", Subscription{Topics: []string{"*"}, Producers: []string{"orrery"}}, true},
		{"another producer", Subscription{Topics: []string{"*"}, Producers: []string{"muster"}}, false},
		{"the entity itself", Subscription{Topics: []string{"*"}, Anchor: "azure_app_service:beacon-api-prod"}, true},
		{"a program it reaches", Subscription{Topics: []string{"*"}, Anchor: "program:4242"}, true},
		{"a program it does not reach", Subscription{Topics: []string{"*"}, Anchor: "program:1"}, false},
		{"a label that holds", Subscription{Topics: []string{"*"}, Match: map[string]string{"env": "Pro*"}}, true},
		{"a label that does not", Subscription{Topics: []string{"*"}, Match: map[string]string{"env": "Test"}}, false},
	}
	for _, c := range cases {
		if got := c.sub.Takes(e, reach); got != c.want {
			t.Errorf("%s: took %v, want %v", c.name, got, c.want)
		}
	}
	reached = false
	(Subscription{Topics: []string{"*"}, Anchor: "azure_app_service:beacon-api-prod"}).Takes(e, reach)
	if reached {
		t.Error("the catalog was asked although the anchor is the event's own entity")
	}
}

func TestRetry(t *testing.T) {
	now := time.Now()
	if at, ok := Retry(1, now); !ok || at.Sub(now) != 30*time.Second {
		t.Errorf("first retry at %v, %v", at.Sub(now), ok)
	}
	if _, ok := Retry(len(Waits)+1, now); ok {
		t.Error("retried past the last wait")
	}
	if !Permanent(404) || Permanent(429) || Permanent(503) {
		t.Error("permanent answers misread")
	}
}

func TestTeamsCard(t *testing.T) {
	raw, err := Body(Teams, event())
	if err != nil {
		t.Fatal(err)
	}
	var msg struct {
		Type        string `json:"type"`
		Attachments []struct {
			ContentType string `json:"contentType"`
			Content     struct {
				Type    string `json:"type"`
				Body    []map[string]any
				Actions []map[string]string
			} `json:"content"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "message" || len(msg.Attachments) != 1 || msg.Attachments[0].ContentType != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("not a Workflows message: %s", raw)
	}
	card := msg.Attachments[0].Content
	if card.Type != "AdaptiveCard" || card.Actions[0]["url"] != "https://orrery.example.org/runs/1" {
		t.Fatalf("card: %s", raw)
	}
	if !strings.Contains(string(raw), `"Environment"`) || !strings.Contains(string(raw), `"beacon-api-prod"`) {
		t.Errorf("facts missing: %s", raw)
	}
}

func TestAnEventsPageIsItsLetterAndNothingElse(t *testing.T) {
	e := event()
	e.Page = `<html><body><h1>Edition 3</h1></body></html>`
	l := Mail(e, "")
	if l.HTML != e.Page {
		t.Errorf("the letter is not the page: %s", l.HTML)
	}
	if !strings.Contains(l.Text, e.Title) {
		t.Errorf("the plain text lost the event's words: %s", l.Text)
	}
	raw, err := Body(Webhook, e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Edition 3") {
		t.Errorf("a webhook channel was given the page: %s", raw)
	}
	e.Page = strings.Repeat("x", MaxPage+1)
	if err := e.Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a page over the limit was taken: %v", err)
	}
}
