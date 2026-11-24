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

// accepted runs TM-001 to an accepted session with one commit of work.
func accepted(t *testing.T) (*rig, *testrepo.Repo, *serve.Session) {
	t.Helper()
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 0}"})
	rg := startReview(t, repo, "clean")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitState(s.ID, serve.StateComplete)
	work(t, repo, s, "a.txt", "a\n")
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	if _, err := acceptNow(rg, s.ID); err != nil {
		t.Fatal(err)
	}
	return rg, repo, s
}

func branchExists(repo *testrepo.Repo, b string) bool {
	return exec.Command("git", "-C", repo.Dir, "rev-parse", "-q", "--verify", "refs/heads/"+b).Run() == nil
}

func cleanOne(t *testing.T, rg *rig, id string) serve.CleanupItem {
	t.Helper()
	items, err := rg.client.Cleanup(context.Background(), id, "", false, false)
	if err != nil || len(items) != 1 {
		t.Fatalf("%+v %v", items, err)
	}
	return items[0]
}

func TestCleanupAnAcceptedTask(t *testing.T) {
	rg, repo, s := accepted(t)
	ctx := context.Background()
	// neither one nor all: refused (nothing malformed selects everything)
	if _, err := rg.client.Cleanup(ctx, "", "", false, false); err == nil {
		t.Fatal("cleaned up without being told what")
	}
	// a dry run moves nothing
	items, err := rg.client.Cleanup(ctx, "", "", true, true)
	if err != nil || len(items) != 1 || !items[0].Removed || items[0].Trash == "" {
		t.Fatalf("%+v %v", items, err)
	}
	if _, err := os.Stat(s.Worktree); err != nil || rg.session(s.ID).CleanedUp {
		t.Fatal("a dry run changed something")
	}
	// an uncommitted change keeps it
	testrepo.WriteIn(t, s.Worktree, "scratch.txt", "mine\n")
	if it := cleanOne(t, rg, "TM-001"); it.Removed || !strings.Contains(it.Kept, "scratch.txt") {
		t.Fatalf("%+v", it)
	}
	os.Remove(filepath.Join(s.Worktree, "scratch.txt"))
	// ignored files go with the worktree, into the trash: nothing deleted
	repo.Write(".git/info/exclude", "build/\n")
	testrepo.WriteIn(t, s.Worktree, "build/out.bin", "x")
	it := cleanOne(t, rg, s.ID)
	if !it.Removed || it.Kept != "" || it.Trash == "" {
		t.Fatalf("%+v", it)
	}
	if _, err := os.Stat(s.Worktree); !os.IsNotExist(err) || branchExists(repo, s.Branch) {
		t.Fatal("the worktree or the branch stayed")
	}
	if data, err := os.ReadFile(filepath.Join(it.Trash, "build", "out.bin")); err != nil || string(data) != "x" {
		t.Fatal("the ignored file is not in the trash")
	}
	if out := repo.Git("worktree", "list"); strings.Contains(out, s.Worktree) {
		t.Fatalf("git still lists it: %s", out)
	}
	if !strings.Contains(rg.feedText(s.ID), "cleaned up") || !rg.session(s.ID).CleanedUp {
		t.Fatal("not recorded")
	}
	if _, err := rg.client.Cleanup(ctx, s.ID, "", false, false); err == nil {
		t.Fatal("cleaned up twice")
	}
	if items, _ := rg.client.Cleanup(ctx, "", "", true, false); len(items) != 0 {
		t.Fatalf("%+v", items)
	}
	// emptying the trash is its own request
	gone, err := rg.client.EmptyTrash(ctx, "")
	if err != nil || len(gone) != 1 {
		t.Fatalf("%v %v", gone, err)
	}
	if _, err := os.Stat(it.Trash); !os.IsNotExist(err) {
		t.Fatal("the trash was not emptied")
	}
}

// A branch with commits that are not in main stays, with its worktree.
func TestCleanupKeepsUnmergedWork(t *testing.T) {
	rg, repo, s := accepted(t)
	repo.GitIn(s.Worktree, "commit", "-q", "--allow-empty", "-m", "after the merge")
	if it := cleanOne(t, rg, s.ID); it.Removed || !strings.Contains(it.Kept, "not in main") {
		t.Fatalf("%+v", it)
	}
	if _, err := os.Stat(s.Worktree); err != nil || !branchExists(repo, s.Branch) {
		t.Fatal("unmerged work was removed")
	}
	if _, err := rg.client.Cleanup(context.Background(), "nope", "", false, false); err == nil {
		t.Fatal("an unknown id")
	}
}

