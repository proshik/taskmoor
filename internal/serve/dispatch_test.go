package serve_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

// startDispatch runs a codex-fake session (the question scenario) with the
// claude fake as the dispatcher answering reply.
func startDispatch(t *testing.T, repo *testrepo.Repo, reply string, on bool, extraEnv ...string) *rig {
	t.Helper()
	return startWith(t, repo, "question", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "agent", Harness: "codex", Command: fakeACP(t)})
		c.DefaultProfile = "agent"
		if on {
			c.DispatcherProfile = "fake"
		}
	}, func(s *serve.Service) {
		s.CodexLogin = func() (string, error) { return "Logged in using ChatGPT", nil }
	},
		append([]string{"FAKE_DISPATCH=" + reply}, extraEnv...)...)
}

// dispatchCalls are the prompts the dispatcher fake was given.
func (rg *rig) dispatchCalls() []string {
	f, err := os.Open(rg.log)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e struct {
			Kind string
			V    struct{ Prompt string }
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Kind == "dispatch" {
			out = append(out, e.V.Prompt)
		}
	}
	return out
}

func (rg *rig) decisions() []serve.Ruling {
	ds, err := rg.client.Decisions(context.Background())
	if err != nil {
		rg.t.Fatal(err)
	}
	return ds
}

func TestDispatcherAnswersLevelB(t *testing.T) {
	repo := newRepo(t)
	rg := startDispatch(t, repo, `{"questions":[{"level":"b","option":"SQLite","reason":"a reversible storage choice"}]}`, true)
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitState(s.ID, serve.StateComplete)
	lines := feedTexts(rg, s.ID)
	if !hasLine(lines, "answered by the dispatcher (b): a reversible storage choice → SQLite") || !hasLine(lines, "answer: accept sqlite") {
		t.Fatalf("%q", lines)
	}
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	ds := rg.decisions()
	if len(ds) != 1 || ds[0].Level != "b" || ds[0].AnsweredBy != "dispatcher" || ds[0].Answer[0][0] != "SQLite" {
		t.Fatalf("%+v", ds)
	}
	// the call has the question and the task file, from main
	p := rg.dispatchCalls()
	if len(p) != 1 || !strings.Contains(p[0], "Which store?") || !strings.Contains(p[0], "=== FILE: tasks/TM-001") || !strings.Contains(p[0], "(recommended)") {
		t.Fatalf("%q", p)
	}
	// the person overrides: the same session gets the instruction
	if _, err := rg.client.Override(ctx, ds[0].ID, serve.Answer{Choices: [][]string{{"1"}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the override", func() bool {
		return hasLine(feedTexts(rg, s.ID), "The person overrides the dispatcher") && rg.session(s.ID).State == serve.StateComplete
	})
	if d := rg.decisions()[0]; d.Overridden == nil || d.Overridden.Answer != "Plain files" {
		t.Fatalf("%+v", d)
	}
	// a second override is sent too, and replaces the first in the record
	if _, err := rg.client.Override(ctx, ds[0].ID, serve.Answer{Choices: [][]string{{"2"}}}); err != nil {
		t.Fatal(err)
	}
	if d := rg.decisions()[0]; d.Overridden.Answer != "SQLite" {
		t.Fatalf("%+v", d.Overridden)
	}
}

func TestDispatcherLevelANeedsARealCitationFromMain(t *testing.T) {
	repo := newRepo(t)
	repo.Write("AGENTS.md", "# Rules\n\nThe store: use plain files for every store; no database.\n")
	repo.Commit("Agents rules")
	cite := `{"questions":[{"level":"a","option":"Plain files","file":"AGENTS.md","quote":"use plain files for every store","reason":"AGENTS.md says so"}]}`
	rg := startDispatch(t, repo, cite, true)
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitState(s.ID, serve.StateComplete)
	if !hasLine(feedTexts(rg, s.ID), `answered by the dispatcher (a): AGENTS.md: “use plain files for every store”`) || !hasLine(feedTexts(rg, s.ID), "answer: accept files") {
		t.Fatalf("%q", feedTexts(rg, s.ID))
	}
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	if d := rg.decisions()[0]; d.Level != "a" || !strings.Contains(d.Citation, "AGENTS.md") {
		t.Fatalf("%+v", d)
	}
}

// What the dispatcher may not decide stays in the inbox, with the reason.
func TestDispatcherLeavesThePersonsQuestions(t *testing.T) {
	for name, c := range map[string]struct {
		reply, want string
		env         []string
	}{
		"invented citation":  {`{"questions":[{"level":"a","option":"Plain files","file":"AGENTS.md","quote":"always use plain files here","reason":"x"}]}`, "citation", nil},
		"not recommended":    {`{"questions":[{"level":"b","option":"Plain files","reason":"x"}]}`, "non-recommended", nil},
		"level c":            {`{"questions":[{"level":"c","option":"SQLite","reason":"a scope choice"}]}`, "a scope choice", nil},
		"not json":           {`I think SQLite.`, "not JSON", nil},
		"api billing":        {`{"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`, "API key", []string{"FAKE_DISPATCH_SOURCE=ANTHROPIC_API_KEY"}},
		"hard stop, no call": {`{"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`, "deploy", []string{"FAKE_QUESTION=Deploy to which store?"}},
		"other option label": {`{"questions":[{"level":"b","option":"Postgres","reason":"x"}]}`, "not an option", nil},
		"too few answers":    {`{"questions":[]}`, "0 of 1", nil},
		"secret path":        {`{"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`, ".env", []string{"FAKE_QUESTION=Which store goes in .env?"}},
		"manifest":           {`{"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`, "go.mod", []string{"FAKE_QUESTION=Which store for go.mod?"}},
		"embedded manifest":  {`{"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`, "go.mod", []string{"FAKE_QUESTION=Which store (file=go.mod#L12)?"}},
		"embedded secret":    {`{"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`, ".env", []string{"FAKE_QUESTION=Which store for --config=.env?"}},
	} {
		t.Run(name, func(t *testing.T) {
			rg := startDispatch(t, newRepo(t), c.reply, true, c.env...)
			_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
			a := rg.waitAsk()
			eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
			d := rg.decisions()[0]
			if d.Level != "c" || d.AnsweredBy != "person" || !strings.Contains(d.Reason, c.want) {
				t.Fatalf("%+v", d)
			}
			eventually(t, "the ask's dispatch note", func() bool {
				snap, _ := rg.client.Snapshot(context.Background())
				return len(snap.Asks) == 1 && snap.Asks[0].ID == a.ID && strings.HasPrefix(snap.Asks[0].Dispatch, "for you:")
			})
			if name == "hard stop, no call" && len(rg.dispatchCalls()) != 0 {
				t.Fatal("a hard stop was sent to the dispatcher")
			}
		})
	}
}

func TestDispatcherOffLeavesEverything(t *testing.T) {
	rg := startDispatch(t, newRepo(t), `{"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`, false)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	time.Sleep(500 * time.Millisecond)
	if len(rg.decisions()) != 0 || len(rg.dispatchCalls()) != 0 {
		t.Fatal("the dispatcher acted while off")
	}
}

// Approvals are never the dispatcher's (C3).
func TestDispatcherNeverAnswersApprovals(t *testing.T) {
	rg := startWith(t, newRepo(t), "approval", func(c *serve.Config) {
		c.DispatcherProfile = "fake"
	}, nil, `FAKE_DISPATCH={"questions":[{"level":"b","option":"Allow","reason":"x"}]}`)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	time.Sleep(500 * time.Millisecond)
	if a.Kind != "approval" || len(rg.decisions()) != 0 || len(rg.dispatchCalls()) != 0 {
		t.Fatalf("%+v %d", a, len(rg.dispatchCalls()))
	}
}

// A document only in a working tree (not committed to main) answers nothing.
func TestDispatcherReadsTheMainBranchOnly(t *testing.T) {
	repo := newRepo(t)
	repo.Write("AGENTS.md", "The store: use plain files for every store; no database.\n") // not committed
	cite := `{"questions":[{"level":"a","option":"Plain files","file":"AGENTS.md","quote":"use plain files for every store","reason":"x"}]}`
	rg := startDispatch(t, repo, cite, true)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	if d := rg.decisions()[0]; d.Level != "c" || !strings.Contains(d.Reason, "citation") {
		t.Fatalf("%+v", d)
	}
}

// Codex's optional own-answer field is left empty, not a reason to stop.
func TestDispatcherSkipsTheOptionalNoteField(t *testing.T) {
	rg := startDispatch(t, newRepo(t), `{"questions":[{"level":"b","option":"SQLite","reason":"reversible"}]}`, true, "FAKE_NOTE=1")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitState(s.ID, serve.StateComplete)
	if !hasLine(feedTexts(rg, s.ID), "answer: accept sqlite") {
		t.Fatalf("%q", feedTexts(rg, s.ID))
	}
	if p := rg.dispatchCalls(); len(p) != 1 || strings.Contains(p[0], "Additional answer") {
		t.Fatalf("the note field went to the dispatcher: %q", p)
	}
}

// A real quote that does not name the chosen answer authorizes nothing.
func TestDispatcherCitationMustStateTheAnswer(t *testing.T) {
	repo := newRepo(t)
	repo.Write("AGENTS.md", "The store: use plain files for every store; no database.\n")
	repo.Commit("Agents rules")
	cite := `{"questions":[{"level":"a","option":"SQLite","file":"AGENTS.md","quote":"use plain files for every store","reason":"x"}]}`
	rg := startDispatch(t, repo, cite, true)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	if d := rg.decisions()[0]; d.Level != "c" || !strings.Contains(d.Reason, "does not name the answer") {
		t.Fatalf("%+v", d)
	}
}

// A tag of the main branch's name is not the main branch.
func TestDispatcherReadsTheBranchNotATag(t *testing.T) {
	repo := newRepo(t)
	repo.Write("AGENTS.md", "The store: use plain files for every store; no database.\n")
	repo.Commit("Agents rules on a side commit")
	repo.Git("tag", "main")               // the tag points here
	repo.Git("reset", "--hard", "HEAD~1") // main does not have the rule
	cite := `{"questions":[{"level":"a","option":"Plain files","file":"AGENTS.md","quote":"use plain files for every store","reason":"x"}]}`
	rg := startDispatch(t, repo, cite, true)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	if d := rg.decisions()[0]; d.Level != "c" || !strings.Contains(d.Reason, "citation") {
		t.Fatalf("%+v", d)
	}
}

// Managed hooks would run in the dispatcher's call: the person decides.
func TestDispatcherRefusesManagedHooks(t *testing.T) {
	f := t.TempDir() + "/managed-settings.json"
	_ = os.WriteFile(f, []byte(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"true"}]}]}}`), 0o644)
	defer serve.SetManagedSettings(f)()
	rg := startDispatch(t, newRepo(t), `{"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`, true)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	if d := rg.decisions()[0]; d.Level != "c" || !strings.Contains(d.Reason, "hooks") || len(rg.dispatchCalls()) != 0 {
		t.Fatalf("%+v", d)
	}
}

