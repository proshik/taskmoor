package serve

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// claudeHost receives what a Claude session does. Calls come from the
// runner's reading goroutine, one at a time.
type claudeHost interface {
	// bind gives the host its runner before any other call.
	bind(r agentRunner)
	initialized(apiKeySource, model, permissionMode string)
	activity(a Activity)
	// permission is a can_use_tool request; the host answers it with
	// runner.allow / runner.deny, now or later.
	permission(requestID, tool string, input map[string]any)
	// cancelled withdraws a pending permission request.
	cancelled(requestID string)
	// result ends a turn.
	result(r claudeResult)
	// exited is the last call: the process ended.
	exited(code int, err error)
}

type claudeResult struct {
	IsError bool
	Subtype string
	Text    string
	CostUSD float64
	Reason  string // stop_reason / terminal_reason
}

// claudeRunner runs one `claude -p` stream-json process (C2, F2) in which
// the service is the permission host (`--permission-prompt-tool stdio`):
// every tool call that needs permission, and every AskUserQuestion, comes
// to the host as a can_use_tool control request and waits for its answer.
type claudeRunner struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	host  claudeHost

	mu     sync.Mutex // serializes writes to stdin
	closed atomic.Bool
	nextID atomic.Int64
	done   chan struct{}
}

type claudeStart struct {
	Program   string // "claude" or a configured command
	Dir       string // the worktree
	SessionID string
	Resume    bool // --resume SessionID instead of --session-id
	Model     string
	// APIBilling keeps API-key variables in the environment.
	APIBilling bool
	// Sandbox confines the session's commands; nil runs them unconfined.
	Sandbox *sandboxPlan
	// Protected are taskmoor's own files, denied to every file tool.
	Protected []string
	// SettingsFile receives the settings JSON (it grows with the
	// repository's branches, past what one argument may hold); empty: the
	// JSON goes on the command line.
	SettingsFile string
	Env          []string // the base environment
}

// claudeArgs is the command line of a session.
func claudeArgs(s claudeStart) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-prompt-tool", "stdio",
		// the person's settings may set another mode (auto, bypass); in
		// default mode what needs permission comes to the host
		"--permission-mode", "default",
		// A11 as deny rules, which win over any allow rule in the person's
		// settings; the host's rules catch the forms these prefixes miss
		"--settings", settingsArg(s)}
	if s.Sandbox != nil {
		// only taskmoor's settings: the person's and the repository's could
		// add hooks, MCP servers, excluded commands or allowed hosts, which
		// run or reach outside the sandbox (the worktree's .claude/ is the
		// agent's to write)
		args = append(args, "--setting-sources=", "--strict-mcp-config")
	}
	args = append(args, "--disallowedTools")
	args = append(args, deniedTools...)
	if s.Resume {
		args = append(args, "--resume", s.SessionID)
	} else {
		args = append(args, "--session-id", s.SessionID)
	}
	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	return args
}

func settingsArg(s claudeStart) string {
	js := sessionSettings(s.Sandbox, s.Protected)
	if s.SettingsFile == "" {
		return js
	}
	if err := os.WriteFile(s.SettingsFile, []byte(js), 0o600); err != nil {
		return js // too long for an argument, it fails at start, visibly
	}
	return s.SettingsFile
}

