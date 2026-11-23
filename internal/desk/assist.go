package desk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/orchard-suite/dispatch/internal/annals"
	"github.com/orchard-suite/dispatch/internal/assist"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
	"github.com/orchard-suite/dispatch/internal/orrery"
)

// Assistant writes and investigates for the desk; nil turns it off.
type Assistant interface {
	Write(ctx context.Context, task, material string) (string, error)
	Investigate(ctx context.Context, brief string, tools []assist.Tool) (assist.Finding, error)
}

// UseAssistant turns on the assistant.
func (d *Desk) UseAssistant(a Assistant) { d.assistant = a }

// UseToolDoor lends investigations the read tools the catalog hub offers
// on an incident's service. The assistant holds no key and no reader of
// its own: each tool runs on a runner of the hub's, with that runner's key.
func (d *Desk) UseToolDoor(door *orrery.Door) { d.door = door }

// HasAssistant says whether the assistant is on.
func (d *Desk) HasAssistant() bool { return d.assistant != nil }

// Asks are what a person may ask the assistant about an incident, and
// what each asks it to write.
var Asks = map[string]string{
	"summary":     "Catch someone up on this incident in five short lines: what broke, its impact, what has been done, what is open, and who has it.",
	"update":      "Draft a status update for people outside the team: what they notice, what is being done, and when the next update comes. No internal names or systems.",
	"postmortem":  "Draft a postmortem in Markdown with the headings: What happened, Impact, Timeline, Cause, What we change. Mark anything the material does not settle as open.",
	"investigate": "",
}

// Ask asks the assistant about an incident on a person's behalf. It runs
// on its own and keeps its answer for the incident's page; it changes
// nothing.
func (d *Desk) Ask(ctx context.Context, id int64, kind, by string) (int64, error) {
	if d.assistant == nil {
		return 0, fmt.Errorf("%w: this desk has no assistant", ErrInput)
	}
	if _, ok := Asks[kind]; !ok {
		return 0, fmt.Errorf("%w: ask for a summary, an update, a postmortem, or an investigation", ErrInput)
	}
	inc, err := d.Store.Get(ctx, d.Store.DB(), id)
	if err != nil {
		return 0, err
	}
	now := d.Now()
	note, err := d.Store.AskAssist(ctx, id, kind, by, now)
	if err != nil {
		return 0, err
	}
	d.annals.Decide(ctx, annals.Decision{Verb: "assist." + kind, Kind: "incident", ID: fmt.Sprint(id), Title: inc.Title, By: incident.Person(by),
		Key: fmt.Sprintf("assist:%d", note), At: now})
	d.log.InfoContext(ctx, "assistant asked", "incident", id, "kind", kind)
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		material := d.material(ctx, inc)
		var text string
		var evidence []assist.Step
		var err error
		if kind == "investigate" {
			var f assist.Finding
			tools, brief := d.incidentTools(ctx, inc, material)
			f, err = d.assistant.Investigate(ctx, brief, tools)
			text, evidence = f.Text, f.Evidence
		} else {
			text, err = d.assistant.Write(ctx, Asks[kind], material)
		}
		state := "done"
		d.metrics.Assist(ctx, map[bool]string{true: "failed", false: "done"}[err != nil])
		if err != nil {
			state, text = "failed", err.Error()
			d.log.WarnContext(ctx, "assistant failed", "incident", id, "kind", kind, "error", err)
		}
		if err := d.Store.AnswerAssist(ctx, note, state, text, evidence); err != nil {
			d.log.ErrorContext(ctx, "assistant answer not kept", "note", note, "error", err)
		}
	}()
	return note, nil
}

