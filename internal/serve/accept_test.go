package serve_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

// startReview runs codex-fake sessions ("work": they finish at once) with
// the fake revust answering FAKE_REVUST.
func startReview(t *testing.T, repo *testrepo.Repo, revust string) *rig {
	t.Helper()
	return startAgent(t, repo, "codex", "work", nil, "FAKE_REVUST="+revust)
}

func (rg *rig) revustRuns() []string {
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
			V    struct{ Scope string }
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Kind == "revust" {
			out = append(out, e.V.Scope)
		}
	}
	return out
}

func (rg *rig) reviewState(id string) *serve.Review { return rg.session(id).Review }

// The agent's work: a commit on the task branch.
func work(t *testing.T, repo *testrepo.Repo, s *serve.Session, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Worktree, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	repo.CommitIn(s.Worktree, "Add "+file)
}

func TestCleanReviewThenAccept(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 1}"})
	rg := startReview(t, repo, "clean")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	work(t, repo, s, "greeting.txt", "hello\n")
	rg.waitState(s.ID, serve.StateComplete)
	eventually(t, "the review", func() bool { rv := rg.reviewState(s.ID); return rv != nil && rv.State == "clean" })
	if rv := rg.reviewState(s.ID); rv.Done != 1 || rv.Rounds != 1 {
		t.Fatalf("%+v", rv)
	}
	if runs := rg.revustRuns(); len(runs) != 1 || !strings.Contains(runs[0], "task/TM-001") || !strings.Contains(runs[0], "git diff") {
		t.Fatalf("%q", runs)
	}
	a, err := rg.client.Acceptance(ctx, s.ID)
	if err != nil || !a.Ready || len(a.Commits) != 1 || !strings.Contains(a.Diff, "+hello") || !strings.Contains(a.Stat, "greeting.txt") {
		t.Fatalf("%+v %v", a, err)
	}
	res, err := acceptNow(rg, s.ID)
	if err != nil || res.Merged == "" {
		t.Fatalf("%+v %v", res, err)
	}
	// the merge commit has the trailer on main's first-parent history; the
	// task is merged, its state file gone, the worktree and branch kept
	msg := repo.Git("log", "-1", "--format=%B", "main")
	if !strings.Contains(msg, "Task: TM-001") || !strings.Contains(repo.Git("log", "-1", "--format=%P", "main"), " ") {
		t.Fatalf("%q", msg)
	}
	if _, err := os.Stat(filepath.Join(repo.StateDir(), "TM-001.json")); !os.IsNotExist(err) {
		t.Fatal("the state file stayed")
	}
	if _, err := os.Stat(s.Worktree); err != nil {
		t.Fatal("the worktree went")
	}
	eventually(t, "merged", func() bool {
		snap, _ := rg.client.Snapshot(ctx)
		return snap.Projects[0].Tasks[0].Lifecycle == "merged"
	})
	if got := rg.session(s.ID); got.State != serve.StateAccepted || got.Merged != res.Merged {
		t.Fatalf("%+v", got)
	}
	if _, err := acceptNow(rg, s.ID); err == nil {
		t.Fatal("accepted twice")
	}
}

// Findings go back into the session, round after round; the last round's
// fixes are marked unreviewed.
func TestReviewRoundsSendFindingsBack(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 3}"}) // capped at 2
	rg := startReview(t, repo, "findings")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	eventually(t, "two rounds and the last fixes", func() bool {
		rv := rg.reviewState(s.ID)
		return rv != nil && rv.Done == 2 && rv.Unreviewed && rg.session(s.ID).State == serve.StateComplete
	})
	lines := feedTexts(rg, s.ID)
	if !hasLine(lines, "resumed with: Code review round 1 of 2") || !hasLine(lines, "resumed with: Code review round 2 of 2") || !hasLine(lines, "The greeting is wrong") {
		t.Fatalf("%q", lines)
	}
	if rv := rg.reviewState(s.ID); rv.Rounds != 2 || rv.Findings != 1 || len(rg.revustRuns()) != 2 {
		t.Fatalf("%+v %d", rv, len(rg.revustRuns()))
	}
}

