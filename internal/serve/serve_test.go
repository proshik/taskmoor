package serve_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

var (
	fakeOnce sync.Once
	fakePath string
	fakeErr  error
)

// fake builds testdata/fakeclaude once.
func fake(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "taskmoor-fakeclaude-")
		if err != nil {
			fakeErr = err
			return
		}
		fakePath = filepath.Join(dir, "claude")
		out, err := exec.Command("go", "build", "-o", fakePath, "./testdata/fakeclaude").CombinedOutput()
		if err != nil {
			fakeErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakePath
}

type rig struct {
	t      *testing.T
	repo   *testrepo.Repo
	home   string
	paths  serve.Paths
	svc    *serve.Service
	client *serve.Client
	log    string
	cancel context.CancelFunc
	done   chan error
	// configure adjusts a service before it serves (test doubles)
	configure func(*serve.Service)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// start runs a service over repo with the fake claude playing scenario.
func start(t *testing.T, repo *testrepo.Repo, scenario string, extraEnv ...string) *rig {
	t.Helper()
	home := t.TempDir()
	rg := &rig{t: t, repo: repo, home: home, paths: serve.Paths{Config: filepath.Join(home, "config.toml"), Store: filepath.Join(home, "store")},
		log: filepath.Join(home, "fake.log")}
	cfg := &serve.Config{Port: freePort(t), DefaultProfile: "fake",
		Profiles: []serve.Profile{{Name: "fake", Harness: "claude", Command: fake(t)}, {Name: "codexy", Harness: "codex"}},
		Projects: []serve.Project{{Name: "repo", Path: repo.Dir}}}
	if err := cfg.Save(rg.paths.Config); err != nil {
		t.Fatal(err)
	}
	rg.open(append([]string{"FAKE_SCENARIO=" + scenario, "FAKE_LOG=" + rg.log, "ANTHROPIC_API_KEY=sk-should-be-stripped"}, extraEnv...))
	return rg
}

func (rg *rig) open(env []string) {
	t := rg.t
	svc, err := serve.Open(rg.paths, "test", append(os.Environ(), env...), rg.home)
	if err != nil {
		t.Fatal(err)
	}
	if rg.configure != nil {
		rg.configure(svc)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rg.svc, rg.cancel, rg.done = svc, cancel, make(chan error, 1)
	go func() { rg.done <- serve.Serve(ctx, svc, func(string, ...any) {}) }()
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	rg.client, err = serve.WaitReady(wctx, rg.paths.Store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rg.stop)
}

func (rg *rig) stop() {
	if rg.cancel == nil {
		return
	}
	rg.cancel()
	select {
	case <-rg.done:
	case <-time.After(15 * time.Second):
		rg.t.Error("the service did not stop")
	}
	rg.cancel = nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (rg *rig) session(id string) serve.Session {
	rg.t.Helper()
	s, _, err := rg.client.Feed(context.Background(), id)
	if err != nil {
		rg.t.Fatal(err)
	}
	return *s
}

func (rg *rig) waitState(id, want string) serve.Session {
	rg.t.Helper()
	var s serve.Session
	eventually(rg.t, "session "+want, func() bool { s = rg.session(id); return s.State == want })
	return s
}

func (rg *rig) asks() []serve.Ask {
	snap, err := rg.client.Snapshot(context.Background())
	if err != nil {
		rg.t.Fatal(err)
	}
	return snap.Asks
}

func (rg *rig) waitAsk() serve.Ask {
	rg.t.Helper()
	var a []serve.Ask
	eventually(rg.t, "an ask", func() bool { a = rg.asks(); return len(a) > 0 })
	return a[0]
}

func (rg *rig) feedText(id string) string {
	rg.t.Helper()
	_, acts, err := rg.client.Feed(context.Background(), id)
	if err != nil {
		rg.t.Fatal(err)
	}
	var b strings.Builder
	for _, a := range acts {
		fmt.Fprintf(&b, "%s|%s|%s|%s\n", a.Type, a.Tool, a.By, a.Text)
	}
	return b.String()
}

func (rg *rig) stateFile(id string) map[string]any {
	rg.t.Helper()
	data, err := os.ReadFile(filepath.Join(rg.repo.StateDir(), id+".json"))
	if err != nil {
		rg.t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}

// fakeLog returns what the fake recorded: its argument lists and input lines.
func (rg *rig) fakeLog() (args [][]string, inputs []map[string]any, env []map[string]any) {
	rg.t.Helper()
	f, err := os.Open(rg.log)
	if err != nil {
		rg.t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e struct {
			Kind string          `json:"kind"`
			V    json.RawMessage `json:"v"`
		}
		_ = json.Unmarshal(sc.Bytes(), &e)
		switch e.Kind {
		case "args":
			var a []string
			_ = json.Unmarshal(e.V, &a)
			args = append(args, a)
		case "in":
			var m map[string]any
			_ = json.Unmarshal(e.V, &m)
			inputs = append(inputs, m)
		case "env":
			var m map[string]any
			_ = json.Unmarshal(e.V, &m)
			env = append(env, m)
		}
	}
	return
}

// events lists the fake's process starts and exits in order.
func (rg *rig) events() []string {
	f, err := os.Open(rg.log)
	if err != nil {
		rg.t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e struct{ Kind string }
		_ = json.Unmarshal(sc.Bytes(), &e)
		if e.Kind == "start" || e.Kind == "exit" {
			out = append(out, e.Kind)
		}
	}
	return out
}

func (rg *rig) lifetimes() (starts, exits int) {
	for _, e := range rg.events() {
		if e == "start" {
			starts++
		} else {
			exits++
		}
	}
	return
}

func newRepo(t *testing.T, tasks ...testrepo.Task) *testrepo.Repo {
	t.Helper()
	r := testrepo.New(t)
	if len(tasks) == 0 {
		tasks = []testrepo.Task{{ID: "TM-001", Title: "Fix the thing"}}
	}
	r.AddTasks(tasks...)
	return r
}

func TestRunAQuestionAndCompletion(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "work")
	ctx := context.Background()
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(filepath.Dir(repo.Dir), "repo-tm-001")
	if s.Worktree != wt || s.Branch != "task/TM-001-task" || s.Profile != "fake" {
		t.Fatalf("session %+v", s)
	}
	if got := repo.GitIn(wt, "symbolic-ref", "HEAD"); got != "refs/heads/task/TM-001-task" {
		t.Fatalf("worktree HEAD %s", got)
	}

	// the question reaches the inbox with the recommended option marked
	a := rg.waitAsk()
	if a.Kind != "question" || a.Task != "TM-001" || len(a.Questions) != 1 {
		t.Fatalf("ask %+v", a)
	}
	q := a.Questions[0]
	if q.Recommended != 0 || q.Options[0].Label != "Small fix" || q.Options[1].Label != "Rewrite" {
		t.Fatalf("question %+v", q)
	}
	if st := rg.session(s.ID); st.State != serve.StateAwaitingInput {
		t.Fatalf("state %s while asking", st.State)
	}
	eventually(t, "harness blocked/question", func() bool {
		h, _ := rg.stateFile("TM-001")["harness"].(map[string]any)
		return h["status"] == "blocked" && h["kind"] == "question" && h["session"] == s.ID
	})
	if err := rg.client.Answer(ctx, a.ID, serve.Answer{Choices: [][]string{{"1"}}}); err != nil {
		t.Fatal(err)
	}
	done := rg.waitState(s.ID, serve.StateComplete)
	if done.Billing != "subscription" {
		t.Errorf("billing %q", done.Billing)
	}

	feed := rg.feedText(s.ID)
	for _, want := range []string{
		"prompt||person|You are working on task TM-001",
		"response|||Looking at the task.",
		"elicitation|AskUserQuestion||Which approach?",
		"response||person|Small fix (Recommended)",
		"response|||Going with Small fix (Recommended).",
		"response|Bash|rules|refused by the rules: push, publish and deploy stay the person's",
		"response|||push refused: push, publish and deploy stay the person's",
		"result|||Done: small fix committed.",
	} {
		if !strings.Contains(feed, want) {
			t.Errorf("feed lacks %q:\n%s", want, feed)
		}
	}

	// the task is handed over: lifecycle review, harness done
	eventually(t, "lifecycle review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	m := rg.stateFile("TM-001")
	h := m["harness"].(map[string]any)
	if h["status"] != "done" || h["name"] != "claude" || !strings.HasPrefix(m["writer"].(string), "taskmoor serve ") {
		t.Fatalf("state %v", m)
	}

	args, inputs, env := rg.fakeLog()
	joined := strings.Join(args[0], " ")
	for _, want := range []string{"-p", "--input-format stream-json", "--output-format stream-json", "--permission-prompt-tool stdio",
		"--permission-mode default", `"permissions":{"ask":["Bash","Read","Write","Edit"`, `"sandbox":{"allowUnsandboxedCommands":false`, "--setting-sources= --strict-mcp-config", "--disallowedTools Bash(git push:*)",
		"Bash(kubectl apply:*)", "Bash(terraform apply:*)", "--session-id " + s.ID} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q lack %q", joined, want)
		}
	}
	if env[0]["ANTHROPIC_API_KEY"] != "" || env[0]["pwd"] != wt {
		t.Errorf("env %v", env[0])
	}
	// the Bash call was allowed with its input
	allowed := false
	for _, in := range inputs {
		if resp, ok := in["response"].(map[string]any); ok && resp["request_id"] == "r1" {
			p := resp["response"].(map[string]any)
			allowed = p["behavior"] == "allow" && p["updatedInput"].(map[string]any)["command"] == "go test ./..."
		}
	}
	if !allowed {
		t.Error("go test was not allowed")
	}
}

func TestSayResumesAnEndedSession(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval")
	ctx := context.Background()
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	a := rg.waitAsk()
	if a.Kind != "approval" || a.Tool != "Write" || a.Detail != "/etc/hosts (1 bytes)" || !strings.Contains(a.Reason, "outside the task's worktree") {
		t.Fatalf("ask %+v", a)
	}
	if err := rg.client.Answer(ctx, a.ID, serve.Answer{Allow: false, Text: "not there"}); err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateComplete)
	if feed := rg.feedText(s.ID); !strings.Contains(feed, "write: deny") || !strings.Contains(feed, "declined: not there") {
		t.Fatalf("feed:\n%s", feed)
	}
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })

	if _, err := rg.client.Say(ctx, "TM-001", "Also update the README."); err != nil {
		t.Fatal(err)
	}
	eventually(t, "resumed run", func() bool {
		return strings.Contains(rg.feedText(s.ID), "resumed with: Also update the README.")
	})
	rg.waitState(s.ID, serve.StateComplete)
	args, _, _ := rg.fakeLog()
	if len(args) != 2 || !strings.Contains(strings.Join(args[1], " "), "--resume "+s.ID) {
		t.Fatalf("args %v", args)
	}
	eventually(t, "review again", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
}

