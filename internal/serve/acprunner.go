package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// CodexACP is the codex-acp release the Codex runner starts through npx
// (stage 3 decision: a pinned version in npm's cache, nothing installed
// globally). A profile's command replaces it.
const CodexACP = "@agentclientprotocol/codex-acp@2.9.0"

// ACP setup deadlines: the first one covers npx fetching the bridge.
var (
	acpInitTimeout  = 3 * time.Minute
	acpSetupTimeout = 2 * time.Minute
)

// acpStart describes one ACP agent process (Codex through codex-acp, or
// `opencode acp`).
type acpStart struct {
	Harness string   // codex | opencode
	Argv    []string // the program and its arguments
	Dir     string   // the worktree
	// Resume is the agent's own session id to continue; "" starts one.
	Resume     string
	Model      string
	APIBilling bool
	Env        []string // the base environment
	Extra      []string // added after acpEnv
	// OnSession receives the agent's session id once it is known.
	OnSession func(id string)
	// OnModelRefused: the agent does not offer Model; its default runs.
	OnModelRefused func()
	// Secrets are the session's secret answers so far (masked in what this
	// runner records); OnSecret receives new ones.
	Secrets  []string
	OnSecret func(string)
	// OnPrompted: the first prompt is out, so the conversation exists for
	// the agent's own resume (Codex writes its rollout then).
	OnPrompted func()
}

// acpRunner runs one ACP agent process for one session: the service is the
// ACP client (F8), the agent's permission requests and form elicitations
// come to the host like Claude's tool permissions.
type acpRunner struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	conn    *acpConn
	host    claudeHost
	harness string
	done    chan struct{}
	closed  atomic.Bool

	mu       sync.Mutex
	session  string // the agent's session id
	ready    bool   // initialized and the session is open
	turn     bool   // a prompt is in flight
	ending   bool   // the conversation's last turn ended: no more input
	held     bool   // the turn was interrupted for an asynchronous question
	queue    []string
	text     strings.Builder // the agent's current message
	last     string          // its last message, the result's text
	loading  bool            // session/load replays history: not journalled
	cost     float64         // cumulative, from usage_update
	reported float64
	reqs     map[string]*acpPending
	stopping bool
	dir      string              // the worktree, for paths the agent gives relative
	paths    map[string][]string // per tool call, the paths its diffs name
	secrets  []string            // secret answers given: kept out of the feed
	prompted bool                // the first prompt went out
	onPrompt func()              // called once, when it did
	onSecret func(string)        // a secret answer given: the session keeps it
	turns    sync.WaitGroup      // prompt goroutines, joined before exited
}

type acpPending struct {
	id   json.RawMessage
	kind string // permission | elicitation
	// permission: the options by kind
	options map[string]string
	// elicitation: per question text, its field
	fields map[string]*acpField
	// async: a Codex question that does not stop the turn (A26): the turn
	// was interrupted, the answer goes in the next prompt
	async bool
	asked string
}

type acpField struct {
	key      string
	typ      string            // string | number | integer | boolean | array
	consts   map[string]string // option label shown → value
	secret   bool
	required bool
	min, max *float64 // a number's bounds
}

// acpStripped are inherited variables that would change what an agent
// loads or bills: the session's own environment decides them.
var acpStripped = map[string][]string{
	"codex": {"CODEX_CONFIG", "MODEL_PROVIDER", "DEFAULT_AUTH_REQUEST", "INITIAL_AGENT_MODE", "CODEX_PATH"},
	"opencode": {"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT", "OPENCODE_CLI_CONFIG_CONTENT", "OPENCODE_PERMISSION",
		"OPENCODE_CONFIG_PROJECT_DISABLE", "OPENCODE_DISABLE_PROJECT_CONFIG"},
}

func acpBaseEnv(harness string, env []string) []string {
	drop := map[string]bool{}
	for _, k := range acpStripped[harness] {
		drop[k] = true
	}
	var out []string
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			out = append(out, kv)
		}
	}
	return out
}