// deniedTools are Claude permission rules an agent session never gets past.
var deniedTools = []string{
	"Bash(git push:*)", "Bash(npm publish:*)", "Bash(pnpm publish:*)", "Bash(yarn publish:*)",
	"Bash(cargo publish:*)", "Bash(gem push:*)", "Bash(twine upload:*)", "Bash(poetry publish:*)",
	"Bash(uv publish:*)", "Bash(dotnet nuget push:*)", "Bash(mvn deploy:*)", "Bash(gradle publish:*)",
	"Bash(gh release:*)", "Bash(gh pr create:*)", "Bash(gh pr merge:*)", "Bash(gh repo create:*)", "Bash(gh repo delete:*)",
	"Bash(glab release:*)", "Bash(glab mr create:*)", "Bash(glab mr merge:*)",
	"Bash(docker push:*)", "Bash(podman push:*)",
	"Bash(kubectl apply:*)", "Bash(kubectl delete:*)", "Bash(kubectl rollout:*)", "Bash(kubectl set:*)",
	"Bash(kubectl patch:*)", "Bash(kubectl replace:*)", "Bash(kubectl edit:*)", "Bash(kubectl scale:*)", "Bash(kubectl create:*)",
	"Bash(helm install:*)", "Bash(helm upgrade:*)", "Bash(helm delete:*)", "Bash(helm uninstall:*)",
	"Bash(terraform apply:*)", "Bash(terraform destroy:*)", "Bash(tofu apply:*)", "Bash(tofu destroy:*)",
	"Bash(fly deploy:*)", "Bash(flyctl deploy:*)", "Bash(vercel deploy:*)", "Bash(netlify deploy:*)", "Bash(heroku releases:*)",
}

// apiKeyVars are stripped unless the profile allows API billing (C2).
// The provider switches are stripped too: Bedrock, Vertex or Foundry bill
// the cloud account, not the subscription.
var apiKeyVars = map[string]bool{"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
	"CLAUDE_CODE_USE_BEDROCK": true, "CLAUDE_CODE_USE_VERTEX": true, "CLAUDE_CODE_USE_FOUNDRY": true,
	"AWS_BEARER_TOKEN_BEDROCK": true}

// releaseVars are publishing and deployment credentials an agent never
// needs (A11), and the service's own location.
var releaseVars = map[string]bool{
	"NPM_TOKEN": true, "NODE_AUTH_TOKEN": true, "CARGO_REGISTRY_TOKEN": true, "TWINE_PASSWORD": true,
	"TWINE_USERNAME": true, "PYPI_TOKEN": true, "UV_PUBLISH_TOKEN": true, "POETRY_PYPI_TOKEN_PYPI": true,
	"GEM_HOST_API_KEY": true, "NUGET_API_KEY": true, "DOCKER_PASSWORD": true, "DOCKER_AUTH_CONFIG": true,
	"GH_TOKEN": true, "GITHUB_TOKEN": true, "GITLAB_TOKEN": true, "GL_TOKEN": true, "CI_JOB_TOKEN": true,
	"VERCEL_TOKEN": true, "NETLIFY_AUTH_TOKEN": true, "FLY_API_TOKEN": true, "HEROKU_API_KEY": true,
	"TASKMOOR_HOME": true,
}

// nestingVars belong to the Claude Code session the service may have been
// started from; settings-like CLAUDE_CODE_* variables are kept.
var nestingVars = map[string]bool{
	"CLAUDECODE": true, "CLAUDE_PID": true, "CLAUDE_CODE_ENTRYPOINT": true, "CLAUDE_CODE_SSE_PORT": true,
	"CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_CHILD_SESSION": true, "CLAUDE_CODE_SESSION_ATTENDED": true,
	"CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true, "CLAUDE_CODE_CHILD_NOTES_DIR": true,
	"CLAUDE_CODE_EXECPATH": true,
}

