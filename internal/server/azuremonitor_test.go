package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orchard-suite/dispatch/internal/azuremonitor/azuremonitortest"
	"github.com/orchard-suite/dispatch/internal/incident"
	"github.com/orchard-suite/dispatch/internal/store"
)

func azureAlert(t *testing.T, condition string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "azuremonitor-pvc.json"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Replace(string(raw), "CONDITION", condition, 1)
}

func TestAnAzureMonitorAlertThatPagesOpensAnIncident(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	post := func(body, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/azuremonitor", strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return h.do(h.api, req)
	}
	if rec := post(azureAlert(t, "Fired"), ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := post(azureAlert(t, "Fired"), "Bearer "+shared); rec.Code != http.StatusOK {
		t.Fatalf("fired %d %s", rec.Code, rec.Body.String())
	}
	list, _ := h.desk.Store.List(ctx, store.Filter{})
	if len(list) != 1 || list[0].Severity != incident.Critical || list[0].Title != "beacon-files in beacon-prod is 93% full" || list[0].Service != "beacon-prod" {
		t.Fatalf("a critical disk alert opens an incident: %+v", list)
	}
	if rec := post(azureAlert(t, "Resolved"), "Bearer "+shared); rec.Code != http.StatusOK {
		t.Fatalf("resolved %d", rec.Code)
	}
	inc, _ := h.desk.Store.Get(ctx, h.desk.Store.DB(), list[0].ID)
	if inc.State != incident.Open || inc.FiringAlerts != 0 {
		t.Errorf("the alert resolves, not the incident: %+v", inc)
	}
	if rec := post(`{"schemaId":"other"}`, "Bearer "+shared); rec.Code != http.StatusBadRequest {
		t.Errorf("another schema: %d", rec.Code)
	}
}

func TestTheAzureMonitorDoorTakesASecureWebhooksToken(t *testing.T) {
	h := newHarness(t)
	entra := azuremonitortest.New(t)
	post := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/azuremonitor", strings.NewReader(azureAlert(t, "Fired")))
		req.Header.Set("Authorization", auth)
		return h.do(h.api, req)
	}
	if rec := post(entra.Sign(entra.Claims())); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a signed token with no app set: %d", rec.Code)
	}
	h.srv.UseAzureMonitor(entra.Door(t))
	if rec := post(entra.Sign(entra.Claims())); rec.Code != http.StatusOK {
		t.Fatalf("Azure Monitor's token: %d %s", rec.Code, rec.Body.String())
	}
	claims := entra.Claims()
	claims["roles"] = []string{}
	if rec := post(entra.Sign(claims)); rec.Code != http.StatusUnauthorized {
		t.Errorf("a token without the role: %d", rec.Code)
	}
	if rec := post("Bearer " + shared); rec.Code != http.StatusOK {
		t.Errorf("the shared secret still opens the door: %d", rec.Code)
	}
	events, _ := h.desk.Store.Events(context.Background(), 1)
	if len(events) == 0 || events[0].Text != "Opened from Azure Monitor: PersistentVolumeAlmostFull firing on beacon-prod" {
		t.Errorf("the timeline names Azure Monitor as the source: %+v", events)
	}
}