// material is what the assistant reads about an incident: its fields,
// roles, alerts, timeline with every note, changes, and the catalog's
// word on its workload.
func (d *Desk) material(ctx context.Context, inc incident.Incident) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Incident #%d: %s\nSeverity: %s. State: %s. Service: %s. Owner: %s.\nOpened %s.\n", inc.ID, inc.Title, inc.Severity, inc.State, inc.Service, inc.Owner, inc.OpenedAt.UTC().Format(time.RFC3339))
	if roles, err := d.Store.Roles(ctx, inc.ID); err == nil {
		for r, p := range roles {
			fmt.Fprintf(&b, "%s: %s\n", r, p)
		}
	}
	if alerts, err := d.Store.Alerts(ctx, inc.ID); err == nil {
		b.WriteString("\nAlerts:\n")
		for _, a := range alerts {
			labels, _ := json.Marshal(a.Labels)
			fmt.Fprintf(&b, "- %s %s since %s, %s: %s\n", a.Name, a.Status, a.StartsAt.UTC().Format(time.RFC3339), labels, a.Annotations["summary"])
		}
	}
	if events, err := d.Store.Events(ctx, inc.ID); err == nil {
		b.WriteString("\nTimeline:\n")
		for _, e := range events {
			fmt.Fprintf(&b, "- %s %s (%s): %s\n", e.At.UTC().Format(time.RFC3339), e.Kind, e.Actor, e.Text)
		}
	}
	if changes, err := d.ChangesNear(ctx, inc); err == nil && len(changes) > 0 {
		b.WriteString("\nChanges to the service:\n")
		for _, c := range changes {
			fmt.Fprintf(&b, "- %s %s: %s %s\n", c.At.UTC().Format(time.RFC3339), c.Kind, c.Title, c.URL)
		}
	}
	for k, v := range d.About(ctx, inc) {
		fmt.Fprintf(&b, "Catalog %s: %s\n", k, v)
	}
	return b.String()
}

// IncidentTools name the tools the desk adds to every investigation, so
// the assistant can be started allowing them.
var IncidentTools = []string{"incident_record", "service_changes"}

// HubTool is the one tool an investigation calls every hub tool through.
// The assistant's permissions name its tools exactly, once, at start, and
// the hub's list is read anew for each investigation. So one fixed name
// stands for whatever the hub lends, and the investigation's brief lists
// what that is.
const HubTool = "hub_tool"

// listFor bounds reading the hub's tools for one investigation.
const listFor = 30 * time.Second

// lent is one hub tool an investigation may call, and the entity it runs
// on; nil is a tool of no entity.
type lent struct {
	tool   orrery.Tool
	entity *notice.Ref
}

