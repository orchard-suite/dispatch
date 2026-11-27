package signals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orchard-suite/platformkit/v3/telemetry"
)

// Tokens gives the bearer token for a scope, such as the pod's workload
// identity signed in to Log Analytics.
type Tokens interface {
	Token(ctx context.Context) (string, error)
}

// AzureMonitorScope is what the workload identity signs in for.
const AzureMonitorScope = "https://api.loganalytics.io/.default"

// AzureMonitor reads KQL from a Log Analytics workspace over the window,
// signed in as the pod's workload identity, which needs Log Analytics
// Reader on the workspace or the resources it reads.
type AzureMonitor struct {
	workspace string
	tokens    Tokens
	base      string
	client    *http.Client
}

// NewAzureMonitor reads the workspace with tokens.
func NewAzureMonitor(workspace string, tokens Tokens) *AzureMonitor {
	return &AzureMonitor{workspace: workspace, tokens: tokens, base: "https://api.loganalytics.io", client: telemetry.Client(60 * time.Second)}
}

// UseBase points the adapter at another server, for a test.
func (a *AzureMonitor) UseBase(base string) { a.base = strings.TrimRight(base, "/") }

// kqlString quotes a value for a KQL string in double quotes.
func kqlString(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// Read answers the first number in the first row the query returns.
func (a *AzureMonitor) Read(ctx context.Context, q Query) (float64, error) {
	text, err := Render(q, kqlString)
	if err != nil {
		return 0, err
	}
	token, err := a.tokens.Token(ctx)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]string{"query": text, "timespan": q.From.UTC().Format(time.RFC3339) + "/" + q.To.UTC().Format(time.RFC3339)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/v1/workspaces/"+url.PathEscape(a.workspace)+"/query", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("azure monitor: %w", err)
	}
	defer res.Body.Close()
	var answer struct {
		Tables []struct {
			Columns []struct {
				Type string `json:"type"`
			} `json:"columns"`
			Rows [][]any `json:"rows"`
		} `json:"tables"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&answer); err != nil {
		return 0, fmt.Errorf("azure monitor answered %d with no JSON", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("azure monitor answered %d: %s", res.StatusCode, answer.Error.Message)
	}
	if len(answer.Tables) == 0 || len(answer.Tables[0].Rows) == 0 {
		return 0, ErrNoData
	}
	for _, cell := range answer.Tables[0].Rows[0] {
		if f, ok := cell.(float64); ok {
			return number(f)
		}
	}
	return 0, ErrNoData
}