func TestNoReviewBudgetGoesStraightToAcceptance(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	rg := startReview(t, repo, "findings")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitState(s.ID, serve.StateComplete)
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	if a, err := rg.client.Acceptance(context.Background(), s.ID); err != nil || !a.Ready || a.Review != nil {
		t.Fatalf("%+v %v", a, err)
	}
	if len(rg.revustRuns()) != 0 {
		t.Fatal("revust ran")
	}
}

func TestAcceptWithConflictsSendsBack(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	rg := startReview(t, repo, "clean")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	work(t, repo, s, "greeting.txt", "hello\n")
	rg.waitState(s.ID, serve.StateComplete)
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	repo.Write("greeting.txt", "hi\n") // main moved on the same file
	repo.Commit("Greeting on main")
	before := repo.Git("rev-parse", "main")
	res, err := acceptNow(rg, s.ID)
	if err != nil || !res.Conflicted || len(res.Conflicts) != 1 || res.Conflicts[0] != "greeting.txt" {
		t.Fatalf("%+v %v", res, err)
	}
	if repo.Git("rev-parse", "main") != before || repo.Git("status", "--porcelain") != "" {
		t.Fatal("the trial merge touched main or the checkout")
	}
	eventually(t, "sent back", func() bool { return hasLine(feedTexts(rg, s.ID), "resumed with: Accept found") })
}