func TestStopKeepsTheClaim(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	ctx := context.Background()
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateActive)
	if _, err := rg.client.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateStopped)
	m := rg.stateFile("TM-001")
	if m["lifecycle"] != "in_progress" || m["harness"].(map[string]any)["status"] != "exited" {
		t.Fatalf("state %v", m)
	}
	if _, err := os.Stat(s.Worktree); err != nil {
		t.Fatalf("worktree gone: %v", err)
	}
}

func TestAPIBillingIsStopped(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait", "FAKE_APIKEYSOURCE=ANTHROPIC_API_KEY")
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	got := rg.waitState(s.ID, serve.StateError)
	if got.Billing != "api" || !strings.Contains(rg.feedText(s.ID), "would bill an API key") {
		t.Fatalf("%+v\n%s", got, rg.feedText(s.ID))
	}
	eventually(t, "the process ended", func() bool { return rg.session(s.ID).ExitCode != nil })
}

func TestBypassPermissionsIsStopped(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait", "FAKE_MODE=bypassPermissions")
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateError)
	if !strings.Contains(rg.feedText(s.ID), "bypassPermissions") {
		t.Fatal(rg.feedText(s.ID))
	}
}

func TestCrashIsAnError(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "crash")
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	got := rg.waitState(s.ID, serve.StateError)
	if got.ExitCode == nil || *got.ExitCode != 3 {
		t.Fatalf("%+v", got)
	}
	eventually(t, "harness error", func() bool {
		h, _ := rg.stateFile("TM-001")["harness"].(map[string]any)
		return h["status"] == "error"
	})
}

