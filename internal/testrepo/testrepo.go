// Package testrepo builds real git repositories in temporary directories
// for tests: tasks.toml and task files, commits, branches, merges with a
// `Task:` trailer, squashes, tags, worktrees and state files.
package testrepo

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a test repository.
type Repo struct {
	t   testing.TB
	Dir string
	n   int
}

// Isolate points git at an empty global config and fixed identities, so
// the person's own git configuration (signing, hooks) does not leak into
// tests.
func Isolate(t testing.TB) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   empty,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Test",
		"GIT_AUTHOR_EMAIL":    "test@example.invalid",
		"GIT_COMMITTER_NAME":  "Test",
		"GIT_COMMITTER_EMAIL": "test@example.invalid",
		"GIT_AUTHOR_DATE":     "2026-11-01T10:00:00Z",
		"GIT_COMMITTER_DATE":  "2026-11-01T10:00:00Z",
	} {
		t.Setenv(k, v)
	}
}

// DefaultConfig is a tasks.toml with prefix TM, main branch main, a
// tag-released component `app` (pattern v*) and a merge-released `docs`.
const DefaultConfig = `format = 1

[project]
id_prefix = "TM"
main_branch = "main"

[components.app]
paths = ["src/**"]
release = { rule = "tag", pattern = "v*" }

[components.docs]
paths = ["docs/**"]
release = { rule = "merge" }
`