func startACP(s acpStart, host claudeHost) (*acpRunner, error) {
	if len(s.Argv) == 0 {
		return nil, fmt.Errorf("no command for the %s agent", s.Harness)
	}
	cmd := exec.Command(s.Argv[0], s.Argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env = append(append(agentEnv(acpBaseEnv(s.Harness, s.Env), s.APIBilling), acpEnv(s.Harness)...), s.Extra...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second // a helper holding stderr cannot hold Wait
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// stdout through a pipe of our own: after the process exits, a helper
	// that kept the write end open cannot keep the reader (and Stop) waiting
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = pw
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, n: 16 << 10}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("start %s: %w", s.Argv[0], err)
	}
	pw.Close()
	r := &acpRunner{cmd: cmd, stdin: stdin, host: host, harness: s.Harness, done: make(chan struct{}), reqs: map[string]*acpPending{},
		dir: s.Dir, paths: map[string][]string{}, onPrompt: s.OnPrompted,
		secrets: append([]string(nil), s.Secrets...), onSecret: s.OnSecret}
	r.conn = newACPConn(stdin, r.handle)
	host.bind(r)
	readDone := make(chan struct{})
	go func() {
		r.conn.read(pr)
		close(readDone)
	}()
	go func() {
		err := cmd.Wait()
		select {
		case <-readDone:
		case <-time.After(2 * time.Second):
			pr.Close() // what is left in the pipe is a helper's
			<-readDone
		}
		pr.Close()
		// a turn whose reply was read finishes its bookkeeping first, so a
		// completion is not overtaken by the exit
		joined := make(chan struct{})
		go func() { r.turns.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
		}
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		if err != nil && stderr.Len() > 0 {
			err = fmt.Errorf("%w: %s", err, lastLines(stderr.String(), 3))
		}
		r.closed.Store(true)
		host.exited(code, err)
		close(r.done)
	}()
	go r.setup(s)
	return r, nil
}

// acpEnv is what each agent gets besides the base environment.
func acpEnv(harness string) []string {
	switch harness {
	case "codex":
		return []string{
			// questions in Default mode (F15, A26): Codex asks through
			// item/tool/requestUserInput, which codex-acp turns into a form
			`CODEX_CONFIG={"features":{"default_mode_request_user_input":true}}`,
			// Codex's own sandbox: writes in the worktree, no network; more
			// is asked through session/request_permission
			"INITIAL_AGENT_MODE=workspace-write",
		}
	case "opencode":
		return []string{
			// A28: shell commands, edits outside the worktree and fetches
			// are asked; the repository's opencode config cannot loosen it
			`OPENCODE_CONFIG_CONTENT={"permission":{"bash":"ask","external_directory":"ask","webfetch":"ask","edit":"allow"}}`,
			"OPENCODE_DISABLE_PROJECT_CONFIG=1",
			"OPENCODE_CONFIG_PROJECT_DISABLE=1", // the newer name wins in 2.2.5
			"OPENCODE_DISABLE_AUTOUPDATE=1",
		}
	}
	return nil
}

type acpInit struct {
	ProtocolVersion   int `json:"protocolVersion"`
	AgentCapabilities struct {
		LoadSession         bool `json:"loadSession"`
		SessionCapabilities struct {
			Resume *json.RawMessage `json:"resume"`
		} `json:"sessionCapabilities"`
	} `json:"agentCapabilities"`
}

type acpConfigOption struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Options  []struct {
		Value string `json:"value"`
		Name  string `json:"name"`
	} `json:"options"`
}

var errACPTimeout = errors.New("the agent did not answer in time")

// requestWithin is request with a deadline; on timeout the process is
// ended (its late reply has nobody to read it).
func (r *acpRunner) requestWithin(d time.Duration, method string, params, out any) error {
	ch, err := r.conn.call(method, params)
	if err != nil {
		return err
	}
	select {
	case rep, ok := <-ch:
		if !ok {
			return errACPClosed
		}
		if rep.Err != nil {
			return fmt.Errorf("%s: %w", method, rep.Err)
		}
		if out != nil && len(rep.Result) > 0 {
			return json.Unmarshal(rep.Result, out)
		}
		return nil
	case <-time.After(d):
		return fmt.Errorf("%s: %w (%s)", method, errACPTimeout, d)
	case <-r.done:
		return errACPClosed
	}
}