// incidentTools are the investigation's tools and its brief: the
// incident's own record and its service's changes over a longer look, and
// the tools the hub lends on the incident's service, which the brief lists.
func (d *Desk) incidentTools(ctx context.Context, inc incident.Incident, material string) ([]assist.Tool, string) {
	tools := []assist.Tool{
		{Name: IncidentTools[0], Description: "The incident's fields, alerts, timeline, recent changes, and catalog entry.", Inputs: map[string]any{},
			Run: func(context.Context, json.RawMessage) (string, error) { return material, nil }},
		{Name: IncidentTools[1], Description: "Changes to a service, such as deploys, over the last few days.", Inputs: map[string]any{
			"service": map[string]any{"type": "string"}, "days": map[string]any{"type": "integer", "description": "1 to 14"}}, Required: []string{"service"},
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var in struct {
					Service string `json:"service"`
					Days    int    `json:"days"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
				if in.Days < 1 || in.Days > 14 {
					in.Days = 3
				}
				changes, err := d.Store.Changes(ctx, in.Service, d.Now().AddDate(0, 0, -in.Days), d.Now().Add(time.Second))
				if err != nil {
					return "", err
				}
				out, _ := json.Marshal(changes)
				return string(out), nil
			}},
	}
	hub := d.lentTools(ctx, inc)
	if len(hub) == 0 {
		return tools, material + "\nHub tools: none. There is no tool here to read logs, metrics, or how the service is set up.\n"
	}
	var b strings.Builder
	b.WriteString(material)
	b.WriteString("\nHub tools:\n")
	for _, l := range hub {
		on := "no entity"
		if l.entity != nil {
			on = l.entity.Blueprint + ":" + l.entity.Key
		}
		schema, _ := json.Marshal(l.tool.Schema())
		fmt.Fprintf(&b, "- id: %s\n  title: %s\n  description: %s\n  runs on: %s\n  inputs: %s\n", l.tool.ID, l.tool.Title, l.tool.Description, on, schema)
	}
	return append(tools, d.hubTool(inc.ID, hub)), b.String()
}

// lentTools reads what the hub lends this investigation: the tools on the
// incident's service, and the tools of no entity. The hub lists only what
// the assistant's token holds the tier for; a tool that is not a read tool
// is left out here as well, so a token that holds too much still lends
// the assistant nothing that changes anything.
func (d *Desk) lentTools(ctx context.Context, inc incident.Incident) []lent {
	if d.door == nil {
		d.log.InfoContext(ctx, "investigation has no data tools: the hub's tool door is not set up", "incident", inc.ID)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, listFor)
	defer cancel()
	var entities []*notice.Ref
	if inc.Workload != "" {
		entities = append(entities, &notice.Ref{Blueprint: "sw_workload", Key: inc.Workload})
	}
	entities = append(entities, nil)
	var out []lent
	seen := map[string]bool{}
	for _, e := range entities {
		on := ""
		if e != nil {
			on = e.Blueprint + ":" + e.Key
		}
		tools, err := d.door.Tools(ctx, e)
		if err != nil {
			d.log.WarnContext(ctx, "hub tools not listed", "incident", inc.ID, "entity", on, "error", err)
			continue
		}
		for _, t := range tools {
			switch {
			case t.Tier != orrery.TierRead:
				d.log.WarnContext(ctx, "hub tool left out: the assistant takes read tools only", "incident", inc.ID, "tool", t.ID, "tier", t.Tier)
			case !seen[t.ID]:
				seen[t.ID] = true
				out = append(out, lent{tool: t, entity: e})
			}
		}
	}
	if len(out) == 0 {
		d.log.InfoContext(ctx, "investigation has no data tools: the hub lends none", "incident", inc.ID)
		return nil
	}
	d.log.InfoContext(ctx, "investigation has data tools", "incident", inc.ID, "tools", len(out))
	return out
}

// hubTool calls one of the tools the hub lent this investigation, by its
// id. What the tool printed is the answer; a tool that did not succeed, or
// that the hub refused, is a failed step that carries what the hub said.
func (d *Desk) hubTool(incidentID int64, hub []lent) assist.Tool {
	byID := map[string]lent{}
	ids := make([]string, 0, len(hub))
	for _, l := range hub {
		byID[l.tool.ID] = l
		ids = append(ids, l.tool.ID)
	}
	type call struct {
		Tool   string         `json:"tool"`
		Inputs map[string]any `json:"inputs"`
	}
	return assist.Tool{
		Name:        HubTool,
		Description: "Run one read-only tool the catalog hub lends this investigation. The incident's material lists them under Hub tools, each with its id and the JSON schema of its inputs.",
		Inputs: map[string]any{
			"tool":   map[string]any{"type": "string", "description": "The id of a hub tool listed in the material."},
			"inputs": map[string]any{"type": "object", "description": "The tool's inputs, as its schema in the material says."},
		},
		Required: []string{"tool"},
		Called: func(raw json.RawMessage) string {
			var in call
			json.Unmarshal(raw, &in)
			if _, ok := byID[in.Tool]; ok {
				return in.Tool
			}
			return ""
		},
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var in call
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			l, ok := byID[in.Tool]
			if !ok {
				return "", fmt.Errorf("this investigation has no hub tool %q; it has: %s", in.Tool, strings.Join(ids, ", "))
			}
			run, err := d.door.Call(ctx, l.tool.ID, l.entity, in.Inputs)
			if err != nil {
				d.log.WarnContext(ctx, "hub tool call failed", "incident", incidentID, "tool", l.tool.ID, "error", err)
				return "", err
			}
			d.log.InfoContext(ctx, "hub tool called", "incident", incidentID, "tool", l.tool.ID, "state", run.State, "truncated", run.Truncated)
			text := run.Text
			if run.Truncated {
				text += "\n(The hub cut this answer: it keeps only the first 64 KB of what a tool prints.)"
			}
			if !run.Succeeded() {
				return "", fmt.Errorf("the tool ended %s: %s\n%s", run.State, run.Summary, text)
			}
			if strings.TrimSpace(text) == "" {
				text = run.Summary
			}
			return text, nil
		},
	}
}