func TestAcceptRefusesTheOtherBranchAndDirtyFiles(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	rg := startReview(t, repo, "clean")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	work(t, repo, s, "greeting.txt", "hello\n")
	rg.waitState(s.ID, serve.StateComplete)
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	// the person's untracked file of the same name: refused, nothing changes
	repo.Write("greeting.txt", "mine\n")
	if _, err := acceptNow(rg, s.ID); err == nil || !strings.Contains(err.Error(), "does not track") {
		t.Fatalf("%v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo.Dir, "greeting.txt")); string(data) != "mine\n" {
		t.Fatalf("the person's file changed: %q", data)
	}
	os.Remove(filepath.Join(repo.Dir, "greeting.txt"))
	// the checkout on another branch: refused, never switched
	repo.Git("switch", "-q", "-c", "elsewhere")
	if _, err := acceptNow(rg, s.ID); err == nil || !strings.Contains(err.Error(), "never switches") {
		t.Fatalf("%v", err)
	}
	repo.Git("switch", "-q", "main")
	if res, err := acceptNow(rg, s.ID); err != nil || res.Merged == "" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAcceptRefusedWhileWorking(t *testing.T) {
	repo := newRepo(t)
	rg := startAgent(t, repo, "codex", "wait", nil)
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	eventually(t, "active", func() bool { return rg.session(s.ID).State == serve.StateActive })
	if _, err := acceptNow(rg, s.ID); err == nil || !strings.Contains(err.Error(), "still working") {
		t.Fatalf("%v", err)
	}
}

func reviewReady(t *testing.T, rg *rig, repo *testrepo.Repo) *serve.Session {
	t.Helper()
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	work(t, repo, s, "greeting.txt", "hello\n")
	rg.waitState(s.ID, serve.StateComplete)
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	return s
}

// An ignored file in the person's checkout is never written over.
func TestAcceptKeepsIgnoredFiles(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	repo.Write(".gitignore", "greeting.txt\n")
	repo.Commit("Ignore greetings")
	rg := startReview(t, repo, "clean")
	s := reviewReady(t, rg, repo)
	repo.GitIn(s.Worktree, "add", "-f", "greeting.txt") // the agent tracks it on its branch
	repo.CommitIn(s.Worktree, "Track greeting")
	repo.Write("greeting.txt", "local data\n") // ignored here
	if _, err := acceptNow(rg, s.ID); err == nil || !strings.Contains(err.Error(), "does not track") {
		t.Fatalf("%v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo.Dir, "greeting.txt")); string(data) != "local data\n" {
		t.Fatalf("%q", data)
	}
}

// The person's own merge in progress is never aborted.
func TestAcceptLeavesThePersonsMerge(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	rg := startReview(t, repo, "clean")
	s := reviewReady(t, rg, repo)
	repo.Git("switch", "-q", "-c", "side")
	repo.Write("x.txt", "side\n")
	repo.Commit("side")
	repo.Git("switch", "-q", "main")
	repo.Write("x.txt", "main\n")
	repo.Commit("main x")
	_, _ = gitTry(repo, "merge", "side") // conflicts: the person's merge in progress
	repo.Write("x.txt", "resolved\n")
	repo.Git("add", "x.txt")
	if _, err := acceptNow(rg, s.ID); err == nil || !strings.Contains(err.Error(), "middle of a merge") {
		t.Fatalf("%v", err)
	}
	if _, err := gitTry(repo, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err != nil {
		t.Fatal("the person's merge was aborted")
	}
	if st := repo.Git("diff", "--cached", "--name-only"); !strings.Contains(st, "x.txt") {
		t.Fatalf("the staged resolution went: %q", st)
	}
}

// A branch that moved since it was shown is not merged.
func TestAcceptMergesWhatWasShown(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	rg := startReview(t, repo, "clean")
	s := reviewReady(t, rg, repo)
	ctx := context.Background()
	a, _ := rg.client.Acceptance(ctx, s.ID)
	work(t, repo, s, "later.txt", "x\n")
	if _, err := rg.client.Accept(ctx, s.ID, a.Tip); err == nil || !strings.Contains(err.Error(), "moved since you looked") {
		t.Fatalf("%v", err)
	}
	a2, _ := rg.client.Acceptance(ctx, s.ID)
	res, err := rg.client.Accept(ctx, s.ID, a2.Tip)
	if err != nil || repo.Git("rev-parse", res.Merged+"^2") != a2.Tip {
		t.Fatalf("%+v %v", res, err)
	}
}

// merge.log and merge options of the person's config do not hide or skip
// the trailer.
func TestAcceptTrailerSurvivesTheConfig(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	repo.Git("config", "merge.log", "true")
	repo.Git("config", "branch.main.mergeOptions", "--no-commit")
	rg := startReview(t, repo, "clean")
	s := reviewReady(t, rg, repo)
	res, err := acceptNow(rg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	trailers := gitInput(repo, repo.Git("log", "-1", "--format=%B", res.Merged), "interpret-trailers", "--parse")
	if !strings.Contains(trailers, "Task: TM-001") {
		t.Fatalf("%q", trailers)
	}
	eventually(t, "merged", func() bool {
		snap, _ := rg.client.Snapshot(context.Background())
		return snap.Projects[0].Tasks[0].Lifecycle == "merged"
	})
}

// The review budget goes with the claim through Restart.
func TestRestartKeepsTheReviewBudget(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 1}"})
	rg := startReview(t, repo, "clean")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	eventually(t, "the review", func() bool { rv := rg.reviewState(s.ID); return rv != nil && rv.State == "clean" })
	nu, err := rg.client.Restart(ctx, s.ID, "agent")
	if err != nil {
		t.Fatal(err)
	}
	rg.waitState(nu.ID, serve.StateComplete)
	eventually(t, "the successor's review record", func() bool { rv := rg.reviewState(nu.ID); return rv != nil && rv.Done == 1 })
	if n := len(rg.revustRuns()); n != 1 {
		t.Fatalf("%d review rounds after a restart", n)
	}
}

func gitTry(repo *testrepo.Repo, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", repo.Dir}, args...)...).CombinedOutput()
	return string(out), err
}

func gitInput(repo *testrepo.Repo, in string, args ...string) string {
	c := exec.Command("git", append([]string{"-C", repo.Dir}, args...)...)
	c.Stdin = strings.NewReader(in)
	out, _ := c.Output()
	return string(out)
}

// acceptNow accepts what the acceptance view shows now.
func acceptNow(rg *rig, id string) (*serve.AcceptResult, error) {
	a, err := rg.client.Acceptance(context.Background(), id)
	if err != nil {
		return nil, err
	}
	return rg.client.Accept(context.Background(), id, a.Tip)
}

// Every way a merge can land on an untracked or ignored file refuses it.
func TestAcceptRefusesEveryWayOverIgnoredFiles(t *testing.T) {
	for name, c := range map[string]struct {
		ignore, local string // .gitignore line, the person's ignored file
		branch        func(wt string)
	}{
		"file where a directory goes": {"settings", "settings", func(wt string) {
			_ = os.MkdirAll(filepath.Join(wt, "settings"), 0o755)
			_ = os.WriteFile(filepath.Join(wt, "settings", "default.toml"), []byte("x\n"), 0o644)
		}},
		"ignored content of a directory that becomes a file": {"cache/local.db", "cache/local.db", func(wt string) {
			_ = os.RemoveAll(filepath.Join(wt, "cache"))
			_ = os.WriteFile(filepath.Join(wt, "cache"), []byte("now a file\n"), 0o644)
		}},
		"a literal name with pathspec characters": {"draft*", "draft[1].txt", func(wt string) {
			_ = os.WriteFile(filepath.Join(wt, "draft[1].txt"), []byte("task\n"), 0o644)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
			repo.Write(".gitignore", c.ignore+"\n")
			repo.Write("cache/README", "r\n")
			repo.Write("draft1.txt", "tracked\n")
			repo.Commit("Base")
			rg := startReview(t, repo, "clean")
			s := reviewReady(t, rg, repo)
			c.branch(s.Worktree)
			repo.GitIn(s.Worktree, "add", "-A", "-f")
			repo.CommitIn(s.Worktree, "Task change")
			_ = os.MkdirAll(filepath.Dir(filepath.Join(repo.Dir, c.local)), 0o755)
			_ = os.WriteFile(filepath.Join(repo.Dir, c.local), []byte("local data\n"), 0o644)
			before := repo.Git("rev-parse", "main")
			if _, err := acceptNow(rg, s.ID); err == nil || !strings.Contains(err.Error(), "does not track") {
				t.Fatalf("%v", err)
			}
			if data, _ := os.ReadFile(filepath.Join(repo.Dir, c.local)); string(data) != "local data\n" || repo.Git("rev-parse", "main") != before {
				t.Fatalf("changed: %q", data)
			}
		})
	}
}

// The person's merge strategy options cannot drop the task's changes, and
// the trailer is exactly the task's.
func TestAcceptIgnoresTheMergeStrategyConfig(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	repo.Git("config", "branch.main.mergeOptions", "-s ours")
	rg := startReview(t, repo, "clean")
	s := reviewReady(t, rg, repo)
	res, err := acceptNow(rg, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := repo.Git("show", res.Merged+":greeting.txt"); got != "hello" {
		t.Fatalf("the merge lost the task's change: %q", got)
	}
	if data, _ := os.ReadFile(filepath.Join(repo.Dir, "greeting.txt")); string(data) != "hello\n" || repo.Git("status", "--porcelain") != "" {
		t.Fatalf("the checkout: %q %q", data, repo.Git("status", "--porcelain"))
	}
}

func TestAcceptNeedsTheTip(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	rg := startReview(t, repo, "clean")
	s := reviewReady(t, rg, repo)
	if _, err := rg.client.Accept(context.Background(), s.ID, ""); err == nil || !strings.Contains(err.Error(), "needs the commit") {
		t.Fatalf("%v", err)
	}
}