// setup opens the connection and the session, then sends what was queued.
// Every step has a deadline: an agent that does not answer ends as an
// error, so its slot (keep N busy) and its claim do not wait forever.
func (r *acpRunner) setup(s acpStart) {
	fail := func(err error) {
		r.host.result(claudeResult{IsError: true, Text: err.Error()})
		r.kill()
	}
	var ini acpInit
	err := r.requestWithin(acpInitTimeout, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
			// questions come as forms only if the client says it shows them
			// (F17, F18)
			"elicitation": map[string]any{"form": map[string]any{}},
		},
		"clientInfo": map[string]any{"name": "taskmoor", "version": "serve"},
	}, &ini)
	if err != nil {
		fail(err)
		return
	}
	apiKey := s.Harness == "codex" && s.APIBilling && (envValue(s.Env, "CODEX_API_KEY") != "" || envValue(s.Env, "OPENAI_API_KEY") != "")
	if s.Harness == "codex" {
		// the ChatGPT login, whatever API keys the environment holds (A17),
		// unless the profile allows API billing and a key is there
		method := "chat-gpt"
		if apiKey {
			method = "api-key"
		}
		if err := r.requestWithin(acpSetupTimeout, "authenticate", map[string]any{"methodId": method}, nil); err != nil {
			fail(fmt.Errorf("codex-acp: the %s sign-in did not work (run `codex login`): %w", method, err))
			return
		}
	}
	var opened struct {
		SessionID     string            `json:"sessionId"`
		ConfigOptions []acpConfigOption `json:"configOptions"`
	}
	params := map[string]any{"cwd": s.Dir, "mcpServers": []any{}}
	switch {
	case s.Resume == "":
		err = r.requestWithin(acpSetupTimeout, "session/new", params, &opened)
	case ini.AgentCapabilities.SessionCapabilities.Resume != nil:
		params["sessionId"] = s.Resume
		err = r.requestWithin(acpSetupTimeout, "session/resume", params, &opened)
		opened.SessionID = s.Resume
	case ini.AgentCapabilities.LoadSession:
		params["sessionId"] = s.Resume
		r.mu.Lock()
		r.loading = true // the replayed history is already in the feed
		r.mu.Unlock()
		err = r.requestWithin(acpSetupTimeout, "session/load", params, &opened)
		r.mu.Lock()
		r.loading = false
		r.text.Reset()
		r.mu.Unlock()
		opened.SessionID = s.Resume
	default:
		err = fmt.Errorf("the %s agent cannot resume a session", s.Harness)
	}
	if err != nil {
		fail(err)
		return
	}
	if opened.SessionID == "" {
		fail(fmt.Errorf("the %s agent opened no session", s.Harness))
		return
	}
	model := s.Model
	if model != "" {
		if err := r.setModel(opened.SessionID, opened.ConfigOptions, model); err != nil {
			if errors.Is(err, errACPTimeout) || errors.Is(err, errACPClosed) {
				fail(err) // the model is not known: no turn on a guess
				return
			}
			r.host.activity(Activity{Type: ActError, Text: err.Error()})
			model = ""
			if s.OnModelRefused != nil {
				s.OnModelRefused()
			}
		}
	}
	if s.OnSession != nil {
		s.OnSession(opened.SessionID)
	}
	source := "none" // a subscription login
	if s.Harness == "opencode" || apiKey {
		source = s.Harness // A27: opencode bills an API key here
	}
	r.mu.Lock()
	r.session = opened.SessionID
	r.mu.Unlock()
	r.host.initialized(source, model, "")
	r.mu.Lock()
	r.ready = true
	r.mu.Unlock()
	r.next()
}

// setModel picks the model through the session's model option.
func (r *acpRunner) setModel(session string, opts []acpConfigOption, model string) error {
	for _, o := range opts {
		if o.Category != "model" && o.ID != "model" {
			continue
		}
		for _, v := range o.Options {
			if v.Value == model || strings.EqualFold(v.Name, model) {
				return r.requestWithin(acpSetupTimeout, "session/set_config_option", map[string]any{"sessionId": session, "configId": o.ID, "value": v.Value}, nil)
			}
		}
		return fmt.Errorf("model %q is not one the %s agent offers; its default runs", model, r.harness)
	}
	return fmt.Errorf("the %s agent offers no model choice; its default runs", r.harness)
}

// send queues an instruction; it goes out when the agent is ready and no
// turn is in flight (ACP has one prompt at a time). A conversation that
// is ending or stopping takes none: errACPClosed, the caller resumes.
func (r *acpRunner) send(text string) error {
	r.mu.Lock()
	if r.closed.Load() || r.ending || r.stopping {
		r.mu.Unlock()
		return errACPClosed
	}
	r.queue = append(r.queue, text) // behind an asynchronous answer, if one is awaited
	r.mu.Unlock()
	r.next()
	return nil
}

// answer puts an asynchronous question's answer first: it is the next
// prompt, before instructions queued while it was awaited (A26).
func (r *acpRunner) answer(text string) error {
	r.mu.Lock()
	if r.closed.Load() || r.ending || r.stopping {
		r.mu.Unlock()
		return errACPClosed
	}
	r.queue = append([]string{text}, r.queue...)
	r.held = r.asyncPending()
	r.mu.Unlock()
	r.next()
	return nil
}

// asyncPending: an asynchronous question waits for the person (r.mu held).
func (r *acpRunner) asyncPending() bool {
	for _, p := range r.reqs {
		if p.async {
			return true
		}
	}
	return false
}

// next starts the next queued prompt if it can.
func (r *acpRunner) next() {
	r.mu.Lock()
	if !r.ready || r.turn || r.held || len(r.queue) == 0 || r.stopping || r.ending {
		r.mu.Unlock()
		return
	}
	text := r.queue[0]
	r.queue = r.queue[1:]
	r.turn = true
	session := r.session
	r.turns.Add(1)
	r.mu.Unlock()
	go r.prompt(session, text)
}

