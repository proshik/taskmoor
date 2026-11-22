package validate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/proshik/taskmoor/internal/derive"
	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/testrepo"
	"github.com/proshik/taskmoor/internal/validate"
)

func run(t *testing.T, r *testrepo.Repo) []finding.Finding {
	t.Helper()
	var l project.Loader
	snap, err := l.Load(context.Background(), r.Dir, project.SourceWorktree)
	if err != nil {
		t.Fatal(err)
	}
	return validate.Run(snap, derive.Derive(snap))
}

func find(fs []finding.Finding, sev finding.Severity, want string) *finding.Finding {
	for i, f := range fs {
		if f.Severity == sev && strings.Contains(f.String(), want) {
			return &fs[i]
		}
	}
	return nil
}

func expect(t *testing.T, fs []finding.Finding, sev finding.Severity, want string) {
	t.Helper()
	if find(fs, sev, want) == nil {
		t.Errorf("no %s containing %q in:\n%s", sev, want, render(fs))
	}
}

func render(fs []finding.Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  " + f.String() + "\n")
	}
	return b.String()
}

func TestCleanProjectHasNoFindings(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"}, testrepo.Task{ID: "TM-002", DependsOn: []string{"TM-001"}})
	if fs := run(t, r); len(fs) != 0 {
		t.Fatalf("findings:\n%s", render(fs))
	}
}

func TestTaskErrors(t *testing.T) {
	r := testrepo.New(t)
	base := r.Git("rev-parse", "HEAD")
	r.Git("checkout", "-q", "-b", "side")
	side := r.SquashTask("side only")
	r.Git("checkout", "-q", "main")
	r.AddTasks(
		testrepo.Task{ID: "TM-001", Component: "nope"},
		testrepo.Task{ID: "TM-002", DependsOn: []string{"TM-002"}},
		testrepo.Task{ID: "TM-003", DependsOn: []string{"TM-099"}},
		testrepo.Task{ID: "TM-004", DependsOn: []string{"TM-005"}},
		testrepo.Task{ID: "TM-005", DependsOn: []string{"TM-004"}},
		testrepo.Task{ID: "TM-006", AfterRelease: "abcdef12"},
		testrepo.Task{ID: "TM-007", AfterRelease: side[:10]},
		testrepo.Task{ID: "TM-008", AfterRelease: base[:10]},
		testrepo.Task{ID: "XX-009"},
	)
	r.Write("tasks/TM-010-dup.md", testrepo.TaskFile(testrepo.Task{ID: "TM-001"}))
	r.Write("tasks/TM-011-date.md", strings.Replace(testrepo.TaskFile(testrepo.Task{ID: "TM-011"}), `created: "2026-11-07"`, "created: 2026-11-07", 1))
	r.Write("tasks/bad name.md", testrepo.TaskFile(testrepo.Task{ID: "TM-012"}))
	fs := run(t, r)
	expect(t, fs, finding.Error, "tasks/TM-001-task.md:7: error: component nope is not defined")
	expect(t, fs, finding.Error, "TM-002-task.md:8: error: depends_on names the task itself")
	expect(t, fs, finding.Error, "depends_on TM-099: no such task")
	expect(t, fs, finding.Error, "dependency cycle: TM-004 → TM-005 → TM-004")
	expect(t, fs, finding.Error, "after_release abcdef12 names no commit")
	expect(t, fs, finding.Error, "after_release "+side[:10]+" is not on the main branch")
	expect(t, fs, finding.Error, "id XX-009 does not use the project's prefix TM")
	expect(t, fs, finding.Error, "tasks/TM-010-dup.md:2: error: id TM-001 does not equal the file name's prefix TM-010")
	expect(t, fs, finding.Error, "tasks/TM-011-date.md:13: error: front matter: 2026-11-07 is an unquoted date")
	expect(t, fs, finding.Error, "bad name.md: error: file name is not")
	if f := find(fs, finding.Error, "TM-008"); f != nil {
		t.Errorf("TM-008's after_release is on main: %s", f)
	}
}

func TestDuplicateIDAcrossFiles(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	r.Write("tasks/TM-001-other.md", testrepo.TaskFile(testrepo.Task{ID: "TM-001"}))
	fs := run(t, r)
	expect(t, fs, finding.Error, "tasks/TM-001-task.md:2: error: id TM-001 is also used by tasks/TM-001-other.md")
}

func TestConfigErrors(t *testing.T) {
	r := testrepo.New(t)
	r.Write("tasks/tasks.toml", strings.Replace(testrepo.DefaultConfig, "[components.docs]", "[components.docs]\nowner = \"x\"", 1))
	fs := run(t, r)
	expect(t, fs, finding.Error, `tasks/tasks.toml:12: error: unknown key "components.docs.owner"`)
}

func TestStateFileErrors(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	r.WriteRawState("TM-001.json", `{"format": 1}`)
	r.WriteRawState("TM-002.json", `{`)
	r.WriteState(testrepo.State{ID: "TM-003"}) // no such task
	fs := run(t, r)
	expect(t, fs, finding.Error, "TM-001.json: error: state file fails state.schema.json")
	expect(t, fs, finding.Error, "TM-002.json: error: state file not JSON")
	expect(t, fs, finding.Error, "TM-003.json: error: state file for TM-003, which is not a known task")
}

func TestGitFindings(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"}, testrepo.Task{ID: "TM-002"})
	r.SquashTask("first\n\nTask: TM-001")
	r.SquashTask("second\n\nTask: TM-001")
	r.SquashTask("plan\n\nTask: osc7501-agent-status")
	r.SquashTask("ghost\n\nTask: TM-404")
	r.MergeTask("TM-002")
	r.WriteState(testrepo.State{ID: "TM-002", Lifecycle: "review"})
	r.AddTask(testrepo.Task{ID: "TM-003"}) // not committed: not on main yet
	fs := run(t, r)
	expect(t, fs, finding.Error, "TM-001 has 2 `Task:` commits on main's first-parent history")
	expect(t, fs, finding.Warning, "`Task: osc7501-agent-status`, which is not a TM task id; ignored")
	expect(t, fs, finding.Warning, "`Task: TM-404`, which is not a known task")
	expect(t, fs, finding.Warning, "TM-002.json: warning: stale: TM-002 is merged, the state file still says review")
	expect(t, fs, finding.Warning, "tasks/TM-003-task.md: warning: not on the main branch main yet")
	errs, _ := finding.Count(fs)
	if errs != 1 {
		t.Errorf("%d errors, want 1:\n%s", errs, render(fs))
	}
}
