package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orchard-suite/dispatch/internal/desk"
	"github.com/orchard-suite/dispatch/internal/incident"
)

func TestTheMCPServerReadsAndActsAsThePerson(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inc, _ := h.desk.Open(ctx, desk.OpenRequest{Title: "Checkout slow", Severity: incident.Critical, Service: "shop"}, incident.SourceDesk, incident.Person(admin))
	_, plain, _ := h.desk.Store.MintToken(ctx, "robot", admin)
	_, personal, _ := h.desk.Store.MintPersonToken(ctx, "agent of "+member, member)
	call := func(token, tool string, args any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
		req := bearer(httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))), token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		rec := h.do(h.api, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %d %s", tool, rec.Code, rec.Body.String())
		}
		return out
	}
	text := func(out map[string]any) string {
		raw, _ := json.Marshal(out)
		return string(raw)
	}
	if rec := h.do(h.api, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", rec.Code)
	}
	if got := text(call(plain, "list_incidents", map[string]any{})); !strings.Contains(got, "Checkout slow") {
		t.Errorf("list %s", got)
	}
	if got := text(call(plain, "move_incident", map[string]any{"id": inc.ID, "to": "acknowledged"})); !strings.Contains(got, "personal token") {
		t.Errorf("a plain token acknowledged: %s", got)
	}
	if got := text(call(personal, "move_incident", map[string]any{"id": inc.ID, "to": "acknowledged"})); !strings.Contains(got, "acknowledged") {
		t.Errorf("acknowledge %s", got)
	}
	after, _ := h.desk.Store.Get(ctx, h.desk.Store.DB(), inc.ID)
	if after.State != incident.Acknowledged || after.Owner != member {
		t.Errorf("the person owns it: %+v", after)
	}
	if got := text(call(plain, "send_event", map[string]any{"kind": "agent.done", "title": "Done", "to": []string{"person:" + member}})); !strings.Contains(got, `\"producer\":\"robot\"`) {
		t.Errorf("send %s", got)
	}
	// A personal token acts as its person on the plain API too.
	other, _ := h.desk.Open(ctx, desk.OpenRequest{Title: "Other", Service: "x"}, incident.SourceDesk, incident.Person(admin))
	h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/incidents/"+itoa(other.ID)+"/acknowledge", nil), personal))
	if o, _ := h.desk.Store.Get(ctx, h.desk.Store.DB(), other.ID); o.Owner != member {
		t.Errorf("api acknowledge as the person: %+v", o)
	}
}

// result is the JSON a tool answered with, decoded into v.
func result(t *testing.T, out map[string]any, v any) {
	t.Helper()
	res, _ := out["result"].(map[string]any)
	if res == nil || res["isError"] == true {
		t.Fatalf("tool failed: %v", out)
	}
	content, _ := res["content"].([]any)
	first, _ := content[0].(map[string]any)
	if err := json.Unmarshal([]byte(first["text"].(string)), v); err != nil {
		t.Fatalf("result: %v in %v", err, first["text"])
	}
}

