package assist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The test binary doubles as a fake `ion`: run with FAKE_ION set, it reads
// the prompt and the engine.json the assistant wrote, plays one run the way
// the engine streams it, and keeps the stream open after the run goes idle,
// as `ion prompt --output stream-json` does.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_ION"); mode != "" {
		os.Exit(fakeIon(mode))
	}
	os.Exit(m.Run())
}

func emit(typ string, extra map[string]any) {
	ev := map[string]any{"type": typ}
	for k, v := range extra {
		ev[k] = v
	}
	b, _ := json.Marshal(map[string]any{"key": "prompt-1", "event": ev})
	fmt.Println(string(b))
}

func status(state string) {
	emit("engine_status", map[string]any{"fields": map[string]any{"state": state}})
}

var invRe = regexp.MustCompile(`inv-[0-9a-f]{16}`)

func fakeIon(mode string) int {
	prompt, _ := io.ReadAll(os.Stdin)
	raw, err := os.ReadFile(filepath.Join(os.Getenv("ION_DATA_DIR"), "engine.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "no engine.json")
		return 1
	}
	var conf struct {
		MCPServers map[string]struct{ URL string } `json:"mcpServers"`
	}
	json.Unmarshal(raw, &conf)
	status("running")
	switch mode {
	case "fail":
		emit("engine_error", map[string]any{"message": "auth: no API key resolved"})
		emit("engine_dead", map[string]any{"exitCode": 1})
	case "quit":
		fmt.Fprintln(os.Stderr, "Error: engine failed to start within 5s")
		return 1
	case "write":
		if !strings.Contains(string(prompt), "<data>\nthe material\n</data>") {
			emit("engine_text_delta", map[string]any{"text": "the prompt lost the material"})
		} else {
			emit("engine_text_delta", map[string]any{"text": "Five short "})
			emit("engine_text_delta", map[string]any{"text": "lines."})
		}
		status("idle")
	case "investigate":
		id := invRe.FindString(string(prompt))
		emit("engine_text_delta", map[string]any{"text": "Let me look at the logs."})
		emit("engine_tool_start", map[string]any{"toolName": "mcp__dispatch__hub_tool"})
		answer, err := callTool(conf.MCPServers["dispatch"].URL, "hub_tool", map[string]any{"investigation": id, "tool": "shop_logs", "inputs": map[string]any{"since": "30m"}})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if !strings.HasPrefix(answer, "[E1]") {
			emit("engine_text_delta", map[string]any{"text": "the tool answered without its number: " + answer})
		} else {
			emit("engine_text_delta", map[string]any{"text": "Checkout fails on a full disk [E1]."})
		}
		status("idle")
	}
	// The stream stays open after idle; the assistant must stop reading.
	time.Sleep(time.Minute)
	return 0
}

func callTool(url, name string, args map[string]any) (string, error) {
	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "fake-ion", Version: "1"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		return "", err
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", err
	}
	return res.Content[0].(*mcp.TextContent).Text, nil
}

