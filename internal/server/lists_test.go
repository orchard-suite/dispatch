package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

func TestHistRowsFoldRepeatsInADay(t *testing.T) {
	now := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	ack := at(9)
	list := []incident.Incident{
		{ID: 3, Title: "CPU high", Service: "kubelet", OpenedAt: at(2)},
		{ID: 2, Title: "CPU high", Service: "kubelet", OpenedAt: at(10), AcknowledgedAt: &ack},
		{ID: 1, Title: "CPU high", Service: "kubelet", OpenedAt: at(30)},
	}
	rows := histRows(list, true, now)
	if len(rows) != 2 || rows[0].I.ID != 3 || rows[0].Count != 2 || rows[0].Day != "today" || rows[1].Day != "yesterday" {
		t.Fatalf("folded %+v", rows)
	}
	if !strings.Contains(rows[0].Repeats, "fold=0") || !strings.Contains(rows[0].Repeats, "service=kubelet") {
		t.Errorf("repeats link %s", rows[0].Repeats)
	}
	if all := histRows(list, false, now); len(all) != 3 || all[1].TTA != "1h" || all[0].TTAv != noTime {
		t.Errorf("unfolded %+v", all)
	}
}

func TestServicesClockUsesWholeCells(t *testing.T) {
	now := time.Date(2026, 11, 8, 21, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		window string
		step   time.Duration
		n      int
		unit   string
	}{{"7d", 4 * time.Hour, 42, "4 hours"}, {"30d", 24 * time.Hour, 30, "a day"}, {"90d", 72 * time.Hour, 30, "3 days"}} {
		clk := clockFor(c.window, now)
		if clk.Step != c.step || clk.N != c.n || clk.Unit != c.unit || clk.End.Sub(clk.Start) != time.Duration(c.n)*c.step {
			t.Errorf("%s: %+v", c.window, clk)
		}
	}
	// Seven days is Home's clock: from a week ago to now, labelled by day.
	clk := clockFor("7d", now)
	if !clk.Start.Equal(now.Add(-stripSpan)) || len(clk.ticks()) != 7 || clk.ticks()[0].Label != "Mon" {
		t.Errorf("week clock %+v %+v", clk, clk.ticks())
	}
	ack := now.Add(-2 * time.Hour)
	open := incident.Incident{ID: 1, Severity: incident.Warning, State: incident.Acknowledged, OpenedAt: now.Add(-9 * time.Hour), AcknowledgedAt: &ack}
	cells := clk.cells([]incident.Incident{open}, []store.Firing{{IncidentID: 1, At: now.Add(-9 * time.Hour)}, {IncidentID: 2, At: now.Add(-time.Hour)}}, now)
	if cells[39].Fired != incident.Warning || cells[39].Count != 1 || !cells[40].Quiet || !cells[41].Quiet || cells[38].Quiet {
		t.Errorf("cells %+v", cells[38:])
	}
	bars := clk.bars([]incident.Incident{open}, now)
	if len(bars) != 1 || bars[0].Left+bars[0].Width < 99.9 || bars[0].Clipped {
		t.Errorf("an open bar runs to now: %+v", bars)
	}
	if old := clk.bars([]incident.Incident{{ID: 2, OpenedAt: now.Add(-10 * 24 * time.Hour)}}, now); !old[0].Clipped || old[0].Left != 0 {
		t.Errorf("an old one is clipped: %+v", old)
	}
}

func TestBoardIsOneRowOfOpenWork(t *testing.T) {
	now := time.Now()
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	done := at(1)
	cols := histBoard(histRows([]incident.Incident{
		{ID: 1, Title: "a", Severity: incident.Warning, State: incident.Open, OpenedAt: at(5)},
		{ID: 2, Title: "b", Severity: incident.Critical, State: incident.Open, OpenedAt: at(1)},
		{ID: 3, Title: "c", Severity: incident.Warning, State: incident.Open, OpenedAt: at(9)},
		{ID: 4, Title: "d", Severity: incident.Info, State: incident.Resolved, OpenedAt: at(3), ResolvedAt: &done},
	}, false, now))
	var order []string
	for _, c := range cols {
		order = append(order, c.Phase)
	}
	if strings.Join(order, ",") != "open,acknowledged,mitigated,settling,resolved" {
		t.Errorf("columns %v", order)
	}
	if ids := []int64{cols[0].Rows[0].I.ID, cols[0].Rows[1].I.ID, cols[0].Rows[2].I.ID}; ids[0] != 2 || ids[1] != 3 || ids[2] != 1 {
		t.Errorf("worst then oldest: %v", ids)
	}
	if len(cols[4].Rows) != 0 || cols[4].N != 1 {
		t.Errorf("resolved folds to a count: %+v", cols[4])
	}
}

func TestStatusSaysADegradedComponentWithoutAnIncident(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	settings, _ := h.desk.Store.Settings(ctx)
	settings.PublicComponents = map[string]string{"shop": "Online shop"}
	h.desk.Store.SaveSettings(ctx, settings)
	h.desk.Open(ctx, desk.OpenRequest{Title: "Private: pod shop-7f9 crashing", Severity: "warning", Service: "shop"}, incident.SourceAPI, incident.System("x"))
	body := h.do(h.status, httptest.NewRequest(http.MethodGet, "/", nil)).Body.String()
	if !strings.Contains(body, "<b>Online shop</b> is degraded. We are looking into it.") || strings.Contains(body, "Nothing is happening") || strings.Contains(body, "shop-7f9") {
		t.Errorf("status page: %s", body)
	}
}

func TestIncidentsListViewsAndBoard(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mine, _ := h.desk.Open(ctx, desk.OpenRequest{Title: "Checkout failing", Severity: "critical", Service: "shop"}, incident.SourceDesk, incident.Person(member))
	h.desk.Assign(ctx, mine.ID, member, member)
	h.desk.Open(ctx, desk.OpenRequest{Title: "Disk filling", Severity: "warning", Service: "nas"}, incident.SourceAPI, incident.System("board"))
	get := func(path string) string {
		rec := h.do(h.session, asPerson(httptest.NewRequest(http.MethodGet, path, nil), member, "User"))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		return rec.Body.String()
	}
	page := get("/history?view=mine")
	if !strings.Contains(page, "Checkout failing") || strings.Contains(page, "Disk filling") || !strings.Contains(page, `data-href="/incidents/`+itoa(mine.ID)+`"`) {
		t.Errorf("mine view")
	}
	if page := get("/history?view=critical"); strings.Contains(page, "Disk filling") {
		t.Errorf("critical view keeps a warning")
	}
	page = get("/history?layout=board")
	if !strings.Contains(page, `class="cols"`) || !strings.Contains(page, "Disk filling") {
		t.Errorf("board")
	}
	if strings.Contains(page, ">"+member+"<") {
		t.Errorf("the list shows an address")
	}
}