func TestAnAgentWorksTheBoardWithItsPerson(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, personal, _ := h.desk.Store.MintPersonToken(ctx, "agent of "+member, member)
	_, plain, _ := h.desk.Store.MintToken(ctx, "robot", admin)
	call := func(token, tool string, args any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
		req := bearer(httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))), token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		var out map[string]any
		if err := json.Unmarshal(h.do(h.api, req).Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return out
	}
	free, _ := h.desk.Open(ctx, desk.OpenRequest{Title: "Disk filling", Severity: incident.Warning, Service: "nas"}, incident.SourceDesk, incident.Person(admin))
	theirs, _ := h.desk.Open(ctx, desk.OpenRequest{Title: "Backups late", Severity: incident.Warning, Service: "nas"}, incident.SourceDesk, incident.Person(admin))
	h.desk.Assign(ctx, theirs.ID, admin, admin)

	// Take the free one, then the board puts it under mine.
	var taken struct {
		Owner string           `json:"owner"`
		Moves []incident.State `json:"moves"`
		URL   string           `json:"url"`
		Tmpl  string           `json:"postmortem_template"`
	}
	result(t, call(personal, "assign_incident", map[string]any{"id": free.ID, "person": "me"}), &taken)
	if taken.Owner != member || len(taken.Moves) == 0 || !strings.HasSuffix(taken.URL, "/incidents/"+itoa(free.ID)) || taken.Tmpl == "" {
		t.Errorf("take: %+v", taken)
	}
	var b struct {
		Person  string              `json:"person"`
		Mine    []incident.Incident `json:"mine"`
		Unowned []incident.Incident `json:"unowned"`
		Others  []incident.Incident `json:"others"`
		Actions []struct {
			Text     string `json:"text"`
			Incident string `json:"incident_title"`
		} `json:"my_action_items"`
		Postmortems []incident.Incident `json:"postmortems_due"`
	}
	result(t, call(personal, "board", map[string]any{}), &b)
	if b.Person != member || len(b.Mine) != 1 || b.Mine[0].ID != free.ID || len(b.Others) != 1 || len(b.Unowned) != 0 {
		t.Errorf("board: %+v", b)
	}

	// Work it: a status update, mitigate, resolve, an action item, a postmortem.
	result(t, call(personal, "add_note", map[string]any{"id": free.ID, "text": "Pruned old snapshots.", "public": true}), &taken)
	result(t, call(personal, "move_incident", map[string]any{"id": free.ID, "to": "mitigated"}), &taken)
	result(t, call(personal, "move_incident", map[string]any{"id": free.ID, "to": "resolved"}), &taken)
	result(t, call(personal, "add_action_item", map[string]any{"id": free.ID, "text": "Alert at 80 percent", "owner": "me"}), &taken)
	result(t, call(personal, "board", map[string]any{}), &b)
	if len(b.Mine) != 0 || len(b.Postmortems) != 1 || len(b.Actions) != 1 || b.Actions[0].Incident != "Disk filling" {
		t.Errorf("after resolving: %+v", b)
	}
	result(t, call(personal, "save_postmortem", map[string]any{"id": free.ID, "body": "## What happened\nThe disk filled."}), &taken)
	result(t, call(personal, "board", map[string]any{}), &b)
	if len(b.Postmortems) != 0 {
		t.Errorf("a written postmortem is still due: %+v", b.Postmortems)
	}
	events, _ := h.desk.Store.Events(ctx, free.ID)
	public := false
	for _, e := range events {
		public = public || (e.Public && e.Actor == member && e.Text == "Pruned old snapshots.")
	}
	if !public {
		t.Errorf("the status update is not the person's public note: %+v", events)
	}

	// Lists by owner.
	var list []incident.Incident
	result(t, call(personal, "list_incidents", map[string]any{"owner": "me", "state": "all"}), &list)
	if len(list) != 1 || list[0].ID != free.ID {
		t.Errorf("mine: %+v", list)
	}
	result(t, call(plain, "list_incidents", map[string]any{"owner": admin}), &list)
	if len(list) != 1 || list[0].ID != theirs.ID {
		t.Errorf("theirs: %+v", list)
	}
	if got := call(plain, "list_incidents", map[string]any{"owner": "me"}); !strings.Contains(fmt.Sprint(got), "personal token") {
		t.Errorf("me without a person: %v", got)
	}
	if got := call(plain, "assign_incident", map[string]any{"id": theirs.ID, "person": "me"}); !strings.Contains(fmt.Sprint(got), "personal token") {
		t.Errorf("a plain token assigned: %v", got)
	}
}

func TestTheBoardListsSettlingIncidentsApart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_, personal, _ := h.desk.Store.MintPersonToken(ctx, "agent of "+member, member)
	rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/alertmanager", fixture(t, "alertmanager_firing.json")), shared))
	if rec.Code != http.StatusOK {
		t.Fatalf("firing: %d %s", rec.Code, rec.Body)
	}
	rec = h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, "/v1/alertmanager", fixture(t, "alertmanager_resolved.json")), shared))
	if rec.Code != http.StatusOK {
		t.Fatalf("resolved: %d %s", rec.Code, rec.Body)
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "board", "arguments": map[string]any{}}})
	req := bearer(httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body))), personal)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	var out map[string]any
	if err := json.Unmarshal(h.do(h.api, req).Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var b struct {
		Unowned  []incident.Incident `json:"unowned"`
		Settling []incident.Incident `json:"settling"`
	}
	result(t, out, &b)
	if len(b.Settling) == 0 || len(b.Unowned) != 0 {
		t.Fatalf("every incident whose alerts stopped is settling, none is unowned work: %+v", b)
	}
	for _, i := range b.Settling {
		if i.ClosesAt == nil {
			t.Errorf("a settling incident says when it closes: %+v", i)
		}
	}
}
