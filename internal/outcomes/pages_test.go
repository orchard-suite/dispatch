package outcomes

import (
	"testing"
	"time"
)

func TestPagesCountByServiceAndLoadByPerson(t *testing.T) {
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r := Build(nil, "7d", since, since.Add(7*24*time.Hour))
	night := func(t time.Time) bool { return t.Hour() >= 22 || t.Hour() < 7 }
	AddPages(&r, []Page{
		{Service: "shop", StartedAt: since.Add(3 * time.Hour), Answered: true, AckBy: "a@example.com", Told: []string{"a@example.com"}},
		{Service: "shop", StartedAt: since.Add(12 * time.Hour), Answered: false, Told: []string{"a@example.com", "b@example.com"}},
		{Service: "api", StartedAt: since.Add(-time.Hour), Told: []string{"b@example.com"}},
	}, map[string]float64{"c@example.com": 168}, night)
	if r.Total.Pages != 2 || r.Total.NightPages != 1 || r.Total.UnansweredPages != 1 {
		t.Errorf("total %+v", r.Total)
	}
	if len(r.Services) != 1 || r.Services[0].Service != "shop" || r.Services[0].Pages != 2 {
		t.Errorf("services %+v", r.Services)
	}
	if len(r.People) != 3 || r.People[0].Person != "a@example.com" || r.People[0].Pages != 2 || r.People[0].NightPages != 1 || r.People[0].Acknowledged != 1 {
		t.Errorf("people %+v", r.People)
	}
	for _, l := range r.People {
		if l.Person == "c@example.com" && l.HoursOnCall != 168 {
			t.Errorf("hours %+v", l)
		}
	}
	if empty := Build(nil, "7d", since, since); empty.People == nil || empty.Services == nil {
		t.Error("an empty report answers empty lists, not null")
	}
}
