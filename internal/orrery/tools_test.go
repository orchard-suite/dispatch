package orrery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestTheDoorIsOffWithoutAURLOrAToken(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if NewDoor("", "token", log) != nil || NewDoor("http://hub", "", log) != nil || NewDoor("hub", "token", log) != nil {
		t.Error("no URL, no token, or a relative URL: off")
	}
	if NewDoor("http://hub", "token", log) == nil {
		t.Error("a URL and a token: on")
	}
}

// The door lists a entity's tools or the tools of no entity, and calls one
// with the caller's token, as the hub's contract says.
func TestTheDoorListsAndCallsAsTheHubSays(t *testing.T) {
	var calls int
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tools-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/tools":
			if r.URL.Query().Get("entity") == "sw_workload:shop-api" {
				w.Write([]byte(`{"tools":[{"id":"shop_logs","title":"Read logs","description":"The last lines.","tier":"read","inputs":{"since":{"type":"string","required":true}}}]}`))
				return
			}
			if r.URL.RawQuery != "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"tools":[{"id":"estate_health","title":"Health","tier":"read","inputs":{}}]}`))
		case "POST /v1/tools/shop_logs/call":
			calls++
			var body struct {
				Entity map[string]string `json:"entity"`
				Inputs map[string]any    `json:"inputs"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Entity["blueprint"] != "sw_workload" || body.Entity["key"] != "shop-api" || body.Inputs["since"] != "30m" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"run":"r1","state":"succeeded","summary":"exit 0","text":"no space left on device","truncated":true,"outputs":{}}`))
		case "POST /v1/tools/estate_health/call":
			var body map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&body)
			if _, has := body["entity"]; has || string(body["inputs"]) != "{}" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"run":"r2","state":"failed","summary":"exit 1","text":"boom","truncated":false,"outputs":{}}`))
		case "POST /v1/tools/restart/call":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"the tier act is not held"}`))
		case "POST /v1/tools/gone/call":
			calls++
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"no runner serves it"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()
	door := NewDoor(hub.URL, "tools-token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	shop := &notice.Ref{Blueprint: "sw_workload", Key: "shop-api"}

	tools, err := door.Tools(ctx, shop)
	if err != nil || len(tools) != 1 || tools[0].ID != "shop_logs" || tools[0].Tier != TierRead || !tools[0].Inputs["since"].Required {
		t.Fatalf("an entity's tools: %+v %v", tools, err)
	}
	if tools, err = door.Tools(ctx, nil); err != nil || len(tools) != 1 || tools[0].ID != "estate_health" {
		t.Fatalf("the tools of no entity: %+v %v", tools, err)
	}

	run, err := door.Call(ctx, "shop_logs", shop, map[string]any{"since": "30m"})
	if err != nil || !run.Succeeded() || run.Text != "no space left on device" || !run.Truncated || run.Summary != "exit 0" {
		t.Errorf("a call: %+v %v", run, err)
	}
	if run, err = door.Call(ctx, "estate_health", nil, nil); err != nil || run.Succeeded() || run.State != "failed" || run.Text != "boom" {
		t.Errorf("a failed run is still an answer: %+v %v", run, err)
	}
	if _, err = door.Call(ctx, "restart", shop, nil); !errors.Is(err, ErrToolRefused) {
		t.Errorf("403: %v", err)
	}
	before := calls
	if _, err = door.Call(ctx, "gone", shop, nil); !errors.Is(err, ErrToolRefused) || calls != before+1 {
		t.Errorf("409 is refused, and sent once: %v, %d sends", err, calls-before)
	}
}

func TestAToolsInputsBecomeAJSONSchema(t *testing.T) {
	var tool Tool
	json.Unmarshal([]byte(`{"id":"t","inputs":{
		"app":{"type":"string","title":"App","description":"Which app.","required":true},
		"lines":{"type":"number"},
		"env":{"type":"enum","values":["dev","prod"],"required":true},
		"image":{"type":"enum","values":"{{ inputs.app.fields.images }}"},
		"since":{"type":"datetime"},
		"hosts":{"type":"entity","blueprint":"host","many":true},
		"odd":{"type":"shape"}}}`), &tool)
	want := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"app":   map[string]any{"type": "string", "title": "App", "description": "Which app."},
			"lines": map[string]any{"type": "number"},
			"env":   map[string]any{"type": "string", "enum": []string{"dev", "prod"}},
			"image": map[string]any{"type": "string"},
			"since": map[string]any{"type": "string", "format": "date-time"},
			"hosts": map[string]any{"type": "array", "items": map[string]any{"type": "string", "description": "An entity id of blueprint host, as host:key."}},
			"odd":   map[string]any{},
		},
		"required": []string{"app", "env"},
	}
	if got := tool.Schema(); !reflect.DeepEqual(got, want) {
		t.Errorf("schema\n got %v\nwant %v", got, want)
	}
}
