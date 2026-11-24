package serve_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

// startSuggest: two claude profiles to choose from (the fake), the fake as
// the dispatcher replying reply.
func startSuggest(t *testing.T, repo *testrepo.Repo, reply string, mod func(*serve.Config), extraEnv ...string) *rig {
	t.Helper()
	return startWith(t, repo, "work", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "small", Harness: "claude", Model: "haiku", Command: c.Profiles[0].Command, Description: "quick, well specified changes"},
			serve.Profile{Name: "keyed", Harness: "claude", Command: c.Profiles[0].Command, APIBilling: true},
			serve.Profile{Name: "signedout", Harness: "codex", Command: fakeACP(t)}) // its program is there; Codex is not signed in
		c.DispatcherProfile = "fake"
		if mod != nil {
			mod(c)
		}
	}, nil, append([]string{"FAKE_DISPATCH=" + reply}, extraEnv...)...)
}

func (rg *rig) route(task string) serve.Route {
	rg.t.Helper()
	snap, err := rg.client.Snapshot(context.Background())
	if err != nil {
		rg.t.Fatal(err)
	}
	for _, p := range snap.Projects {
		if p.Name == "repo" {
			return p.Routes[task]
		}
	}
	rg.t.Fatal("no project")
	return serve.Route{}
}

func TestDispatcherSuggestsAProfile(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Title: "Rename a label", Body: "Rename the Save button to Keep."})
	rg := startSuggest(t, repo, "```json\n{\"profile\": \"small\", \"reason\": \"A one-line rename.\"}\n```", nil)
	ctx := context.Background()
	if r := rg.route("TM-001"); !r.Suggestable || r.Suggested != nil || r.From != "default_profile" || r.Profile != "fake" {
		t.Fatalf("%+v", r)
	}
	sug, err := rg.client.SuggestProfile(ctx, "repo", "TM-001")
	if err != nil || sug.Profile != "small" || sug.Reason != "A one-line rename." {
		t.Fatalf("%+v %v", sug, err)
	}
	calls := rg.dispatchCalls()
	if len(calls) != 1 {
		t.Fatalf("%d calls", len(calls))
	}
	p := calls[0]
	for _, want := range []string{"Rename the Save button to Keep.", "TM-001", "- small: harness claude, model haiku — quick, well specified changes", "- fake: harness claude"} {
		if !strings.Contains(p, want) {
			t.Fatalf("the prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "keyed") || strings.Contains(p, "codexy") || strings.Contains(p, "signedout") {
		t.Fatal("an API-billing or unrunnable profile was offered")
	}
	if r := rg.route("TM-001"); r.Suggested == nil || r.Suggested.Profile != "small" || r.Profile != "fake" {
		t.Fatalf("%+v", r) // the route itself is unchanged: the board preselects
	}
	if _, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001", Suggestion: "nope"}); err == nil || !strings.Contains(err.Error(), "out of date") {
		t.Fatal(err)
	}
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001", Suggestion: sug.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := rg.session(s.ID); got.Profile != "small" || got.Model != "haiku" || got.ChosenBy != "the dispatcher: A one-line rename." {
		t.Fatalf("%+v", got)
	}
}

// Picking the suggested profile by hand is the person's pick.
func TestDispatcherSuggestionManualPickIsThePersons(t *testing.T) {
	rg := startSuggest(t, newRepo(t), `{"profile": "small", "reason": "Small."}`, nil)
	ctx := context.Background()
	if _, err := rg.client.SuggestProfile(ctx, "repo", "TM-001"); err != nil {
		t.Fatal(err)
	}
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001", Profile: "small"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rg.session(s.ID); got.Profile != "small" || got.ChosenBy != "" {
		t.Fatalf("%+v", got)
	}
}

