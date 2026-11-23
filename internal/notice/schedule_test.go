package notice

import (
	"testing"
	"time"
)

func TestARotationWithAFollowTheSunLayerAndAnOverride(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, ny) // a Monday
	day, err := ParseLayer("days", "a@example.com, b@example.com", "2026-11-02 09:00", "1w", "", ny)
	if err != nil {
		t.Fatal(err)
	}
	nights, err := ParseLayer("nights", "n@example.com", "2026-11-02 09:00", "1w", "22:00-06:00 mon-fri", ny)
	if err != nil {
		t.Fatal(err)
	}
	s := Schedule{Slug: "platform", Name: "Platform", Zone: "America/New_York", Layers: []Layer{day, nights}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) string { return s.OnCall(start.Add(d)) }
	if at(time.Hour) != "a@example.com" || at(8*24*time.Hour) != "b@example.com" || at(15*24*time.Hour) != "a@example.com" {
		t.Errorf("weekly turns: %s %s %s", at(time.Hour), at(8*24*time.Hour), at(15*24*time.Hour))
	}
	if at(14*time.Hour) != "n@example.com" || at(20*time.Hour) != "n@example.com" || at(22*time.Hour) != "a@example.com" {
		t.Errorf("the night layer covers 22:00 to 06:00: %s %s %s", at(14*time.Hour), at(20*time.Hour), at(22*time.Hour))
	}
	// Friday night runs into Saturday morning; Saturday night is not a weekday.
	if at(4*24*time.Hour+15*time.Hour) != "n@example.com" || at(5*24*time.Hour+15*time.Hour) != "a@example.com" {
		t.Error("the weekday hours follow the night they began")
	}
	s.Overrides = []Override{{Person: "c@example.com", From: start.Add(2 * time.Hour), To: start.Add(4 * time.Hour)}}
	if at(3*time.Hour) != "c@example.com" || at(5*time.Hour) != "a@example.com" {
		t.Error("an override covers its stretch only")
	}
	shifts := s.Shifts(start, start.Add(24*time.Hour))
	want := []string{"a@example.com", "c@example.com", "a@example.com", "n@example.com", "a@example.com"}
	if len(shifts) != len(want) {
		t.Fatalf("shifts %+v", shifts)
	}
	for i, w := range want {
		if shifts[i].Person != w {
			t.Errorf("shift %d: %+v", i, shifts[i])
		}
	}
	if !shifts[3].From.Equal(time.Date(2026, 11, 2, 22, 0, 0, 0, ny)) || !shifts[3].To.Equal(time.Date(2026, 11, 3, 6, 0, 0, 0, ny)) {
		t.Errorf("the night shift %+v", shifts[3])
	}
	if until := s.Until(start.Add(5 * time.Hour)); !until.Equal(time.Date(2026, 11, 2, 22, 0, 0, 0, ny)) {
		t.Errorf("until %v", until)
	}
}

func TestGapsAreStretchesWithNobody(t *testing.T) {
	utc := time.UTC
	l, _ := ParseLayer("", "a@example.com", "2026-11-02 00:00", "1d", "09:00-17:00 mon-fri", utc)
	s := Schedule{Slug: "biz", Name: "Business hours", Zone: "UTC", Layers: []Layer{l}}
	gaps := s.Gaps(time.Date(2026, 11, 2, 0, 0, 0, 0, utc), time.Date(2026, 11, 3, 0, 0, 0, 0, utc))
	if len(gaps) != 2 || gaps[0].To.Hour() != 9 || gaps[1].From.Hour() != 17 {
		t.Errorf("gaps %+v", gaps)
	}
	if h, err := ParseHours("09:00-17:00 sat,sun"); err != nil || h.String() != "09:00-17:00 sun,sat" && h.String() != "09:00-17:00 sat,sun" {
		t.Errorf("hours %v %v", h, err)
	}
	if _, err := ParseLength("3x"); err == nil {
		t.Error("a bad length")
	}
	if LengthString(14*24*time.Hour) != "2w" || LengthString(36*time.Hour) != "36h" {
		t.Error("length strings")
	}
}
