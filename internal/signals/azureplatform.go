package signals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"
)

// AzurePlatformScope is what the workload identity signs in for to read
// through Azure Resource Manager.
const AzurePlatformScope = "https://management.azure.com/.default"

// The API versions the platform store reads.
const (
	metricsAPI = "2023-10-01"
	healthAPI  = "2025-05-01"
)

// The two reads a platform query makes.
const (
	ReadMetrics = "metrics"
	ReadHealth  = "health"
)

// The aggregations a metrics read takes, each as the Metrics API names it.
var aggregations = map[string]string{
	"average": "Average", "minimum": "Minimum", "maximum": "Maximum", "total": "Total", "count": "Count",
}

// PlatformQuery is one line of an azure-platform store, in one of two
// forms:
//
//	health <resource>
//	metrics <resource> <metric> <aggregation> [namespace <ns>] [seen <metric>] [where <filter>]
//
// health reads the resource's Resource Health history and answers the
// minutes it was Unavailable or Degraded in the window. metrics reads one
// platform metric at one-minute grain over the window and answers it under
// its aggregation across every point and series: the sum for total and
// count, the least for minimum, the greatest for maximum, the mean for
// average. where is the Metrics API's own $filter on the metric's
// dimensions, to the end of the line. seen, for total and count only, names
// a metric whose points prove the resource reported in the window: with no
// points of its own, the read answers 0 when the seen metric has some,
// since Azure sends no series for a filter that matched nothing.
type PlatformQuery struct {
	Read        string
	Resource    string
	Metric      string
	Aggregation string
	Namespace   string
	Seen        string
	Filter      string
}

// ParsePlatform reads a platform query, before or after its parameters
// are filled.
func ParsePlatform(text string) (PlatformQuery, error) {
	head, filter, hasFilter := text, "", false
	if at := whereRe.FindStringIndex(text); at != nil {
		head, filter, hasFilter = text[:at[0]], text[at[1]:], true
	}
	words := strings.Fields(head)
	if len(words) < 2 {
		return PlatformQuery{}, errors.New("a platform query is `health <resource>` or `metrics <resource> <metric> <aggregation> ...`")
	}
	q := PlatformQuery{Read: words[0], Resource: words[1]}
	switch q.Read {
	case ReadHealth:
		if len(words) != 2 || hasFilter {
			return PlatformQuery{}, errors.New("a health query is `health <resource>` and nothing more")
		}
		return q, nil
	case ReadMetrics:
	default:
		return PlatformQuery{}, fmt.Errorf("a platform query starts with %s or %s, not %q", ReadHealth, ReadMetrics, q.Read)
	}
	if len(words) < 4 {
		return PlatformQuery{}, errors.New("a metrics query names its resource, metric, and aggregation")
	}
	q.Metric, q.Aggregation = words[2], strings.ToLower(words[3])
	if _, ok := aggregations[q.Aggregation]; !ok {
		return PlatformQuery{}, fmt.Errorf("aggregation %q is average, minimum, maximum, total, or count", words[3])
	}
	rest := words[4:]
	for len(rest) > 0 {
		if len(rest) < 2 {
			return PlatformQuery{}, fmt.Errorf("%q needs a value", rest[0])
		}
		switch rest[0] {
		case "namespace":
			q.Namespace = rest[1]
		case "seen":
			q.Seen = rest[1]
		default:
			return PlatformQuery{}, fmt.Errorf("%q is not namespace, seen, or where", rest[0])
		}
		rest = rest[2:]
	}
	if q.Seen != "" && q.Aggregation != "total" && q.Aggregation != "count" {
		return PlatformQuery{}, errors.New("seen is for a total or a count: no points of an average, a minimum, or a maximum is never 0")
	}
	if hasFilter {
		if q.Filter = strings.TrimSpace(filter); q.Filter == "" {
			return PlatformQuery{}, errors.New("where needs a filter")
		}
	}
	return q, nil
}

// whereRe is the word that starts a metrics query's filter.
var whereRe = regexp.MustCompile(`\swhere(\s|$)`)

// odataString quotes a value for an OData string in single quotes, as in
// ApiName eq '{x}'.
func odataString(v string) string { return strings.ReplaceAll(v, "'", "''") }

// AzurePlatform reads Azure's own platform metrics and Resource Health for
// a resource through Azure Resource Manager, signed in as the pod's
// workload identity. It needs nothing set up on the resource: no
// workspace and no diagnostic settings. Its role needs
// Microsoft.Insights/Metrics/Read and
// Microsoft.ResourceHealth/AvailabilityStatuses/read where it reads.
type AzurePlatform struct {
	tokens Tokens
	base   string
	client *http.Client
}

// NewAzurePlatform reads through Resource Manager with tokens.
func NewAzurePlatform(tokens Tokens) *AzurePlatform {
	return &AzurePlatform{tokens: tokens, base: "https://management.azure.com", client: telemetry.Client(60 * time.Second)}
}

// UseBase points the adapter at another server, for a test.
func (a *AzurePlatform) UseBase(base string) { a.base = strings.TrimRight(base, "/") }

// Read answers the query's number over the window.
func (a *AzurePlatform) Read(ctx context.Context, q Query) (float64, error) {
	text, err := Render(q, odataString)
	if err != nil {
		return 0, err
	}
	pq, err := ParsePlatform(text)
	if err != nil {
		return 0, err
	}
	if !strings.HasPrefix(strings.ToLower(pq.Resource), "/subscriptions/") || strings.ContainsAny(pq.Resource, "?#") {
		return 0, fmt.Errorf("%q is not a resource id", pq.Resource)
	}
	if pq.Read == ReadHealth {
		return a.health(ctx, pq.Resource, q.From, q.To)
	}
	v, err := a.metric(ctx, pq, pq.Metric, pq.Filter, q.From, q.To)
	if errors.Is(err, ErrNoData) && pq.Seen != "" {
		if _, seen := a.metric(ctx, pq, pq.Seen, "", q.From, q.To); seen == nil {
			return 0, nil
		}
	}
	return v, err
}

