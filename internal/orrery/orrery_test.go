package orrery

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/notice"
)

func TestReadTokenFallsBackToPushToken(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	quiet := func(context.Context, string) {}
	if c := New(Config{URL: "http://hub/", PushToken: "src"}, log, quiet); !c.Resolves() || !c.Pushes() {
		t.Error("one push token should do both")
	}
	if c := New(Config{URL: "http://hub", Token: "read"}, log, quiet); !c.Resolves() || c.Pushes() {
		t.Error("a read token alone only reads")
	}
	if c := New(Config{URL: "http://hub"}, log, quiet); c.Resolves() || c.Pushes() {
		t.Error("no token: off")
	}
	if c := New(Config{URL: "hub", Token: "read", PushToken: "src"}, log, quiet); c.Resolves() || c.Pushes() {
		t.Error("a relative URL: off")
	}
	if New(Config{}, log, quiet).Workload(context.Background(), "ns") != "" {
		t.Error("off: no lookup")
	}
}

// The offered blueprint and the catalog's copy name the standard's ids, and
// the offer renames a hub's older incident instead of making a second one.
func TestBlueprintMatchesCatalogModel(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/orrery-catalog/model/incidents.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		Blueprints []struct {
			ID        string   `yaml:"id"`
			Formerly  []string `yaml:"formerly"`
			Relations map[string]struct {
				Target string `yaml:"target"`
			} `yaml:"relations"`
		} `yaml:"blueprints"`
	}
	if err := yaml.Unmarshal(raw, &model); err != nil {
		t.Fatal(err)
	}
	m := model.Blueprints[0]
	if Blueprint["id"] != "it_incident" || m.ID != "it_incident" {
		t.Fatalf("id = %v and %q, want it_incident", Blueprint["id"], m.ID)
	}
	if f, _ := Blueprint["formerly"].([]string); !slices.Equal(f, []string{"incident"}) || !slices.Equal(m.Formerly, []string{"incident"}) {
		t.Fatalf("formerly = %v and %v, want [incident]", Blueprint["formerly"], m.Formerly)
	}
	rel := Blueprint["relations"].(map[string]any)["workload"].(map[string]any)["target"]
	if rel != "sw_workload" || m.Relations["workload"].Target != "sw_workload" {
		t.Fatalf("workload target = %v and %q, want sw_workload", rel, m.Relations["workload"].Target)
	}
}

func TestReachFollowsRelationsTwoHops(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{
			"/v1/entities/azure_app_service/beacon-api-prod": `{"blueprint":"azure_app_service","key":"beacon-api-prod","relations":{"program":[{"blueprint":"program","key":"4242"}],"resource_group":[{"blueprint":"azure_resource_group","key":"rg"}]}}`,
			"/v1/entities/program/4242":                      `{"blueprint":"program","key":"4242","relations":{"portfolio":[{"blueprint":"portfolio","key":"web"}]}}`,
			"/v1/entities/azure_resource_group/rg":           `{"blueprint":"azure_resource_group","key":"rg","relations":{}}`,
		}[r.URL.Path]
		if body == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer hub.Close()
	c := New(Config{URL: hub.URL, Token: "read"}, slog.New(slog.DiscardHandler), func(context.Context, string) {})
	got := c.Reach(context.Background(), notice.Ref{Blueprint: "azure_app_service", Key: "beacon-api-prod"})
	slices.Sort(got)
	want := []string{"azure_resource_group:rg", "portfolio:web", "program:4242"}
	if !slices.Equal(got, want) {
		t.Fatalf("reach = %v, want %v", got, want)
	}
	if New(Config{}, slog.New(slog.DiscardHandler), func(context.Context, string) {}).Reach(context.Background(), notice.Ref{Blueprint: "x", Key: "y"}) != nil {
		t.Error("off: reached something")
	}
}

// Time to restore ends at mitigation, so the entity carries when that was.
func TestFieldsCarryMitigation(t *testing.T) {
	opened := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	mitigated := opened.Add(40 * time.Minute)
	f := Fields(incident.Incident{ID: 3, OpenedAt: opened, UpdatedAt: mitigated, MitigatedAt: &mitigated}, "https://desk")
	if f["mitigated_at"] != "2026-11-07T10:40:00Z" {
		t.Fatalf("mitigated_at = %v", f["mitigated_at"])
	}
	if _, ok := f["resolved_at"]; ok {
		t.Fatal("not resolved: no resolved_at")
	}
	if _, ok := Blueprint["fields"].(map[string]any)["mitigated_at"]; !ok {
		t.Fatal("the blueprint offers no mitigated_at")
	}
}

func TestFieldsSaySettling(t *testing.T) {
	opened := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	quiet := opened.Add(time.Hour)
	closes := quiet.Add(24 * time.Hour)
	f := Fields(incident.Incident{ID: 3, State: incident.Acknowledged, OpenedAt: opened, UpdatedAt: quiet, Alerts: 1, QuietSince: &quiet, ClosesAt: &closes}, "https://desk")
	if f["phase"] != "settling" || f["firing"] != false || f["closes_at"] != "2026-11-08T11:00:00Z" || f["state"] != "acknowledged" {
		t.Fatalf("fields: %+v", f)
	}
	if Blueprint["status"].(map[string]any)["field"] != "phase" {
		t.Fatal("the status follows the phase")
	}
}