func TestRunRefusals(t *testing.T) {
	repo := newRepo(t,
		testrepo.Task{ID: "TM-001"},
		testrepo.Task{ID: "TM-002", DependsOn: []string{"TM-001"}},
		testrepo.Task{ID: "TM-003", NeedsPerson: true},
		testrepo.Task{ID: "TM-004", Extra: "profile: nope\n"},
		testrepo.Task{ID: "TM-005"},
	)
	rg := start(t, repo, "wait")
	if err := os.Mkdir(filepath.Join(filepath.Dir(repo.Dir), "repo-tm-005"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []struct {
		req  serve.RunRequest
		want string
	}{
		{serve.RunRequest{Project: "repo", Task: "TM-002"}, "not ready: dependency TM-001 is backlog"},
		{serve.RunRequest{Project: "repo", Task: "TM-003"}, "needs a person"},
		{serve.RunRequest{Project: "repo", Task: "TM-004"}, `profile "nope"`},
		{serve.RunRequest{Project: "repo", Task: "TM-001", Profile: "codexy"}, "runs only claude profiles"},
		{serve.RunRequest{Project: "repo", Task: "TM-005"}, "already exists"},
		{serve.RunRequest{Project: "repo", Task: "TM-404"}, "no such task"},
		{serve.RunRequest{Project: "elsewhere", Task: "TM-001"}, "no project"},
	} {
		_, err := rg.client.Run(ctx, c.req)
		var ae *serve.APIError
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.req, err, c.want)
		} else if !asAPI(err, &ae) || !ae.Refused() {
			t.Errorf("%+v: not a refusal: %v", c.req, err)
		}
	}
	// nothing was claimed or created by a refused run
	if des, _ := os.ReadDir(repo.StateDir()); len(des) > 1 { // .lock only
		t.Fatalf("state dir %v", des)
	}
	if out := repo.Git("branch", "--list", "task/*"); out != "" {
		t.Fatalf("branches left: %s", out)
	}
	// needs_person with --person runs
	if _, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-003", Person: true}); err != nil {
		t.Fatal(err)
	}
}