// A form accepted after the task got an explicit route, or after the
// suggested profile changed, is refused.
func TestDispatcherSuggestionStaleSubmission(t *testing.T) {
	ctx := context.Background()
	t.Run("the task gets a profile", func(t *testing.T) {
		repo := newRepo(t)
		rg := startSuggest(t, repo, `{"profile": "small", "reason": "Small."}`, nil)
		sug, err := rg.client.SuggestProfile(ctx, "repo", "TM-001")
		if err != nil {
			t.Fatal(err)
		}
		snap, _ := rg.client.Snapshot(ctx)
		repo.Write(snap.Projects[0].Tasks[0].File, testrepo.TaskFile(testrepo.Task{ID: "TM-001", Extra: "profile: fake\n"}))
		repo.Commit("TM-001 is the fake's")
		if _, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001", Suggestion: sug.ID}); err == nil || !strings.Contains(err.Error(), "out of date") {
			t.Fatal(err)
		}
	})
	off := false
	for name, change := range map[string]func(*serve.Profile){
		"model":   func(p *serve.Profile) { p.Model = "opus" },
		"sandbox": func(p *serve.Profile) { p.Sandbox = &off },
		"domains": func(p *serve.Profile) { p.AllowedDomains = []string{"example.com"} },
	} {
		t.Run("the profile's "+name+" changes under its name", func(t *testing.T) {
			rg := startSuggest(t, newRepo(t), `{"profile": "small", "reason": "Small."}`, nil)
			sug, err := rg.client.SuggestProfile(ctx, "repo", "TM-001")
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := serve.LoadConfig(rg.paths.Config)
			if err != nil {
				t.Fatal(err)
			}
			change(cfg.Profile("small"))
			if err := cfg.Save(rg.paths.Config); err != nil {
				t.Fatal(err)
			}
			if r := rg.route("TM-001"); r.Suggested != nil {
				t.Fatalf("%+v", r)
			}
			if _, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001", Suggestion: sug.ID}); err == nil || !strings.Contains(err.Error(), "out of date") {
				t.Fatal(err)
			}
			if snap, _ := rg.client.Snapshot(ctx); len(snap.Sessions) != 0 {
				t.Fatal("a session started")
			}
		})
	}
}

// A suggestion is for the task as it is on main: a change there drops it;
// a run with another profile does not claim it.
func TestDispatcherSuggestionFollowsTheTask(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Body: "One."}, testrepo.Task{ID: "TM-002", Body: "Two."})
	rg := startSuggest(t, repo, `{"profile": "small", "reason": "Small."}`, nil)
	ctx := context.Background()
	for _, id := range []string{"TM-001", "TM-002"} {
		if _, err := rg.client.SuggestProfile(ctx, "repo", id); err != nil {
			t.Fatal(err)
		}
	}
	repo.Write(rg.route("TM-001").Suggested.Profile+".txt", "x") // an unrelated file: still fresh
	repo.Commit("unrelated")
	if rg.route("TM-001").Suggested == nil {
		t.Fatal("dropped by an unrelated commit")
	}
	snap, _ := rg.client.Snapshot(ctx)
	var file string
	for _, tv := range snap.Projects[0].Tasks {
		if tv.ID == "TM-001" {
			file = tv.File
		}
	}
	repo.Write(file, testrepo.TaskFile(testrepo.Task{ID: "TM-001", Body: "One, and much more."}))
	repo.Commit("TM-001 grows")
	if r := rg.route("TM-001"); r.Suggested != nil {
		t.Fatalf("a stale suggestion: %+v", r)
	}
	if rg.route("TM-002").Suggested == nil {
		t.Fatal("another task's suggestion went")
	}
}

