package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"

	"github.com/orchard-suite/dispatch/internal/changetest"
	"github.com/orchard-suite/dispatch/internal/incident"
)

func (h *harness) post(path, token, body string) *httptest.ResponseRecorder {
	return h.do(h.api, bearer(httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)), token))
}

func (h *harness) changes(t *testing.T, token, query string) []incident.Change {
	t.Helper()
	rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/changes"+query, nil), token))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/changes%s: %d %s", query, rec.Code, rec.Body.String())
	}
	var out struct {
		Changes []incident.Change `json:"changes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Changes
}

func TestAChangePostIsStrictAndKeepsTheOldFields(t *testing.T) {
	h := newHarness(t)
	ci := producerToken(t, h, "ci")
	// What a caller sent before things, run, and record still works.
	if rec := h.post("/v1/changes", ci, `{"service":"shop","title":"shop 1.2.0","kind":"deploy","url":"https://forge.example.org/shop/1","by":"ci","at":"2026-11-01T10:00:00Z"}`); rec.Code != http.StatusCreated {
		t.Fatalf("old fields: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.post("/v1/changes", ci, `{"service":"shop","title":"x","environment":"prod"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "environment") {
		t.Errorf("an unknown field is refused, never dropped: %d %s", rec.Code, rec.Body.String())
	}
	keyed := `{"title":"shop db moved","things":[{"blueprint":"azure_resource","key":"sub/shop/shop-db"}],"run":"https://forge.example.org/shop/runs/9","record":{"sender":"ci","key":"run-9"},"at":"2026-11-01T11:00:00Z"}`
	if rec := h.post("/v1/changes", ci, keyed); rec.Code != http.StatusCreated {
		t.Fatalf("keyed: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.post("/v1/changes", ci, keyed); rec.Code != http.StatusOK {
		t.Errorf("its record again: %d %s", rec.Code, rec.Body.String())
	}
	if rec := h.post("/v1/changes", ci, `{"title":"nowhere"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("no service and no things: %d", rec.Code)
	}
	byEntity := h.changes(t, ci, "?entity=azure_resource:sub/shop/shop-db")
	if len(byEntity) != 1 || byEntity[0].Run != "https://forge.example.org/shop/runs/9" || byEntity[0].Record == nil || byEntity[0].Record.Key != "run-9" {
		t.Errorf("by entity: %+v", byEntity)
	}
	if got := h.changes(t, ci, "?service=shop"); len(got) != 1 || got[0].Title != "shop 1.2.0" {
		t.Errorf("by service: %+v", got)
	}
	if got := h.changes(t, ci, "?since=2026-11-01T10:30:00Z&until=2026-11-01T12:00:00Z"); len(got) != 1 || got[0].Title != "shop db moved" {
		t.Errorf("by time: %+v", got)
	}
	if rec := h.do(h.api, bearer(httptest.NewRequest(http.MethodGet, "/v1/changes?entity=nokey", nil), ci)); rec.Code != http.StatusBadRequest {
		t.Errorf("an entity with no key: %d", rec.Code)
	}
}

func TestChangeEventsComeFromAProducersTokenAndAreKeptWhole(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	orrery := producerToken(t, h, "orrery")
	_, personal, err := h.desk.Store.MintPersonToken(ctx, "agent", member)
	if err != nil {
		t.Fatal(err)
	}
	db := change.Ref{Blueprint: "azure_resource", Key: "sub/shop/shop-db"}
	c := changetest.New("orrery", "run-1", []change.Ref{db}, changetest.Stops(db))
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	batch := string(changetest.Batch(c.Stated(at), c.Routed(change.RouteNoOne, at), c.Ended(change.OutcomeOK, at.Add(time.Minute))))

	if rec := h.post("/v1/change-events", shared, batch); rec.Code != http.StatusForbidden {
		t.Errorf("the shared secret: %d", rec.Code)
	}
	if rec := h.post("/v1/change-events", personal, batch); rec.Code != http.StatusForbidden {
		t.Errorf("a personal token: %d", rec.Code)
	}
	bad := string(changetest.Batch(c.Stated(at), bytes.Replace(c.Ended(change.OutcomeOK, at), []byte(`"outcome"`), []byte(`"extra":true,"outcome"`), 1)))
	rec := h.post("/v1/change-events", orrery, bad)
	var refused struct {
		Index  int    `json:"index"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &refused)
	if rec.Code != http.StatusBadRequest || refused.Index != 1 || refused.Reason == "" {
		t.Errorf("a refused batch names its event: %d %s", rec.Code, rec.Body.String())
	}
	if got := h.changes(t, orrery, ""); len(got) != 0 {
		t.Errorf("a refused batch keeps none: %+v", got)
	}
	if rec := h.post("/v1/change-events", orrery, `{"events":[],"more":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field in the body: %d", rec.Code)
	}
	rec = h.post("/v1/change-events", orrery, batch)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"kept":3,"repeats":0}` {
		t.Fatalf("kept: %d %s", rec.Code, rec.Body.String())
	}
	rec = h.post("/v1/change-events", orrery, batch)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"kept":0,"repeats":3}` {
		t.Errorf("idempotent by sender and key: %d %s", rec.Code, rec.Body.String())
	}
	got := h.changes(t, orrery, "?entity=azure_resource:sub/shop/shop-db")
	if len(got) != 1 || got[0].Outcome != change.OutcomeOK || got[0].Route != change.RouteNoOne || len(got[0].Effects) != 1 || len(got[0].Events) != 3 {
		t.Errorf("read back: %+v", got)
	}
	// Another producer cannot speak for the first.
	other := producerToken(t, h, "trellis")
	c2 := changetest.New("orrery", "run-2", []change.Ref{db})
	rec = h.post("/v1/change-events", other, string(changetest.Batch(c2.Stated(at))))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sender_mismatch") {
		t.Errorf("a sender that is not the token: %d %s", rec.Code, rec.Body.String())
	}
}