func asAPI(err error, target **serve.APIError) bool {
	ae, ok := err.(*serve.APIError)
	if ok {
		*target = ae
	}
	return ok
}

func TestRestartMakesRunningSessionsStale(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateActive)
	rg.stop() // the service stops its sessions on the way out
	rg.open([]string{"FAKE_SCENARIO=wait", "FAKE_LOG=" + rg.log})
	got := rg.session(s.ID)
	if got.State != serve.StateStopped {
		t.Fatalf("after a clean restart: %s", got.State)
	}
	// the claim stays; saying something resumes the conversation
	if rg.stateFile("TM-001")["lifecycle"] != "in_progress" {
		t.Fatal("claim lost")
	}
	if _, err := rg.client.Say(context.Background(), s.ID, "carry on"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "resumed", func() bool { return strings.Contains(rg.feedText(s.ID), "resumed with: carry on") })
}

func TestAPINeedsTokenAndLoopbackHost(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	data, _ := os.ReadFile(filepath.Join(rg.paths.Store, "serve.json"))
	var info serve.Info
	_ = json.Unmarshal(data, &info)
	url := fmt.Sprintf("http://127.0.0.1:%d/api/snapshot", info.Port)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	tok, _ := os.ReadFile(filepath.Join(rg.paths.Store, "token"))
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	req.Host = "evil.example:80"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", resp.StatusCode)
	}
	if info, err := os.Stat(filepath.Join(rg.paths.Store, "token")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %v %v", info.Mode(), err)
	}
	// a second service on the same store refuses before touching anything
	if _, err := serve.Open(rg.paths, "test", os.Environ(), rg.home); err == nil || !strings.Contains(err.Error(), "another taskmoor serve") {
		t.Fatalf("second open: %v", err)
	}
}

