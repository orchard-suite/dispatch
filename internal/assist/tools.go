package assist

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolServer serves investigations' tools to Ion over MCP, on the loopback
// interface only. Ion's configuration names one server, so every tool takes
// the investigation it belongs to as an argument, and the server routes
// each call to that investigation's tools and keeps its evidence.
type toolServer struct {
	url            string
	srv            *http.Server
	mcp            *mcp.Server
	mu             sync.Mutex
	investigations map[string]*investigation
	listed         map[string]bool
}

type investigation struct {
	mu    sync.Mutex
	tools map[string]Tool
	steps []Step
}

func (i *investigation) evidence() []Step {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]Step(nil), i.steps...)
}

// serverName is the MCP server's name in Ion's configuration; Claude sees
// each tool as mcp__dispatch__<name>.
const serverName = "dispatch"

func startToolServer() (*toolServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ts := &toolServer{investigations: map[string]*investigation{}, listed: map[string]bool{}}
	ts.mcp = mcp.NewServer(&mcp.Implementation{Name: serverName, Version: "1"}, nil)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ts.mcp }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	ts.srv = &http.Server{Handler: mux}
	ts.url = "http://" + ln.Addr().String() + "/mcp"
	go ts.srv.Serve(ln)
	return ts, nil
}

func (ts *toolServer) close() { ts.srv.Close() }

// open registers an investigation's tools under its id, and lists each tool
// with the server the first time its name is seen.
func (ts *toolServer) open(id string, tools []Tool) *investigation {
	inv := &investigation{tools: map[string]Tool{}}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, t := range tools {
		inv.tools[t.Name] = t
		if !ts.listed[t.Name] {
			ts.listed[t.Name] = true
			ts.mcp.AddTool(&mcp.Tool{Name: t.Name, Description: t.Description, InputSchema: schema(t)}, ts.handler(t.Name))
		}
	}
	ts.investigations[id] = inv
	return inv
}

func (ts *toolServer) end(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.investigations, id)
}

// schema is the tool's own inputs plus the investigation it belongs to.
func schema(t Tool) map[string]any {
	props := map[string]any{"investigation": map[string]any{"type": "string", "description": "The investigation id from your instructions."}}
	for k, v := range t.Inputs {
		props[k] = v
	}
	return map[string]any{"type": "object", "properties": props, "required": append([]string{"investigation"}, t.Required...)}
}

// handler runs one call for whichever investigation it names, numbers it
// as evidence, and answers it to Claude with its number.
func (ts *toolServer) handler(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]json.RawMessage
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return failed("the arguments must be a JSON object"), nil
		}
		var id string
		json.Unmarshal(args["investigation"], &id)
		delete(args, "investigation")
		ts.mu.Lock()
		inv := ts.investigations[id]
		ts.mu.Unlock()
		if inv == nil {
			return failed("there is no investigation " + fmt.Sprintf("%q", id) + "; pass the id from your instructions"), nil
		}
		input, _ := json.Marshal(args)
		inv.mu.Lock()
		step := Step{N: len(inv.steps) + 1, Tool: name, Input: string(input)}
		t, known := inv.tools[name]
		if known && t.Called != nil {
			if called := t.Called(input); called != "" {
				step.Tool = called
			}
		}
		full := len(inv.steps) >= maxCalls
		inv.steps = append(inv.steps, step)
		inv.mu.Unlock()
		switch {
		case full:
			step.Answer, step.Failed = "No more tool calls: finish with what you have.", true
		case !known:
			step.Answer, step.Failed = "This investigation has no such tool.", true
		default:
			answer, err := t.Run(ctx, input)
			if err != nil {
				answer, step.Failed = "The tool failed: "+err.Error(), true
			}
			step.Answer = clip(answer, 12000)
		}
		inv.mu.Lock()
		inv.steps[step.N-1] = step
		inv.mu.Unlock()
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("[E%d]\n<data>\n%s\n</data>", step.N, step.Answer)}},
			IsError: step.Failed,
		}, nil
	}
}

func failed(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}, IsError: true}
}
