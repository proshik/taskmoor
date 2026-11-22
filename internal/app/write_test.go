package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/schema"
	"github.com/proshik/taskmoor/internal/state"
	"github.com/proshik/taskmoor/internal/testrepo"
)

var fixedNow = time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)

func newApp(dir string) *app.App {
	return &app.App{Dir: dir, Source: project.SourceMain, Now: func() time.Time { return fixedNow }, Version: "0.1.0-test"}
}

func refusedWith(t *testing.T, err error, want string) {
	t.Helper()
	var r *app.Refused
	if !errors.As(err, &r) {
		t.Fatalf("error %v, want a refusal containing %q", err, want)
	}
	if !strings.Contains(r.Error(), want) {
		t.Fatalf("refusal %q, want %q", r.Error(), want)
	}
}

func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		if strings.HasSuffix(de.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", de.Name())
		}
	}
}

func readState(t *testing.T, r *testrepo.Repo, id string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(r.StateDir(), id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	sch, err := schema.Load()
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if vs := schema.Validate(sch.State, v); len(vs) > 0 {
		t.Fatalf("written state fails the schema: %v", vs)
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	return m
}

func TestClaimReviewRelease(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	wt := r.Worktree("task/TM-001-x")
	a := newApp(r.Dir)
	ctx := context.Background()

	f, err := a.Claim(ctx, app.ClaimArgs{ID: "TM-001", Branch: "task/TM-001-x", Worktree: wt, Harness: "claude", Session: "s-1"})
	if err != nil {
		t.Fatal(err)
	}
	m := readState(t, r, "TM-001")
	if m["lifecycle"] != "in_progress" || m["branch"] != "task/TM-001-x" || m["worktree"] != wt ||
		m["base"] != r.Git("rev-parse", "main") || m["writer"] != "taskmoor 0.1.0-test" ||
		m["started_at"] != "2026-11-22T09:30:00Z" {
		t.Fatalf("state %v", m)
	}
	h := m["harness"].(map[string]any)
	if h["name"] != "claude" || h["status"] != "unknown" || h["session"] != "s-1" {
		t.Fatalf("harness %v", h)
	}
	if f.Lifecycle != "in_progress" {
		t.Fatalf("returned %+v", f)
	}
	noTempFiles(t, r.StateDir())

	// claimed twice: refused
	_, err = a.Claim(ctx, app.ClaimArgs{ID: "TM-001", Branch: "task/TM-001-x", Worktree: wt})
	refusedWith(t, err, "already has a state file")

	// review, then review again is a no-op
	a.Now = func() time.Time { return fixedNow.Add(time.Hour) }
	_, changed, err := a.Review(ctx, "TM-001")
	if err != nil || !changed {
		t.Fatalf("review: changed=%v err=%v", changed, err)
	}
	m = readState(t, r, "TM-001")
	if m["lifecycle"] != "review" || m["updated_at"] != "2026-11-22T10:30:00Z" || m["started_at"] != "2026-11-22T09:30:00Z" {
		t.Fatalf("after review %v", m)
	}
	if _, changed, err = a.Review(ctx, "TM-001"); err != nil || changed {
		t.Fatalf("second review: changed=%v err=%v", changed, err)
	}
	noTempFiles(t, r.StateDir())

	// release back to backlog
	if after, err := a.Release(ctx, "TM-001"); err != nil || after != "backlog" {
		t.Fatalf("release: %q %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(r.StateDir(), "TM-001.json")); !os.IsNotExist(err) {
		t.Fatalf("state file still there: %v", err)
	}
	_, err = a.Release(ctx, "TM-001")
	refusedWith(t, err, "has no state file")
	_, _, err = a.Review(ctx, "TM-001")
	refusedWith(t, err, "has no state file")
}

func TestClaimRefusals(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(
		testrepo.Task{ID: "TM-001"},
		testrepo.Task{ID: "TM-002", DependsOn: []string{"TM-001"}},
		testrepo.Task{ID: "TM-003", NeedsPerson: true},
		testrepo.Task{ID: "TM-004", Status: "cancelled"},
		testrepo.Task{ID: "TM-005"},
	)
	r.MergeTask("TM-005")
	wt := r.Worktree("task/x")
	a := newApp(r.Dir)
	ctx := context.Background()
	claim := func(id string, mod func(*app.ClaimArgs)) error {
		c := app.ClaimArgs{ID: id, Branch: "task/x", Worktree: wt}
		if mod != nil {
			mod(&c)
		}
		_, err := a.Claim(ctx, c)
		if err == nil {
			if _, err := a.Release(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		return err
	}
	refusedWith(t, claim("TM-002", nil), "dependency TM-001 is backlog")
	if err := claim("TM-002", func(c *app.ClaimArgs) { c.Force = true }); err != nil {
		t.Fatalf("--force skips readiness: %v", err)
	}
	refusedWith(t, claim("TM-003", nil), "needs a person")
	if err := claim("TM-003", func(c *app.ClaimArgs) { c.Person = true }); err != nil {
		t.Fatalf("--person: %v", err)
	}
	refusedWith(t, claim("TM-004", func(c *app.ClaimArgs) { c.Force = true }), "TM-004 is cancelled, not backlog")
	refusedWith(t, claim("TM-005", func(c *app.ClaimArgs) { c.Force = true }), "TM-005 is merged, not backlog")
	refusedWith(t, claim("TM-404", nil), "no such task")
	refusedWith(t, claim("TM-001", func(c *app.ClaimArgs) { c.Worktree = "relative/path" }), "not an absolute path")
	refusedWith(t, claim("TM-001", func(c *app.ClaimArgs) { c.Worktree = t.TempDir() }), "is not a worktree of this repository")
	refusedWith(t, claim("TM-001", func(c *app.ClaimArgs) { c.Branch = "task/other" }), "has task/x checked out, not task/other")
	noTempFiles(t, r.StateDir())
}

func TestClaimRefusedWhileOverlappingTaskRuns(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(
		testrepo.Task{ID: "TM-001", Touches: []string{"src/a/**"}},
		testrepo.Task{ID: "TM-002", Touches: []string{"src/a/b.go"}},
	)
	r.WriteState(testrepo.State{ID: "TM-001", Lifecycle: "review"})
	wt := r.Worktree("task/x")
	_, err := newApp(r.Dir).Claim(context.Background(), app.ClaimArgs{ID: "TM-002", Branch: "task/x", Worktree: wt})
	refusedWith(t, err, "touches overlap TM-001 (review)")
}

func TestConcurrentClaimsOfOneTask(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	wt := r.Worktree("task/x")
	wins := concurrently(t, 8, func(i int) error {
		_, err := newApp(r.Dir).Claim(context.Background(), app.ClaimArgs{ID: "TM-001", Branch: "task/x", Worktree: wt})
		return err
	})
	if wins != 1 {
		t.Fatalf("%d claims won, want 1", wins)
	}
	noTempFiles(t, r.StateDir())
}

func TestConcurrentClaimsWithOverlappingTouches(t *testing.T) {
	r := testrepo.New(t)
	var ts []testrepo.Task
	ids := []string{"TM-001", "TM-002", "TM-003", "TM-004", "TM-005", "TM-006"}
	for _, id := range ids {
		ts = append(ts, testrepo.Task{ID: id, Touches: []string{"src/shared/**"}})
	}
	r.AddTasks(ts...)
	wt := r.Worktree("task/x")
	wins := concurrently(t, len(ids), func(i int) error {
		_, err := newApp(r.Dir).Claim(context.Background(), app.ClaimArgs{ID: ids[i], Branch: "task/x", Worktree: wt})
		return err
	})
	if wins != 1 {
		t.Fatalf("%d claims of overlapping tasks won, want 1 (the lock serializes the re-check)", wins)
	}
}

// concurrently runs n calls at once and returns how many succeeded; every
// failure must be a refusal.
func concurrently(t *testing.T, n int, call func(i int) error) int {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := call(i)
			mu.Lock()
			defer mu.Unlock()
			var ref *app.Refused
			switch {
			case err == nil:
				wins++
			case !errors.As(err, &ref):
				t.Errorf("claim %d: unexpected error %v", i, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	return wins
}

func TestReviewAndReleaseOfStaleOrInvalidState(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"}, testrepo.Task{ID: "TM-002"})
	r.MergeTask("TM-001")
	r.WriteState(testrepo.State{ID: "TM-001"})
	r.WriteRawState("TM-002.json", "{broken")
	a := newApp(r.Dir)
	ctx := context.Background()
	_, _, err := a.Review(ctx, "TM-001")
	refusedWith(t, err, "stale")
	_, _, err = a.Review(ctx, "TM-002")
	refusedWith(t, err, "invalid")
	for id, want := range map[string]string{"TM-001": "merged", "TM-002": "backlog"} {
		if after, err := a.Release(ctx, id); err != nil || after != want {
			t.Fatalf("release %s: %q %v, want %s", id, after, err, want)
		}
	}
	_, err = a.Release(ctx, "../../x")
	refusedWith(t, err, "no such task")
}

func TestLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	unlock, err := state.Lock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		u, err := state.Lock(context.Background(), dir)
		if err != nil {
			t.Error(err)
			return
		}
		u()
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second lock taken while the first is held")
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("second lock not taken after unlock")
	}
}

func TestClaimRefusesARemovedWorktree(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	wt := r.Worktree("task/x")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	_, err := newApp(r.Dir).Claim(context.Background(), app.ClaimArgs{ID: "TM-001", Branch: "task/x", Worktree: wt})
	refusedWith(t, err, "worktree "+wt)
	noState(t, r, "TM-001")
}

func noState(t *testing.T, r *testrepo.Repo, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(r.StateDir(), id+".json")); !os.IsNotExist(err) {
		t.Fatalf("%s has a state file: %v", id, err)
	}
}

func TestWritesReadTheMainBranchWhateverTheSource(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001", NeedsPerson: true})
	// a local edit drops needs_person; the claim still reads main
	r.AddTask(testrepo.Task{ID: "TM-001"})
	wt := r.Worktree("task/x")
	a := newApp(r.Dir)
	a.Source = project.SourceWorktree
	_, err := a.Claim(context.Background(), app.ClaimArgs{ID: "TM-001", Branch: "task/x", Worktree: wt})
	refusedWith(t, err, "needs a person")
}

func TestClaimRefusedWhileARunningTaskIsNotOnMain(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	r.WriteState(testrepo.State{ID: "TM-002"}) // a task only on some branch
	wt := r.Worktree("task/x")
	a := newApp(r.Dir)
	_, err := a.Claim(context.Background(), app.ClaimArgs{ID: "TM-001", Branch: "task/x", Worktree: wt})
	refusedWith(t, err, "TM-002 is running (state file) but is not a task on the main branch")
	if _, err := a.Claim(context.Background(), app.ClaimArgs{ID: "TM-001", Branch: "task/x", Worktree: wt, Force: true}); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

func TestConcurrentClaimsFromTwoWorktrees(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(
		testrepo.Task{ID: "TM-001", Touches: []string{"src/shared/**"}},
		testrepo.Task{ID: "TM-002", Touches: []string{"src/shared/a.go"}},
	)
	wts := []string{r.Worktree("task/a"), r.Worktree("task/b")}
	ids := []string{"TM-001", "TM-002"}
	for round := 0; round < 5; round++ {
		wins := concurrently(t, 2, func(i int) error {
			// each claim runs from its own worktree, as two launchers would
			_, err := newApp(wts[i]).Claim(context.Background(), app.ClaimArgs{ID: ids[i], Branch: []string{"task/a", "task/b"}[i], Worktree: wts[i]})
			return err
		})
		des, _ := os.ReadDir(r.StateDir())
		n := 0
		for _, de := range des {
			if strings.HasSuffix(de.Name(), ".json") {
				n++
				_ = os.Remove(filepath.Join(r.StateDir(), de.Name()))
			}
		}
		if wins != 1 || n != 1 {
			t.Fatalf("round %d: %d claims won, %d state files; want 1 and 1", round, wins, n)
		}
	}
}

func TestClaimRefusedWhenTasksTomlOnMainHasErrors(t *testing.T) {
	r := testrepo.New(t)
	base := r.Git("rev-parse", "HEAD")
	// rule = "tag" and Rule = "merge": the decoder may take either
	r.Write("tasks/tasks.toml", strings.Replace(testrepo.DefaultConfig,
		`release = { rule = "tag", pattern = "v*" }`, `release = { rule = "tag", Rule = "merge", pattern = "v*" }`, 1))
	r.AddTasks(testrepo.Task{ID: "TM-001", AfterRelease: base[:8]})
	wt := r.Worktree("task/x")
	_, err := newApp(r.Dir).Claim(context.Background(), app.ClaimArgs{ID: "TM-001", Branch: "task/x", Worktree: wt, Force: true})
	refusedWith(t, err, "tasks/tasks.toml on the main branch has errors")
	noState(t, r, "TM-001")
}

func TestClaimRefusesAForeignCheckoutAtAWorktreePath(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	wt := r.Worktree("task/x")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	other := testrepo.New(t) // another repository, same branch name, at the old path
	other.Git("branch", "task/x")
	other.Git("worktree", "add", "-q", wt, "task/x")
	_, err := newApp(r.Dir).Claim(context.Background(), app.ClaimArgs{ID: "TM-001", Branch: "task/x", Worktree: wt})
	refusedWith(t, err, "worktree")
	noState(t, r, "TM-001")
}

func TestClaimRefusesABranchWithoutAMergedDependency(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"}, testrepo.Task{ID: "TM-002", DependsOn: []string{"TM-001"}})
	wt := r.Worktree("task/tm-002") // made before TM-001 merged
	r.MergeTask("TM-001")
	a := newApp(r.Dir)
	_, err := a.Claim(context.Background(), app.ClaimArgs{ID: "TM-002", Branch: "task/tm-002", Worktree: wt})
	refusedWith(t, err, "does not contain the merge of dependency TM-001")
	r.GitIn(wt, "merge", "-q", "--ff-only", "main")
	if _, err := a.Claim(context.Background(), app.ClaimArgs{ID: "TM-002", Branch: "task/tm-002", Worktree: wt}); err != nil {
		t.Fatalf("after updating the branch: %v", err)
	}
}

func TestClaimRefusesAStateFileReadersWouldDrop(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	wt := r.Worktree("task/x")
	_, err := newApp(r.Dir).Claim(context.Background(), app.ClaimArgs{ID: "TM-001", Branch: "task/x", Worktree: wt,
		Harness: "claude", Session: strings.Repeat("s", state.MaxSize)})
	if err == nil || !strings.Contains(err.Error(), "readers take at most") {
		t.Fatalf("err %v", err)
	}
	noState(t, r, "TM-001")
}