func TestRecoveryAfterACrash(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateActive)
	rg.stop()
	// as if the service had died mid-session: the records still say running
	rec := filepath.Join(rg.paths.Store, "sessions", s.ID+".json")
	data, _ := os.ReadFile(rec)
	if err := os.WriteFile(rec, []byte(strings.Replace(string(data), `"state": "stopped"`, `"state": "active"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	sf := filepath.Join(repo.StateDir(), "TM-001.json")
	var m0 map[string]any
	data, _ = os.ReadFile(sf)
	_ = json.Unmarshal(data, &m0)
	h0 := m0["harness"].(map[string]any)
	h0["status"] = "working"
	delete(h0, "exit_code")
	data, _ = json.Marshal(m0)
	if err := os.WriteFile(sf, data, 0o644); err != nil {
		t.Fatal(err)
	}
	rg.open([]string{"FAKE_SCENARIO=wait", "FAKE_LOG=" + rg.log})
	if st := rg.session(s.ID).State; st != serve.StateStale {
		t.Fatalf("state %s, want stale", st)
	}
	m := rg.stateFile("TM-001")
	if m["lifecycle"] != "in_progress" || m["harness"].(map[string]any)["status"] != "unknown" {
		t.Fatalf("state file %v", m)
	}
}

func TestSecondOpenLeavesTheLiveServiceAlone(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateActive)
	before := rg.feedText(s.ID)
	if _, err := serve.Open(rg.paths, "test", os.Environ(), rg.home); err == nil {
		t.Fatal("a second service opened the store")
	}
	if got := rg.session(s.ID); got.State != serve.StateActive || rg.feedText(s.ID) != before {
		t.Fatalf("the live session changed: %s\n%s", got.State, rg.feedText(s.ID))
	}
}

func TestLateRefusalRemovesOnlyWhatTheRunMade(t *testing.T) {
	repo := newRepo(t)
	// an invalid state file: readiness treats it as no state, the claim
	// refuses it after the worktree and branch exist
	repo.WriteRawState("TM-001.json", "{broken")
	rg := start(t, repo, "wait")
	_, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err == nil || !strings.Contains(err.Error(), "already has a state file") {
		t.Fatalf("err %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo.StateDir(), "TM-001.json")); string(data) != "{broken" {
		t.Fatalf("the existing file changed: %q", data)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(repo.Dir), "repo-tm-001")); !os.IsNotExist(err) {
		t.Fatalf("worktree left: %v", err)
	}
	if out := repo.Git("branch", "--list", "task/*"); out != "" {
		t.Fatalf("branch left: %s", out)
	}
	snap, _ := rg.client.Snapshot(context.Background())
	if len(snap.Sessions) != 0 {
		t.Fatalf("a session was recorded: %+v", snap.Sessions)
	}
}

func TestSayRefusesAClaimThatIsNoLongerTheSessions(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval")
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	a := rg.waitAsk()
	_ = rg.client.Answer(context.Background(), a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	// released and claimed again by someone else
	sf := filepath.Join(repo.StateDir(), "TM-001.json")
	data, _ := os.ReadFile(sf)
	other := strings.Replace(string(data), s.ID, "00000000-0000-4000-8000-000000000000", 1)
	if err := os.WriteFile(sf, []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = rg.client.Say(context.Background(), s.ID, "go on")
	if err == nil || !strings.Contains(err.Error(), "belongs to another session") {
		t.Fatalf("err %v", err)
	}
	if after, _ := os.ReadFile(sf); string(after) != other {
		t.Fatal("the other claim was changed")
	}
	if args, _, _ := rg.fakeLog(); len(args) != 1 {
		t.Fatalf("a runner started: %v", args)
	}
}

func TestConcurrentSaysStartOneRunner(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	_ = rg.client.Answer(context.Background(), a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := rg.client.Say(context.Background(), s.ID, fmt.Sprintf("more %d", i)); err != nil {
				t.Logf("say %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	eventually(t, "every runner ended", func() bool {
		starts, exits := rg.lifetimes()
		return starts == exits && rg.session(s.ID).State == serve.StateComplete
	})
	starts, _ := rg.lifetimes()
	if starts < 2 {
		t.Fatalf("no resume ran (%d processes)", starts)
	}
	// never two at once: each process exits before the next starts
	running := 0
	for _, ev := range rg.events() {
		switch ev {
		case "start":
			if running++; running > 1 {
				t.Fatal("two claude processes of one session ran at once")
			}
		case "exit":
			running--
		}
	}
}

func TestRefusedResumeKeepsReview(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	_ = rg.client.Answer(context.Background(), a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	// confined: a worktree whose .git no longer names its own git
	// directory gives the sandbox nothing safe to allow
	gitFile := filepath.Join(s.Worktree, ".git")
	orig, _ := os.ReadFile(gitFile)
	_ = os.WriteFile(gitFile, []byte("gitdir: /tmp\n"), 0o644)
	if _, err := rg.client.Say(context.Background(), s.ID, "more"); err == nil || !strings.Contains(err.Error(), "git directory") {
		t.Fatalf("err %v", err)
	}
	if lc := rg.stateFile("TM-001")["lifecycle"]; lc != "review" {
		t.Fatalf("lifecycle %v after a refused confined resume", lc)
	}
	_ = os.WriteFile(gitFile, orig, 0o644)
	// unconfined, the worktree now makes Claude bill an API key
	unconfine(t, rg)
	if err := os.MkdirAll(filepath.Join(s.Worktree, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Worktree, ".claude", "settings.local.json"), []byte(`{"apiKeyHelper":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := rg.client.Say(context.Background(), s.ID, "more"); err == nil || !strings.Contains(err.Error(), "apiKeyHelper") {
		t.Fatalf("err %v", err)
	}
	if lc := rg.stateFile("TM-001")["lifecycle"]; lc != "review" {
		t.Fatalf("lifecycle %v after a refused resume", lc)
	}
	if starts, _ := rg.lifetimes(); starts != 1 {
		t.Fatalf("%d processes", starts)
	}
}

func TestRecoveryFinishesAnInterruptedHandover(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	_ = rg.client.Answer(context.Background(), a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	rg.stop()
	// as if the service died after recording "complete" but before the
	// state file's handover
	sf := filepath.Join(repo.StateDir(), "TM-001.json")
	var m map[string]any
	data, _ := os.ReadFile(sf)
	_ = json.Unmarshal(data, &m)
	m["lifecycle"] = "in_progress"
	m["harness"].(map[string]any)["status"] = "working"
	delete(m["harness"].(map[string]any), "exit_code")
	data, _ = json.Marshal(m)
	if err := os.WriteFile(sf, data, 0o644); err != nil {
		t.Fatal(err)
	}
	rg.open([]string{"FAKE_SCENARIO=approval", "FAKE_LOG=" + rg.log})
	got := rg.stateFile("TM-001")
	if got["lifecycle"] != "review" || got["harness"].(map[string]any)["status"] != "done" {
		t.Fatalf("state %v", got)
	}
}

func TestTaskIDIsAmbiguousAcrossProjects(t *testing.T) {
	a := newRepo(t)
	b := newRepo(t)
	rg := start(t, a, "wait")
	cfg, _ := serve.LoadConfig(rg.paths.Config)
	cfg.Projects = append(cfg.Projects, serve.Project{Name: "other", Path: b.Dir})
	if err := cfg.Save(rg.paths.Config); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rg.client.Run(ctx, serve.RunRequest{Project: "other", Task: "TM-001"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rg.client.Feed(ctx, "TM-001"); err == nil || !strings.Contains(err.Error(), "sessions in other, repo") {
		t.Fatalf("unscoped: %v", err)
	}
	s, _, err := rg.client.Project("other").Feed(ctx, "TM-001")
	if err != nil || s.Project != "other" {
		t.Fatalf("scoped: %+v %v", s, err)
	}
}

func TestReleaseThroughTheService(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateActive)
	got, after, err := rg.client.Release(context.Background(), s.ID)
	if err != nil || after != "backlog" || got.State != serve.StateStopped {
		t.Fatalf("%+v %q %v", got, after, err)
	}
	if _, err := os.Stat(filepath.Join(repo.StateDir(), "TM-001.json")); !os.IsNotExist(err) {
		t.Fatal("state file left")
	}
	if _, err := os.Stat(s.Worktree); err != nil {
		t.Fatal("the worktree must stay (A7)")
	}
}

func TestPreflightRefusesBeforeCreatingAnything(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	unconfine(t, rg) // a confined run does not load these settings
	// the person's Claude settings put an API key in env
	if err := os.MkdirAll(filepath.Join(rg.home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rg.home, ".claude", "settings.json"), []byte(`{"env":{"ANTHROPIC_API_KEY":"sk-x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err == nil || !strings.Contains(err.Error(), "env.ANTHROPIC_API_KEY") {
		t.Fatalf("err %v", err)
	}
	os.Remove(filepath.Join(rg.home, ".claude", "settings.json"))
	cfg, _ := serve.LoadConfig(rg.paths.Config)
	cfg.Profiles[0].Command = "/nonexistent/claude"
	_ = cfg.Save(rg.paths.Config)
	_, err = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err == nil || !strings.Contains(err.Error(), "not on the service's PATH") {
		t.Fatalf("err %v", err)
	}
	if out := repo.Git("branch", "--list", "task/*"); out != "" {
		t.Fatalf("branch made: %s", out)
	}
}

func TestSandboxedAndUnconfinedLaunches(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001"}, testrepo.Task{ID: "TM-002", Touches: []string{"other/**"}})
	rg := start(t, repo, "wait")
	cfg, _ := serve.LoadConfig(rg.paths.Config)
	off := false
	cfg.Profiles = append(cfg.Profiles, serve.Profile{Name: "open", Harness: "claude", Command: cfg.Profiles[0].Command, Sandbox: &off})
	_ = cfg.Save(rg.paths.Config)
	ctx := context.Background()
	a, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-002", Profile: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.Confined || b.Confined {
		t.Fatalf("confined %v %v", a.Confined, b.Confined)
	}
	eventually(t, "two processes", func() bool { starts, _ := rg.lifetimes(); return starts == 2 })
	args, _, _ := rg.fakeLog()
	settings := func(argv []string) string {
		for i, x := range argv {
			if x == "--settings" {
				return argv[i+1]
			}
		}
		return ""
	}
	byID := map[string]string{}
	for _, argv := range args {
		for i, x := range argv {
			if x == "--session-id" {
				byID[argv[i+1]] = settings(argv)
			}
		}
	}
	if !strings.Contains(byID[a.ID], `"enabled":true`) || !strings.Contains(byID[a.ID], rg.paths.Store) {
		t.Errorf("sandboxed settings %s", byID[a.ID])
	}
	if !strings.Contains(byID[b.ID], `"enabled":false`) {
		t.Errorf("unconfined settings %s", byID[b.ID])
	}
}

// A resume runs under the profile as it is now, and the session says so,
// also after a restart.
func TestResumeFollowsTheProfilesSandbox(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	_ = rg.client.Answer(ctx, a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	if !rg.session(s.ID).Confined {
		t.Fatal("not confined")
	}
	setSandbox := func(on bool) {
		cfg, _ := serve.LoadConfig(rg.paths.Config)
		cfg.Profiles[0].Sandbox = &on
		if err := cfg.Save(rg.paths.Config); err != nil {
			t.Fatal(err)
		}
	}
	lastSettings := func() string {
		args, _, _ := rg.fakeLog()
		argv := args[len(args)-1]
		for i, x := range argv {
			if x == "--settings" {
				return argv[i+1]
			}
		}
		return ""
	}
	setSandbox(false)
	if _, err := rg.client.Say(ctx, s.ID, "more"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the second run", func() bool { starts, _ := rg.lifetimes(); return starts == 2 })
	if !strings.Contains(lastSettings(), `"enabled":false`) || rg.session(s.ID).Confined {
		t.Fatalf("the resume kept the old profile: %s", lastSettings())
	}
	rg.waitState(s.ID, serve.StateComplete)
	rg.stop()
	setSandbox(true)
	rg.open([]string{"FAKE_SCENARIO=approval", "FAKE_LOG=" + rg.log})
	if rg.session(s.ID).Confined {
		t.Fatal("the record lost the last run's flag")
	}
	if _, err := rg.client.Say(ctx, s.ID, "again"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the third run", func() bool { starts, _ := rg.lifetimes(); return starts == 3 })
	if !strings.Contains(lastSettings(), `"enabled":true`) || !rg.session(s.ID).Confined {
		t.Fatalf("resumed after a restart: %s", lastSettings())
	}
}

func TestAnOldClaudeIsRefusedForASandboxedProfile(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait", "FAKE_VERSION=2.1.100")
	_, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err == nil || !strings.Contains(err.Error(), "older than") {
		t.Fatalf("%v", err)
	}
}

func unconfine(t *testing.T, rg *rig) {
	t.Helper()
	cfg, _ := serve.LoadConfig(rg.paths.Config)
	off := false
	cfg.Profiles[0].Sandbox = &off
	if err := cfg.Save(rg.paths.Config); err != nil {
		t.Fatal(err)
	}
}

// A confined run loads none of the person's settings, so an API key there
// does not refuse it (init still reports the billing source).
func TestConfinedRunIgnoresSettingsItDoesNotLoad(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	_ = os.MkdirAll(filepath.Join(rg.home, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(rg.home, ".claude", "settings.json"), []byte(`{"apiKeyHelper":"x"}`), 0o644)
	if _, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"}); err != nil {
		t.Fatal(err)
	}
}

// The resumed run's record says pending with its own confinement before
// the process has said anything.
func TestPendingResumeRecordCarriesItsConfinement(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval", "FAKE_RESUME_HOLD=1")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	_ = rg.client.Answer(ctx, a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	unconfine(t, rg)
	if _, err := rg.client.Say(ctx, s.ID, "more"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(rg.paths.Store, "sessions", s.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	_ = json.Unmarshal(data, &rec)
	if rec["state"] != serve.StatePending || rec["confined"] != false {
		t.Fatalf("on disk: state %v confined %v", rec["state"], rec["confined"])
	}
	_, _ = rg.client.Stop(ctx, s.ID)
}