func (r *acpRunner) prompt(session, text string) {
	defer r.turns.Done()
	var res struct {
		StopReason string `json:"stopReason"`
	}
	err := r.promptCall(session, text, &res)
	r.flush()
	r.mu.Lock()
	r.turn = false
	more := len(r.queue) > 0 && !r.stopping
	stopping := r.stopping
	// an asynchronous question still waits: the conversation stays open
	// for its answer, however the turn ended
	held := r.held && err == nil
	// the last turn: from here no instruction is taken (Say resumes)
	if !more && !held {
		r.ending = true
	}
	last := r.last
	cost := r.cost - r.reported
	r.reported = r.cost
	r.mu.Unlock()
	switch {
	case held:
		return // interrupted for an asynchronous question: its answer is the next prompt
	case err != nil:
		if !stopping && !r.closed.Load() {
			r.host.result(claudeResult{IsError: true, Text: err.Error()})
		}
	case more:
		r.next() // an instruction came during the turn
	case res.StopReason == "refusal" || res.StopReason == "max_tokens" || res.StopReason == "max_turn_requests":
		r.host.result(claudeResult{IsError: true, Text: "the turn ended: " + res.StopReason, CostUSD: cost, Reason: res.StopReason})
	default:
		r.host.result(claudeResult{Text: last, CostUSD: cost, Reason: res.StopReason})
	}
}

func (r *acpRunner) promptCall(session, text string, out any) error {
	ch, err := r.conn.call("session/prompt", map[string]any{"sessionId": session,
		"prompt": []any{map[string]any{"type": "text", "text": text}}})
	if err != nil {
		return err
	}
	r.mu.Lock()
	first := !r.prompted
	r.prompted = true
	r.mu.Unlock()
	if first && r.onPrompt != nil {
		r.onPrompt()
	}
	rep, ok := <-ch
	if !ok {
		return errACPClosed
	}
	if rep.Err != nil {
		return fmt.Errorf("session/prompt: %w", rep.Err)
	}
	if len(rep.Result) > 0 {
		return json.Unmarshal(rep.Result, out)
	}
	return nil
}

// redact keeps secret answers out of what the feed records, also when the
// agent repeats them.
func (r *acpRunner) redact(t string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sec := range r.secrets {
		if sec != "" {
			t = strings.ReplaceAll(t, sec, "•••")
		}
	}
	return t
}

// flush journals the agent's current message.
func (r *acpRunner) flush() {
	r.mu.Lock()
	t := strings.TrimSpace(r.text.String())
	r.text.Reset()
	r.mu.Unlock()
	t = r.redact(t)
	r.mu.Lock()
	if t != "" {
		r.last = t
	}
	r.mu.Unlock()
	if t != "" {
		r.host.activity(Activity{Type: ActResponse, Text: t})
	}
}

func (r *acpRunner) handle(method string, id, params json.RawMessage) {
	switch method {
	case "session/update":
		r.update(params)
	case "session/request_permission":
		r.permission(id, params)
	case "elicitation/create":
		r.elicitation(id, params)
	default:
		if id != nil {
			_ = r.conn.replyError(id, -32601, "taskmoor does not offer "+method)
		}
	}
}

type acpToolCall struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	Locations  []struct {
		Path string `json:"path"`
	} `json:"locations"`
}

func (r *acpRunner) update(params json.RawMessage) {
	var n struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			// a content block for message chunks, an array for tool calls
			Content json.RawMessage `json:"content"`
			Cost    *struct {
				Amount float64 `json:"amount"`
			} `json:"cost"`
			acpToolCall
		} `json:"update"`
	}
	if json.Unmarshal(params, &n) != nil {
		return
	}
	u := n.Update
	r.mu.Lock()
	loading := r.loading
	r.mu.Unlock()
	if loading {
		return
	}
	switch u.SessionUpdate {
	case "agent_message_chunk":
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(u.Content, &c) == nil && c.Type == "text" {
			r.mu.Lock()
			r.text.WriteString(c.Text)
			r.mu.Unlock()
		}
	case "tool_call", "tool_call_update":
		r.notePaths(u.ToolCallID, u.Content)
	}
	switch u.SessionUpdate {
	case "tool_call":
		r.flush()
		tool, input := r.acpTool(u.acpToolCall)
		text := u.Title
		if text == "" {
			text = summarizeMap(tool, input)
		}
		r.host.activity(Activity{Type: ActAction, Tool: tool, Text: firstLine(r.redact(text), 200)})
	case "tool_call_update":
		if u.Status == "failed" {
			r.host.activity(Activity{Type: ActError, Text: firstLine(r.redact("failed: "+u.Title), 300)})
		}
	case "usage_update":
		if u.Cost != nil {
			r.mu.Lock()
			r.cost = u.Cost.Amount
			r.mu.Unlock()
		}
	}
}

