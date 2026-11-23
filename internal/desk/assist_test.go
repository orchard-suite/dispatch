package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/dispatch/internal/assist"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/orrery"
)

// fakeAssistant writes what it was asked and calls one tool.
type fakeAssistant struct{}

func (fakeAssistant) Write(_ context.Context, task, material string) (string, error) {
	return "summary of " + material[:12], nil
}

func (fakeAssistant) Investigate(ctx context.Context, brief string, tools []assist.Tool) (assist.Finding, error) {
	for _, t := range tools {
		if t.Name == "incident_record" {
			got, _ := t.Run(ctx, nil)
			return assist.Finding{Text: "found [E1]", Evidence: []assist.Step{{N: 1, Tool: t.Name, Answer: got}}}, nil
		}
	}
	return assist.Finding{}, nil
}

func TestTheAssistantAnswersOnTheIncident(t *testing.T) {
	d := newDesk(t)
	ctx := context.Background()
	inc, _ := d.Open(ctx, OpenRequest{Title: "Checkout slow", Service: "shop"}, incident.SourceDesk, incident.Person("a@example.com"))
	if _, err := d.Ask(ctx, inc.ID, "summary", "a@example.com"); err == nil {
		t.Fatal("asked with no assistant")
	}
	d.UseAssistant(fakeAssistant{})
	if _, err := d.Ask(ctx, inc.ID, "poem", "a@example.com"); err == nil {
		t.Error("an unknown ask")
	}
	d.Ask(ctx, inc.ID, "summary", "a@example.com")
	d.Ask(ctx, inc.ID, "investigate", "a@example.com")
	var notes []any
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		got, _ := d.Store.AssistNotes(ctx, inc.ID)
		done := 0
		for _, n := range got {
			if n.State == "done" {
				done++
			}
		}
		if done == 2 {
			if got[0].Kind != "investigate" || len(got[0].Evidence) != 1 || got[1].Text != "summary of Incident #1:" {
				t.Fatalf("notes %+v", got)
			}
			return
		}
		notes = append(notes[:0], got)
	}
	t.Fatalf("not answered: %+v", notes)
}

