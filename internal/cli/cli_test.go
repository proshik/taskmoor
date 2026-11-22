package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/cli"
	"github.com/proshik/taskmoor/internal/testrepo"
)

type result struct {
	code           int
	stdout, stderr string
}

func run(t *testing.T, dir string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), args, cli.Env{
		Stdout: &out, Stderr: &errb, Dir: dir, Version: "0.1.0-test",
		Getenv: func(string) string { return "" }, // outside Guild
		Now:    func() time.Time { return time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC) },
	})
	return result{code, out.String(), errb.String()}
}

func fixture(t *testing.T) *testrepo.Repo {
	t.Helper()
	r := testrepo.New(t)
	r.AddTasks(
		testrepo.Task{ID: "TM-001", Priority: 2, Title: "First"},
		testrepo.Task{ID: "TM-002", Priority: 1, DependsOn: []string{"TM-001"}},
		testrepo.Task{ID: "TM-003", Priority: 1, NeedsPerson: true},
		testrepo.Task{ID: "TM-004", Priority: 3},
		testrepo.Task{ID: "TM-005", Priority: 3, Touches: []string{"src/tm-004/x.go"}},
	)
	r.MergeTask("TM-004")
	r.WriteState(testrepo.State{ID: "TM-001", Harness: map[string]any{"name": "claude", "status": "working", "observed_at": "2026-11-22T09:00:00Z"}})
	return r
}

func TestListJSON(t *testing.T) {
	r := fixture(t)
	res := run(t, r.Dir, "list", "--json")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stderr)
	}
	var out struct {
		Format        int    `json:"format"`
		Project       string `json:"project"`
		HarnessSource string `json:"harness_source"`
		Warnings      []string
		Tasks         []struct {
			ID, Lifecycle, File string
			Ready               bool
			NeedsPerson         bool `json:"needs_person"`
			Reasons             []string
			MergeCommit         string `json:"merge_commit"`
			State               *struct {
				Lifecycle string
				Stale     bool
			}
			Harness *struct{ Name, Status, Source string }
		}
	}
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatalf("%v\n%s", err, res.stdout)
	}
	if out.Format != 1 || out.Project != "repo" || out.HarnessSource != "none" || len(out.Tasks) != 5 || out.Warnings == nil {
		t.Fatalf("%+v", out)
	}
	byID := map[string]int{}
	for i, tk := range out.Tasks {
		byID[tk.ID] = i
	}
	t1 := out.Tasks[byID["TM-001"]]
	if t1.Lifecycle != "in_progress" || t1.State == nil || t1.Harness == nil || t1.Harness.Status != "working" || t1.Harness.Source != "state" {
		t.Errorf("TM-001 %+v", t1)
	}
	t2 := out.Tasks[byID["TM-002"]]
	if t2.Ready || len(t2.Reasons) != 1 || t2.Reasons[0] != "dependency TM-001 is in_progress" {
		t.Errorf("TM-002 %+v", t2)
	}
	if t3 := out.Tasks[byID["TM-003"]]; !t3.Ready || !t3.NeedsPerson {
		t.Errorf("TM-003 %+v", t3)
	}
	if t4 := out.Tasks[byID["TM-004"]]; t4.Lifecycle != "merged" || t4.MergeCommit == "" || t4.File != "tasks/TM-004-task.md" {
		t.Errorf("TM-004 %+v", t4)
	}
}

func TestReadyJSONOrder(t *testing.T) {
	r := fixture(t)
	res := run(t, r.Dir, "ready", "--json")
	var out struct {
		Format int
		Tasks  []struct{ ID string }
	}
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil || res.code != 0 {
		t.Fatalf("exit %d %v\n%s", res.code, err, res.stdout)
	}
	var ids []string
	for _, tk := range out.Tasks {
		ids = append(ids, tk.ID)
	}
	// TM-003 (P1) before TM-005 (P3); TM-002 waits on TM-001
	if strings.Join(ids, " ") != "TM-003 TM-005" {
		t.Fatalf("ready %v", ids)
	}
	text := run(t, r.Dir, "ready")
	if !strings.Contains(text.stdout, "TM-003  P1  Task TM-003 ⚑ needs a person") {
		t.Fatalf("text:\n%s", text.stdout)
	}
}