// notePaths keeps the paths a tool call's diffs name (a rename's
// destination is only there), for its permission request.
func (r *acpRunner) notePaths(id string, content json.RawMessage) {
	if id == "" || len(content) == 0 {
		return
	}
	var items []struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		OldPath string `json:"oldPath"`
		NewPath string `json:"newPath"`
	}
	if json.Unmarshal(content, &items) != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, it := range items {
		if it.Type != "diff" {
			continue
		}
		for _, p := range []string{it.Path, it.OldPath, it.NewPath} {
			if p != "" && !containsStr(r.paths[id], p) {
				r.paths[id] = append(r.paths[id], p)
			}
		}
	}
}

// acpAsk is the tool name prefix of an ACP request the rules cannot read
// (another kind, more sandbox permissions, a network destination): it is
// always the person's (host.permission).
const acpAsk = "ACP:"

// acpTool maps an ACP tool call to the tool names and inputs taskmoor's
// rules read (policy.go): execute → Bash (run in its working directory),
// edit → Edit (every path it changes), read → Read, and so on; what the
// rules cannot judge is named acpAsk+kind.
func (r *acpRunner) acpTool(tc acpToolCall) (string, map[string]any) {
	var raw map[string]any
	_ = json.Unmarshal(tc.RawInput, &raw)
	if raw == nil {
		raw = map[string]any{}
	}
	str := func(keys ...string) string {
		for _, k := range keys {
			switch v := raw[k].(type) {
			case string:
				if v != "" {
					return v
				}
			case []any: // codex: the command as argv
				var parts []string
				for _, p := range v {
					if s, ok := p.(string); ok {
						parts = append(parts, s)
					}
				}
				if len(parts) > 0 {
					return strings.Join(parts, " ")
				}
			}
		}
		return ""
	}
	var paths []string
	add := func(p string) {
		if p != "" && !containsStr(paths, p) {
			paths = append(paths, p)
		}
	}
	for _, l := range tc.Locations {
		add(l.Path)
	}
	add(str("path", "filePath", "file_path"))
	r.mu.Lock()
	for _, p := range r.paths[tc.ToolCallID] {
		add(p)
	}
	r.mu.Unlock()
	if changes, ok := raw["changes"].([]any); ok { // codex file changes, renames too
		for _, c := range changes {
			if m, ok := c.(map[string]any); ok {
				for _, k := range []string{"path", "move_path", "movePath"} {
					if p, ok := m[k].(string); ok {
						add(p)
					}
				}
			}
		}
	}
	for _, k := range []string{"permissions", "additionalPermissions", "additional_permissions", "sandboxPermissions", "sandbox_permissions"} {
		if v, ok := raw[k]; ok && v != nil {
			return acpAsk + "permissions", raw // more of the sandbox, alone or with a command
		}
	}
	first := ""
	if len(paths) > 0 {
		first = paths[0]
	}
	switch tc.Kind {
	case "execute":
		if u := str("url", "host"); u != "" {
			raw["description"] = tc.Title
			return acpAsk + "network", raw // a network destination: the rules do not judge it
		}
		cmd := str("command", "cmd")
		if cmd == "" {
			cmd = tc.Title
		}
		cmd = unwrapShell(cmd)
		// where it runs: the rules read commands as run in the worktree, so
		// one that runs anywhere else is the person's
		dirs := append([]string{str("cwd", "workdir")}, paths...)
		for _, d := range dirs {
			if d == "" {
				continue
			}
			if !filepath.IsAbs(d) {
				d = filepath.Join(r.dir, d)
			}
			if !within(filepath.Clean(d), r.dir) {
				raw["description"] = tc.Title
				raw["cwd"] = d
				return acpAsk + "elsewhere", raw
			}
		}
		return "Bash", map[string]any{"command": cmd, "description": tc.Title}
	case "move":
		if len(paths) < 2 {
			raw["title"] = tc.Title
			return acpAsk + "move", raw // the destination is not known
		}
		return "Edit", map[string]any{"file_path": first, "paths": paths}
	case "edit", "delete":
		return "Edit", map[string]any{"file_path": first, "paths": paths}
	case "read":
		return "Read", map[string]any{"file_path": first}
	case "search":
		return "Grep", map[string]any{"path": first, "pattern": str("pattern", "query")}
	case "fetch":
		raw["title"] = tc.Title
		return acpAsk + "fetch", raw // a destination the person sees (A28)
	}
	kind := tc.Kind
	if kind == "" {
		kind = "other"
	}
	raw["title"] = tc.Title
	return acpAsk + kind, raw
}