// metric reads one metric of the query's resource and reduces it to one
// number under the query's aggregation.
func (a *AzurePlatform) metric(ctx context.Context, pq PlatformQuery, name, filter string, from, to time.Time) (float64, error) {
	agg := aggregations[pq.Aggregation]
	params := url.Values{
		"api-version": {metricsAPI},
		"metricnames": {name},
		"aggregation": {agg},
		"interval":    {"PT1M"},
		"timespan":    {from.UTC().Format(time.RFC3339) + "/" + to.UTC().Format(time.RFC3339)},
	}
	if filter != "" {
		params.Set("$filter", filter)
	}
	if pq.Namespace != "" {
		params.Set("metricnamespace", pq.Namespace)
	}
	var answer struct {
		Value []struct {
			ErrorCode    string `json:"errorCode"`
			ErrorMessage string `json:"errorMessage"`
			Timeseries   []struct {
				Data []point `json:"data"`
			} `json:"timeseries"`
		} `json:"value"`
	}
	if err := a.get(ctx, a.base+pq.Resource+"/providers/Microsoft.Insights/metrics?"+params.Encode(), &answer); err != nil {
		return 0, err
	}
	var values []float64
	for _, m := range answer.Value {
		if m.ErrorCode != "" && m.ErrorCode != "Success" {
			return 0, fmt.Errorf("azure metrics answered %s: %s", m.ErrorCode, m.ErrorMessage)
		}
		for _, s := range m.Timeseries {
			for _, p := range s.Data {
				if v := p.under(pq.Aggregation); v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) {
					values = append(values, *v)
				}
			}
		}
	}
	if len(values) == 0 {
		return 0, ErrNoData
	}
	out := values[0]
	switch pq.Aggregation {
	case "total", "count", "average":
		out = 0
		for _, v := range values {
			out += v
		}
		if pq.Aggregation == "average" {
			out /= float64(len(values))
		}
	case "minimum":
		for _, v := range values {
			out = math.Min(out, v)
		}
	case "maximum":
		for _, v := range values {
			out = math.Max(out, v)
		}
	}
	return out, nil
}

// point is one minute of a metric, under each aggregation it was asked for.
type point struct {
	Average *float64 `json:"average"`
	Minimum *float64 `json:"minimum"`
	Maximum *float64 `json:"maximum"`
	Total   *float64 `json:"total"`
	Count   *float64 `json:"count"`
}

// under is the point's value under an aggregation, or nil when it has none.
func (p point) under(aggregation string) *float64 {
	switch aggregation {
	case "average":
		return p.Average
	case "minimum":
		return p.Minimum
	case "maximum":
		return p.Maximum
	case "total":
		return p.Total
	}
	return p.Count
}

// transition is one Resource Health state and when it began.
type transition struct {
	state string
	at    time.Time
}

// health answers the minutes the resource was Unavailable or Degraded in
// the window, from its availability history (Availability Statuses -
// List, which lists every transition Resource Health keeps for the
// resource). A window that is partly Unknown, or that starts before the
// history does, is no data unless it saw harm.
func (a *AzurePlatform) health(ctx context.Context, resource string, from, to time.Time) (float64, error) {
	var ts []transition
	next := a.base + resource + "/providers/Microsoft.ResourceHealth/availabilityStatuses?api-version=" + healthAPI
	for page := 0; next != "" && page < 20; page++ {
		var answer struct {
			Value []struct {
				Properties struct {
					AvailabilityState string    `json:"availabilityState"`
					OccuredTime       time.Time `json:"occuredTime"`
				} `json:"properties"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := a.get(ctx, next, &answer); err != nil {
			return 0, err
		}
		for _, v := range answer.Value {
			// A status with no state is an impacting event, not a transition.
			if v.Properties.AvailabilityState != "" {
				ts = append(ts, transition{state: v.Properties.AvailabilityState, at: v.Properties.OccuredTime})
			}
		}
		// The token goes only to Resource Manager: a next page anywhere
		// else is not followed.
		next = ""
		if strings.HasPrefix(answer.NextLink, a.base+"/") {
			next = answer.NextLink
		}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].at.Before(ts[j].at) })
	unknown := len(ts) == 0 || ts[0].at.After(from)
	var harm time.Duration
	for i, t := range ts {
		end := to
		if i+1 < len(ts) {
			end = ts[i+1].at
		}
		start := t.at
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if !end.After(start) {
			continue
		}
		switch t.state {
		case "Unavailable", "Degraded":
			harm += end.Sub(start)
		case "Available":
		default:
			unknown = true
		}
	}
	switch {
	case harm > 0:
		return harm.Minutes(), nil
	case unknown:
		return 0, ErrNoData
	}
	return 0, nil
}

// get reads one Resource Manager answer into out.
func (a *AzurePlatform) get(ctx context.Context, u string, out any) error {
	token, err := a.tokens.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("azure resource manager: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("azure resource manager: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		// Resource Manager wraps its error; Resource Health answers it bare.
		var e struct {
			Error   struct{ Code, Message string } `json:"error"`
			Code    string                         `json:"code"`
			Message string                         `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		code, msg := e.Error.Code, e.Error.Message
		if code == "" {
			code, msg = e.Code, e.Message
		}
		return fmt.Errorf("azure resource manager answered %d %s: %s", res.StatusCode, code, msg)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("azure resource manager answered %d with no JSON", res.StatusCode)
	}
	return nil
}
