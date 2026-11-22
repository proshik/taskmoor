package derive_test

import (
	"context"
	"strings"
	"testing"

	"github.com/proshik/taskmoor/internal/derive"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/testrepo"
)

func load(t *testing.T, r *testrepo.Repo) (*project.Snapshot, map[string]*derive.Status) {
	t.Helper()
	var l project.Loader
	snap, err := l.Load(context.Background(), r.Dir, project.SourceMain)
	if err != nil {
		t.Fatal(err)
	}
	return snap, derive.Derive(snap)
}

func lifecycle(t *testing.T, r *testrepo.Repo, id string) *derive.Status {
	t.Helper()
	_, st := load(t, r)
	s, ok := st[id]
	if !ok {
		t.Fatalf("%s not derived", id)
	}
	return s
}

func TestLifecycleRulesInOrder(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(
		testrepo.Task{ID: "TM-001"},
		testrepo.Task{ID: "TM-002"},
		testrepo.Task{ID: "TM-003"},
		testrepo.Task{ID: "TM-004", Component: "docs"},
		testrepo.Task{ID: "TM-005", Status: "cancelled"},
		testrepo.Task{ID: "TM-006"},
		testrepo.Task{ID: "TM-007"},
	)
	m1 := r.MergeTask("TM-001")
	r.Tag("v1.0.0", m1)
	r.MergeTask("TM-002")
	r.MergeTask("TM-004")
	r.MergeTask("TM-005")
	r.WriteState(testrepo.State{ID: "TM-003"})
	r.WriteState(testrepo.State{ID: "TM-006", Lifecycle: "review"})

	_, st := load(t, r)
	want := map[string]string{
		"TM-001": derive.Done,       // tag v* contains the merge
		"TM-002": derive.Merged,     // merged, no release yet
		"TM-003": derive.InProgress, // state file
		"TM-004": derive.Done,       // rule = "merge": done once merged
		"TM-005": derive.Cancelled,  // cancelled wins over the merge
		"TM-006": derive.Review,
		"TM-007": derive.Backlog,
	}
	for id, lc := range want {
		if got := st[id].Lifecycle; got != lc {
			t.Errorf("%s: lifecycle %s, want %s", id, got, lc)
		}
	}
	if st["TM-001"].ReleaseTag != "v1.0.0" {
		t.Errorf("TM-001 release tag %q", st["TM-001"].ReleaseTag)
	}
	if st["TM-001"].MergeCommit != m1 {
		t.Errorf("TM-001 merge %s, want %s", st["TM-001"].MergeCommit, m1)
	}
}

func TestFirstReleaseTagByVersionOrder(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	m := r.MergeTask("TM-001")
	r.Tag("v1.10.0", m)
	r.Tag("v1.9.0", m)
	r.Tag("other-1", m) // not the component's pattern
	if got := lifecycle(t, r, "TM-001").ReleaseTag; got != "v1.9.0" {
		t.Fatalf("release tag %q, want v1.9.0", got)
	}
}

func TestTrailerOnlyOnBranchIsNotMerged(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	r.Git("checkout", "-q", "-b", "side")
	r.Write("src/a.txt", "a\n")
	r.Commit("work\n\nTask: TM-001")
	r.Git("checkout", "-q", "main")
	// merged without the trailer on the merge commit: the branch commit is
	// not first-parent history
	r.Git("merge", "-q", "--no-ff", "-m", "Merge side", "side")
	if got := lifecycle(t, r, "TM-001").Lifecycle; got != derive.Backlog {
		t.Fatalf("lifecycle %s, want backlog", got)
	}
}

func TestTrailerNotInLastParagraph(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	r.SquashTask("Do it\n\nTask: TM-001\n\nA last paragraph that is prose.")
	if got := lifecycle(t, r, "TM-001").Lifecycle; got != derive.Backlog {
		t.Fatalf("lifecycle %s, want backlog", got)
	}
}

func TestSquashCommitWithTrailerIsMerged(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	c := r.SquashTask("Do it (#1)\n\nTask: TM-001")
	st := lifecycle(t, r, "TM-001")
	if st.Lifecycle != derive.Merged || st.MergeCommit != c {
		t.Fatalf("got %s %s, want merged %s", st.Lifecycle, st.MergeCommit, c)
	}
}

func TestSecondTrailerUsesOldest(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	first := r.SquashTask("first\n\nTask: TM-001")
	r.SquashTask("second\n\nTask: TM-001")
	snap, st := load(t, r)
	if st["TM-001"].MergeCommit != first {
		t.Fatalf("merge %s, want the oldest %s", st["TM-001"].MergeCommit, first)
	}
	if n := len(snap.AllMerges["TM-001"]); n != 2 {
		t.Fatalf("AllMerges has %d commits, want 2", n)
	}
}