// fakeHub is a hub's tool door: one read tool on the shop's workload, one
// read tool of no entity, and one tool the assistant must never be given.
func fakeHub(t *testing.T, called *[]string) *httptest.Server {
	t.Helper()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/tools":
			switch r.URL.Query().Get("entity") {
			case "sw_workload:shop-api":
				w.Write([]byte(`{"tools":[
					{"id":"shop_logs","title":"Read the shop's logs","description":"The last lines the shop wrote.","tier":"read","inputs":{"since":{"type":"string","required":true}}},
					{"id":"shop_restart","title":"Restart the shop","tier":"act","inputs":{}}]}`))
			case "":
				w.Write([]byte(`{"tools":[{"id":"estate_health","title":"Estate health","tier":"read","inputs":{}}]}`))
			default:
				w.Write([]byte(`{"tools":[]}`))
			}
		case "POST /v1/tools/shop_logs/call":
			var body struct {
				Entity map[string]string `json:"entity"`
				Inputs map[string]any    `json:"inputs"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			*called = append(*called, "shop_logs on "+body.Entity["blueprint"]+":"+body.Entity["key"])
			w.WriteHeader(http.StatusCreated)
			if body.Inputs["since"] == "all" {
				w.Write([]byte(`{"run":"r2","state":"succeeded","summary":"exit 0","text":"line one","truncated":true}`))
				return
			}
			w.Write([]byte(`{"run":"r1","state":"succeeded","summary":"exit 0","text":"no space left on device","truncated":false}`))
		case "POST /v1/tools/estate_health/call":
			*called = append(*called, "estate_health")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"run":"r3","state":"failed","summary":"exit 2","text":"the probe timed out","truncated":false}`))
		default:
			*called = append(*called, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"the tier is not held"}`))
		}
	}))
	t.Cleanup(hub.Close)
	return hub
}

func named(tools []assist.Tool, name string) (assist.Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return assist.Tool{}, false
}

// An investigation is lent the hub's read tools for the incident's service
// and the read tools of no entity, through one tool, and each is listed in
// its brief with its id and its inputs' schema.
func TestAnInvestigationIsLentTheHubsReadTools(t *testing.T) {
	d := newDesk(t)
	var logs bytes.Buffer
	d.log = slog.New(slog.NewJSONHandler(&logs, nil))
	var called []string
	hub := fakeHub(t, &called)
	d.UseToolDoor(orrery.NewDoor(hub.URL, "tools-token", d.log))
	ctx := context.Background()
	tools, brief := d.incidentTools(ctx, incident.Incident{ID: 7, Workload: "shop-api"}, "the material")
	if len(tools) != 3 {
		t.Fatalf("the record, the changes, and the hub tool: %+v", tools)
	}
	for _, want := range []string{"the material", "Hub tools:", "- id: shop_logs", "title: Read the shop's logs", "description: The last lines the shop wrote.",
		"runs on: sw_workload:shop-api", `"required":["since"]`, "- id: estate_health", "runs on: no entity"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "shop_restart") || !strings.Contains(logs.String(), "hub tool left out") {
		t.Errorf("a tool that is not a read tool is left out, and logged:\n%s\n%s", brief, logs.String())
	}
	if !strings.Contains(logs.String(), `"msg":"investigation has data tools"`) || strings.Contains(logs.String(), "tools-token") {
		t.Errorf("logs: %s", logs.String())
	}
	hubTool, ok := named(tools, HubTool)
	if !ok {
		t.Fatal("no hub tool")
	}
	input := func(tool string, inputs map[string]any) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"tool": tool, "inputs": inputs})
		return raw
	}

	got, err := hubTool.Run(ctx, input("shop_logs", map[string]any{"since": "30m"}))
	if err != nil || got != "no space left on device" {
		t.Errorf("a call's text is the answer: %q %v", got, err)
	}
	if len(called) != 1 || called[0] != "shop_logs on sw_workload:shop-api" {
		t.Errorf("the tool runs on the entity it was listed for: %v", called)
	}
	if name := hubTool.Called(input("shop_logs", nil)); name != "shop_logs" {
		t.Errorf("the step is kept under the hub tool's id: %q", name)
	}
	if got, err = hubTool.Run(ctx, input("shop_logs", map[string]any{"since": "all"})); err != nil || !strings.HasPrefix(got, "line one\n") || !strings.Contains(got, "cut this answer") {
		t.Errorf("a truncated answer says it was cut: %q %v", got, err)
	}
	if _, err = hubTool.Run(ctx, input("estate_health", nil)); err == nil || !strings.Contains(err.Error(), "failed") || !strings.Contains(err.Error(), "exit 2") || !strings.Contains(err.Error(), "the probe timed out") {
		t.Errorf("a failed run is a tool error with its summary and text: %v", err)
	}
	before := len(called)
	for _, id := range []string{"shop_restart", "anything_else"} {
		if _, err = hubTool.Run(ctx, input(id, nil)); err == nil || !strings.Contains(err.Error(), "no hub tool") {
			t.Errorf("%s was not lent: %v", id, err)
		}
		if hubTool.Called(input(id, nil)) != "" {
			t.Errorf("%s names no step", id)
		}
	}
	if len(called) != before {
		t.Errorf("a tool that was not lent never reaches the hub: %v", called[before:])
	}
}

// With no door, or a hub that lends nothing, an investigation has no data
// tools, says so in the log, and tells the assistant so.
func TestAnInvestigationWithNoDataToolsSaysSo(t *testing.T) {
	d := newDesk(t)
	var logs bytes.Buffer
	d.log = slog.New(slog.NewJSONHandler(&logs, nil))
	ctx := context.Background()
	tools, brief := d.incidentTools(ctx, incident.Incident{ID: 7, Workload: "shop-api"}, "the material")
	if _, ok := named(tools, HubTool); ok || len(tools) != 2 || !strings.Contains(brief, "Hub tools: none") {
		t.Errorf("no door: %d tools, brief %q", len(tools), brief)
	}
	if !strings.Contains(logs.String(), "investigation has no data tools: the hub's tool door is not set up") {
		t.Errorf("no door is logged: %s", logs.String())
	}

	logs.Reset()
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"tools":[]}`)) }))
	defer empty.Close()
	d.UseToolDoor(orrery.NewDoor(empty.URL, "tools-token", d.log))
	tools, brief = d.incidentTools(ctx, incident.Incident{ID: 7}, "the material")
	if _, ok := named(tools, HubTool); ok || !strings.Contains(brief, "Hub tools: none") {
		t.Errorf("a hub that lends nothing: %d tools, brief %q", len(tools), brief)
	}
	if !strings.Contains(logs.String(), "investigation has no data tools: the hub lends none") {
		t.Errorf("an empty list is logged: %s", logs.String())
	}

	logs.Reset()
	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"no"}`))
	}))
	defer refuses.Close()
	d.UseToolDoor(orrery.NewDoor(refuses.URL, "tools-token", d.log))
	if tools, _ = d.incidentTools(ctx, incident.Incident{ID: 7}, "the material"); len(tools) != 2 || !strings.Contains(logs.String(), "hub tools not listed") {
		t.Errorf("a hub that refuses the list: %d tools, logs %s", len(tools), logs.String())
	}
}

// The hub refusing a call is a tool error the assistant reads.
func TestAHubRefusalIsAToolError(t *testing.T) {
	d := newDesk(t)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"tools":[{"id":"shop_logs","title":"Logs","tier":"read","inputs":{}}]}`))
			return
		}
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"no runner serves it"}`))
	}))
	defer hub.Close()
	d.UseToolDoor(orrery.NewDoor(hub.URL, "tools-token", d.log))
	tools, _ := d.incidentTools(context.Background(), incident.Incident{ID: 7}, "the material")
	hubTool, _ := named(tools, HubTool)
	if _, err := hubTool.Run(context.Background(), json.RawMessage(`{"tool":"shop_logs"}`)); err == nil || !strings.Contains(err.Error(), "does not serve it now") {
		t.Errorf("409: %v", err)
	}
}