// The dispatcher never second-guesses an explicit choice, and its reply is
// checked.
func TestDispatcherSuggestionRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("routing by kind", func(t *testing.T) {
		repo := newRepo(t)
		repo.Write("tasks/tasks.toml", testrepo.DefaultConfig+"\n[routing]\nfeature = \"fake\"\n")
		repo.Commit("route features")
		rg := startSuggest(t, repo, `{"profile": "small", "reason": "x"}`, nil)
		if r := rg.route("TM-001"); r.Suggestable || r.From != "routing.feature" {
			t.Fatalf("%+v", r)
		}
		if _, err := rg.client.SuggestProfile(ctx, "repo", "TM-001"); err == nil || !strings.Contains(err.Error(), "routing.feature") {
			t.Fatal(err)
		}
		if len(rg.dispatchCalls()) != 0 {
			t.Fatal("called")
		}
	})
	t.Run("the task's own profile or model", func(t *testing.T) {
		repo := newRepo(t, testrepo.Task{ID: "TM-001", Extra: "profile: fake\n"}, testrepo.Task{ID: "TM-002", Extra: "model: opus\n"})
		rg := startSuggest(t, repo, `{"profile": "small", "reason": "x"}`, nil)
		for _, id := range []string{"TM-001", "TM-002"} {
			if r := rg.route(id); r.Suggestable || r.From != "task" {
				t.Fatalf("%s %+v", id, r)
			}
			if _, err := rg.client.SuggestProfile(ctx, "repo", id); err == nil {
				t.Fatal(id)
			}
		}
	})
	t.Run("an explicit route to a missing profile", func(t *testing.T) {
		repo := newRepo(t)
		repo.Write("tasks/tasks.toml", testrepo.DefaultConfig+"\n[routing]\nfeature = \"gone\"\n")
		repo.Commit("route features to a removed profile")
		rg := startSuggest(t, repo, `{"profile": "small", "reason": "x"}`, nil)
		if _, err := rg.client.SuggestProfile(ctx, "repo", "TM-001"); err == nil || !strings.Contains(err.Error(), "routing.feature") {
			t.Fatal(err)
		}
		if len(rg.dispatchCalls()) != 0 {
			t.Fatal("called")
		}
	})
	t.Run("dispatcher off", func(t *testing.T) {
		rg := startSuggest(t, newRepo(t), `{"profile": "small", "reason": "x"}`, func(c *serve.Config) { c.DispatcherProfile = "" })
		if r := rg.route("TM-001"); r.Suggestable {
			t.Fatalf("%+v", r)
		}
		if _, err := rg.client.SuggestProfile(ctx, "repo", "TM-001"); err == nil || !strings.Contains(err.Error(), "off") {
			t.Fatal(err)
		}
	})
	for name, reply := range map[string]string{
		"unknown profile": `{"profile": "huge", "reason": "x"}`,
		"not runnable":    `{"profile": "codexy", "reason": "x"}`,
		"not signed in":   `{"profile": "signedout", "reason": "x"}`,
		"API billing":     `{"profile": "keyed", "reason": "x"}`,
		"no reason":       `{"profile": "small", "reason": " "}`,
		"not JSON":        `small, I think`,
	} {
		t.Run(name, func(t *testing.T) {
			rg := startSuggest(t, newRepo(t), reply, nil)
			if _, err := rg.client.SuggestProfile(ctx, "repo", "TM-001"); err == nil || !strings.Contains(err.Error(), "not usable") {
				t.Fatal(err)
			}
			if r := rg.route("TM-001"); r.Suggested != nil {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// Two suggestions at a time; a third is told the dispatcher is busy and
// reads nothing.
func TestDispatcherSuggestionsAreBounded(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "go")
	repo := newRepo(t, testrepo.Task{ID: "TM-001"}, testrepo.Task{ID: "TM-002"})
	broken := filepath.Join(t.TempDir(), "gone")
	rg := startSuggest(t, repo, `{"profile": "small", "reason": "Small."}`, func(c *serve.Config) {
		c.Projects = append(c.Projects, serve.Project{Name: "broken", Path: broken})
	}, "FAKE_DISPATCH_WAIT="+gate)
	ctx := context.Background()
	done := make(chan error, 2)
	for _, id := range []string{"TM-001", "TM-002"} {
		go func() { _, err := rg.client.SuggestProfile(ctx, "repo", id); done <- err }()
	}
	eventually(t, "two calls", func() bool { return len(rg.dispatchCalls()) == 2 })
	// busy before reading the project: a broken one says busy, not broken
	if _, err := rg.client.SuggestProfile(ctx, "broken", "TM-001"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n := len(rg.dispatchCalls()); n != 2 {
		t.Fatalf("%d calls", n)
	}
	if _, err := rg.client.SuggestProfile(ctx, "broken", "TM-001"); err == nil || strings.Contains(err.Error(), "busy") {
		t.Fatal(err)
	}
}

// A suggestion waiting for a slot while the plan falls below the
// threshold never makes its call.
func TestDispatcherSuggestionRechecksThePlanAfterTheWait(t *testing.T) {
	var left atomic.Int32
	left.Store(50)
	repo := newRepo(t)
	rg := startWith(t, repo, "work", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "small", Harness: "claude", Model: "haiku", Command: c.Profiles[0].Command})
		c.DispatcherProfile = "fake"
	}, func(s *serve.Service) {
		s.UsageClaude = func(context.Context) (serve.Usage, error) {
			l := int(left.Load())
			return serve.Usage{Windows: []serve.Window{{Name: "session", Left: l}}, Left: l}, nil
		}
	}, `FAKE_DISPATCH={"profile": "small", "reason": "x"}`)
	ctx := context.Background()
	eventually(t, "the usage read", func() bool { snap, _ := rg.client.Snapshot(ctx); return len(snap.Usage) == 1 })
	release := rg.svc.HoldDispatchSlots()
	done := make(chan error, 1)
	go func() { _, err := rg.client.SuggestProfile(ctx, "repo", "TM-001"); done <- err }()
	eventually(t, "the request waits for a slot", func() bool { return rg.svc.SuggestionsInFlight() == 1 })
	left.Store(3)
	rg.svc.ReadUsageNow()
	release()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "waits") {
		t.Fatal(err)
	}
	if n := len(rg.dispatchCalls()); n != 0 {
		t.Fatalf("%d calls", n)
	}
}
