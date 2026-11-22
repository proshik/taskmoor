package serve_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

var (
	acpOnce sync.Once
	acpPath string
	acpErr  error
)

// fakeACP builds testdata/fakeacp once.
func fakeACP(t *testing.T) string {
	t.Helper()
	acpOnce.Do(func() {
		dir, err := os.MkdirTemp("", "taskmoor-fakeacp-")
		if err != nil {
			acpErr = err
			return
		}
		acpPath = filepath.Join(dir, "acp")
		out, err := exec.Command("go", "build", "-o", acpPath, "./testdata/fakeacp").CombinedOutput()
		if err != nil {
			acpErr = fmt.Errorf("%v: %s", err, out)
		}
	})
	if acpErr != nil {
		t.Fatal(acpErr)
	}
	return acpPath
}

// startAgent runs a service whose default profile is the fake ACP agent
// as harness (codex or opencode).
func startAgent(t *testing.T, repo *testrepo.Repo, harness, scenario string, adjust func(*serve.Profile), extraEnv ...string) *rig {
	t.Helper()
	return startWith(t, repo, scenario, func(c *serve.Config) {
		p := serve.Profile{Name: "agent", Harness: harness, Command: fakeACP(t), APIBilling: harness == "opencode"}
		if adjust != nil {
			adjust(&p)
		}
		c.Profiles = append(c.Profiles, p)
		c.DefaultProfile = "agent"
	}, func(s *serve.Service) {
		s.CodexLogin = func() (string, error) { return "Logged in using ChatGPT", nil }
	}, extraEnv...)
}

func feedTexts(rg *rig, id string) []string {
	_, acts, err := rg.client.Feed(context.Background(), id)
	if err != nil {
		rg.t.Fatal(err)
	}
	var out []string
	for _, a := range acts {
		out = append(out, a.Type+": "+a.Text)
	}
	return out
}

func hasLine(lines []string, sub string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, sub) })
}

// inputs are the messages the fake agent received, by method.
func (rg *rig) acpCalls(method string) []map[string]any {
	_, ins, _ := rg.fakeLog()
	var out []map[string]any
	for _, m := range ins {
		if m["method"] == method {
			p, _ := m["params"].(map[string]any)
			out = append(out, p)
		}
	}
	return out
}

func TestCodexRunAutoApprovedAndResumed(t *testing.T) {
	repo := newRepo(t)
	rg := startAgent(t, repo, "codex", "approval", nil)
	ctx := context.Background()
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateComplete)
	got := rg.session(s.ID)
	if got.AgentSession != "fake-acp-session" || got.Isolation != "codex" || got.Billing != "subscription" ||
		!strings.Contains(got.Resume, "codex resume fake-acp-session") || got.CostUSD != 0.02 {
		t.Fatalf("%+v", got)
	}
	// the rules allow `go test` (the shell wrapper is read through)
	lines := feedTexts(rg, s.ID)
	if !hasLine(lines, "permission: yes") || !hasLine(lines, "action: go test ./...") {
		t.Fatalf("%q", lines)
	}
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	ini := rg.acpCalls("initialize")[0]
	caps, _ := ini["clientCapabilities"].(map[string]any)
	if el, _ := caps["elicitation"].(map[string]any); el["form"] == nil {
		t.Fatalf("form elicitation not advertised: %v", caps)
	}
	if a := rg.acpCalls("authenticate"); len(a) != 1 || a[0]["methodId"] != "chat-gpt" {
		t.Fatalf("authenticate %v", a)
	}
	_, _, env := rg.fakeLog()
	if !strings.Contains(fmt.Sprint(env[0]["CODEX_CONFIG"]), "default_mode_request_user_input") || env[0]["INITIAL_AGENT_MODE"] != "workspace-write" ||
		env[0]["ANTHROPIC_API_KEY"] != nil {
		t.Fatalf("env %v %v", env[0]["CODEX_CONFIG"], env[0]["INITIAL_AGENT_MODE"])
	}
	// Say resumes through session/load; the replayed history is not journalled again
	if _, err := rg.client.Say(ctx, s.ID, "more work"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the resumed turn", func() bool { return hasLine(feedTexts(rg, s.ID), "resumed with: more work") })
	rg.waitState(s.ID, serve.StateComplete)
	if l := rg.acpCalls("session/load"); len(l) != 1 || l[0]["sessionId"] != "fake-acp-session" {
		t.Fatalf("load %v", l)
	}
	if hasLine(feedTexts(rg, s.ID), "an old answer") {
		t.Fatal("the replayed history was journalled")
	}
}

