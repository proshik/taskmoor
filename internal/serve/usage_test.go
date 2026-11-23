package serve_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

// startUsage runs the claude fake with Claude's plan at left percent
// (-1: the read fails).
func startUsage(t *testing.T, repo *testrepo.Repo, scenario string, left int, mod func(*serve.Config)) *rig {
	t.Helper()
	rg := startWith(t, repo, scenario, mod, func(s *serve.Service) {
		s.UsageClaude = func(context.Context) (serve.Usage, error) {
			if left < 0 {
				return serve.Usage{}, errors.New("no sign-in")
			}
			return serve.Usage{Windows: []serve.Window{{Name: "session", Left: left}}, Left: left}, nil
		}
	})
	eventually(t, "the usage read", func() bool {
		snap, _ := rg.client.Snapshot(context.Background())
		return len(snap.Usage) == 1
	})
	return rg
}

func TestLowPlanNeedsAnyway(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Touches: []string{"a/**"}}, testrepo.Task{ID: "TM-002", Touches: []string{"b/**"}})
	rg := startUsage(t, repo, "wait", 5, nil)
	ctx := context.Background()
	if _, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"}); err == nil || !strings.Contains(err.Error(), "--anyway") {
		t.Fatalf("%v", err)
	}
	// keep busy waits too
	_ = rg.client.Keep(ctx, "repo", 1)
	time.Sleep(2 * serve.WatchInterval)
	snap, _ := rg.client.Snapshot(ctx)
	if len(snap.Sessions) != 0 || !strings.Contains(snap.Projects[0].KeepError, "automatic starts wait") {
		t.Fatalf("%d sessions, keep error %q", len(snap.Sessions), snap.Projects[0].KeepError)
	}
	_ = rg.client.Keep(ctx, "repo", 0)
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001", Anyway: true})
	if err != nil {
		t.Fatal(err)
	}
	if snap, _ := rg.client.Snapshot(ctx); snap.Threshold != 10 || snap.Usage[0].Left != 5 {
		t.Fatalf("%+v", snap.Usage)
	}
	_, _ = rg.client.Stop(ctx, s.ID)
}

func TestUsageGuardSparesAPIBillingAndUnknownPlans(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Touches: []string{"a/**"}}, testrepo.Task{ID: "TM-002", Touches: []string{"b/**"}})
	rg := startUsage(t, repo, "wait", 5, func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "paid", Harness: "claude", Command: c.Profiles[0].Command, APIBilling: true})
	})
	if _, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001", Profile: "paid"}); err != nil {
		t.Fatalf("an API-billed profile was guarded: %v", err)
	}
	rg2 := startUsage(t, newRepo(t), "wait", -1, nil)
	if _, err := rg2.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"}); err != nil {
		t.Fatalf("an unknown plan stopped a run: %v", err)
	}
}

func TestTheMacStaysAwakeWhileAgentsRun(t *testing.T) {
	var mu sync.Mutex
	var on, off int
	old := serve.Awake
	serve.Awake = func(int) (func(), <-chan struct{}, error) {
		mu.Lock()
		on++
		mu.Unlock()
		return func() { mu.Lock(); off++; mu.Unlock() }, make(chan struct{}), nil
	}
	defer func() { serve.Awake = old }()
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Touches: []string{"a/**"}}, testrepo.Task{ID: "TM-002", Touches: []string{"b/**"}})
	rg := start(t, repo, "wait")
	ctx := context.Background()
	a, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	b, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-002"})
	eventually(t, "awake", func() bool { snap, _ := rg.client.Snapshot(ctx); return snap.AgentsRunning })
	_, _ = rg.client.Stop(ctx, a.ID)
	rg.waitState(a.ID, serve.StateStopped)
	mu.Lock()
	if on != 1 || off != 0 {
		t.Fatalf("on %d off %d with one agent left", on, off)
	}
	mu.Unlock()
	_, _ = rg.client.Stop(ctx, b.ID)
	eventually(t, "asleep again", func() bool { mu.Lock(); defer mu.Unlock(); return off == 1 })
	if snap, _ := rg.client.Snapshot(ctx); snap.AgentsRunning {
		t.Fatal("still held awake")
	}
}

// A low plan keeps the dispatcher out: the question is the person's.
func TestDispatcherWaitsOnALowPlan(t *testing.T) {
	rg := startWith(t, newRepo(t), "question", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "agent", Harness: "codex", Command: fakeACP(t)})
		c.DefaultProfile = "agent"
		c.DispatcherProfile = "fake"
	}, func(s *serve.Service) {
		s.CodexLogin = func() (string, error) { return "Logged in using ChatGPT", nil }
		s.UsageClaude = func(context.Context) (serve.Usage, error) { return serve.Usage{Left: 3}, nil }
	}, `FAKE_DISPATCH={"questions":[{"level":"b","option":"SQLite","reason":"x"}]}`)
	eventually(t, "the usage read", func() bool { snap, _ := rg.client.Snapshot(context.Background()); return len(snap.Usage) == 1 })
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	eventually(t, "the decision", func() bool { return len(rg.decisions()) == 1 })
	if d := rg.decisions()[0]; d.Level != "c" || !strings.Contains(d.Reason, "dispatcher waits") || len(rg.dispatchCalls()) != 0 {
		t.Fatalf("%+v", d)
	}
}

// A Say that resumes an ended session starts the agent: guarded.
func TestResumeIsGuarded(t *testing.T) {
	rg := startUsage(t, newRepo(t), "approval", 4, nil)
	ctx := context.Background()
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001", Anyway: true})
	if err != nil {
		t.Fatal(err)
	}
	a := rg.waitAsk()
	_ = rg.client.Answer(ctx, a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	if _, err := rg.client.Say(ctx, s.ID, "more"); err == nil || !strings.Contains(err.Error(), "--anyway") {
		t.Fatalf("%v", err)
	}
	if _, err := rg.client.Say(ctx, s.ID, "more", true); err != nil {
		t.Fatal(err)
	}
}

// A review a low plan postponed runs once the plan is back.
func TestPostponedReviewRunsWhenThePlanRecovers(t *testing.T) {
	old := serve.UsageInterval
	serve.UsageInterval = 300 * time.Millisecond
	defer func() { serve.UsageInterval = old }()
	var mu sync.Mutex
	left := 3
	repo := newRepo(t, testrepo.Task{ID: "TM-001", Reviews: "{spec: 0, code: 1}"})
	rg := startWith(t, repo, "work", func(c *serve.Config) {
		c.Profiles = append(c.Profiles, serve.Profile{Name: "agent", Harness: "codex", Command: fakeACP(t)})
		c.DefaultProfile = "agent"
	}, func(s *serve.Service) {
		s.CodexLogin = func() (string, error) { return "Logged in using ChatGPT", nil }
		s.UsageCodex = func(context.Context) (serve.Usage, error) {
			mu.Lock()
			defer mu.Unlock()
			return serve.Usage{Left: left}, nil
		}
	}, "FAKE_REVUST=clean")
	eventually(t, "the usage read", func() bool { snap, _ := rg.client.Snapshot(context.Background()); return len(snap.Usage) == 1 })
	s, err := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001", Anyway: true})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "postponed", func() bool {
		rv := rg.reviewState(s.ID)
		return rv != nil && strings.HasPrefix(rv.Error, "postponed:") && rv.Done == 0
	})
	mu.Lock()
	left = 60
	mu.Unlock()
	eventually(t, "the round after the plan is back", func() bool { rv := rg.reviewState(s.ID); return rv != nil && rv.State == "clean" && rv.Done == 1 })
}
