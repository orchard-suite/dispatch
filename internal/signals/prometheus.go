package signals

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"
)

// Prometheus reads PromQL from a Prometheus HTTP API: an instant query at
// the window's end, so a range in the query, such as [{window}], covers
// the window.
type Prometheus struct {
	url    string
	client *http.Client
}

// NewPrometheus reads the Prometheus at url.
func NewPrometheus(url string) *Prometheus {
	return &Prometheus{url: strings.TrimRight(url, "/"), client: telemetry.Client(30 * time.Second)}
}

// promString quotes a value for a PromQL string, as in namespace="{x}".
func promString(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// Read answers the query's number: the largest of the series it returns,
// so the worst of several pods counts.
func (p *Prometheus) Read(ctx context.Context, q Query) (float64, error) {
	text, err := Render(q, promString)
	if err != nil {
		return 0, err
	}
	form := url.Values{"query": {text}, "time": {strconv.FormatInt(q.To.Unix(), 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url+"/api/v1/query", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := p.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("prometheus: %w", err)
	}
	defer res.Body.Close()
	var answer struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&answer); err != nil {
		return 0, fmt.Errorf("prometheus answered %d with no JSON", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK || answer.Status != "success" {
		return 0, fmt.Errorf("prometheus answered %d: %s", res.StatusCode, answer.Error)
	}
	var values [][2]any
	switch answer.Data.ResultType {
	case "scalar":
		var v [2]any
		if err := json.Unmarshal(answer.Data.Result, &v); err != nil {
			return 0, err
		}
		values = append(values, v)
	case "vector":
		var series []struct {
			Value [2]any `json:"value"`
		}
		if err := json.Unmarshal(answer.Data.Result, &series); err != nil {
			return 0, err
		}
		for _, s := range series {
			values = append(values, s.Value)
		}
	default:
		return 0, fmt.Errorf("prometheus answered a %s; a watch query answers a number or a vector", answer.Data.ResultType)
	}
	best, seen := 0.0, false
	for _, v := range values {
		s, _ := v[1].(string)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		if f, err = number(f); err != nil {
			continue
		}
		if !seen || f > best {
			best, seen = f, true
		}
	}
	if !seen {
		return 0, ErrNoData
	}
	return best, nil
}