func TestCodexQuestionByForm(t *testing.T) {
	repo := newRepo(t)
	rg := startAgent(t, repo, "codex", "question", nil)
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	if a.Kind != "question" || len(a.Questions) != 1 || a.Questions[0].Recommended != 1 || a.Questions[0].Options[1].Label != "SQLite" {
		t.Fatalf("%+v", a)
	}
	if err := rg.client.Answer(ctx, a.ID, serve.Answer{Choices: [][]string{{"1"}}}); err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateComplete)
	if !hasLine(feedTexts(rg, s.ID), "answer: accept files") {
		t.Fatalf("%q", feedTexts(rg, s.ID))
	}
}

func TestOpenCodeNeedsAPIBillingAndMarksTheDefault(t *testing.T) {
	repo := newRepo(t)
	rg := startAgent(t, repo, "opencode", "opencode", func(p *serve.Profile) { p.APIBilling = false })
	if _, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"}); err == nil || !strings.Contains(err.Error(), "api_billing") {
		t.Fatalf("%v", err)
	}
	rg.stop()

	rg = startAgent(t, newRepo(t), "opencode", "opencode", nil)
	ctx := context.Background()
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	a := rg.waitAsk()
	if a.Questions[0].Recommended != 1 {
		t.Fatalf("the default is not the recommended option: %+v", a.Questions[0])
	}
	_ = rg.client.Answer(ctx, a.ID, serve.Answer{Choices: [][]string{{"SQLite"}}})
	rg.waitState(s.ID, serve.StateComplete)
	got := rg.session(s.ID)
	if got.Billing != "api" || got.Isolation != "" || !strings.Contains(got.Resume, "opencode --session fake-acp-session") {
		t.Fatalf("%+v", got)
	}
	args, _, env := rg.fakeLog()
	if !slices.Equal(args[0], []string{"acp"}) || !strings.Contains(fmt.Sprint(env[0]["OPENCODE_CONFIG_CONTENT"]), `"bash":"ask"`) ||
		env[0]["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" || !strings.HasSuffix(fmt.Sprint(env[0]["XDG_CONFIG_HOME"]), "opencode-config") {
		t.Fatalf("args %v env %v", args[0], env[0]["OPENCODE_CONFIG_CONTENT"])
	}
	if !hasLine(feedTexts(rg, s.ID), "answer: accept sqlite") {
		t.Fatalf("%q", feedTexts(rg, s.ID))
	}
}

func TestACPPermissionRefusedByTheRules(t *testing.T) {
	repo := newRepo(t)
	rg := startAgent(t, repo, "codex", "approval", nil, "FAKE_ACP_COMMAND=git push origin main")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitState(s.ID, serve.StateComplete)
	lines := feedTexts(rg, s.ID)
	if !hasLine(lines, "permission: no") || !hasLine(lines, "refused by the rules") {
		t.Fatalf("%q", lines)
	}
}

func TestACPStopCancelsTheTurn(t *testing.T) {
	repo := newRepo(t)
	rg := startAgent(t, repo, "codex", "wait", func(p *serve.Profile) { p.Model = "big-1" })
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	eventually(t, "working", func() bool {
		return hasLine(feedTexts(rg, s.ID), "Working.") || rg.session(s.ID).State == serve.StateActive
	})
	if _, err := rg.client.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateStopped)
	if c := rg.acpCalls("session/cancel"); len(c) != 1 {
		t.Fatalf("cancel %v", c)
	}
	if m := rg.acpCalls("session/set_config_option"); len(m) != 1 || m[0]["value"] != "big-1" {
		t.Fatalf("model %v", m)
	}
}

func TestCodexNotSignedInIsRefused(t *testing.T) {
	repo := newRepo(t)
	rg := startWith(t, repo, "work", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "agent", Harness: "codex", Command: fakeACP(t)})
		c.DefaultProfile = "agent"
	}, func(s *serve.Service) {
		s.CodexLogin = func() (string, error) { return "Not logged in", fmt.Errorf("exit status 1") }
	})
	_, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err == nil || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.StateDir(), "TM-001.json")); !os.IsNotExist(err) {
		t.Fatal("a refused run claimed the task")
	}
}