// A level-c record gets the person's answer.
func TestDispatcherRecordsThePersonsAnswer(t *testing.T) {
	rg := startDispatch(t, newRepo(t), `{"questions":[{"level":"c","option":"SQLite","reason":"scope"}]}`, true)
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	_ = rg.client.Answer(ctx, a.ID, serve.Answer{Choices: [][]string{{"1"}}})
	rg.waitState(s.ID, serve.StateComplete)
	if d := rg.decisions()[0]; d.AnsweredBy != "person" || len(d.Answer) != 1 || !strings.Contains(d.Answer[0][0], "Plain files") {
		t.Fatalf("%+v", d)
	}
}

// A quote that forbids the option is the person's to read.
func TestDispatcherCitationAgainstTheAnswer(t *testing.T) {
	repo := newRepo(t)
	repo.Write("AGENTS.md", "The store: never use SQLite for the store here.\n")
	repo.Commit("Agents rules")
	cite := `{"questions":[{"level":"a","option":"SQLite","file":"AGENTS.md","quote":"never use SQLite for the store","reason":"x"}]}`
	rg := startDispatch(t, repo, cite, true)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	if d := rg.decisions()[0]; d.Level != "c" || !strings.Contains(d.Reason, "rule against") {
		t.Fatalf("%+v", d)
	}
}

// Managed settings: a key in the env block, or a hook in a later drop-in
// behind an apiKeyHelper, keep the dispatcher out.
func TestDispatcherManagedCredentialsAndDropIns(t *testing.T) {
	dir := t.TempDir()
	base, drop := dir+"/managed-settings.json", dir+"/10-hooks.json"
	_ = os.WriteFile(base, []byte(`{"apiKeyHelper":"/bin/echo","env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`), 0o644)
	_ = os.WriteFile(drop, []byte(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"true"}]}]}}`), 0o644)
	defer serve.SetManagedSettings(base, drop)()
	rg := startDispatch(t, newRepo(t), `{"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`, true)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	if d := rg.decisions()[0]; d.Level != "c" || !strings.Contains(d.Reason, "hooks") || len(rg.dispatchCalls()) != 0 {
		t.Fatalf("%+v", d)
	}
}