func agentEnv(base []string, apiBilling bool) []string {
	var env []string
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !apiBilling && apiKeyVars[name] || releaseVars[name] {
			continue
		}
		// a service started from inside a Claude Code session must not hand
		// that session's identity, IDE port or messaging channel to agents
		if nestingVars[name] {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// apiKeyInSettings returns the first Claude settings file Claude would
// load for dir that makes it bill an API key (A30) — an apiKeyHelper, or an
// API key in its env block — and which of them; stripping the service's
// variables disables neither.
func apiKeyInSettings(home, configDir, dir string) (file, what string) {
	user := filepath.Join(home, ".claude")
	if configDir != "" {
		user = configDir // CLAUDE_CONFIG_DIR, as the agent will see it
	}
	files := []string{
		filepath.Join(user, "settings.json"),
		filepath.Join(dir, ".claude", "settings.json"),
		filepath.Join(dir, ".claude", "settings.local.json"),
		"/Library/Application Support/ClaudeCode/managed-settings.json",
		"/etc/claude-code/managed-settings.json",
	}
	for _, f := range files {
		data, err := readSettings(f)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return f, "an unreadable settings file (" + err.Error() + ")"
			}
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if v, ok := m["apiKeyHelper"]; ok && v != nil && v != "" {
			return f, "apiKeyHelper"
		}
		if env, ok := m["env"].(map[string]any); ok {
			for k := range apiKeyVars {
				if v, ok := env[k]; ok && v != nil && v != "" {
					return f, "env." + k
				}
			}
		}
	}
	return "", ""
}

// readSettings reads a regular settings file of at most 1 MiB, without
// blocking on a FIFO.
func readSettings(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("%s is larger than 1 MiB", p)
	}
	return io.ReadAll(io.LimitReader(f, 1<<20+1))
}

func startClaude(s claudeStart, host claudeHost) (*claudeRunner, error) {
	prog := s.Program
	if prog == "" {
		prog = "claude"
	}
	cmd := exec.Command(prog, claudeArgs(s)...)
	cmd.Dir = s.Dir
	cmd.Env = agentEnv(s.Env, s.APIBilling)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // stop takes its children too
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 16 << 10}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", prog, err)
	}
	r := &claudeRunner{cmd: cmd, stdin: stdin, host: host, done: make(chan struct{})}
	host.bind(r)
	go func() {
		r.read(stdout)
		err := cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		if err != nil && stderr.Len() > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		r.closed.Store(true)
		host.exited(code, err)
		close(r.done)
	}()
	return r, nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, err := l.w.Write(q)
	return len(p), err
}

func (r *claudeRunner) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if r.closed.Load() {
		return errors.New("the session's input is closed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.stdin.Write(append(data, '\n'))
	return err
}

// send adds a user message: the assignment, or an instruction.
func (r *claudeRunner) send(text string) error {
	return r.write(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": text},
		"parent_tool_use_id": nil,
	})
}

func (r *claudeRunner) respond(requestID string, response map[string]any) error {
	return r.write(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   response,
		},
	})
}

func (r *claudeRunner) allow(requestID string, input map[string]any) error {
	return r.respond(requestID, map[string]any{"behavior": "allow", "updatedInput": input})
}

func (r *claudeRunner) deny(requestID, message string) error {
	return r.respond(requestID, map[string]any{"behavior": "deny", "message": message})
}

// interrupt asks Claude to end the current turn.
func (r *claudeRunner) interrupt() error {
	id := fmt.Sprintf("taskmoor-%d", r.nextID.Add(1))
	return r.write(map[string]any{"type": "control_request", "request_id": id,
		"request": map[string]any{"subtype": "interrupt"}})
}

// inputClosed reports whether the conversation's input is closed (the
// turn finished, a stop, or the process ended).
func (r *claudeRunner) inputClosed() bool { return r.closed.Load() }

// closeInput ends the conversation: Claude exits after the current turn.
//
// It does not take the write lock: closing the pipe also unblocks a write
// stuck on a full pipe.
func (r *claudeRunner) closeInput() {
	if r.closed.CompareAndSwap(false, true) {
		_ = r.stdin.Close()
	}
}

// kill ends the process group at once.
func (r *claudeRunner) kill() {
	r.closeInput()
	if p := r.cmd.Process; p != nil {
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	}
}

// stop interrupts, closes the input and, if the process lingers, ends its
// process group.
func (r *claudeRunner) stop(grace time.Duration) {
	// the interrupt may block on a full pipe: never wait for it
	go func() { _ = r.interrupt() }()
	time.Sleep(50 * time.Millisecond)
	r.closeInput()
	select {
	case <-r.done:
		return
	case <-time.After(grace):
	}
	if p := r.cmd.Process; p != nil {
		_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
	}
	select {
	case <-r.done:
	case <-time.After(grace):
		if p := r.cmd.Process; p != nil {
			_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
		}
		<-r.done
	}
}

