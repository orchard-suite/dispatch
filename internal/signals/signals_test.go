package signals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestRenderLeavesTheStoresOwnBraces(t *testing.T) {
	q := Query{Text: `sum(increase(r{ns="{<-k8s_namespace:workload.title}", code=~"5..", pod=~"{name}-([a-z0-9]{6,10}-)?[a-z0-9]{1,5}"}[{window}])) / count(up{job="{labels.app.kubernetes.io/name}"})`,
		Params: map[string]string{"<-k8s_namespace:workload.title": "shop", "name": "shop", "labels.app.kubernetes.io/name": "shop"}, From: from, To: to}
	got, err := Render(q, promString)
	want := `sum(increase(r{ns="shop", code=~"5..", pod=~"shop-([a-z0-9]{6,10}-)?[a-z0-9]{1,5}"}[1800s])) / count(up{job="shop"})`
	if err != nil || got != want {
		t.Errorf("%s %v", got, err)
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
	if len(good.For("k8s_workload", "stops", nil)) != 1 || len(good.For("k8s_workload", "cuts", nil)) != 0 {
		t.Error("For picks by blueprint and kind")
	}
	platform := Settings{
		Stores: map[string]Store{"azure": {Kind: StoreAzurePlatform}},
		Queries: []Signal{{Blueprint: "azure_resource", Where: map[string]string{"type": "Microsoft.Storage/storageAccounts"}, Signal: "availability",
			Kinds: []string{"stops"}, Store: "azure", Query: "metrics {resource_id} Availability minimum", Below: num(100)}},
	}
	if err := platform.Validate(kinds); err != nil {
		t.Errorf("a platform store and a where: %v", err)
	}
	for name, edit := range map[string]func(*Settings){
		"an unknown store":        func(s *Settings) { s.Queries[0].Store = "loki" },
		"an exposure kind":        func(s *Settings) { s.Queries[0].Kinds = []string{"grants"} },
		"an unknown kind":         func(s *Settings) { s.Queries[0].Kinds = []string{"halts"} },
		"no query":                func(s *Settings) { s.Queries[0].Query = " " },
		"a store with no url":     func(s *Settings) { s.Stores["prom"] = Store{Kind: StorePrometheus} },
		"an unknown adapter":      func(s *Settings) { s.Stores["prom"] = Store{Kind: "loki", URL: "http://x"} },
		"a where on no parameter": func(s *Settings) { s.Queries[0].Where = map[string]string{"Type": "x"} },
		"a where with no value":   func(s *Settings) { s.Queries[0].Where = map[string]string{"type": ""} },
		"below above above":       func(s *Settings) { s.Queries[0].Below, s.Queries[0].Above = num(5), num(1) },
		"a platform store with a url": func(s *Settings) {
			s.Stores["prom"] = Store{Kind: StoreAzurePlatform, URL: "http://x"}
		},
		"a platform query it cannot read": func(s *Settings) {
			s.Stores["prom"] = Store{Kind: StoreAzurePlatform}
			s.Queries[0].Query = "metrics {resource_id} Availability median"
		},
	} {
		s := Settings{Stores: map[string]Store{"prom": good.Stores["prom"]}, Queries: []Signal{good.Queries[0]}}
		edit(&s)
		if err := s.Validate(kinds); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
}

func num(v float64) *float64 { return &v }

func TestAQueryReadsOnlyTheSubjectsItsWhereMatches(t *testing.T) {
	s := Settings{Queries: []Signal{
		{Blueprint: "azure_resource", Where: map[string]string{"type": "Microsoft.Storage/storageAccounts"}, Signal: "health", Kinds: []string{"stops"}},
		{Blueprint: "azure_resource", Where: map[string]string{"type": "Microsoft.KeyVault/vaults"}, Signal: "health", Kinds: []string{"stops"}},
		{Blueprint: "azure_resource", Signal: "any", Kinds: []string{"stops"}},
	}}
	got := s.For("azure_resource", "stops", map[string]string{"type": "Microsoft.KeyVault/vaults"})
	if len(got) != 2 || got[0].Where["type"] != "Microsoft.KeyVault/vaults" || got[1].Signal != "any" {
		t.Errorf("the vault's query and the one with no where: %+v", got)
	}
	if got := s.For("azure_resource", "stops", map[string]string{"title": "shop"}); len(got) != 1 {
		t.Errorf("a subject with no such field matches no where: %+v", got)
	}
}

func TestHarmIsAboveOrBelowAndAboveZeroWithNeither(t *testing.T) {
	for name, tc := range map[string]struct {
		q    Signal
		v    float64
		harm bool
	}{
		"neither, 1":       {Signal{}, 1, true},
		"neither, 0":       {Signal{}, 0, false},
		"above 5, 6":       {Signal{Above: num(5)}, 6, true},
		"above 5, 5":       {Signal{Above: num(5)}, 5, false},
		"below 100, 99.5":  {Signal{Below: num(100)}, 99.5, true},
		"below 100, 100":   {Signal{Below: num(100)}, 100, false},
		"below 100, 0":     {Signal{Below: num(100)}, 0, true},
		"a band, inside":   {Signal{Below: num(1), Above: num(5)}, 3, false},
		"a band, under it": {Signal{Below: num(1), Above: num(5)}, 0, true},
	} {
		if harm, why := tc.q.Harm(tc.v); harm != tc.harm || (harm && why == "") {
			t.Errorf("%s: %v %q", name, harm, why)
		}
	}
}

func TestPlatformQueriesParse(t *testing.T) {
	q, err := ParsePlatform(`metrics {resource_id} Transactions Total namespace Microsoft.Storage/storageAccounts seen Transactions where ResponseType eq 'AuthorizationError' or ResponseType eq 'ClientThrottlingError'`)
	if err != nil || q.Read != ReadMetrics || q.Resource != "{resource_id}" || q.Metric != "Transactions" || q.Aggregation != "total" ||
		q.Namespace != "Microsoft.Storage/storageAccounts" || q.Seen != "Transactions" || q.Filter != "ResponseType eq 'AuthorizationError' or ResponseType eq 'ClientThrottlingError'" {
		t.Errorf("%+v %v", q, err)
	}
	if q, err := ParsePlatform("health {resource_id}"); err != nil || q.Read != ReadHealth {
		t.Errorf("%+v %v", q, err)
	}
	for _, bad := range []string{
		"", "health", "health {resource_id} extra", "logs {resource_id}",
		"metrics {resource_id} Availability", "metrics {resource_id} Availability median",
		"metrics {resource_id} Availability minimum seen Transactions",
		"metrics {resource_id} Transactions total where", "metrics {resource_id} Transactions total label x",
		"metrics {resource_id} Transactions total seen",
	} {
		if _, err := ParsePlatform(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

const shopAccount = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/shop/providers/Microsoft.Storage/storageAccounts/shop"

func TestAzurePlatformReadsAMetricUnderItsAggregation(t *testing.T) {
	var asked []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != shopAccount+"/providers/Microsoft.Insights/metrics" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"AuthorizationFailed","message":"no"}}`))
			return
		}
		q := r.URL.Query()
		asked = append(asked, q)
		switch {
		case q.Get("metricnames") == "Broken":
			_, _ = w.Write([]byte(`{"value":[{"errorCode":"InvalidSamplingType","errorMessage":"bad","timeseries":[]}]}`))
		case q.Get("$filter") == "ResponseType eq 'AuthorizationError'":
			_, _ = w.Write([]byte(`{"value":[{"errorCode":"Success","timeseries":[]}]}`))
		case q.Get("metricnames") == "Quiet":
			_, _ = w.Write([]byte(`{"value":[{"errorCode":"Success","timeseries":[{"data":[{"timeStamp":"2026-11-01T12:00:00Z"}]}]}]}`))
		case q.Get("aggregation") == "Minimum":
			_, _ = w.Write([]byte(`{"value":[{"errorCode":"Success","timeseries":[{"data":[{"timeStamp":"2026-11-01T12:00:00Z","minimum":100},{"timeStamp":"2026-11-01T12:01:00Z"},{"timeStamp":"2026-11-01T12:02:00Z","minimum":97.5}]}]}]}`))
		default:
			_, _ = w.Write([]byte(`{"value":[{"errorCode":"Success","timeseries":[{"data":[{"timeStamp":"2026-11-01T12:00:00Z","total":2},{"timeStamp":"2026-11-01T12:01:00Z","total":0}]},{"data":[{"timeStamp":"2026-11-01T12:00:00Z","total":3}]}]}]}`))
		}
	}))
	defer srv.Close()
	a := NewAzurePlatform(fixedToken("tok"))
	a.UseBase(srv.URL)
	ctx := context.Background()
	read := func(text string) (float64, error) {
		return a.Read(ctx, Query{Text: text, Params: map[string]string{"resource_id": shopAccount, "title": "o'shop"}, From: from, To: to})
	}
	if v, err := read("metrics {resource_id} Transactions total where ApiName eq '{title}'"); err != nil || v != 5 {
		t.Errorf("a total is the sum of every point of every series: %v %v", v, err)
	}
	q := asked[len(asked)-1]
	if q.Get("timespan") != "2026-11-01T12:00:00Z/2026-11-01T12:30:00Z" || q.Get("interval") != "PT1M" || q.Get("aggregation") != "Total" ||
		q.Get("$filter") != "ApiName eq 'o''shop'" || q.Get("api-version") == "" {
		t.Errorf("the request: %v", q)
	}
	if v, err := read("metrics {resource_id} Availability minimum"); err != nil || v != 97.5 {
		t.Errorf("a minimum is the least point: %v %v", v, err)
	}
	if _, err := read("metrics {resource_id} Transactions total where ResponseType eq 'AuthorizationError'"); !errors.Is(err, ErrNoData) {
		t.Errorf("no series is no data: %v", err)
	}
	if v, err := read("metrics {resource_id} Transactions total seen Transactions where ResponseType eq 'AuthorizationError'"); err != nil || v != 0 {
		t.Errorf("no series while the seen metric reports is 0: %v %v", v, err)
	}
	if _, err := read("metrics {resource_id} Transactions total seen Quiet where ResponseType eq 'AuthorizationError'"); !errors.Is(err, ErrNoData) {
		t.Errorf("no series while the seen metric has no points is no data: %v", err)
	}
	if _, err := read("metrics {resource_id} Broken total"); err == nil || errors.Is(err, ErrNoData) {
		t.Errorf("a metric's error is a failed store: %v", err)
	}
	if _, err := a.Read(ctx, Query{Text: "metrics {resource_id} Transactions total", Params: map[string]string{"resource_id": "shop"}, From: from, To: to}); err == nil {
		t.Error("a resource that is no resource id was read")
	}
	b := NewAzurePlatform(fixedToken("wrong"))
	b.UseBase(srv.URL)
	if _, err := b.Read(ctx, Query{Text: "metrics {resource_id} Transactions total", Params: map[string]string{"resource_id": shopAccount}, From: from, To: to}); err == nil || errors.Is(err, ErrNoData) || !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("a refusal is a failed store: %v", err)
	}
}

func TestAzurePlatformHealthCountsTheMinutesDownInTheWindow(t *testing.T) {
	answers := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/shop/providers/Microsoft.Storage/storageAccounts/"), "/providers/Microsoft.ResourceHealth/availabilityStatuses")
		body, ok := answers[name]
		if !ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":"UnsupportedResourceType","message":"Resource type not supported."}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	status := func(state, at string) string {
		return `{"properties":{"availabilityState":"` + state + `","occuredTime":"` + at + `"}}`
	}
	answers["down"] = `{"value":[` + status("Available", "2026-11-01T12:20:00Z") + `,` + status("Unavailable", "2026-11-01T12:10:00Z") + `,` + status("Available", "2026-10-01T00:00:00Z") + `,{"properties":{"occuredTime":"2026-11-01T12:12:00Z"}}]}`
	answers["degraded"] = `{"value":[` + status("Degraded", "2026-11-01T12:25:00Z") + `],"nextLink":"` + srv.URL + shopAccount[:len(shopAccount)-4] + `older/providers/Microsoft.ResourceHealth/availabilityStatuses"}`
	answers["older"] = `{"value":[` + status("Available", "2026-10-01T00:00:00Z") + `]}`
	answers["up"] = `{"value":[` + status("Available", "2026-10-01T00:00:00Z") + `]}`
	answers["new"] = `{"value":[` + status("Available", "2026-11-01T12:05:00Z") + `]}`
	answers["unknown"] = `{"value":[` + status("Unknown", "2026-11-01T12:05:00Z") + `,` + status("Available", "2026-10-01T00:00:00Z") + `]}`
	a := NewAzurePlatform(fixedToken("tok"))
	a.UseBase(srv.URL)
	read := func(name string) (float64, error) {
		return a.Read(context.Background(), Query{Text: "health {resource_id}", Params: map[string]string{"resource_id": shopAccount[:len(shopAccount)-4] + name}, From: from, To: to})
	}
	for name, want := range map[string]float64{"down": 10, "degraded": 5, "up": 0} {
		if v, err := read(name); err != nil || v != want {
			t.Errorf("%s: %v %v, want %v", name, v, err, want)
		}
	}
	for _, name := range []string{"new", "unknown"} {
		if _, err := read(name); !errors.Is(err, ErrNoData) {
			t.Errorf("%s: a window the history does not cover is no data: %v", name, err)
		}
	}
	if _, err := read("vm"); err == nil || errors.Is(err, ErrNoData) || !strings.Contains(err.Error(), "UnsupportedResourceType") {
		t.Errorf("a type Resource Health does not cover is a failed store: %v", err)
	}
}

func TestThePlatformStoreSignsInForResourceManager(t *testing.T) {
	var scopes []string
	o := NewOpener(func(scope string) (Tokens, error) { scopes = append(scopes, scope); return fixedToken("tok"), nil })
	st := Store{Kind: StoreAzurePlatform}
	if _, err := o.Open("azure", st); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Open("azure", st); err != nil || len(scopes) != 1 || scopes[0] != AzurePlatformScope {
		t.Errorf("signed in once for Resource Manager: %v %v", scopes, err)
	}
	if _, err := NewOpener(nil).Open("azure", st); err == nil {
		t.Error("a desk with no workload identity opened an Azure store")
	}
}