// New makes an empty repository on branch main with one initial commit
// holding tasks/tasks.toml = DefaultConfig.
func New(t testing.TB) *Repo {
	t.Helper()
	Isolate(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(dir, "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Repo{t: t, Dir: dir}
	r.Git("init", "-q", "-b", "main")
	r.Write("tasks/tasks.toml", DefaultConfig)
	r.Commit("initial")
	return r
}

// Git runs git in the repository and returns its trimmed stdout; it fails
// the test on error.
func (r *Repo) Git(args ...string) string {
	r.t.Helper()
	return r.GitIn(r.Dir, args...)
}

// GitIn runs git in dir.
func (r *Repo) GitIn(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write writes a file relative to the repository root.
func (r *Repo) Write(rel, content string) {
	r.t.Helper()
	WriteIn(r.t, r.Dir, rel, content)
}

// WriteIn writes a file relative to dir.
func WriteIn(t testing.TB, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Remove deletes a file relative to the repository root.
func (r *Repo) Remove(rel string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.Dir, rel)); err != nil {
		r.t.Fatal(err)
	}
}

// Commit stages everything and commits with message; returns the hash.
func (r *Repo) Commit(message string) string {
	r.t.Helper()
	return r.CommitIn(r.Dir, message)
}

// CommitIn commits everything in a worktree dir.
func (r *Repo) CommitIn(dir, message string) string {
	r.t.Helper()
	r.GitIn(dir, "add", "-A")
	r.GitIn(dir, "commit", "-q", "--allow-empty", "-m", message)
	return r.GitIn(dir, "rev-parse", "HEAD")
}

// Task is a task file's front matter for tests; zero fields get defaults.
type Task struct {
	ID, Title, Kind, Status, Component, Harness, Created string
	Priority                                             int
	DependsOn, Touches                                   []string
	NeedsPerson                                          bool
	AfterRelease                                         string
	Extra                                                string // raw YAML lines appended to the front matter
	Body                                                 string
}

// TaskFile renders a task file.
func TaskFile(t Task) string {
	if t.Title == "" {
		t.Title = "Task " + t.ID
	}
	if t.Kind == "" {
		t.Kind = "feature"
	}
	if t.Status == "" {
		t.Status = "backlog"
	}
	if t.Priority == 0 {
		t.Priority = 2
	}
	if t.Component == "" {
		t.Component = "app"
	}
	if t.Harness == "" {
		t.Harness = "any"
	}
	if t.Created == "" {
		t.Created = "2026-11-07"
	}
	if t.Touches == nil {
		t.Touches = []string{"src/" + strings.ToLower(t.ID) + "/**"}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\nid: %s\ntitle: %q\nkind: %s\nstatus: %s\npriority: %d\ncomponent: %s\n",
		t.ID, t.Title, t.Kind, t.Status, t.Priority, t.Component)
	fmt.Fprintf(&b, "depends_on: [%s]\ntouches:\n", strings.Join(t.DependsOn, ", "))
	for _, p := range t.Touches {
		fmt.Fprintf(&b, "  - %q\n", p)
	}
	fmt.Fprintf(&b, "harness: %s\nreviews: {spec: 0, code: 2}\ncreated: %q\n", t.Harness, t.Created)
	if t.NeedsPerson {
		b.WriteString("needs_person: true\n")
	}
	if t.AfterRelease != "" {
		fmt.Fprintf(&b, "after_release: %q\n", t.AfterRelease)
	}
	b.WriteString(t.Extra)
	b.WriteString("---\n\n")
	if t.Body == "" {
		t.Body = "## Goal\n\nDo " + t.ID + ".\n"
	}
	b.WriteString(t.Body)
	return b.String()
}

// TaskPath is the file a task is written to.
func TaskPath(t Task) string {
	return "tasks/" + t.ID + "-task.md"
}

// AddTask writes a task file in the working tree (not committed).
func (r *Repo) AddTask(t Task) {
	r.t.Helper()
	r.Write(TaskPath(t), TaskFile(t))
}

// AddTasks writes the task files and commits them on the current branch.
func (r *Repo) AddTasks(ts ...Task) string {
	r.t.Helper()
	for _, t := range ts {
		r.AddTask(t)
	}
	return r.Commit("add tasks")
}

// MergeTask makes a branch from main with one commit, then merges it into
// main with --no-ff and a `Task: <id>` trailer. Returns the merge commit.
func (r *Repo) MergeTask(id string) string {
	r.t.Helper()
	r.n++
	branch := fmt.Sprintf("task/%s-%d", strings.ToLower(id), r.n)
	r.Git("checkout", "-q", "-b", branch, "main")
	r.Write(fmt.Sprintf("src/%s/change-%d.txt", strings.ToLower(id), r.n), "x\n")
	r.Commit("work on " + id)
	r.Git("checkout", "-q", "main")
	r.Git("merge", "-q", "--no-ff", "-m", "Merge "+id+"\n\nTask: "+id, branch)
	return r.Git("rev-parse", "HEAD")
}

// SquashTask commits a change straight onto main with message (which
// should carry the trailer, or not).
func (r *Repo) SquashTask(message string) string {
	r.t.Helper()
	r.n++
	r.Write(fmt.Sprintf("src/squash-%d.txt", r.n), "x\n")
	return r.Commit(message)
}

// Tag tags rev.
func (r *Repo) Tag(name, rev string) {
	r.t.Helper()
	r.Git("tag", name, rev)
}

// Worktree adds a worktree beside the repository on a new branch from main.
func (r *Repo) Worktree(branch string) string {
	r.t.Helper()
	dir := filepath.Join(filepath.Dir(r.Dir), strings.ReplaceAll(branch, "/", "-"))
	r.Git("worktree", "add", "-q", "-b", branch, dir, "main")
	return dir
}

// StateDir is the repository's shared state directory.
func (r *Repo) StateDir() string {
	return filepath.Join(r.Dir, ".git", "tasks", "state")
}

// State is a state file's content for WriteState; zero fields get defaults.
type State struct {
	ID, Lifecycle, Worktree, Branch, Base string
	Harness                               map[string]any
}

// WriteState writes <state>/<id>.json directly (as another writer would).
func (r *Repo) WriteState(s State) {
	r.t.Helper()
	if s.Lifecycle == "" {
		s.Lifecycle = "in_progress"
	}
	if s.Worktree == "" {
		s.Worktree = "/tmp/wt-" + strings.ToLower(s.ID)
	}
	if s.Branch == "" {
		s.Branch = "task/" + s.ID
	}
	if s.Base == "" {
		s.Base = r.Git("rev-parse", "main")
	}
	m := map[string]any{
		"format": 1, "id": s.ID, "lifecycle": s.Lifecycle, "worktree": s.Worktree,
		"branch": s.Branch, "base": s.Base, "started_at": "2026-11-08T10:00:00Z",
		"updated_at": "2026-11-08T10:00:00Z", "writer": "test 1",
	}
	if s.Harness != nil {
		m["harness"] = s.Harness
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		r.t.Fatal(err)
	}
	r.WriteRawState(s.ID+".json", string(data))
}

// WriteRawState writes any bytes as a state file name.
func (r *Repo) WriteRawState(name, content string) {
	r.t.Helper()
	WriteIn(r.t, r.StateDir(), name, content)
}