func TestStaleStateFile(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"}, testrepo.Task{ID: "TM-002", Status: "cancelled"})
	r.MergeTask("TM-001")
	r.WriteState(testrepo.State{ID: "TM-001", Lifecycle: "review"})
	r.WriteState(testrepo.State{ID: "TM-002"})
	_, st := load(t, r)
	if s := st["TM-001"]; s.Lifecycle != derive.Merged || !s.Stale {
		t.Errorf("TM-001: %s stale=%v, want merged stale", s.Lifecycle, s.Stale)
	}
	if s := st["TM-002"]; s.Lifecycle != derive.Cancelled || !s.Stale {
		t.Errorf("TM-002: %s stale=%v, want cancelled stale", s.Lifecycle, s.Stale)
	}
}

func TestInvalidStateFileIsNoState(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"}, testrepo.Task{ID: "TM-002"}, testrepo.Task{ID: "TM-003"})
	r.WriteRawState("TM-001.json", `{"format": 1, "id": "TM-001", "lifecycle": "merged"}`) // fails the schema
	r.WriteRawState("TM-002.json", `not json`)
	r.WriteRawState("TM-003.json", stateJSON("TM-002")) // id differs from the file name
	snap, st := load(t, r)
	for _, id := range []string{"TM-001", "TM-002", "TM-003"} {
		if st[id].Lifecycle != derive.Backlog || st[id].State != nil {
			t.Errorf("%s: %s with state %v, want backlog without state", id, st[id].Lifecycle, st[id].State)
		}
	}
	for _, e := range snap.States {
		if e.Valid() {
			t.Errorf("%s: valid, want a problem", e.Name)
		}
	}
	if len(snap.States) != 3 {
		t.Errorf("%d state entries, want 3", len(snap.States))
	}
}

// stateJSON is a valid state file for id.
func stateJSON(id string) string {
	return `{"format": 1, "id": "` + id + `", "lifecycle": "in_progress", "worktree": "/w", "branch": "b",
	"base": "0123456", "started_at": "2026-11-08T10:00:00Z", "updated_at": "2026-11-08T10:00:00Z", "writer": "x"}`
}

func TestReadinessDependencies(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(
		testrepo.Task{ID: "TM-001"},
		testrepo.Task{ID: "TM-002"},
		testrepo.Task{ID: "TM-003", DependsOn: []string{"TM-001"}},
		testrepo.Task{ID: "TM-004", DependsOn: []string{"TM-002"}},
		testrepo.Task{ID: "TM-005", DependsOn: []string{"TM-009"}},
	)
	r.MergeTask("TM-001")
	_, st := load(t, r)
	if !st["TM-003"].Ready {
		t.Errorf("TM-003 should be ready: %v", st["TM-003"].Reasons)
	}
	if st["TM-004"].Ready || !hasReason(st["TM-004"], "dependency TM-002 is backlog") {
		t.Errorf("TM-004 reasons %v", st["TM-004"].Reasons)
	}
	if st["TM-005"].Ready || !hasReason(st["TM-005"], "dependency TM-009 does not exist") {
		t.Errorf("TM-005 reasons %v", st["TM-005"].Reasons)
	}
	if st["TM-001"].Ready {
		t.Errorf("a merged task is not ready")
	}
}

