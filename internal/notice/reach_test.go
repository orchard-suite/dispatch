package notice

import (
	"errors"
	"testing"
	"time"
)

func TestRulesParseAndFormat(t *testing.T) {
	steps, err := ParseRule("teams, sms+5 email+15")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 || steps[1] != (RuleStep{Medium: SMS, After: 5}) {
		t.Fatalf("steps %+v", steps)
	}
	if got := FormatRule(steps); got != "teams, sms+5, email+15" {
		t.Errorf("format %q", got)
	}
	if steps, _ := ParseRule("none"); steps == nil || len(steps) != 0 {
		t.Errorf("none is an empty rule: %#v", steps)
	}
	if _, err := ParseRule("pager"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown medium: %v", err)
	}
	if err := (Rules{WeightPage: {{Medium: SMS, After: 500}}}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a step past four hours: %v", err)
	}
}

func TestDefaultRulesFollowWhatAPersonHas(t *testing.T) {
	p := Person{Email: "a@example.com", Phone: "+15555550100"}
	all := func(Medium) bool { return true }
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	media := func(sends []Send) []Medium {
		var out []Medium
		for _, s := range sends {
			out = append(out, s.Medium)
		}
		return out
	}
	if got := media(p.Plan(WeightPage, all, now)); len(got) != 3 || got[0] != TeamsChat || got[1] != SMS || got[2] != Email {
		t.Errorf("a page takes every medium the person has: %v", got)
	}
	if got := media(p.Plan(WeightNotify, all, now)); len(got) != 2 || got[1] != Email {
		t.Errorf("notify takes all but SMS: %v", got)
	}
	if got := p.Plan(WeightNotice, all, now); len(got) != 0 {
		t.Errorf("a notice sends nothing: %v", got)
	}
	noSMS := func(m Medium) bool { return m != SMS }
	p.Rules = Rules{WeightPage: {{Medium: SMS}, {Medium: Email, After: 5}}}
	got := p.Plan(WeightPage, noSMS, now)
	if len(got) != 1 || got[0].Medium != Email || !got[0].At.Equal(now.Add(5*time.Minute)) {
		t.Errorf("a medium the desk cannot send is left out: %+v", got)
	}
}

func TestQuietHoursHoldNotifyNotPages(t *testing.T) {
	p := Person{Email: "a@example.com", Quiet: &Quiet{From: "22:00", To: "07:00", Zone: "America/New_York"}}
	if err := p.Quiet.Validate(); err != nil {
		t.Fatal(err)
	}
	all := func(Medium) bool { return true }
	// 23:30 in New York is 04:30 the next day in UTC in November.
	night := time.Date(2026, 11, 7, 4, 30, 0, 0, time.UTC)
	morning := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for _, s := range p.Plan(WeightNotify, all, night) {
		if !s.At.Equal(morning) {
			t.Errorf("notify at night waits for 07:00 New York: %v", s.At)
		}
	}
	// A notice a person asked to be sent waits too: only a page does not.
	told := p.Choosing(WeightNotice, []Medium{Email})
	if plan := told.Plan(WeightNotice, all, night); len(plan) != 1 || !plan[0].At.Equal(morning) {
		t.Errorf("a notice at night waits for 07:00 New York: %+v", plan)
	}
	for _, s := range p.Plan(WeightPage, all, night) {
		if !s.At.Equal(night) {
			t.Errorf("a page never waits: %v", s.At)
		}
	}
	noon := time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC)
	if got := p.Quiet.Until(noon); !got.Equal(noon) {
		t.Errorf("outside quiet: %v", got)
	}
	if err := (Quiet{From: "22:00", To: "07:00", Zone: "Mars/Base"}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad zone: %v", err)
	}
}

func TestPhonesAndDiscordIDs(t *testing.T) {
	if got, err := NormalPhone(" +1 (555) 555-0100 "); err != nil || got != "+15555550100" {
		t.Errorf("phone %q %v", got, err)
	}
	if _, err := NormalPhone("555-0100"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a number without its country: %v", err)
	}
	if !ValidDiscordID("123456789012345678") || ValidDiscordID("ann#1234") {
		t.Error("discord ids are numbers")
	}
}
