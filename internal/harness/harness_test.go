package harness_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/harness"
	"github.com/proshik/taskmoor/internal/state"
)

var now = time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)

// recorded is the shape `guild agent list --json` prints (trimmed).
const recorded = `{
  "answer": "agents",
  "agents": [
    {"session": "s-working", "harness": "claude", "worktree": "/wt/a", "status": "working", "task": true, "open": true, "exited": false},
    {"session": "s-waiting", "harness": "codex", "worktree": "/wt/b", "status": "waiting", "kind": "permission", "task": true, "open": true, "exited": false},
    {"session": "s-waiting-nokind", "harness": "codex", "worktree": "/wt/c", "status": "waiting", "task": true, "open": true, "exited": false},
    {"session": "s-failed", "harness": "claude", "worktree": "/wt/d", "status": "failed", "task": true, "open": true, "exited": false},
    {"session": "s-exited", "harness": "claude", "worktree": "/wt/e", "status": "idle", "task": true, "open": false, "exited": true},
    {"session": "s-new", "harness": "claude", "worktree": "/wt/f", "status": "thinking", "task": true, "open": true, "exited": false},
    {"session": "s-two-1", "harness": "claude", "worktree": "/wt/g", "status": "idle", "task": true, "open": true, "exited": false},
    {"session": "s-two-2", "harness": "claude", "worktree": "/wt/g", "status": "done", "task": true, "open": true, "exited": false},
    {"session": "s-pick-task", "harness": "claude", "worktree": "/wt/h", "status": "done", "task": true, "open": true, "exited": false},
    {"session": "s-pick-person", "harness": "claude", "worktree": "/wt/h", "status": "working", "task": false, "open": true, "exited": false}
  ]
}`

func fakeGuild(out string, err error, delay time.Duration) *harness.Guild {
	return &harness.Guild{Program: "guild", Timeout: 50 * time.Millisecond, Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name != "guild" || len(args) != 3 || args[0] != "agent" || args[1] != "list" || args[2] != "--json" {
			return nil, errors.New("unexpected command")
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []byte(out), err
	}}
}

func running(worktree string, h *state.Harness) *state.File {
	return &state.File{Worktree: worktree, Lifecycle: state.InProgress, Harness: h}
}

func TestGuildMapping(t *testing.T) {
	g := fakeGuild(recorded, nil, 0)
	tasks := map[string]*state.File{
		"TM-001": running("/wt/a", nil),
		"TM-002": running("/wt/b/", nil), // cleaned before matching
		"TM-003": running("/wt/c", nil),
		"TM-004": running("/wt/d", nil),
		"TM-005": running("/wt/e", nil),
		"TM-006": running("/wt/f", nil),
		"TM-007": running("/wt/g", nil),
		"TM-008": running("/wt/h", nil),
		// session wins over the worktree (D2)
		"TM-009": running("/wt/a", &state.Harness{Name: "codex", Status: "unknown", ObservedAt: "2026-11-22T09:00:00Z", Session: "s-failed"}),
	}
	recs, src, _ := harness.Resolve(context.Background(), g, tasks, now)
	if src != harness.ReportGuild {
		t.Fatalf("source %s", src)
	}
	want := map[string][3]string{ // status, kind, raw
		"TM-001": {"working", "", ""},
		"TM-002": {"blocked", "permission", ""},
		"TM-003": {"blocked", "", ""},
		"TM-004": {"error", "", ""},
		"TM-005": {"exited", "", ""},
		"TM-006": {"unknown", "", "thinking"},
		"TM-007": {"unknown", "", ""},
		"TM-008": {"done", "", ""},
		"TM-009": {"error", "", ""},
	}
	for id, w := range want {
		r := recs[id]
		if r.Status != w[0] || r.Kind != w[1] || r.Raw != w[2] || r.Source != harness.SourceGuild {
			t.Errorf("%s: %+v, want %v from guild", id, r, w)
		}
	}
	if recs["TM-007"].Note != "several sessions in the worktree" {
		t.Errorf("TM-007 note %q", recs["TM-007"].Note)
	}
	if recs["TM-008"].Session != "s-pick-task" || recs["TM-009"].Session != "s-failed" {
		t.Errorf("sessions %q %q", recs["TM-008"].Session, recs["TM-009"].Session)
	}
}

func TestSymlinkedWorktreeMatches(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	g := fakeGuild(`{"agents": [{"session": "s", "harness": "claude", "worktree": "`+real+`", "status": "idle", "task": true, "open": true}]}`, nil, 0)
	recs, _, _ := harness.Resolve(context.Background(), g, map[string]*state.File{"TM-001": running(link, nil)}, now)
	if recs["TM-001"].Status != "idle" || recs["TM-001"].Source != harness.SourceGuild {
		t.Fatalf("%+v", recs["TM-001"])
	}
}

func TestFallbackToStateWhenGuildFailsOrTimesOut(t *testing.T) {
	stateRec := &state.Harness{Name: "claude", Status: "blocked", Kind: "question", ObservedAt: "2026-11-22T09:00:00Z"}
	tasks := map[string]*state.File{
		"TM-001": running("/wt/a", stateRec),
		"TM-002": running("/wt/z", nil),
	}
	for name, g := range map[string]*harness.Guild{
		"error":   fakeGuild("", errors.New("socket gone"), 0),
		"timeout": fakeGuild(recorded, nil, time.Second),
		"garbage": fakeGuild("not json", nil, 0),
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			recs, src, _ := harness.Resolve(context.Background(), g, tasks, now)
			if time.Since(start) > 500*time.Millisecond {
				t.Fatalf("took %v", time.Since(start))
			}
			if src != harness.ReportGuildUnavailable {
				t.Fatalf("source %s", src)
			}
			if r := recs["TM-001"]; r.Status != "blocked" || r.Kind != "question" || r.Source != harness.SourceState {
				t.Errorf("TM-001 %+v", r)
			}
			if r := recs["TM-002"]; r.Status != "unknown" || r.Source != harness.SourceNone {
				t.Errorf("TM-002 %+v", r)
			}
		})
	}
}

func TestNoGuild(t *testing.T) {
	recs, src, _ := harness.Resolve(context.Background(), nil, map[string]*state.File{
		"TM-001": running("/wt/a", &state.Harness{Name: "codex", Status: "working", ObservedAt: "2026-11-22T09:00:00Z"}),
	}, now)
	if src != harness.ReportNone || recs["TM-001"].Status != "working" || recs["TM-001"].Source != harness.SourceState {
		t.Fatalf("%s %+v", src, recs)
	}
	// a task Guild knows nothing about keeps its state record
	g := fakeGuild(recorded, nil, 0)
	recs, _, _ = harness.Resolve(context.Background(), g, map[string]*state.File{
		"TM-002": running("/wt/nowhere", &state.Harness{Name: "codex", Status: "idle", ObservedAt: "2026-11-22T09:00:00Z"}),
	}, now)
	if recs["TM-002"].Source != harness.SourceState || recs["TM-002"].Status != "idle" {
		t.Fatalf("%+v", recs["TM-002"])
	}
}

func TestFromEnv(t *testing.T) {
	if harness.FromEnv(func(string) string { return "" }) != nil {
		t.Fatal("a source without GUILD_SOCKET")
	}
	t.Setenv("PATH", t.TempDir())
	if harness.FromEnv(func(string) string { return "/sock" }) != nil {
		t.Fatal("a source without guild on PATH")
	}
}