func hasReason(s *derive.Status, want string) bool {
	for _, r := range s.Reasons {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

func TestReadinessAfterRelease(t *testing.T) {
	r := testrepo.New(t)
	base := r.Git("rev-parse", "HEAD")
	r.AddTasks(
		testrepo.Task{ID: "TM-001", AfterRelease: base[:8]},
		testrepo.Task{ID: "TM-002", AfterRelease: base[:8], Component: "docs"},
	)
	_, st := load(t, r)
	if st["TM-001"].Ready || !hasReason(st["TM-001"], "not in a v* tag") {
		t.Errorf("TM-001 before the tag: %v", st["TM-001"].Reasons)
	}
	if !st["TM-002"].Ready {
		t.Errorf("TM-002 (rule merge, commit on main) should be ready: %v", st["TM-002"].Reasons)
	}
	r.Tag("v0.1.0", "HEAD")
	_, st = load(t, r)
	if !st["TM-001"].Ready {
		t.Errorf("TM-001 after the tag: %v", st["TM-001"].Reasons)
	}

	// a commit only on a branch is not on main
	r.Git("checkout", "-q", "-b", "side")
	side := r.SquashTask("side")
	r.Git("checkout", "-q", "main")
	r.AddTasks(testrepo.Task{ID: "TM-003", AfterRelease: side[:8], Component: "docs"})
	_, st = load(t, r)
	if st["TM-003"].Ready || !hasReason(st["TM-003"], "is not on the main branch") {
		t.Errorf("TM-003: %v", st["TM-003"].Reasons)
	}
}

func TestReadinessTouchesOverlapCountsReview(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(
		testrepo.Task{ID: "TM-001", Touches: []string{"src/root/**"}},
		testrepo.Task{ID: "TM-002", Touches: []string{"src/ro*"}},
		testrepo.Task{ID: "TM-003", Touches: []string{"docs/a.md"}},
		testrepo.Task{ID: "TM-004", Touches: []string{"src/root/a.rs"}},
		testrepo.Task{ID: "TM-005", Touches: []string{"src/ro"}},
	)
	r.WriteState(testrepo.State{ID: "TM-001", Lifecycle: "review"})
	_, st := load(t, r)
	if st["TM-002"].Ready || !hasReason(st["TM-002"], "touches overlap TM-001 (review): src/ro* ~ src/root/**") {
		t.Errorf("TM-002: %v", st["TM-002"].Reasons)
	}
	if st["TM-004"].Ready {
		t.Errorf("TM-004 overlaps TM-001")
	}
	if !st["TM-003"].Ready || !st["TM-005"].Ready {
		t.Errorf("TM-003 %v, TM-005 %v should be ready", st["TM-003"].Reasons, st["TM-005"].Reasons)
	}
}

func TestOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"src/ro*", "src/root/a.rs", true},
		{"src/ro", "src/root/a.rs", false},
		{"src/**", "src", true},
		{"src/**", "src/a/b.go", true},
		{"src/**", "srcx/a", false},
		{"crates/session/**", "crates/desktop/**", false},
		{"crates/session/src/a.rs", "crates/session/src/a.rs", true},
		{"crates/session/src/a.rs", "crates/session/src/b.rs", false},
		{"a/b", "a/b/c", true},
		{"*", "anything/at/all", true},
		{"docs/*.md", "docs/plan.md", true},
		{"docs/[ab].md", "src/x", false},
		{"src/r?ot/x", "src/root/x", true},
		{"src/ro*", "src/r*", true},
		{"src/root.rs", "src/ro*", true},
		{"src/a*/x", "src/b/x", false},
	}
	for _, c := range cases {
		if got := derive.Overlap(c.a, c.b); got != c.want {
			t.Errorf("Overlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := derive.Overlap(c.b, c.a); got != c.want {
			t.Errorf("Overlap(%q, %q) = %v, want %v (swapped)", c.b, c.a, got, c.want)
		}
	}
}

func TestCycles(t *testing.T) {
	ts := []*project.Task{
		{ID: "TM-001", DependsOn: []string{"TM-002"}},
		{ID: "TM-002", DependsOn: []string{"TM-003"}},
		{ID: "TM-003", DependsOn: []string{"TM-001"}},
		{ID: "TM-004", DependsOn: []string{"TM-001"}},
		{ID: "TM-005", DependsOn: []string{"TM-005"}}, // self: reported by validate, not a cycle here
	}
	cycles := derive.Cycles(ts)
	if len(cycles) != 1 || strings.Join(cycles[0], " ") != "TM-001 TM-002 TM-003 TM-001" {
		t.Fatalf("cycles %v", cycles)
	}
}

func TestTagPatternIsNotAnOption(t *testing.T) {
	r := testrepo.New(t)
	r.Write("tasks/tasks.toml", strings.Replace(testrepo.DefaultConfig, `pattern = "v*"`, `pattern = "--merged"`, 1))
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	r.Tag("v1.0.0", r.MergeTask("TM-001"))
	// with "--merged" read as an option every tag reachable from HEAD would
	// count as a release
	if got := lifecycle(t, r, "TM-001").Lifecycle; got != derive.Merged {
		t.Fatalf("lifecycle %s, want merged", got)
	}
}

func TestDoubledSlashesOverlap(t *testing.T) {
	for _, c := range [][2]string{{"src/api.go", "src//api.go"}, {"src//ro*", "src/root.rs"}, {"src/", "src//x"}} {
		if !derive.Overlap(c[0], c[1]) || !derive.Overlap(c[1], c[0]) {
			t.Errorf("%q and %q do not overlap", c[0], c[1])
		}
	}
}

func TestTrailerValueIsNotSplitOnControlBytes(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	r.SquashTask("sneaky\n\nTask: bogus\x1eTM-001")
	if got := lifecycle(t, r, "TM-001").Lifecycle; got != derive.Backlog {
		t.Fatalf("lifecycle %s, want backlog", got)
	}
}

func TestUnknownReservationBlocksReadiness(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	r.WriteState(testrepo.State{ID: "TM-002"}) // not a task on main
	if st := lifecycle(t, r, "TM-001"); st.Ready || !hasReason(st, "TM-002 is running (state file) but is not a task on the main branch") {
		t.Fatalf("%+v", st)
	}
}
