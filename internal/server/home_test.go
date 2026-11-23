package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/incident"
)

func (h *harness) home(t *testing.T, path string) string {
	t.Helper()
	rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, path, nil), admin, "Admin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func (h *harness) openByHand(t *testing.T, title, sev, description string) incident.Incident {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"title": title, "severity": sev, "service": "shop", "description": description})
	rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/incidents", strings.NewReader(string(body))), shared))
	if rec.Code != http.StatusCreated {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	var inc incident.Incident
	json.Unmarshal(rec.Body.Bytes(), &inc)
	return inc
}

func TestHomeQuietDayShowsTheEstate(t *testing.T) {
	h := newHarness(t)
	body := h.home(t, "/")
	for _, want := range []string{"All quiet. Nothing is open.", "Services, last 7 days", "Noise, last 7 days", `id="queue" data-live`, `id="side" data-live`} {
		if !strings.Contains(body, want) {
			t.Errorf("quiet day: missing %q", want)
		}
	}
	if strings.Contains(body, "Next move") {
		t.Error("quiet day previews an incident")
	}
}

func TestHomeQueuePreviewsWhatNeedsYou(t *testing.T) {
	h := newHarness(t)
	older := h.openByHand(t, "Checkout is failing", "critical", "Payments time out after the deploy.")
	mine := h.openByHand(t, "Search is slow", "warning", "")
	if _, err := h.desk.Assign(t.Context(), mine.ID, admin, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := h.desk.Change(t.Context(), mine.ID, incident.Acknowledged, incident.Person(admin)); err != nil {
		t.Fatal(err)
	}

	body := h.home(t, "/")
	needs, yours := strings.Index(body, "<b>Needs you</b>"), strings.Index(body, "<b>Yours</b>")
	if needs < 0 || yours < 0 || needs > yours {
		t.Fatalf("sections out of order: needs %d, yours %d", needs, yours)
	}
	if !strings.Contains(body, "2 open: 2 need a person (1 critical, 1 warning). Nobody is on 1 of them") {
		t.Error("the headline does not say who is on it")
	}
	if !strings.Contains(body, "Settling, Others: none") {
		t.Error("empty sections do not fold into one line")
	}
	// The first row that needs a person is previewed, with its summary,
	// its phase bar, and one primary move that posts back to Home.
	if !strings.Contains(body, `data-pick-id="`+itoa(older.ID)+`"`) || !strings.Contains(body, "Payments time out after the deploy.") {
		t.Error("the unowned incident is not previewed with its description")
	}
	for _, want := range []string{`class="phase"`, "Next move", `data-key="A"`, `data-key="O"`, `name="back" value="/?i=` + itoa(older.ID) + `"`, `data-since="`,
		`class="rib"`, `action="/incidents/` + itoa(older.ID) + `/note"`, `data-key="N"`} {
		if !strings.Contains(body, want) {
			t.Errorf("preview: missing %q", want)
		}
	}
	// One owner slot per row: an avatar, or a Take target.
	if q := queuePart(body); !strings.Contains(q, `class="take"`) || strings.Contains(q, "Nobody holds it") {
		t.Error("an unowned row lacks its Take slot or says nobody twice")
	}
	if strings.Count(body, `data-key="A"`) != 1 {
		t.Errorf("acknowledge has %d keys", strings.Count(body, `data-key="A"`))
	}
	// People are named, never addressed.
	if strings.Contains(body, admin) {
		t.Error("Home shows an address")
	}

	// Saved views count and filter.
	mineView := h.home(t, "/?view=mine")
	if q := queuePart(mineView); !strings.Contains(q, "Search is slow") || strings.Contains(q, "Checkout is failing") {
		t.Error("the Mine view does not keep only what you hold")
	}
	if !strings.Contains(mineView, `<span class="l">Mine</span> <span class="c">1</span>`) || !strings.Contains(mineView, `<span class="l">Unowned</span> <span class="c">1</span>`) {
		t.Error("the views do not carry their counts")
	}
	// The board lays the same incidents out by state.
	board := h.home(t, "/?layout=board")
	if !strings.Contains(board, `class="board"`) || !strings.Contains(board, "<h3>Acknowledged <span>1</span></h3>") {
		t.Error("the board has no state columns")
	}
	// Asking for no selection shows the estate even with incidents open.
	if estate := h.home(t, "/?i=none"); !strings.Contains(estate, "Services, last 7 days") || strings.Contains(estate, "Next move") {
		t.Error("?i=none does not show the estate")
	}
	// Selecting one previews it.
	if sel := h.home(t, "/?i="+itoa(mine.ID)); !strings.Contains(sel, `data-key="M"`) {
		t.Error("the selected incident is not previewed with its move")
	}
}

func TestHomeMoveReturnsToTheSameView(t *testing.T) {
	h := newHarness(t)
	inc := h.openByHand(t, "Checkout is failing", "critical", "")
	back := "/?i=" + itoa(inc.ID) + "&view=unowned"
	rec := h.do(h.session, asPerson(form("/incidents/"+itoa(inc.ID)+"/state", url.Values{"to": {"acknowledged"}, "back": {back}}), admin, "Admin"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != back {
		t.Fatalf("move from Home: %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	// Anything but Home goes to the incident.
	rec = h.do(h.session, asPerson(form("/incidents/"+itoa(inc.ID)+"/assign", url.Values{"me": {"1"}, "back": {"//evil.test/?"}}), admin, "Admin"))
	if rec.Header().Get("Location") != "/incidents/"+itoa(inc.ID) {
		t.Errorf("a foreign back was followed: %q", rec.Header().Get("Location"))
	}
}

func TestQueueSections(t *testing.T) {
	now := time.Date(2026, 11, 8, 22, 0, 0, 0, time.UTC)
	closes := now.Add(time.Hour)
	quiet := now.Add(-time.Hour)
	rows := []homeRow{
		{Incident: incident.Incident{ID: 1, State: incident.Acknowledged, Owner: admin, OpenedAt: now.Add(-3 * time.Hour)}},
		{Incident: incident.Incident{ID: 2, State: incident.Open, OpenedAt: now.Add(-time.Hour), FiringAlerts: 1}},
		{Incident: incident.Incident{ID: 3, State: incident.Open, OpenedAt: now.Add(-2 * time.Hour)}},
		{Incident: incident.Incident{ID: 4, State: incident.Open, Owner: member, OpenedAt: now, FiringAlerts: 2}},
		{Incident: incident.Incident{ID: 5, State: incident.Open, OpenedAt: now.Add(-2 * time.Hour), QuietSince: &quiet, ClosesAt: &closes}},
		{Incident: incident.Incident{ID: 6, State: incident.Acknowledged, Owner: member, OpenedAt: now}},
	}
	got := map[string][]int64{}
	for _, s := range sectionsOf(rows, admin) {
		for _, r := range s.Rows {
			got[s.Key] = append(got[s.Key], r.ID)
		}
	}
	want := map[string][]int64{"needs": {2, 3, 4}, "yours": {1}, "settling": {5}, "others": {6}}
	for k, ids := range want {
		if len(got[k]) != len(ids) {
			t.Errorf("%s: %v, want %v", k, got[k], ids)
			continue
		}
		for n := range ids {
			if got[k][n] != ids[n] {
				t.Errorf("%s: %v, want %v (paging, then worst, then oldest)", k, got[k], ids)
			}
		}
	}

	clock := newQueueClock([]incident.Incident{rows[0].Incident, rows[4].Incident}, now)
	if b := clock.bar(rows[0].Incident, nil); b.Left != 0 || math.Abs(b.Left+b.Width-b.Now) > 0.01 || b.Now >= 100 {
		t.Errorf("the oldest row starts the clock: %+v", b)
	}
	if b := clock.bar(rows[4].Incident, nil); b.Rest <= 0 || b.Quiet <= b.Left || b.Left+b.Width != b.Quiet {
		t.Errorf("a settling row shows its countdown past quiet: %+v", b)
	}
}

// queuePart is the queue's region of Home, without the pane beside it.
func queuePart(body string) string {
	from, to := strings.Index(body, `id="queue"`), strings.Index(body, `id="side"`)
	if from < 0 || to < from {
		return ""
	}
	return body[from:to]
}

func TestHeadlineCountsEveryOpenIncident(t *testing.T) {
	now := time.Date(2026, 11, 8, 22, 0, 0, 0, time.UTC)
	closes := now.Add(time.Hour)
	open := []incident.Incident{
		{ID: 1, Severity: incident.Critical, State: incident.Open, OpenedAt: now.Add(-time.Hour)},
		{ID: 2, Severity: incident.Warning, State: incident.Acknowledged, Owner: admin, OpenedAt: now.Add(-time.Hour)},
		{ID: 3, Severity: incident.Warning, State: incident.Open, OpenedAt: now.Add(-time.Hour), ClosesAt: &closes},
	}
	if got, want := headline(open, admin, now), "3 open: 2 need a person (1 critical, 1 warning), 1 settling. Nobody is on 1 of them; the oldest is 1h old."; got != want {
		t.Errorf("headline:\n got %q\nwant %q", got, want)
	}
	if got := headline(open[2:], admin, now); !strings.HasPrefix(got, "1 open, all settling") {
		t.Errorf("settling only: %q", got)
	}
}
