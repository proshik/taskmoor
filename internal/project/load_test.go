package project_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/testrepo"
)

func TestSourceMainVersusWorktree(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001", Title: "On main"})
	// an uncommitted edit and a new task in the working tree
	r.AddTask(testrepo.Task{ID: "TM-001", Title: "Edited"})
	r.AddTask(testrepo.Task{ID: "TM-002"})

	var l project.Loader
	main, err := l.Load(context.Background(), r.Dir, project.SourceMain)
	if err != nil {
		t.Fatal(err)
	}
	if len(main.Tasks) != 1 || main.Tasks[0].Title != "On main" {
		t.Fatalf("main source tasks %+v", main.Tasks)
	}
	wt, err := l.Load(context.Background(), r.Dir, project.SourceWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if len(wt.Tasks) != 2 || wt.Tasks[0].Title != "Edited" {
		t.Fatalf("worktree source tasks %+v", wt.Tasks)
	}
	if !wt.OnMain["TM-001-task.md"] || wt.OnMain["TM-002-task.md"] {
		t.Fatalf("OnMain %v", wt.OnMain)
	}
}

func TestMainSourceIsTheSameFromEveryWorktree(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	wt := r.Worktree("task/tm-002")
	testrepo.WriteIn(t, wt, "tasks/TM-002-task.md", testrepo.TaskFile(testrepo.Task{ID: "TM-002"}))
	r.CommitIn(wt, "add TM-002 on the branch")

	var l project.Loader
	snap, err := l.Load(context.Background(), wt, project.SourceMain)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tasks) != 1 || snap.Tasks[0].ID != "TM-001" {
		t.Fatalf("tasks from a worktree: %+v", snap.Tasks)
	}
	if snap.StateDir != r.StateDir() {
		t.Fatalf("state dir %s, want the common %s", snap.StateDir, r.StateDir())
	}
	if snap.Name != "repo" {
		t.Fatalf("name %q", snap.Name)
	}
}

func TestReadmeAndOtherFilesAreNotTasks(t *testing.T) {
	r := testrepo.New(t)
	r.Write("tasks/README.md", "# Notes\n")
	r.Write("tasks/task.schema.json", "{}")
	r.Write("tasks/sub/TM-009-x.md", "not read")
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	var l project.Loader
	snap, err := l.Load(context.Background(), r.Dir, project.SourceMain)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tasks) != 1 || len(snap.Findings) != 0 {
		t.Fatalf("tasks %d, findings %v", len(snap.Tasks), snap.Findings)
	}
}

func TestLoadErrors(t *testing.T) {
	var l project.Loader
	ctx := context.Background()
	var le *project.LoadError

	if _, err := l.Load(ctx, t.TempDir(), project.SourceMain); !errors.As(err, &le) {
		t.Fatalf("not a repository: %v", err)
	}

	r := testrepo.New(t)
	r.Write("tasks/tasks.toml", strings.Replace(testrepo.DefaultConfig, `main_branch = "main"`, `main_branch = "trunk"`, 1))
	r.Commit("rename main")
	_, err := l.Load(ctx, r.Dir, project.SourceMain)
	if !errors.As(err, &le) || !strings.Contains(err.Error(), "main branch trunk does not exist") {
		t.Fatalf("missing main branch: %v", err)
	}
	// the worktree source still loads and reports it
	snap, err := l.Load(ctx, r.Dir, project.SourceWorktree)
	if err != nil {
		t.Fatal(err)
	}
	one(t, snap.Findings, "main branch trunk does not exist")
}

func TestMainBranchMustBeAnExactBranch(t *testing.T) {
	r := testrepo.New(t)
	r.Write("tasks/tasks.toml", strings.Replace(testrepo.DefaultConfig, `main_branch = "main"`, `main_branch = "main~1"`, 1))
	r.Commit("point at a revision")
	var l project.Loader
	if _, err := l.Load(context.Background(), r.Dir, project.SourceMain); err == nil || !strings.Contains(err.Error(), "main branch main~1 does not exist") {
		t.Fatalf("err %v", err)
	}
}

func TestOversizedTaskFiles(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001", Body: strings.Repeat("x", 1<<20+1)})
	var l project.Loader
	for _, src := range []project.Source{project.SourceMain, project.SourceWorktree} {
		_, err := l.Load(context.Background(), r.Dir, src)
		var le *project.LoadError
		if !errors.As(err, &le) || !strings.Contains(err.Error(), "more than") {
			t.Errorf("%s: %v", src, err)
		}
	}
}

func TestBrokenConfigStillValidatesTheWorktree(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001", DependsOn: []string{"TM-009"}})
	r.Write("tasks/tasks.toml", "format = 1\n[routing]\nfix = true\n")
	var l project.Loader
	snap, err := l.Load(context.Background(), r.Dir, project.SourceWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.ConfigBroken || len(snap.Tasks) != 1 {
		t.Fatalf("broken %v tasks %d", snap.ConfigBroken, len(snap.Tasks))
	}
	one(t, snap.Findings, "cannot parse")
	// the main source still reads the committed tasks.toml
	if _, err := l.Load(context.Background(), r.Dir, project.SourceMain); err != nil {
		t.Fatalf("main source with a broken working-tree tasks.toml: %v", err)
	}
}

func TestWorkingTreeCannotRedirectTheMainBranch(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001", NeedsPerson: true})
	r.Git("branch", "other")
	// the working tree names "other"; other's own tasks.toml still says main
	r.Write("tasks/tasks.toml", strings.Replace(testrepo.DefaultConfig, `main_branch = "main"`, `main_branch = "other"`, 1))
	var l project.Loader
	_, err := l.Load(context.Background(), r.Dir, project.SourceMain)
	if err == nil || !strings.Contains(err.Error(), "names main branch other, but other's own tasks/tasks.toml names main") {
		t.Fatalf("err %v", err)
	}
}

func TestTooManyTouches(t *testing.T) {
	var ts []string
	for i := 0; i <= project.MaxTouches; i++ {
		ts = append(ts, fmt.Sprintf("src/f%d", i))
	}
	tk, fs := parse(t, "TM-001-task.md", testrepo.TaskFile(testrepo.Task{ID: "TM-001", Touches: ts}))
	if tk != nil {
		t.Fatal("parsed")
	}
	one(t, fs, "a reader takes at most 1000")
}

func TestSymlinkedConfigInTheWorkingTree(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	r.Write("other.toml", testrepo.DefaultConfig)
	r.Remove("tasks/tasks.toml")
	if err := os.Symlink("../other.toml", filepath.Join(r.Dir, "tasks", "tasks.toml")); err != nil {
		t.Fatal(err)
	}
	var l project.Loader
	snap, err := l.Load(context.Background(), r.Dir, project.SourceWorktree)
	if err != nil {
		t.Fatal(err)
	}
	one(t, snap.Findings, "cannot read")
	if _, err := l.Load(context.Background(), r.Dir, project.SourceMain); err != nil {
		t.Fatalf("main source: %v", err)
	}
}