func assistant(t *testing.T, mode, backend string, tools []string) *Assistant {
	t.Helper()
	t.Setenv("FAKE_ION", mode)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Ion: self, DataDir: t.TempDir(), Backend: backend, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestAWriteIsTheTextOfOneRun(t *testing.T) {
	a := assistant(t, "write", "claude-code", nil)
	start := time.Now()
	got, err := a.Write(context.Background(), "Catch someone up.", "the material")
	if err != nil || got != "Five short lines." {
		t.Fatalf("write: %q %v", got, err)
	}
	if time.Since(start) > 20*time.Second {
		t.Error("the run went idle but the assistant kept reading the stream")
	}
}

// stoodFor is a tool that stands for many, as the desk's hub tool does:
// the call says which one it ran.
func stoodFor(answer string) Tool {
	return Tool{Name: "hub_tool", Inputs: map[string]any{"tool": map[string]any{"type": "string"}, "inputs": map[string]any{"type": "object"}}, Required: []string{"tool"},
		Called: func(raw json.RawMessage) string {
			var in struct{ Tool string }
			json.Unmarshal(raw, &in)
			if in.Tool != "shop_logs" {
				return ""
			}
			return in.Tool
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			if !strings.Contains(string(raw), "30m") {
				return "", errors.New("the inputs were lost")
			}
			return answer, nil
		}}
}

func TestAnInvestigationCitesWhatItRead(t *testing.T) {
	a := assistant(t, "investigate", "claude-code", []string{"hub_tool"})
	f, err := a.Investigate(context.Background(), "Checkout failing", []Tool{stoodFor("no space left on device")})
	if err != nil {
		t.Fatal(err)
	}
	if f.Text != "Checkout fails on a full disk [E1]." {
		t.Errorf("the finding is what Claude wrote after its last tool call: %q", f.Text)
	}
	if len(f.Evidence) != 1 || f.Evidence[0].Failed || !strings.Contains(f.Evidence[0].Answer, "no space left") || strings.Contains(f.Evidence[0].Input, "investigation") {
		t.Fatalf("evidence %+v", f.Evidence)
	}
	if f.Evidence[0].Tool != "shop_logs" {
		t.Errorf("a step is kept under the tool the call really ran: %q", f.Evidence[0].Tool)
	}
}

// A tool that stands for many keeps its own name on a call that names
// nothing it knows, so text Claude wrote never names a step.
func TestAStepIsNamedOnlyByAKnownTool(t *testing.T) {
	ts, err := startToolServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ts.close()
	inv := ts.open("inv-0000000000000002", []Tool{stoodFor("lines")})
	callTool(ts.url, "hub_tool", map[string]any{"investigation": "inv-0000000000000002", "tool": "<b>made up</b>", "inputs": map[string]any{"since": "30m"}})
	if steps := inv.evidence(); len(steps) != 1 || steps[0].Tool != "hub_tool" {
		t.Errorf("steps %+v", steps)
	}
}

func TestAToolCallNamesItsInvestigation(t *testing.T) {
	ts, err := startToolServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ts.close()
	runs := 0
	tool := Tool{Name: "incident_record", Inputs: map[string]any{}, Run: func(context.Context, json.RawMessage) (string, error) { runs++; return "lines", nil }}
	inv := ts.open("inv-0000000000000001", []Tool{tool})
	if got, _ := callTool(ts.url, "incident_record", map[string]any{"investigation": "inv-9999999999999999"}); !strings.Contains(got, "no investigation") {
		t.Errorf("a call for another investigation: %q", got)
	}
	for i := 0; i < maxCalls+1; i++ {
		callTool(ts.url, "incident_record", map[string]any{"investigation": "inv-0000000000000001"})
	}
	steps := inv.evidence()
	if runs != maxCalls || len(steps) != maxCalls+1 || !steps[maxCalls].Failed {
		t.Errorf("an investigation runs at most %d calls: ran %d, kept %d", maxCalls, runs, len(steps))
	}
	ts.end("inv-0000000000000001")
	if got, _ := callTool(ts.url, "incident_record", map[string]any{"investigation": "inv-0000000000000001"}); !strings.Contains(got, "no investigation") {
		t.Errorf("a finished investigation takes no more calls: %q", got)
	}
}

func TestEngineFailuresAreErrors(t *testing.T) {
	for _, mode := range []string{"fail", "quit"} {
		a := assistant(t, mode, "api", nil)
		if _, err := a.Write(context.Background(), "x", "y"); !errors.Is(err, ErrEngine) {
			t.Errorf("%s: %v", mode, err)
		}
	}
}

// Ion's own permissions allow the dispatch tools and nothing else, and
// the configuration names the backend it was given.
func TestTheEngineIsConfiguredToReadOnly(t *testing.T) {
	dir := t.TempDir()
	a, err := New(Config{DataDir: dir, Backend: "api", Tools: []string{"hub_tool", "incident_record"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, "engine.json"))
	var conf struct {
		Backend     string
		Permissions struct {
			Mode  string
			Rules []struct{ Tool, Decision string }
		}
		MCPServers map[string]struct{ Type, URL string } `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, r := range conf.Permissions.Rules {
		allowed[r.Tool] = r.Decision == "allow"
	}
	if conf.Backend != "api" || conf.Permissions.Mode != "deny" || len(allowed) != 3 ||
		!allowed["mcp__dispatch__hub_tool"] || !allowed["mcp__dispatch__incident_record"] || !allowed["ToolSearch"] {
		t.Errorf("config %+v", conf)
	}
	if s := conf.MCPServers["dispatch"]; s.Type != "http" || !strings.HasPrefix(s.URL, "http://127.0.0.1:") {
		t.Errorf("the tools are served on the loopback only: %+v", s)
	}
	if _, err := a.Investigate(context.Background(), "x", []Tool{{Name: "delete_everything"}}); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("a tool the engine does not allow: %v", err)
	}
	gw := t.TempDir()
	b, err := New(Config{DataDir: gw, Backend: "api", BaseURL: "https://ai.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	raw, _ = os.ReadFile(filepath.Join(gw, "engine.json"))
	var withGateway struct {
		Providers map[string]struct {
			BaseURL string `json:"baseURL"`
		}
	}
	json.Unmarshal(raw, &withGateway)
	if withGateway.Providers["anthropic"].BaseURL != "https://ai.example.com" {
		t.Errorf("a base URL sends Anthropic's API to the gateway: %s", raw)
	}
	if _, err := New(Config{DataDir: dir, Backend: "openai"}); err == nil {
		t.Error("an unknown backend")
	}
}