func containsStr(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// unwrapShell drops a `bash -lc '…'` wrapper so the rules read the command.
func unwrapShell(cmd string) string {
	for _, pre := range []string{"/bin/zsh -lc ", "/bin/bash -lc ", "bash -lc ", "zsh -lc ", "sh -c ", "/bin/sh -c "} {
		if rest, ok := strings.CutPrefix(cmd, pre); ok {
			rest = strings.TrimSpace(rest)
			if len(rest) >= 2 && (rest[0] == '\'' || rest[0] == '"') && rest[len(rest)-1] == rest[0] {
				rest = rest[1 : len(rest)-1]
			}
			return rest
		}
	}
	return cmd
}

func summarizeMap(tool string, in map[string]any) string {
	raw, _ := json.Marshal(in)
	return summarize(tool, raw)
}

func (r *acpRunner) permission(id, params json.RawMessage) {
	var p struct {
		ToolCall acpToolCall `json:"toolCall"`
		Options  []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if json.Unmarshal(params, &p) != nil {
		_ = r.conn.replyError(id, -32602, "unreadable permission request")
		return
	}
	pend := &acpPending{id: id, kind: "permission", options: map[string]string{}}
	for _, o := range p.Options {
		if _, ok := pend.options[o.Kind]; !ok {
			pend.options[o.Kind] = o.OptionID
		}
	}
	req := newID()
	r.mu.Lock()
	r.reqs[req] = pend
	r.mu.Unlock()
	r.flush()
	tool, input := r.acpTool(p.ToolCall)
	r.host.permission(req, tool, input)
}

func (r *acpRunner) elicitation(id, params json.RawMessage) {
	var p struct {
		Mode            string `json:"mode"`
		Message         string `json:"message"`
		RequestedSchema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"requestedSchema"`
		Meta struct {
			Codex struct {
				Delivery string `json:"delivery"`
			} `json:"codex"`
		} `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil || p.Mode != "form" {
		_ = r.conn.reply(id, map[string]any{"action": "decline"})
		return
	}
	pend := &acpPending{id: id, kind: "elicitation", fields: map[string]*acpField{}, async: p.Meta.Codex.Delivery == "async"}
	keys := make([]string, 0, len(p.RequestedSchema.Properties))
	for k := range p.RequestedSchema.Properties {
		keys = append(keys, k)
	}
	// the schema's required order first (it is the agent's question order),
	// then the rest by name
	order := map[string]int{}
	for i, k := range p.RequestedSchema.Required {
		order[k] = i + 1
	}
	sort.Slice(keys, func(i, j int) bool {
		oi, oj := order[keys[i]], order[keys[j]]
		if (oi == 0) != (oj == 0) {
			return oi != 0
		}
		if oi != oj {
			return oi < oj
		}
		return keys[i] < keys[j]
	})
	var questions []any
	for _, k := range keys {
		var f struct {
			Type        string   `json:"type"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Default     any      `json:"default"`
			Minimum     *float64 `json:"minimum"`
			Maximum     *float64 `json:"maximum"`
			Enum        []string
			OneOf       []struct {
				Const       string `json:"const"`
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"oneOf"`
			Items struct {
				AnyOf []struct {
					Const string `json:"const"`
					Title string `json:"title"`
				} `json:"anyOf"`
				Enum []string `json:"enum"`
			} `json:"items"`
			Meta struct {
				Codex struct {
					Role       string `json:"role"`
					IsSecret   bool   `json:"isSecret"`
					QuestionID string `json:"questionId"`
				} `json:"codex"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(p.RequestedSchema.Properties[k], &f)
		q := f.Title
		if q == "" {
			q = k
		}
		if f.Meta.Codex.Role == "user_note" {
			// codex-acp's own-answer field beside a choice: a question of its
			// own, so each choice question has its text
			var parent struct {
				Title string `json:"title"`
			}
			_ = json.Unmarshal(p.RequestedSchema.Properties[f.Meta.Codex.QuestionID], &parent)
			if parent.Title != "" {
				q = parent.Title + " — your own answer"
			}
		}
		q = r.redact(q) // the key and the text shown are the same
		if _, dup := pend.fields[q]; dup {
			q += " (" + k + ")" // two fields of one title stay two questions
		}
		def := fmt.Sprint(f.Default)
		if f.Default == nil {
			def = ""
		}
		type opt struct{ label, value, desc string }
		var opts []opt
		for _, o := range f.OneOf {
			opts = append(opts, opt{o.Title, o.Const, o.Description})
		}
		for _, e := range f.Enum {
			opts = append(opts, opt{e, e, ""})
		}
		switch f.Type {
		case "array":
			for _, o := range f.Items.AnyOf {
				opts = append(opts, opt{o.Title, o.Const, ""})
			}
			for _, e := range f.Items.Enum {
				opts = append(opts, opt{e, e, ""})
			}
		case "boolean":
			opts = append(opts, opt{"Yes", "true", ""}, opt{"No", "false", ""})
		}
		fd := &acpField{key: k, typ: f.Type, consts: map[string]string{}, secret: f.Meta.Codex.IsSecret,
			required: order[k] != 0, min: f.Minimum, max: f.Maximum}
		pend.fields[q] = fd
		var options []any
		for _, o := range opts {
			label := o.label
			if def != "" && o.value == def && !strings.Contains(label, "(Recommended)") {
				label += " (Recommended)" // opencode marks it with default (F18)
			}
			shown := r.redact(label)
			for n := 2; ; n++ { // masking can make two labels one: keep them apart
				if _, taken := fd.consts[shown]; !taken {
					break
				}
				shown = fmt.Sprintf("%s (%d)", r.redact(label), n)
			}
			fd.consts[shown] = o.value
			options = append(options, map[string]any{"label": shown, "description": r.redact(o.desc)})
		}
		header := f.Description
		if order[k] == 0 {
			header = strings.TrimSpace(header + " (optional)")
		}
		// a secret given earlier and repeated by the agent is not shown, nor
		// sent on (to the dispatcher)
		questions = append(questions, map[string]any{"question": r.redact(q), "header": r.redact(header), "options": options,
			"multiSelect": f.Type == "array", "freeText": len(options) == 0, "optional": order[k] == 0, "secret": fd.secret,
			"note": f.Meta.Codex.Role == "user_note"})
	}
	if len(questions) == 0 {
		_ = r.conn.reply(id, map[string]any{"action": "decline"})
		return
	}
	req := newID()
	r.mu.Lock()
	r.reqs[req] = pend
	session := r.session
	r.mu.Unlock()
	r.flush()
	if p.Message != "" && len(questions) == 1 {
		if m, ok := questions[0].(map[string]any); ok && m["header"] == "" {
			m["header"] = r.redact(p.Message)
		}
	}
	if pend.async {
		// A26: a question that does not stop the turn: the bridge gets no
		// answer now, the turn is interrupted, the person's answer is the
		// next prompt
		pend.asked = firstQuestion(questions)
		_ = r.conn.reply(id, map[string]any{"action": "cancel"})
		r.mu.Lock()
		r.held = true
		r.mu.Unlock()
		_ = r.conn.notify("session/cancel", map[string]any{"sessionId": session})
	}
	r.host.permission(req, "AskUserQuestion", map[string]any{"questions": questions})
}

func firstQuestion(qs []any) string {
	if len(qs) == 0 {
		return ""
	}
	m, _ := qs[0].(map[string]any)
	s, _ := m["question"].(string)
	return s
}

func (r *acpRunner) take(requestID string) *acpPending {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.reqs[requestID]
	delete(r.reqs, requestID)
	return p
}

// encode is a form field's answer as its schema's type.
func (f *acpField) encode(vals []string) (any, error) {
	if f.typ == "array" {
		return vals, nil
	}
	if len(vals) == 0 {
		return nil, nil
	}
	v := vals[0]
	switch f.typ {
	case "number", "integer":
		x, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("%q is not a number", v)
		}
		if f.typ == "integer" && x != math.Trunc(x) {
			return nil, fmt.Errorf("%q is not a whole number", v)
		}
		if f.min != nil && x < *f.min || f.max != nil && x > *f.max {
			return nil, fmt.Errorf("%v is outside the field's range", v)
		}
		if f.typ == "integer" {
			return int64(x), nil
		}
		return x, nil
	case "boolean":
		return v == "true", nil
	}
	return v, nil
}

// formContent is the elicitation's answer content from the answered input
// (answerInput): option labels back to their constants, typed values.
func (p *acpPending) formContent(input map[string]any) (map[string]any, string, error) {
	content := map[string]any{}
	lists, _ := input["answerLists"].(map[string][]string)
	texts, _ := input["answers"].(map[string]any)
	var said []string
	for q, f := range p.fields {
		var vals []string
		if labels, ok := lists[q]; ok {
			for _, l := range labels {
				if v, ok := f.consts[l]; ok {
					vals = append(vals, v)
				} else {
					vals = append(vals, l)
				}
			}
		} else if t, ok := texts[q].(string); ok && t != "" {
			vals = []string{t}
		}
		if len(vals) == 0 {
			if f.required {
				return nil, "", fmt.Errorf("%q needs an answer", q)
			}
			continue
		}
		v, err := f.encode(vals)
		if err != nil {
			return nil, "", err
		}
		content[f.key] = v
		if f.secret {
			said = append(said, q+": (secret)")
		} else {
			said = append(said, q+": "+strings.Join(vals, ", "))
		}
	}
	sort.Strings(said)
	return content, strings.Join(said, "; "), nil
}

// check validates an answer before the ask is taken (answerChecker): a
// value the form's field cannot hold is refused while it can be retried.
func (r *acpRunner) check(requestID string, input map[string]any) error {
	r.mu.Lock()
	p := r.reqs[requestID]
	r.mu.Unlock()
	if p == nil {
		return errors.New("the agent no longer waits for this answer") // the ask stays, and is withdrawn
	}
	if p.kind != "elicitation" {
		return nil
	}
	_, _, err := p.formContent(input)
	return err
}

func (r *acpRunner) allow(requestID string, input map[string]any) error {
	p := r.take(requestID)
	if p == nil {
		return fmt.Errorf("no pending request %s", requestID)
	}
	if p.kind == "permission" {
		opt := p.options["allow_once"]
		if opt == "" {
			opt = p.options["allow_always"]
		}
		if opt == "" {
			return r.conn.reply(p.id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		}
		return r.conn.reply(p.id, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": opt}})
	}
	content, said, err := p.formContent(input)
	if err != nil {
		return err
	}
	r.mu.Lock()
	for q, f := range p.fields {
		if f.secret {
			if v, ok := content[f.key].(string); ok {
				r.secrets = append(r.secrets, v)
				if r.onSecret != nil {
					r.onSecret(v) // kept by the session for the runners after this one
				}
			}
			_ = q
		}
	}
	r.mu.Unlock()
	if p.async {
		// the secret goes to the agent; the prompt the feed records is redacted
		who := "The person"
		if by, _ := input["by"].(string); by == "dispatcher" {
			who = "taskmoor's dispatcher"
		}
		return r.answer(who + " answered your question (" + p.asked + "): " + said + secretValues(p, content))
	}
	return r.conn.reply(p.id, map[string]any{"action": "accept", "content": content})
}

// secretValues appends secret answers for the agent only (the feed shows
// "(secret)").
func secretValues(p *acpPending, content map[string]any) string {
	var b strings.Builder
	for q, f := range p.fields {
		if f.secret {
			if v, ok := content[f.key]; ok {
				fmt.Fprintf(&b, "\n%s: %v", q, v)
			}
		}
	}
	return b.String()
}

func (r *acpRunner) deny(requestID, message string) error {
	p := r.take(requestID)
	if p == nil {
		return fmt.Errorf("no pending request %s", requestID)
	}
	if p.kind == "elicitation" {
		if p.async {
			return r.answer("The person did not answer your question (" + p.asked + "); continue without it. " + message)
		}
		return r.conn.reply(p.id, map[string]any{"action": "decline"})
	}
	opt := p.options["reject_once"]
	if opt == "" {
		opt = p.options["reject_always"]
	}
	if opt == "" {
		return r.conn.reply(p.id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
	}
	return r.conn.reply(p.id, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": opt}})
}

func (r *acpRunner) inputClosed() bool {
	if r.closed.Load() {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ending || r.stopping
}

func (r *acpRunner) finished() <-chan struct{} { return r.done }

// closeInput ends the conversation: stdin closes and an agent that does
// not exit on its own is ended after a grace period.
func (r *acpRunner) closeInput() {
	r.mu.Lock()
	r.ending = true
	r.mu.Unlock()
	if !r.closed.CompareAndSwap(false, true) {
		return
	}
	_ = r.stdin.Close()
	go func() {
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			r.signal(syscall.SIGTERM)
			select {
			case <-r.done:
			case <-time.After(5 * time.Second):
				r.signal(syscall.SIGKILL)
			}
		}
	}()
}

func (r *acpRunner) signal(sig syscall.Signal) {
	if p := r.cmd.Process; p != nil {
		_ = syscall.Kill(-p.Pid, sig)
	}
}

func (r *acpRunner) kill() {
	r.closed.Store(true)
	_ = r.stdin.Close()
	r.signal(syscall.SIGKILL)
}

// stop cancels the turn, ends the conversation and, if the process
// lingers, its process group. It returns when the runner is done: the
// output pipe is ours (startACP), so a helper that keeps it open cannot
// hold this.
func (r *acpRunner) stop(grace time.Duration) {
	r.mu.Lock()
	r.stopping = true
	session := r.session
	pend := r.reqs
	r.reqs = map[string]*acpPending{}
	r.mu.Unlock()
	// requests the agent waits on are cancelled first (ACP: the client
	// answers them "cancelled" after session/cancel)
	if session != "" {
		go func() { _ = r.conn.notify("session/cancel", map[string]any{"sessionId": session}) }()
	}
	for _, p := range pend {
		if p.kind == "permission" {
			go func() { _ = r.conn.reply(p.id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}) }()
		} else if !p.async {
			go func() { _ = r.conn.reply(p.id, map[string]any{"action": "cancel"}) }()
		}
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case <-r.done:
		return
	case <-time.After(grace / 2):
	}
	if r.closed.CompareAndSwap(false, true) {
		_ = r.stdin.Close()
	}
	select {
	case <-r.done:
		return
	case <-time.After(grace / 2):
	}
	r.signal(syscall.SIGTERM)
	select {
	case <-r.done:
	case <-time.After(grace):
		r.signal(syscall.SIGKILL)
		<-r.done
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}
