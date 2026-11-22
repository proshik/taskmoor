package serve_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

// Requests the rules cannot judge are the person's, never auto-allowed.
func TestACPRequestsTheRulesCannotJudgeAreAsked(t *testing.T) {
	for _, sc := range []string{"permissions", "network"} {
		t.Run(sc, func(t *testing.T) {
			rg := startAgent(t, newRepo(t), "codex", sc, nil)
			s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
			a := rg.waitAsk()
			if a.Kind != "approval" || !strings.HasPrefix(a.Tool, "ACP:") {
				t.Fatalf("%+v", a)
			}
			_ = rg.client.Answer(context.Background(), a.ID, serve.Answer{Allow: false})
			rg.waitState(s.ID, serve.StateComplete)
			if !hasLine(feedTexts(rg, s.ID), "permission: no") {
				t.Fatalf("%q", feedTexts(rg, s.ID))
			}
		})
	}
}

// Every path of a multi-file change is judged: a later one outside the
// worktree makes it the person's.
func TestACPMultiFileEditIsJudgedWhole(t *testing.T) {
	repo := newRepo(t)
	wt := repo.Dir + "-tm-001"
	rg := startAgent(t, repo, "codex", "edits", nil, "FAKE_ACP_PATHS="+filepath.Join(wt, "a.go")+","+filepath.Join(repo.Dir, "main.go"))
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	if a.Kind != "approval" || !strings.Contains(a.Detail, filepath.Join(repo.Dir, "main.go")) {
		t.Fatalf("%+v", a)
	}
}

// A command that runs outside the worktree is the person's, whatever its text.
func TestACPCommandElsewhereIsAsked(t *testing.T) {
	repo := newRepo(t)
	rg := startAgent(t, repo, "codex", "approval", nil, "FAKE_ACP_COMMAND=git merge task/x", "FAKE_ACP_CWD="+repo.Dir)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	if a.Tool != "ACP:elsewhere" || !strings.Contains(a.Detail, repo.Dir) {
		t.Fatalf("%+v", a)
	}
}

// Extra permissions with a command, a rename without its destination, a
// fetch: all the person's.
func TestACPMoreRequestsAreAsked(t *testing.T) {
	for sc, tool := range map[string]string{"cmdperm": "ACP:permissions", "move": "ACP:move", "fetch": "ACP:fetch"} {
		t.Run(sc, func(t *testing.T) {
			rg := startAgent(t, newRepo(t), "codex", sc, nil)
			_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
			if a := rg.waitAsk(); a.Tool != tool {
				t.Fatalf("%+v", a)
			}
		})
	}
}

func TestACPStopWithAHelperHoldingOutput(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "orphan", nil)
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	eventually(t, "working", func() bool {
		return hasLine(feedTexts(rg, s.ID), "Working.") || rg.session(s.ID).State == serve.StateActive
	})
	start := time.Now()
	if _, err := rg.client.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateStopped)
	if d := time.Since(start); d > 12*time.Second {
		t.Fatalf("stop took %s", d)
	}
}

func TestACPIncompleteOrNonFiniteAnswersKeepTheAsk(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "forms", nil)
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	for _, ans := range []serve.Answer{
		{Text: "just this"}, // one text for five questions
		{Choices: [][]string{{"X"}, {"Y"}, {"NaN"}, {"p"}, {}}},
		{Choices: [][]string{{"X"}, {"Y"}, {"3"}, {}, {}}}, // the required token missing
	} {
		if err := rg.client.Answer(ctx, a.ID, ans); err == nil {
			t.Fatalf("accepted %+v", ans)
		}
	}
	// the trailing optional question may be left out
	if err := rg.client.Answer(ctx, a.ID, serve.Answer{Choices: [][]string{{"X"}, {"Y"}, {"3"}, {"p"}}}); err != nil {
		t.Fatalf("the ask was lost: %v", err)
	}
	rg.waitState(s.ID, serve.StateComplete)
}

// A Restart to a profile that cannot run leaves the session running.
func TestRestartChecksTheSuccessorFirst(t *testing.T) {
	rg := startWith(t, newRepo(t), "wait", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "oc", Harness: "opencode", Command: fakeACP(t)})
	}, nil)
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	eventually(t, "active", func() bool { return rg.session(s.ID).State == serve.StateActive })
	if _, err := rg.client.Restart(ctx, s.ID, "oc"); err == nil || !strings.Contains(err.Error(), "api_billing") {
		t.Fatalf("%v", err)
	}
	if st := rg.session(s.ID).State; st != serve.StateActive {
		t.Fatalf("the session was stopped by a refused restart: %s", st)
	}
}

