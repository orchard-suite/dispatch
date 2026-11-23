package assist

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ion runs one prompt at a time through the Ion engine's command line.
// `ion prompt` starts the engine on first use, from the configuration in
// DataDir, and every prompt is its own short-lived session.
type ion struct {
	bin, dataDir, socket, model, work string
	allowed                           map[string]bool
}

// runTimeout bounds one prompt; the desk gives an ask ten minutes.
const runTimeout = 8 * time.Minute

func newIon(cfg Config, toolURL string, tools []string) (*ion, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("ION_DATA_DIR is required for the assistant")
	}
	work := filepath.Join(cfg.DataDir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	socket := cfg.Socket
	if socket == "" {
		socket = filepath.Join(cfg.DataDir, "engine.sock")
	}
	in := &ion{bin: cfg.Ion, dataDir: cfg.DataDir, socket: socket, model: cfg.Model, work: work, allowed: map[string]bool{}}
	// ToolSearch only loads a deferred tool's schema; Claude Code defers MCP
	// tools behind it, so the dispatch tools are unreachable without it.
	rules := []map[string]string{{"tool": "ToolSearch", "decision": "allow"}}
	for _, t := range tools {
		in.allowed[t] = true
		rules = append(rules, map[string]string{"tool": "mcp__" + serverName + "__" + t, "decision": "allow"})
	}
	// The api backend obeys these permissions: only the dispatch tools run.
	// The claude-code backend obeys the Claude Code CLI's own settings
	// instead, which the deployment sets the same way.
	conf := map[string]any{
		"backend":      cfg.Backend,
		"defaultModel": cfg.Model,
		"mcpServers":   map[string]any{serverName: map[string]any{"type": "http", "url": toolURL}},
		"permissions":  map[string]any{"mode": "deny", "rules": rules},
	}
	if cfg.BaseURL != "" {
		conf["providers"] = map[string]any{"anthropic": map[string]any{"baseURL": cfg.BaseURL}}
	}
	raw, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "engine.json"), raw, 0o600); err != nil {
		return nil, err
	}
	return in, nil
}

func (in *ion) allows(tool string) bool { return in.allowed[tool] }

// line is one line of `ion prompt --output stream-json`.
type line struct {
	Event struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Message  string `json:"message"`
		ExitCode int    `json:"exitCode"`
		Fields   struct {
			State string `json:"state"`
		} `json:"fields"`
	} `json:"event"`
}

// ErrEngine is the engine failing a run, with its message.
var ErrEngine = errors.New("the engine failed the run")

// run sends one prompt and answers the text Claude wrote after its last
// tool call. It reads the engine's events until the session goes idle
// after running, then ends the command.
func (in *ion) run(ctx context.Context, prompt string, maxTurns int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, in.bin, "prompt", "-", "--output", "stream-json",
		"--model", in.model, "--max-turns", fmt.Sprint(maxTurns), "--no-extensions", "--timeout", runTimeout.String())
	cmd.Dir = in.work
	cmd.Env = append(os.Environ(), "ION_DATA_DIR="+in.dataDir, "ION_SOCKET_PATH="+in.socket)
	cmd.Stdin = bytes.NewReader([]byte(prompt))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start ion: %w", err)
	}
	defer func() {
		cancel()
		cmd.Wait()
	}()
	var text bytes.Buffer
	running, done := false, false
	var failure string
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for !done && sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Event.Type {
		case "engine_text_delta":
			text.WriteString(l.Event.Text)
		case "engine_tool_start":
			// What Claude says before a tool call is narration; the answer
			// is what it writes after its last one.
			text.Reset()
		case "engine_error":
			failure = l.Event.Message
		case "engine_dead":
			if failure == "" {
				failure = fmt.Sprintf("the engine session ended with code %d", l.Event.ExitCode)
			}
			done = true
		case "engine_status":
			switch l.Event.Fields.State {
			case "running":
				running = true
			case "idle":
				done = running
			}
		}
	}
	if failure != "" {
		return "", fmt.Errorf("%w: %s", ErrEngine, failure)
	}
	if !done {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%w: no answer within %s", ErrEngine, runTimeout)
		}
		return "", fmt.Errorf("%w: ion stopped before the answer: %s", ErrEngine, lastLine(stderr.String()))
	}
	answer := trimmed(text.String())
	if answer == "" {
		return "", fmt.Errorf("%w: the answer was empty", ErrEngine)
	}
	return answer, nil
}

func lastLine(s string) string {
	s = trimmed(s)
	if i := bytes.LastIndexByte([]byte(s), '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