func TestValidateOutputAndExitCodes(t *testing.T) {
	r := fixture(t)
	res := run(t, r.Dir, "validate")
	if res.code != 0 || !strings.Contains(res.stdout, "0 errors, 0 warnings (5 tasks, source worktree)") {
		t.Fatalf("clean: exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	r.Write("tasks/TM-006-bad.md", strings.Replace(testrepo.TaskFile(testrepo.Task{ID: "TM-006"}), "component: app", "component: nope", 1))
	res = run(t, r.Dir, "validate")
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.code, res.stdout)
	}
	for _, want := range []string{
		"tasks/TM-006-bad.md:7: error: component nope is not defined in tasks/tasks.toml",
		"tasks/TM-006-bad.md: warning: not on the main branch main yet",
		"1 error, 1 warning",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, res.stdout)
		}
	}
	js := run(t, r.Dir, "validate", "--json")
	var out struct {
		Format, Errors, Warnings int
		Findings                 []struct{ Severity, File, Message string }
	}
	if err := json.Unmarshal([]byte(js.stdout), &out); err != nil || js.code != 1 || out.Errors != 1 || out.Format != 1 {
		t.Fatalf("json: exit %d %v %+v", js.code, err, out)
	}
	// the main source does not see the uncommitted file
	if res := run(t, r.Dir, "validate", "--source", "main"); res.code != 0 {
		t.Fatalf("--source main: exit %d\n%s", res.code, res.stdout)
	}
}

func TestUsageAndProjectErrors(t *testing.T) {
	r := fixture(t)
	for _, args := range [][]string{
		{"frobnicate"},
		{"show"},
		{"claim", "TM-003"},
		{"claim", "TM-003", "--branch", "b", "--worktree", "/w", "--session", "s"},
		{"--source", "elsewhere", "list"},
		{"list", "--bogus"},
		{"list", "--repo"},
	} {
		if res := run(t, r.Dir, args...); res.code != 2 {
			t.Errorf("%v: exit %d, want 2\n%s", args, res.code, res.stderr)
		}
	}
	res := run(t, t.TempDir(), "list", "--json")
	if res.code != 2 || !strings.Contains(res.stdout, `"error"`) {
		t.Fatalf("not a repository: exit %d\n%s", res.code, res.stdout)
	}
	// --repo after the subcommand
	if res := run(t, t.TempDir(), "list", "--repo", r.Dir); res.code != 0 {
		t.Fatalf("--repo after the subcommand: exit %d %s", res.code, res.stderr)
	}
}

func TestShow(t *testing.T) {
	r := fixture(t)
	res := run(t, r.Dir, "show", "TM-002")
	for _, want := range []string{"TM-002  Task TM-002", "lifecycle:  backlog", "ready:      no", "- dependency TM-001 is in_progress", "TM-001 (in_progress)", "## Goal"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, res.stdout)
		}
	}
	js := run(t, r.Dir, "show", "TM-001", "--json")
	var out struct {
		Format      int
		FrontMatter struct{ ID string } `json:"front_matter"`
		Lifecycle   string
		Body        string
		State       *struct{ Branch string }
	}
	if err := json.Unmarshal([]byte(js.stdout), &out); err != nil || out.FrontMatter.ID != "TM-001" || out.Lifecycle != "in_progress" || !strings.Contains(out.Body, "## Goal") || out.State == nil {
		t.Fatalf("%v %+v\n%s", err, out, js.stdout)
	}
	if res := run(t, r.Dir, "show", "TM-404"); res.code != 2 {
		t.Fatalf("unknown id: exit %d", res.code)
	}
}