func TestACPFormsKeepFieldsApartAndTyped(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "forms", nil)
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	if len(a.Questions) != 5 {
		t.Fatalf("%+v", a.Questions)
	}
	// questions: Pick, Pick (b), How many?, Token (required, in order), Anything else? (optional)
	if a.Questions[1].Question == a.Questions[0].Question || !a.Questions[2].FreeText || !a.Questions[3].Secret || !a.Questions[4].Optional {
		t.Fatalf("%+v", a.Questions)
	}
	if err := rg.client.Answer(ctx, a.ID, serve.Answer{Choices: [][]string{{"X"}, {"Y"}, {"x"}, {"s3cr3t"}, {}}}); err == nil {
		t.Fatal("a non-number was accepted for an integer field")
	}
	if err := rg.client.Answer(ctx, a.ID, serve.Answer{Choices: [][]string{{"X"}, {"Y"}, {"3"}, {"s3cr3t"}, {}}}); err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateComplete)
	lines := feedTexts(rg, s.ID)
	// the agent got the secret (it echoes it); the feed has it masked
	if !hasLine(lines, `content: {"a":"x","b":"y","count":3,"pin":"•••"}`) {
		t.Fatalf("%q", lines)
	}
	// the secret reached the agent (which repeats it), not the journal's
	// record of the person's answer
	if !hasLine(lines, "(secret answer, not recorded)") {
		t.Fatalf("%q", lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "s3cr3t") {
			t.Fatalf("the secret was journalled: %q", l)
		}
	}
}

func TestACPAsyncQuestionInterruptsTheTurn(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "async", nil)
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	eventually(t, "the cancel", func() bool { return len(rg.acpCalls("session/cancel")) == 1 })
	if got := rg.session(s.ID).State; got != serve.StateAwaitingInput {
		t.Fatalf("state %s", got)
	}
	if err := rg.client.Answer(ctx, a.ID, serve.Answer{Choices: [][]string{{"1"}}}); err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateComplete)
	if !hasLine(feedTexts(rg, s.ID), "resumed with: The person answered your question (Go on?): Go on?: yes") {
		t.Fatalf("%q", feedTexts(rg, s.ID))
	}
}

func TestACPSetupThatNeverAnswersEnds(t *testing.T) {
	defer serve.SetACPTimeouts(time.Second)()
	rg := startAgent(t, newRepo(t), "codex", "work", nil, "FAKE_ACP_HANG=initialize")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitState(s.ID, serve.StateError)
	if !strings.Contains(rg.session(s.ID).Error, "did not answer in time") {
		t.Fatalf("%+v", rg.session(s.ID))
	}
}

func TestACPHandoverBeforeTheSessionOpensIsRefused(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "work", nil, "FAKE_ACP_HANG=session/new")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if _, _, err := rg.client.Handover(context.Background(), s.ID); err == nil || !strings.Contains(err.Error(), "no conversation") {
		t.Fatalf("%v", err)
	}
}

func TestTaskModelAndInheritedOverrides(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Extra: "model: big-1\n"})
	rg := startAgent(t, repo, "opencode", "work", func(p *serve.Profile) { p.Model = "fast-1" },
		"OPENCODE_CONFIG_DIR=/elsewhere", "OPENCODE_CONFIG=/elsewhere/x.json")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitState(s.ID, serve.StateComplete)
	if m := rg.acpCalls("session/set_config_option"); len(m) != 1 || m[0]["value"] != "big-1" {
		t.Fatalf("model %v", m)
	}
	if got := rg.session(s.ID); got.Model != "big-1" {
		t.Fatalf("%+v", got)
	}
	_, _, env := rg.fakeLog()
	if env[0]["OPENCODE_CONFIG_DIR"] != nil || env[0]["OPENCODE_CONFIG"] != nil {
		t.Fatalf("inherited overrides kept: %v %v", env[0]["OPENCODE_CONFIG_DIR"], env[0]["OPENCODE_CONFIG"])
	}
}

func TestCodexWithAnAPIProviderNeedsAPIBilling(t *testing.T) {
	repo := newRepo(t)
	rg := startAgent(t, repo, "codex", "work", nil)
	_ = os.MkdirAll(filepath.Join(rg.home, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(rg.home, ".codex", "config.toml"), []byte("model_provider = \"gateway\"\n"), 0o644)
	_, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err == nil || !strings.Contains(err.Error(), "model_provider") {
		t.Fatalf("%v", err)
	}
}

func TestTaskIDNamesTheRestartedSession(t *testing.T) {
	repo := newRepo(t)
	rg := startAgent(t, repo, "codex", "wait", nil)
	ctx := context.Background()
	old, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	nu, err := rg.client.Restart(ctx, old.ID, "agent")
	if err != nil {
		t.Fatal(err)
	}
	got, err := rg.client.Stop(ctx, "TM-001") // by task id, the same second
	if err != nil || got.ID != nu.ID {
		t.Fatalf("%v %v, want %s", got, err, fmt.Sprint(nu.ID))
	}
}
