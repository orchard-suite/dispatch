package signals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orchard-suite/platformkit/v3/change"
)

var (
	from = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	to   = from.Add(30 * time.Minute)
)

func TestRenderFillsParamsAndTheWindow(t *testing.T) {
	q := Query{Text: `increase(x{namespace="{namespace.title}", pod=~"{name}-.*"}[{window}])`, Params: map[string]string{"namespace.title": `sh"op`, "name": "shop"}, From: from, To: to}
	got, err := Render(q, promString)
	if err != nil || got != `increase(x{namespace="sh\"op", pod=~"shop-.*"}[1800s])` {
		t.Errorf("%s %v", got, err)
	}
	q.Params = map[string]string{"name": "shop"}
	if _, err := Render(q, promString); !errors.Is(err, ErrMissingParam) || !strings.Contains(err.Error(), "namespace.title") {
		t.Errorf("a parameter the subject lacks: %v", err)
	}
}

func TestJudgeNeverCallsBlindClean(t *testing.T) {
	clean, harm, blind := Look{Verdict: change.VerdictClean}, Look{Verdict: change.VerdictHarm}, Look{Verdict: change.VerdictBlind}
	for name, tc := range map[string]struct {
		looks []Look
		want  string
	}{
		"no query":          {nil, change.VerdictBlind},
		"all clean":         {[]Look{clean, clean}, change.VerdictClean},
		"one blind":         {[]Look{clean, blind}, change.VerdictBlind},
		"harm beside blind": {[]Look{blind, harm}, change.VerdictHarm},
	} {
		if got := Judge(tc.looks); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func TestPrometheusReadsTheWorstSeriesAtTheWindowsEnd(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		asked = append(asked, r.Form.Get("time"))
		switch r.Form.Get("query") {
		case "two":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"1"]},{"metric":{},"value":[1,"3"]}]}}`))
		case "none":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		case "nan":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"NaN"]}]}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"status":"error","error":"parse error"}`))
		}
	}))
	defer srv.Close()
	p := NewPrometheus(srv.URL)
	ctx := context.Background()
	if v, err := p.Read(ctx, Query{Text: "two", From: from, To: to}); err != nil || v != 3 {
		t.Errorf("the worst series: %v %v", v, err)
	}
	if asked[0] != fmt.Sprint(to.Unix()) {
		t.Errorf("read at the window's end: %s", asked[0])
	}
	for _, q := range []string{"none", "nan"} {
		if _, err := p.Read(ctx, Query{Text: q, From: from, To: to}); !errors.Is(err, ErrNoData) {
			t.Errorf("%s is no data: %v", q, err)
		}
	}
	if _, err := p.Read(ctx, Query{Text: "bad", From: from, To: to}); err == nil || errors.Is(err, ErrNoData) {
		t.Errorf("a refused query is a failed store, not no data: %v", err)
	}
}

type fixedToken string

func (f fixedToken) Token(context.Context) (string, error) { return string(f), nil }

func TestAzureMonitorReadsTheFirstNumberOverTheWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Query, Timespan string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/v1/workspaces/11111111-2222-3333-4444-555555555555/query" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
			return
		}
		if body.Timespan != "2026-11-01T12:00:00Z/2026-11-01T12:30:00Z" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.Contains(body.Query, `"shop-db"`) {
			_, _ = w.Write([]byte(`{"tables":[{"columns":[{"type":"string"},{"type":"long"}],"rows":[["shop-db",4]]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"tables":[{"columns":[],"rows":[]}]}`))
	}))
	defer srv.Close()
	a := NewAzureMonitor("11111111-2222-3333-4444-555555555555", fixedToken("tok"))
	a.UseBase(srv.URL)
	ctx := context.Background()
	if v, err := a.Read(ctx, Query{Text: `AzureMetrics | where Resource == "{title}" | count`, Params: map[string]string{"title": "shop-db"}, From: from, To: to}); err != nil || v != 4 {
		t.Errorf("a number: %v %v", v, err)
	}
	if _, err := a.Read(ctx, Query{Text: "empty", From: from, To: to}); !errors.Is(err, ErrNoData) {
		t.Errorf("no rows is no data: %v", err)
	}
	b := NewAzureMonitor("11111111-2222-3333-4444-555555555555", fixedToken("wrong"))
	b.UseBase(srv.URL)
	if _, err := b.Read(ctx, Query{Text: "x", From: from, To: to}); err == nil || errors.Is(err, ErrNoData) {
		t.Errorf("a refusal is a failed store: %v", err)
	}
}

func TestSettingsAreCheckedWhole(t *testing.T) {
	kinds := change.Base()
	good := Settings{
		Stores:  map[string]Store{"prom": {Kind: StorePrometheus, URL: "http://prometheus.example.org:9090"}},
		Queries: []Signal{{Blueprint: "k8s_workload", Signal: "restarts", Kinds: []string{"stops"}, Store: "prom", Query: "x"}},
	}
	if err := good.Validate(kinds); err != nil {
		t.Fatal(err)
	}
	if len(good.For("k8s_workload", "stops")) != 1 || len(good.For("k8s_workload", "cuts")) != 0 {
		t.Error("For picks by blueprint and kind")
	}
	for name, edit := range map[string]func(*Settings){
		"an unknown store":    func(s *Settings) { s.Queries[0].Store = "loki" },
		"an exposure kind":    func(s *Settings) { s.Queries[0].Kinds = []string{"grants"} },
		"an unknown kind":     func(s *Settings) { s.Queries[0].Kinds = []string{"halts"} },
		"no query":            func(s *Settings) { s.Queries[0].Query = " " },
		"a store with no url": func(s *Settings) { s.Stores["prom"] = Store{Kind: StorePrometheus} },
		"an unknown adapter":  func(s *Settings) { s.Stores["prom"] = Store{Kind: "loki", URL: "http://x"} },
	} {
		s := Settings{Stores: map[string]Store{"prom": good.Stores["prom"]}, Queries: []Signal{good.Queries[0]}}
		edit(&s)
		if err := s.Validate(kinds); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
}