// What git does not show is not lost: it is in the trash.
func TestCleanupMovesWhatGitDoesNotShow(t *testing.T) {
	rg, repo, s := accepted(t)
	inner := filepath.Join(s.Worktree, "a.d")
	os.MkdirAll(inner, 0o755)
	repo.GitIn(s.Worktree, "init", "-q", "a.d")
	testrepo.WriteIn(t, inner, "only-here.txt", "x")
	repo.GitIn(inner, "add", "-A")
	repo.GitIn(inner, "commit", "-q", "-m", "only here")
	repo.Write(".git/info/exclude", "a.d/\n")
	repo.GitIn(s.Worktree, "update-index", "--assume-unchanged", "a.txt")
	testrepo.WriteIn(t, s.Worktree, "a.txt", "an edit git does not see\n")
	it := cleanOne(t, rg, s.ID)
	if !it.Removed || it.Trash == "" {
		t.Fatalf("%+v", it)
	}
	if out, err := exec.Command("git", "-C", filepath.Join(it.Trash, "a.d"), "log", "--format=%s").Output(); err != nil || !strings.Contains(string(out), "only here") {
		t.Fatalf("the nested repository is lost: %s %v", out, err)
	}
	if data, _ := os.ReadFile(filepath.Join(it.Trash, "a.txt")); string(data) != "an edit git does not see\n" {
		t.Fatalf("the hidden edit is lost: %q", data)
	}
}

// A task ref turned into a symbolic ref to main, a worktree moved
// elsewhere, a second checkout of the branch: kept; main never touched.
func TestCleanupRefusesRefTricks(t *testing.T) {
	t.Run("symbolic ref", func(t *testing.T) {
		rg, repo, s := accepted(t)
		repo.Git("worktree", "remove", s.Worktree)
		repo.Git("symbolic-ref", "refs/heads/"+s.Branch, "refs/heads/main")
		main := repo.Git("rev-parse", "main")
		if it := cleanOne(t, rg, s.ID); it.Removed || !strings.Contains(it.Kept, "symbolic") || repo.Git("rev-parse", "main") != main {
			t.Fatalf("%+v", it)
		}
	})
	t.Run("moved worktree", func(t *testing.T) {
		rg, repo, s := accepted(t)
		repo.Git("worktree", "move", s.Worktree, s.Worktree+"-moved")
		if it := cleanOne(t, rg, s.ID); it.Removed || !strings.Contains(it.Kept, "checked out at") || !branchExists(repo, s.Branch) {
			t.Fatalf("%+v", it)
		}
	})
	t.Run("second checkout", func(t *testing.T) {
		rg, repo, s := accepted(t)
		repo.Git("worktree", "add", "-q", "--force", s.Worktree+"-two", s.Branch)
		if it := cleanOne(t, rg, s.ID); it.Removed || !strings.Contains(it.Kept, "too") {
			t.Fatalf("%+v", it)
		}
	})
}

// A branch that could not be deleted is tried again; a dry run changes
// nothing, also when both are gone already.
func TestCleanupBranchRetryAndDryRun(t *testing.T) {
	rg, repo, s := accepted(t)
	ctx := context.Background()
	lock := filepath.Join(repo.Dir, ".git", "refs", "heads", filepath.FromSlash(s.Branch)+".lock")
	os.MkdirAll(filepath.Dir(lock), 0o755)
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	it := cleanOne(t, rg, s.ID)
	if it.Removed || !it.WorktreeRemoved || !branchExists(repo, s.Branch) || rg.session(s.ID).CleanedUp {
		t.Fatalf("%+v", it)
	}
	os.Remove(lock)
	if it := cleanOne(t, rg, s.ID); !it.Removed || branchExists(repo, s.Branch) || !rg.session(s.ID).CleanedUp {
		t.Fatalf("%+v", it)
	}
	// both gone by hand: a dry run says so and changes nothing
	rg2, repo2, s2 := accepted(t)
	repo2.Git("worktree", "remove", s2.Worktree)
	repo2.Git("branch", "-D", s2.Branch)
	items, err := rg2.client.Cleanup(ctx, "", "", true, true)
	if err != nil || len(items) != 1 || !items[0].Removed || rg2.session(s2.ID).CleanedUp {
		t.Fatalf("%+v %v", items, err)
	}
	if it := cleanOne(t, rg2, s2.ID); !it.Removed || !rg2.session(s2.ID).CleanedUp {
		t.Fatalf("%+v", it)
	}
}

// An approved idea's intake goes too: Approve landed its tree.
func TestCleanupAnApprovedIdea(t *testing.T) {
	repo := newRepo(t)
	rg := startGrill(t, repo)
	ctx := context.Background()
	i, s := grill(t, rg)
	rv, err := rg.client.ReviewIdea(ctx, i.ID)
	if err != nil || !rv.Ready {
		t.Fatalf("%+v %v", rv, err)
	}
	if _, err := rg.client.Approve(ctx, i.ID, rv.Tip, rv.Main); err != nil {
		t.Fatal(err)
	}
	if it := cleanOne(t, rg, i.ID); !it.Removed {
		t.Fatalf("%+v", it)
	}
	if _, err := os.Stat(i.Worktree); !os.IsNotExist(err) || branchExists(repo, i.Branch) {
		t.Fatal("the intake stayed")
	}
	x, _ := rg.client.Idea(ctx, i.ID)
	if !x.CleanedUp || x.ApprovedTip != rv.Tip || !rg.session(s.ID).CleanedUp || !strings.Contains(rg.feedText(s.ID), "cleaned up") {
		t.Fatalf("%+v", x)
	}
}
