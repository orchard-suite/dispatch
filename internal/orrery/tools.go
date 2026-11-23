package orrery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/orrery"
	"github.com/orchard-suite/platformkit/v3/telemetry"

	"github.com/orchard-suite/dispatch/internal/notice"
)

// Door is the hub's tool door: the tools the hub lends this caller, each a
// command that runs on a runner with the runner's key. The caller holds
// only its own access token, and the hub lists and runs only the tools
// that token's tiers allow.
type Door struct {
	list *orrery.Client
	call *orrery.Client
}

// callFor bounds one tool call. The hub answers when the tool ends, so the
// wait is the tool's own timeout.
const callFor = 3 * time.Minute

// NewDoor makes the door. It is nil, which is off, without a URL or a
// token, or when the URL is not absolute, which is logged.
func NewDoor(base, token string, log *slog.Logger) *Door {
	list := hub(base, token, log)
	if list == nil {
		return nil
	}
	// A call is never sent twice: a second send would run the tool again.
	call := orrery.New(base, token, orrery.WithLogger(log), orrery.WithHTTPClient(telemetry.Client(callFor)), orrery.WithRetries(0))
	return &Door{list: list, call: call}
}

// TierRead is the tier of a tool that only looks at how a thing is set up
// and how it runs.
const TierRead = "read"

// Tool is one tool the hub offers this caller.
type Tool struct {
	ID          string               `json:"id"`
	Title       string               `json:"title"`
	Description string               `json:"description"`
	Tier        string               `json:"tier"`
	Inputs      map[string]ToolInput `json:"inputs"`
}

// ToolInput is one input of a tool, as the hub describes it. Values is an
// enum's list, or a template the hub renders to one.
type ToolInput struct {
	Type        string          `json:"type"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Required    bool            `json:"required"`
	Many        bool            `json:"many"`
	Values      json.RawMessage `json:"values"`
	Blueprint   string          `json:"blueprint"`
}

// Schema is the tool's inputs as a JSON Schema object.
func (t Tool) Schema() map[string]any {
	props := map[string]any{}
	var required []string
	for name, in := range t.Inputs {
		props[name] = in.schema()
		if in.Required {
			required = append(required, name)
		}
	}
	slices.Sort(required)
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func (in ToolInput) schema() map[string]any {
	s := map[string]any{}
	switch in.Type {
	case "string", "text", "markdown":
		s["type"] = "string"
	case "number":
		s["type"] = "number"
	case "boolean":
		s["type"] = "boolean"
	case "date":
		s["type"], s["format"] = "string", "date"
	case "datetime":
		s["type"], s["format"] = "string", "date-time"
	case "email":
		s["type"], s["format"] = "string", "email"
	case "url":
		s["type"], s["format"] = "string", "uri"
	case "enum":
		s["type"] = "string"
		var values []string
		if json.Unmarshal(in.Values, &values) == nil && len(values) > 0 {
			s["enum"] = values
		}
	case "entity", "person":
		s["type"] = "string"
		hint := "An entity id, as blueprint:key."
		if in.Blueprint != "" {
			hint = "An entity id of blueprint " + in.Blueprint + ", as " + in.Blueprint + ":key."
		}
		in.Description = strings.TrimSpace(in.Description + " " + hint)
	}
	if in.Title != "" {
		s["title"] = in.Title
	}
	if in.Description != "" {
		s["description"] = in.Description
	}
	if in.Many {
		return map[string]any{"type": "array", "items": s}
	}
	return s
}

// ToolRun is how one call of a tool ended. Text is what the tool printed,
// its head kept when Truncated.
type ToolRun struct {
	Run       string `json:"run"`
	State     string `json:"state"`
	Summary   string `json:"summary"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// Succeeded says whether the tool ran to a good end.
func (r ToolRun) Succeeded() bool { return r.State == "succeeded" }

// Tools lists the tools the hub offers this caller on an entity, or, with
// no entity, the tools that run on none.
func (d *Door) Tools(ctx context.Context, entity *notice.Ref) ([]Tool, error) {
	path := "/v1/tools"
	if entity != nil {
		path += "?entity=" + url.QueryEscape(entity.Blueprint+":"+entity.Key)
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := d.list.Call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// ErrToolRefused is the hub refusing a call: the caller lacks the tool's
// tier, or the tool's runner does not serve it now.
var ErrToolRefused = errors.New("the hub refused the tool call")

// Call runs one tool and waits for it to end. A run that did not succeed
// is still an answer; ask Succeeded.
func (d *Door) Call(ctx context.Context, id string, entity *notice.Ref, inputs map[string]any) (ToolRun, error) {
	body := struct {
		Entity *toolEntity    `json:"entity,omitempty"`
		Inputs map[string]any `json:"inputs"`
	}{Inputs: inputs}
	if body.Inputs == nil {
		body.Inputs = map[string]any{}
	}
	if entity != nil {
		body.Entity = &toolEntity{Blueprint: entity.Blueprint, Key: entity.Key}
	}
	var run ToolRun
	err := d.call.Call(ctx, http.MethodPost, "/v1/tools/"+url.PathEscape(id)+"/call", body, &run)
	var refused *orrery.APIError
	if errors.As(err, &refused) {
		switch refused.Status {
		case http.StatusForbidden:
			return ToolRun{}, fmt.Errorf("%w: this caller does not hold the tool's tier: %s", ErrToolRefused, refused.Message)
		case http.StatusConflict:
			return ToolRun{}, fmt.Errorf("%w: the tool's runner does not serve it now: %s", ErrToolRefused, refused.Message)
		}
	}
	return run, err
}

type toolEntity struct {
	Blueprint string `json:"blueprint"`
	Key       string `json:"key"`
}