func TestClaimReviewReleaseCommands(t *testing.T) {
	r := fixture(t)
	wt := r.Worktree("task/TM-003-x")
	res := run(t, r.Dir, "claim", "TM-003", "--branch", "task/TM-003-x", "--worktree", wt)
	if res.code != 1 || !strings.Contains(res.stderr, "needs a person") {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}
	res = run(t, r.Dir, "claim", "--person", "TM-003", "--branch", "task/TM-003-x", "--worktree", wt, "--harness", "codex", "--session", "s-1")
	if res.code != 0 || !strings.Contains(res.stdout, "TM-003 claimed: in_progress on task/TM-003-x") {
		t.Fatalf("exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	if res := run(t, wt, "review", "TM-003"); res.code != 0 || res.stdout != "TM-003: review\n" {
		t.Fatalf("review from the worktree: exit %d %q %s", res.code, res.stdout, res.stderr)
	}
	if res := run(t, r.Dir, "review", "TM-003"); res.code != 0 || res.stdout != "TM-003: already review\n" {
		t.Fatalf("second review: %q", res.stdout)
	}
	if res := run(t, r.Dir, "release", "TM-003"); res.code != 0 {
		t.Fatalf("release: %d %s", res.code, res.stderr)
	}
	if res := run(t, r.Dir, "release", "TM-003"); res.code != 1 {
		t.Fatalf("release without state: exit %d", res.code)
	}
}

func TestVersion(t *testing.T) {
	res := run(t, t.TempDir(), "version")
	if res.code != 0 || res.stdout != "taskmoor 0.1.0-test (format 1)\n" {
		t.Fatalf("%d %q", res.code, res.stdout)
	}
}

func TestDoubleDashEndsFlags(t *testing.T) {
	r := fixture(t)
	wt := r.Worktree("task/TM-003-x")
	// after "--" both words are positional: two ids is a usage error, and
	// "--person" is not read as the flag
	res := run(t, r.Dir, "claim", "--branch", "task/TM-003-x", "--worktree", wt, "--", "TM-003", "--person")
	if res.code != 2 {
		t.Fatalf("exit %d, want 2\n%s%s", res.code, res.stdout, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(r.StateDir(), "TM-003.json")); !os.IsNotExist(err) {
		t.Fatal("claimed despite the usage error")
	}
}

func TestGlobalFlagErrorWithJSON(t *testing.T) {
	r := fixture(t)
	res := run(t, r.Dir, "list", "--json", "--source", "typo")
	var out struct {
		Format int
		Error  string
	}
	if res.code != 2 || json.Unmarshal([]byte(res.stdout), &out) != nil || out.Format != 1 || !strings.Contains(out.Error, "--source") {
		t.Fatalf("exit %d stdout %q", res.code, res.stdout)
	}
}

func TestReleaseOfStaleStateSaysWhatRemains(t *testing.T) {
	r := fixture(t)
	r.WriteState(testrepo.State{ID: "TM-004", Lifecycle: "review"}) // TM-004 is merged
	res := run(t, r.Dir, "release", "TM-004")
	if res.code != 0 || res.stdout != "TM-004: stale state file removed; still merged\n" {
		t.Fatalf("%d %q", res.code, res.stdout)
	}
	res = run(t, r.Dir, "release", "TM-001")
	if res.code != 0 || res.stdout != "TM-001: state file removed; back to backlog\n" {
		t.Fatalf("%d %q", res.code, res.stdout)
	}
}

func TestControlCharactersAreNotPrinted(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001", Title: "Evil \x1b]0;pwned\x07 title", Body: "line one\n\x1b[2Jgone\r\nline two\n"})
	for _, args := range [][]string{{"list"}, {"ready"}, {"show", "TM-001"}} {
		res := run(t, r.Dir, args...)
		if strings.ContainsAny(res.stdout, "\x1b\x07\r") {
			t.Errorf("%v printed a control character: %q", args, res.stdout)
		}
	}
	if res := run(t, r.Dir, "show", "TM-001"); !strings.Contains(res.stdout, "line one\n�[2Jgone\nline two") {
		t.Errorf("body %q", res.stdout)
	}
}

func TestValidateReportsABrokenConfig(t *testing.T) {
	r := fixture(t)
	r.Write("tasks/tasks.toml", strings.Replace(testrepo.DefaultConfig, `main_branch = "main"`, "", 1))
	res := run(t, r.Dir, "validate", "--json")
	var out struct {
		Errors   int
		Findings []struct{ Message string }
	}
	if res.code != 1 || json.Unmarshal([]byte(res.stdout), &out) != nil || out.Errors == 0 {
		t.Fatalf("exit %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "project.main_branch is not set") {
		t.Fatalf("%s", res.stdout)
	}
}

func TestControlCharactersInReasonsAndRefusals(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(
		testrepo.Task{ID: "TM-001", Touches: []string{"src/a\x07b"}},
		testrepo.Task{ID: "TM-002", Touches: []string{"src/a\x07b"}},
	)
	r.WriteState(testrepo.State{ID: "TM-001"})
	wt := r.Worktree("task/x")
	show := run(t, r.Dir, "show", "TM-002")
	claim := run(t, r.Dir, "claim", "TM-002", "--branch", "task/x", "--worktree", wt)
	for name, out := range map[string]string{"show": show.stdout, "claim stderr": claim.stderr} {
		if strings.ContainsRune(out, '\x07') || !strings.Contains(out, "touches overlap TM-001") {
			t.Errorf("%s: %q", name, out)
		}
	}
	js := run(t, r.Dir, "show", "TM-002", "--json")
	if !strings.Contains(js.stdout, `\u0007`) {
		t.Errorf("JSON keeps the data: %s", js.stdout)
	}
}

// A16: validate warns when a task's profile runs another harness than the
// one the task names (the profiles come from the service's configuration).
func TestValidateWarnsOnProfileAndHarness(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001", Harness: "codex", Extra: "profile: main\n"})
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "config.toml"), []byte("[[profiles]]\nname = \"main\"\nharness = \"claude\"\n"), 0o600)
	var out, errb bytes.Buffer
	code := cli.Run(context.Background(), []string{"validate"}, cli.Env{
		Stdout: &out, Stderr: &errb, Dir: r.Dir, Version: "0.1.0-test",
		Getenv: func(k string) string {
			if k == "TASKMOOR_HOME" {
				return home
			}
			return ""
		},
		Now: func() time.Time { return time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC) },
	})
	if code != 0 || !strings.Contains(out.String(), "profile main runs claude, harness says codex") || !strings.Contains(out.String(), "1 warning") {
		t.Fatalf("%d %s %s", code, out.String(), errb.String())
	}
}
