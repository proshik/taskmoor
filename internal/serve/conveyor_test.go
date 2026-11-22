package serve_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

// A16: an explicit profile wins; otherwise harness is a preference among
// the configured profiles; with no profile of that harness, the route.
func TestHarnessIsAPreference(t *testing.T) {
	repo := newRepo(t,
		testrepo.Task{ID: "TM-001", Harness: "codex", Touches: []string{"a/**"}},
		testrepo.Task{ID: "TM-002", Harness: "codex", Extra: "profile: fake\n", Touches: []string{"b/**"}},
		testrepo.Task{ID: "TM-003", Harness: "opencode", Touches: []string{"c/**"}},
		testrepo.Task{ID: "TM-004", Harness: "any", Touches: []string{"d/**"}})
	rg := startWith(t, repo, "wait", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "agent", Harness: "codex", Command: fakeACP(t)})
	}, nil)
	snap, err := rg.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := snap.Projects[0].Routes
	// codexy is the first codex profile; the route (fake) is claude
	for id, want := range map[string]string{"TM-001": "codexy", "TM-002": "fake", "TM-003": "fake", "TM-004": "fake"} {
		if r[id].Profile != want {
			t.Errorf("%s: %+v, want %s", id, r[id], want)
		}
	}
}

func TestRestartWithAProfile(t *testing.T) {
	repo := newRepo(t)
	rg := startWith(t, repo, "approval", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "agent", Harness: "codex", Command: fakeACP(t)})
	}, func(s *serve.Service) {
		s.CodexLogin = func() (string, error) { return "Logged in using ChatGPT", nil }
	})
	ctx := context.Background()
	old, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	rg.waitAsk() // claude waits for an approval
	if _, err := rg.client.Restart(ctx, old.ID, "nope"); err == nil {
		t.Fatal("an unknown profile restarted")
	}
	nu, err := rg.client.Restart(ctx, old.ID, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if nu.ID == old.ID || nu.Worktree != old.Worktree || nu.Branch != old.Branch || nu.Harness != "codex" || nu.RestartOf != old.ID {
		t.Fatalf("%+v", nu)
	}
	rg.waitState(nu.ID, serve.StateComplete)
	if got := rg.session(old.ID); got.State != serve.StateStopped || got.RestartedAs != nu.ID {
		t.Fatalf("old %+v", got)
	}
	h, _ := rg.stateFile("TM-001")["harness"].(map[string]any)
	if h["name"] != "codex" || h["session"] != nu.ID {
		t.Fatalf("harness %v", h)
	}
	eventually(t, "review", func() bool { return rg.stateFile("TM-001")["lifecycle"] == "review" })
	if p := rg.acpCalls("session/prompt"); len(p) != 1 || !strings.Contains(p[0]["prompt"].([]any)[0].(map[string]any)["text"].(string), "git status") {
		t.Fatalf("prompt %v", p)
	}
	// the asks of the old session are gone; it cannot be resumed on the moved claim
	if snap, _ := rg.client.Snapshot(ctx); len(snap.Asks) != 0 {
		t.Fatalf("asks %+v", snap.Asks)
	}
	if _, err := rg.client.Say(ctx, old.ID, "more"); err == nil {
		t.Fatal("the old session resumed on the new session's claim")
	}
}

func TestKeepNBusy(t *testing.T) {
	repo := newRepo(t,
		testrepo.Task{ID: "TM-001", Priority: 2, Touches: []string{"a/**"}},
		testrepo.Task{ID: "TM-002", Priority: 1, Touches: []string{"b/**"}},
		testrepo.Task{ID: "TM-003", Priority: 0, NeedsPerson: true, Touches: []string{"c/**"}},
		testrepo.Task{ID: "TM-004", Priority: 3, Touches: []string{"d/**"}})
	rg := start(t, repo, "wait")
	ctx := context.Background()
	if err := rg.client.Keep(ctx, "repo", 2); err != nil {
		t.Fatal(err)
	}
	running := func() map[string]bool {
		snap, _ := rg.client.Snapshot(ctx)
		out := map[string]bool{}
		for _, s := range snap.Sessions {
			out[s.Task] = true
		}
		return out
	}
	// priority then id, never needs_person: TM-002 (1), then TM-001 (2)
	eventually(t, "two sessions", func() bool { r := running(); return r["TM-001"] && r["TM-002"] })
	time.Sleep(3 * serve.WatchInterval)
	if r := running(); len(r) != 2 || r["TM-003"] {
		t.Fatalf("%v", r)
	}
	snap, _ := rg.client.Snapshot(ctx)
	if snap.Projects[0].KeepBusy != 2 {
		t.Fatalf("keep %d", snap.Projects[0].KeepBusy)
	}
	// a third slot takes TM-004, not the needs_person TM-003
	_ = rg.client.Keep(ctx, "repo", 3)
	eventually(t, "a third session", func() bool { return running()["TM-004"] })
	if running()["TM-003"] {
		t.Fatal("a needs_person task was started")
	}
	if err := rg.client.Keep(ctx, "repo", 99); err == nil {
		t.Fatal("99 accepted")
	}
}