// read decodes the stream until it ends.
func (r *claudeRunner) read(out io.Reader) {
	br := bufio.NewReaderSize(out, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			r.handle(line)
		}
		if err != nil {
			return
		}
	}
}

type streamMsg struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	APIKeySource   string          `json:"apiKeySource"`
	PermissionMode string          `json:"permissionMode"`
	Model          string          `json:"model"`
	Message        json.RawMessage `json:"message"`
	RequestID      string          `json:"request_id"`
	Request        json.RawMessage `json:"request"`
	// result
	IsError        bool    `json:"is_error"`
	Result         string  `json:"result"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	StopReason     string  `json:"stop_reason"`
	TerminalReason string  `json:"terminal_reason"`
}

type contentBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

func (r *claudeRunner) handle(line []byte) {
	var m streamMsg
	if err := json.Unmarshal(line, &m); err != nil {
		return // not a protocol line
	}
	switch m.Type {
	case "system":
		if m.Subtype == "init" {
			r.host.initialized(m.APIKeySource, m.Model, m.PermissionMode)
		}
	case "assistant":
		var msg struct {
			Content []contentBlock `json:"content"`
		}
		if json.Unmarshal(m.Message, &msg) != nil {
			return
		}
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					r.host.activity(Activity{Type: ActResponse, Text: t})
				}
			case "thinking":
				// not shown: the feed is what the agent does and says
			case "tool_use":
				if b.Name == "AskUserQuestion" {
					continue // shown as the elicitation when it is asked
				}
				r.host.activity(Activity{Type: ActAction, Tool: b.Name, Text: summarize(b.Name, b.Input)})
			}
		}
	case "user":
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m.Message, &msg) != nil {
			return
		}
		var blocks []contentBlock
		if json.Unmarshal(msg.Content, &blocks) != nil {
			return
		}
		for _, b := range blocks {
			if b.Type == "tool_result" && b.IsError {
				r.host.activity(Activity{Type: ActError, Text: firstLine(blockText(b.Content), 300)})
			}
		}
	case "control_request":
		var req struct {
			Subtype  string         `json:"subtype"`
			ToolName string         `json:"tool_name"`
			Input    map[string]any `json:"input"`
		}
		if json.Unmarshal(m.Request, &req) != nil {
			return
		}
		if req.Subtype == "can_use_tool" {
			r.host.permission(m.RequestID, req.ToolName, req.Input)
			return
		}
		// other requests (hook callbacks, MCP) are not used: say so
		_ = r.write(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": m.RequestID, "error": "taskmoor serve does not handle " + req.Subtype}})
	case "control_cancel_request":
		r.host.cancelled(m.RequestID)
	case "result":
		reason := m.TerminalReason
		if reason == "" {
			reason = m.StopReason
		}
		r.host.result(claudeResult{IsError: m.IsError, Subtype: m.Subtype, Text: m.Result, CostUSD: m.TotalCostUSD, Reason: reason})
	}
}

// summarize is a one-line description of a tool call for the feed.
func summarize(tool string, raw json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(raw, &in)
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := in[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch tool {
	case "Bash":
		return firstLine(pick("description", "command"), 200)
	case "Read", "Write", "Edit", "MultiEdit", "NotebookEdit":
		return pick("file_path", "notebook_path")
	case "Glob", "Grep":
		return pick("pattern")
	case "WebFetch":
		return pick("url")
	case "WebSearch":
		return pick("query")
	case "Task", "Agent":
		return firstLine(pick("description", "prompt"), 200)
	case "TodoWrite":
		return "plan updated"
	}
	return ""
}

func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len([]rune(s)) > n {
		s = string([]rune(s)[:n]) + "…"
	}
	return s
}
