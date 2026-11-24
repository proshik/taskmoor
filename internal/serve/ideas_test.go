package serve_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

var grilledTask = testrepo.TaskFile(testrepo.Task{ID: "TM-050", Title: "From the idea", Component: "docs",
	Touches: []string{"docs/idea.md"}, Body: "## Goal\n\nWhat was agreed.\n\n## Context\n\nThe grilling, in short.\n"})

func startGrill(t *testing.T, repo *testrepo.Repo, extra ...string) *rig {
	t.Helper()
	env := append([]string{"FAKE_GRILL_FILE=tasks/TM-050-from-the-idea.md", "FAKE_GRILL_BODY=" + grilledTask}, extra...)
	return startAgent(t, repo, "codex", "grill", nil, env...)
}

// grill adds an idea, grills it, answers its question and waits for the
// proposal.
func grill(t *testing.T, rg *rig) (*serve.Idea, *serve.Session) {
	t.Helper()
	ctx := context.Background()
	i, err := rg.client.AddIdea(ctx, "repo", "", "Make the board faster.\nIt is slow with many tasks.")
	if err != nil {
		t.Fatal(err)
	}
	if i.Title != "Make the board faster." || i.State != serve.IdeaNew {
		t.Fatalf("%+v", i)
	}
	s, err := rg.client.Grill(ctx, i.ID, serve.GrillRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Idea != i.ID || s.Task != "" || s.Branch != "intake/"+i.ID {
		t.Fatalf("%+v", s)
	}
	a := rg.waitAsk()
	if a.Task != i.ID || a.Dispatch != "" || len(a.Questions) == 0 || a.Questions[0].Question != "How big is the first cut?" {
		t.Fatalf("%+v", a)
	}
	if err := rg.client.Answer(ctx, a.ID, serve.Answer{Choices: [][]string{{"1"}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "proposed", func() bool {
		x, err := rg.client.Idea(ctx, i.ID)
		return err == nil && x.State == serve.IdeaProposed
	})
	i, _ = rg.client.Idea(ctx, i.ID)
	return i, s
}

func TestIdeaGrillAndApprove(t *testing.T) {
	repo := newRepo(t)
	rg := startGrill(t, repo)
	ctx := context.Background()
	i, s := grill(t, rg)
	if !strings.Contains(rg.feedText(s.ID), "answer: accept small") {
		t.Fatalf("%s", rg.feedText(s.ID))
	}
	if len(i.Tasks) != 1 || i.Tasks[0] != "TM-050" {
		t.Fatalf("%+v", i)
	}
	// the grilling is no task: no claim, no Accept, no Release, no Restart
	if _, err := rg.client.Accept(ctx, s.ID, "x"); err == nil {
		t.Fatal("accepted a grilling")
	}
	if _, _, err := rg.client.Release(ctx, s.ID); err == nil {
		t.Fatal("released a grilling")
	}
	if _, err := os.Stat(filepath.Join(repo.StateDir(), "TM-050.json")); err == nil {
		t.Fatal("a state file for the intake")
	}
	rv, err := rg.client.ReviewIdea(ctx, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !rv.Ready || len(rv.Tasks) != 1 || rv.Tasks[0].ID != "TM-050" || rv.Tasks[0].Title != "From the idea" || len(rv.Outside) != 0 || len(rv.Warnings) != 0 {
		t.Fatalf("%+v", rv)
	}
	if _, err := rg.client.Approve(ctx, i.ID, "0000000", rv.Main); err == nil {
		t.Fatal("approved a commit not looked at")
	}
	if _, err := rg.client.Approve(ctx, i.ID, rv.Tip, rv.Tip); err == nil {
		t.Fatal("approved against a main not looked at")
	}
	res, err := rg.client.Approve(ctx, i.ID, rv.Tip, rv.Main)
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged == "" || res.Idea.State != serve.IdeaApproved {
		t.Fatalf("%+v", res)
	}
	if got := repo.Git("rev-parse", "main"); got != res.Merged {
		t.Fatalf("main at %s, merged %s", got, res.Merged)
	}
	msg := repo.Git("log", "-1", "--format=%B", "main")
	if !strings.Contains(msg, "Intake "+i.ID) || !strings.Contains(msg, "Tasks: TM-050") || strings.Contains(msg, "Task: ") ||
		strings.Contains(msg, "board faster") { // the raw idea stays in the store (C8)
		t.Fatalf("%q", msg)
	}
	// one commit on main, of the reviewed tree; the intake's history stays on its branch
	if parents := repo.Git("log", "-1", "--format=%P", "main"); strings.Contains(parents, " ") {
		t.Fatalf("a merge: %s", parents)
	}
	if repo.Git("rev-parse", "main^{tree}") != repo.Git("rev-parse", rv.Tip+"^{tree}") {
		t.Fatal("main's tree is not the reviewed one")
	}
	if out, err := exec.Command("git", "-C", repo.Dir, "merge-base", "--is-ancestor", rv.Tip, "main").CombinedOutput(); err == nil {
		t.Fatalf("the intake's history reached main %s", out)
	}
	if _, err := os.Stat(filepath.Join(repo.Dir, "tasks", "TM-050-from-the-idea.md")); err != nil {
		t.Fatal("the task is not in the checkout")
	}
	if _, err := os.Stat(i.Worktree); err != nil {
		t.Fatal("the worktree went (A7)")
	}
	// the new task is on the board, in the backlog
	snap, _ := rg.client.Snapshot(ctx)
	found := false
	for _, t := range snap.Projects[0].Tasks {
		found = found || t.ID == "TM-050"
	}
	if !found || len(snap.Ideas) != 1 || snap.Ideas[0].State != serve.IdeaApproved {
		t.Fatalf("%+v %+v", snap.Projects[0].Tasks, snap.Ideas)
	}
	if _, err := rg.client.Approve(ctx, i.ID, rv.Tip, rv.Main); err == nil {
		t.Fatal("approved twice")
	}
	// the record outlives the service
	rg.stop()
	rg.open([]string{"FAKE_SCENARIO=wait", "FAKE_LOG=" + rg.log})
	if x, err := rg.client.Idea(ctx, i.ID); err != nil || x.State != serve.IdeaApproved || x.Merged != res.Merged {
		t.Fatalf("%+v %v", x, err)
	}
}

// Only task files are approved; the tasks must validate.
func TestIdeaApproveRefusals(t *testing.T) {
	t.Run("outside tasks", func(t *testing.T) {
		rg := startGrill(t, newRepo(t), "FAKE_GRILL_OUTSIDE=README.md")
		i, _ := grill(t, rg)
		rv, err := rg.client.ReviewIdea(context.Background(), i.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rv.Ready || len(rv.Outside) != 1 || !strings.Contains(rv.Outside[0], `"README.md"`) || !strings.Contains(rv.Why, "may not") {
			t.Fatalf("%+v", rv)
		}
		if _, err := rg.client.Approve(context.Background(), i.ID, rv.Tip, rv.Main); err == nil {
			t.Fatal("approved a change outside tasks/")
		}
	})
	for name, file := range map[string]string{"tasks README": "tasks/README.md", "nested": "tasks/sub/x.md", "not markdown": "tasks/notes.txt", "outside, spaced": " tasks/x.md"} {
		t.Run(name, func(t *testing.T) {
			rg := startGrill(t, newRepo(t), "FAKE_GRILL_OUTSIDE="+file)
			i, _ := grill(t, rg)
			rv, err := rg.client.ReviewIdea(context.Background(), i.ID)
			if err != nil {
				t.Fatal(err)
			}
			if rv.Ready || len(rv.Outside) != 1 || !strings.Contains(rv.Outside[0], file) {
				t.Fatalf("%+v", rv)
			}
		})
	}
	t.Run("ignored dependency", func(t *testing.T) {
		// a task that leans on a file only the worktree has: what is read
		// is the commit
		dep := strings.Replace(grilledTask, "depends_on: []", `depends_on: ["TM-051"]`, 1)
		rg := startAgent(t, newRepo(t), "codex", "grill", nil, "FAKE_GRILL_FILE=tasks/TM-050-from-the-idea.md", "FAKE_GRILL_BODY="+dep)
		i, _ := grill(t, rg)
		rg.repo.Write(".git/info/exclude", "tasks/TM-051-*\n")
		testrepo.WriteIn(t, i.Worktree, "tasks/TM-051-ghost.md", testrepo.TaskFile(testrepo.Task{ID: "TM-051", Component: "docs", Touches: []string{"docs/g.md"}}))
		rv, err := rg.client.ReviewIdea(context.Background(), i.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rv.Ready || len(rv.Errors) == 0 {
			t.Fatalf("%+v", rv)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		bad := strings.Replace(grilledTask, "component: docs", "component: nowhere", 1)
		rg := startAgent(t, newRepo(t), "codex", "grill", nil, "FAKE_GRILL_FILE=tasks/TM-050-from-the-idea.md", "FAKE_GRILL_BODY="+bad)
		i, _ := grill(t, rg)
		rv, err := rg.client.ReviewIdea(context.Background(), i.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rv.Ready || len(rv.Errors) == 0 || !strings.Contains(rv.Why, "do not validate") {
			t.Fatalf("%+v", rv)
		}
	})
}

// A main that moved since the branch was made is merged into the branch
// by its session before anything is approved.
func TestIdeaApproveAfterMainMoved(t *testing.T) {
	repo := newRepo(t)
	rg := startGrill(t, repo)
	ctx := context.Background()
	i, s := grill(t, rg)
	repo.Write("docs/other.md", "meanwhile\n")
	repo.Commit("main moves on")
	rv, err := rg.client.ReviewIdea(ctx, i.ID)
	if err != nil || !rv.MainMoved || !rv.Ready {
		t.Fatalf("%+v %v", rv, err)
	}
	res, err := rg.client.Approve(ctx, i.ID, rv.Tip, rv.Main)
	if err != nil || !res.SentBack {
		t.Fatalf("%+v %v", res, err)
	}
	eventually(t, "brought up to date", func() bool { return strings.Contains(rg.feedText(s.ID), "merged main: <nil>") })
	eventually(t, "the session ends", func() bool {
		x, err := rg.client.ReviewIdea(ctx, i.ID)
		rv = x
		return err == nil && x.Ready && !x.MainMoved
	})
	res, err = rg.client.Approve(ctx, i.ID, rv.Tip, rv.Main)
	if err != nil || res.Merged == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(repo.Dir, "docs", "other.md")); err != nil {
		t.Fatal("main's own commit was lost")
	}
}

// The dispatcher never sees an idea's questions: they are the person's.
func TestIdeaQuestionsSkipTheDispatcher(t *testing.T) {
	rg := startWith(t, newRepo(t), "grill", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "agent", Harness: "codex", Command: fakeACP(t)})
		c.DefaultProfile = "agent"
		c.DispatcherProfile = "fake"
	}, func(s *serve.Service) {
		s.CodexLogin = func() (string, error) { return "Logged in using ChatGPT", nil }
	}, "FAKE_GRILL_FILE=tasks/TM-050-from-the-idea.md", "FAKE_GRILL_BODY="+grilledTask, `FAKE_DISPATCH={"questions":[{"level":"a","option":"Three tasks","reason":"x"}]}`)
	grill(t, rg) // checks the ask carries no dispatch and waits for the person
	if n := len(rg.dispatchCalls()); n != 0 {
		t.Fatalf("the dispatcher was called %d time(s)", n)
	}
}

func TestIdeaDrop(t *testing.T) {
	rg := startGrill(t, newRepo(t))
	ctx := context.Background()
	i, err := rg.client.AddIdea(ctx, "repo", "An idea", "text")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rg.client.Grill(ctx, i.ID, serve.GrillRequest{}); err != nil {
		t.Fatal(err)
	}
	rg.waitAsk()
	if _, err := rg.client.Grill(ctx, i.ID, serve.GrillRequest{}); err == nil {
		t.Fatal("grilled twice")
	}
	d, err := rg.client.DropIdea(ctx, i.ID)
	if err != nil || d.State != serve.IdeaDropped {
		t.Fatalf("%+v %v", d, err)
	}
	eventually(t, "the session stopped", func() bool { return len(rg.asks()) == 0 })
	if _, err := os.Stat(d.Worktree); err != nil {
		t.Fatal("the worktree went (A7)")
	}
	if _, err := rg.client.DropIdea(ctx, i.ID); err == nil {
		t.Fatal("dropped twice")
	}
	if _, err := rg.client.AddIdea(ctx, "nope", "x", "y"); err == nil {
		t.Fatal("an idea for an unknown project")
	}
}

// A main that took the intake's task id meanwhile: Approve sends the
// branch back, and once main is merged in the clash shows in validation.
func TestIdeaIDClashSendsBack(t *testing.T) {
	repo := newRepo(t)
	rg := startGrill(t, repo)
	ctx := context.Background()
	i, s := grill(t, rg)
	repo.AddTasks(testrepo.Task{ID: "TM-050", Title: "Taken on main"})
	rv, err := rg.client.ReviewIdea(ctx, i.ID)
	if err != nil || !rv.MainMoved || !rv.Ready || len(rv.Tasks) != 1 {
		t.Fatalf("%+v %v", rv, err)
	}
	res, err := rg.client.Approve(ctx, i.ID, rv.Tip, rv.Main)
	if err != nil || !res.SentBack {
		t.Fatalf("%+v %v", res, err)
	}
	eventually(t, "brought up to date", func() bool { return strings.Contains(rg.feedText(s.ID), "merged main: <nil>") })
	eventually(t, "the clash shows", func() bool {
		x, err := rg.client.ReviewIdea(ctx, i.ID)
		rv = x
		return err == nil && !x.MainMoved && len(x.Errors) > 0
	})
	if rv.Ready {
		t.Fatalf("%+v", rv)
	}
	if _, err := rg.client.Approve(ctx, i.ID, rv.Tip, rv.Main); err == nil {
		t.Fatal("approved a clash")
	}
}

// What the intake committed and removed again never reaches main.
func TestIdeaHistoryStaysOnItsBranch(t *testing.T) {
	repo := newRepo(t)
	rg := startGrill(t, repo)
	ctx := context.Background()
	i, _ := grill(t, rg)
	testrepo.WriteIn(t, i.Worktree, "tasks/transcript.txt", "the raw grilling\n")
	repo.GitIn(i.Worktree, "add", "-A")
	repo.GitIn(i.Worktree, "commit", "-q", "-m", "oops")
	repo.GitIn(i.Worktree, "rm", "-q", "tasks/transcript.txt")
	repo.GitIn(i.Worktree, "commit", "-q", "-m", "removed")
	rv, err := rg.client.ReviewIdea(ctx, i.ID)
	if err != nil || !rv.Ready {
		t.Fatalf("%+v %v", rv, err)
	}
	if _, err := rg.client.Approve(ctx, i.ID, rv.Tip, rv.Main); err != nil {
		t.Fatal(err)
	}
	if out := repo.Git("log", "--all-match", "--format=%s", "main"); strings.Contains(out, "oops") {
		t.Fatalf("the intake's commits reached main: %s", out)
	}
	if out, _ := exec.Command("git", "-C", repo.Dir, "log", "main", "--format=%H", "--", "tasks/transcript.txt").CombinedOutput(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the removed file is in main's history: %s", out)
	}
}

// What is validated is the commit Approve would land: a reference to an
// intake commit, which main never gets, does not pass.
func TestIdeaValidatesTheLandingCommit(t *testing.T) {
	repo := newRepo(t)
	rg := startGrill(t, repo)
	ctx := context.Background()
	i, _ := grill(t, rg)
	first := repo.GitIn(i.Worktree, "rev-parse", "HEAD")
	ar := strings.Replace(grilledTask, "depends_on: []", "depends_on: []\nafter_release: "+first, 1)
	testrepo.WriteIn(t, i.Worktree, "tasks/TM-050-from-the-idea.md", ar)
	repo.GitIn(i.Worktree, "commit", "-q", "-am", "after the intake's own commit")
	rv, err := rg.client.ReviewIdea(ctx, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Ready || len(rv.Errors) == 0 {
		t.Fatalf("%+v", rv)
	}
}

// An Approve cut off between moving the checkout and moving main is
// finished at the next start.
func TestIdeaApprovalCutOffIsFinished(t *testing.T) {
	repo := newRepo(t)
	rg := startGrill(t, repo)
	ctx := context.Background()
	i, _ := grill(t, rg)
	rv, err := rg.client.ReviewIdea(ctx, i.ID)
	if err != nil || !rv.Ready {
		t.Fatalf("%+v %v", rv, err)
	}
	commit := repo.Git("commit-tree", rv.Tip+"^{tree}", "-p", rv.Main, "-m", "Intake "+i.ID)
	rg.svc.SetApproving(i.ID, serve.Approval{Commit: commit, Tip: rv.Tip, Main: rv.Main, MainBranch: "main", Tasks: []string{"TM-050"}})
	repo.Git("read-tree", "-m", "-u", rv.Main, commit) // the crash comes here
	rg.stop()
	rg.open([]string{"FAKE_SCENARIO=wait", "FAKE_LOG=" + rg.log})
	x, err := rg.client.Idea(ctx, i.ID)
	if err != nil || x.State != serve.IdeaApproved || x.Merged != commit || x.Approving != nil {
		t.Fatalf("%+v %v", x, err)
	}
	if repo.Git("rev-parse", "main") != commit || repo.Git("status", "--porcelain") != "" {
		t.Fatal("main or the checkout is not the approved commit")
	}
}

// A claim on a task main added meanwhile does not keep the intake from
// being sent back.
func TestIdeaSendBackDespiteAClaimOnMain(t *testing.T) {
	repo := newRepo(t)
	rg := startGrill(t, repo)
	ctx := context.Background()
	i, _ := grill(t, rg)
	repo.AddTasks(testrepo.Task{ID: "TM-060", Title: "New on main"})
	wt := repo.Worktree("task/TM-060-x")
	repo.WriteState(testrepo.State{ID: "TM-060", Lifecycle: "in_progress", Worktree: wt, Branch: "task/TM-060-x", Base: repo.Git("rev-parse", "main")})
	rv, err := rg.client.ReviewIdea(ctx, i.ID)
	if err != nil || !rv.MainMoved || !rv.Ready {
		t.Fatalf("%+v %v", rv, err)
	}
	res, err := rg.client.Approve(ctx, i.ID, rv.Tip, rv.Main)
	if err != nil || !res.SentBack {
		t.Fatalf("%+v %v", res, err)
	}
}
