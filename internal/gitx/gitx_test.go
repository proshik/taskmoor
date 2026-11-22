package gitx_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proshik/taskmoor/internal/gitx"
	"github.com/proshik/taskmoor/internal/testrepo"
)

func TestPathsKeepTrailingSpaces(t *testing.T) {
	testrepo.Isolate(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(base, "repo ")
	for _, d := range []string{dir, filepath.Join(base, "repo")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := (gitx.Repo{Dir: d}).Run(context.Background(), "init", "-q"); err != nil {
			t.Fatal(err)
		}
	}
	top, err := gitx.Toplevel(context.Background(), dir)
	if err != nil || top != dir {
		t.Fatalf("toplevel %q %v, want %q", top, err, dir)
	}
	common, err := gitx.Repo{Dir: dir}.CommonDir(context.Background())
	if err != nil || common != filepath.Join(dir, ".git") {
		t.Fatalf("common dir %q %v", common, err)
	}
}

func TestResolveBranchIsExact(t *testing.T) {
	r := testrepo.New(t)
	r.SquashTask("second")
	repo := gitx.Repo{Dir: r.Dir}
	ctx := context.Background()
	if _, ok, err := repo.ResolveBranch(ctx, "main"); !ok || err != nil {
		t.Fatalf("main: %v %v", ok, err)
	}
	for _, name := range []string{"main~1", "main^", "HEAD", "nope", "-x"} {
		if h, ok, err := repo.ResolveBranch(ctx, name); ok || err != nil {
			t.Errorf("%s resolved to %s (%v)", name, h, err)
		}
	}
}

func TestCatBlobsRefusesOversizedObjects(t *testing.T) {
	r := testrepo.New(t)
	r.Write("big.txt", strings.Repeat("x", gitx.MaxBlob+1))
	r.Commit("big")
	repo := gitx.Repo{Dir: r.Dir}
	hash := r.Git("rev-parse", "HEAD:big.txt")
	_, err := repo.CatBlobs(context.Background(), []string{hash})
	var tb *gitx.TooBigError
	if !errors.As(err, &tb) {
		t.Fatalf("err %v", err)
	}
	small := r.Git("rev-parse", "HEAD:tasks/tasks.toml")
	blobs, err := repo.CatBlobs(context.Background(), []string{small, small})
	if err != nil || len(blobs[small]) == 0 {
		t.Fatalf("%v", err)
	}
}

func TestWorktreesMarkPrunable(t *testing.T) {
	r := testrepo.New(t)
	wt := r.Worktree("task/x")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	list, err := gitx.Repo{Dir: r.Dir}.Worktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		if w.Path == wt && !w.Prunable {
			t.Fatalf("%+v not prunable", w)
		}
	}
}

func TestPathsKeepTrailingCarriageReturn(t *testing.T) {
	testrepo.Isolate(t)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(base, "repo\r")
	for _, d := range []string{dir, filepath.Join(base, "repo")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := (gitx.Repo{Dir: d}).Run(context.Background(), "init", "-q"); err != nil {
			t.Fatal(err)
		}
	}
	if top, err := gitx.Toplevel(context.Background(), dir); err != nil || top != dir {
		t.Fatalf("toplevel %q %v", top, err)
	}
}

func TestOuterGitDirIsIgnored(t *testing.T) {
	a := testrepo.New(t)
	b := testrepo.New(t)
	t.Setenv("GIT_DIR", filepath.Join(a.Dir, ".git"))
	t.Setenv("GIT_WORK_TREE", a.Dir)
	if top, err := gitx.Toplevel(context.Background(), b.Dir); err != nil || top != b.Dir {
		t.Fatalf("toplevel %q %v, want %q", top, err, b.Dir)
	}
}

func TestShowFileRefusesSymlinks(t *testing.T) {
	r := testrepo.New(t)
	if err := os.Symlink("../README.md", filepath.Join(r.Dir, "tasks", "link.toml")); err != nil {
		t.Fatal(err)
	}
	r.Commit("link")
	if _, _, err := (gitx.Repo{Dir: r.Dir}).ShowFile(context.Background(), "HEAD", "tasks/link.toml"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err %v", err)
	}
}
