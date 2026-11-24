package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

type fakeService struct {
	snap *serve.Snapshot
	rev  int64
}

func (f *fakeService) Snapshot(context.Context) (*serve.Snapshot, error) { return f.snap, nil }
func (f *fakeService) Position(context.Context) (string, int64, error)   { return "e", f.rev, nil }

func TestServiceBoardShowsConveyorColumns(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(
		testrepo.Task{ID: "TM-001", Title: "Ready one"},
		testrepo.Task{ID: "TM-002", Title: "Blocked one", DependsOn: []string{"TM-003"}},
		testrepo.Task{ID: "TM-003", Title: "Running one"},
		testrepo.Task{ID: "TM-004", Title: "Asking one"},
	)
	r.WriteState(testrepo.State{ID: "TM-003"})
	r.WriteState(testrepo.State{ID: "TM-004"})
	local := &app.App{Dir: r.Dir, Source: project.SourceMain}
	lrep, err := local.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeService{rev: 1, snap: &serve.Snapshot{Epoch: "e", Rev: 1,
		Projects: []serve.ProjectState{{Name: "repo", Tasks: lrep.Tasks}},
		Sessions: []serve.Session{
			{ID: "s3", Project: "repo", Task: "TM-003", State: serve.StateActive, StartedAt: "2026-11-22T10:00:00Z"},
			{ID: "s4", Project: "repo", Task: "TM-004", State: serve.StateAwaitingInput, StartedAt: "2026-11-22T10:00:00Z"},
		},
		Ideas: []serve.Idea{
			{ID: "idea-1a2b3c4d", Project: "repo", Title: "Faster board", Text: "it is slow", State: serve.IdeaGrilling},
			{ID: "idea-99999999", Project: "repo", Title: "Old", State: serve.IdeaApproved}, // done: not shown
			{ID: "idea-88888888", Project: "other", Title: "Elsewhere", State: serve.IdeaNew},
			{ID: "idea-77777777", Project: "repo", ProjectPath: "/an/earlier/checkout", Title: "Before", State: serve.IdeaNew},
		}}}
	sl := &ServiceLoader{Client: fs, Project: "repo", Local: local}
	rep, err := sl.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), sl, rep)
	m.glamStyle = "notty"
	m = send(t, m, tea.WindowSizeMsg{Width: 200, Height: 40})
	v := view(m)
	must(t, v, "Backlog 1", "Ready 1", "Running 1", "Needs you 1", "Review 0", "TM-001", "TM-004", "serve ●")
	must(t, v, "Ideas 1", "idea-1a2b3c4d", "Faster board")
	must(t, v, "4 tasks") // the idea is not one of them
	mustNot(t, v, "idea-99999999", "idea-88888888", "idea-77777777")

	// a change at the service moves the fingerprint
	fp1, _ := sl.Fingerprint(context.Background(), rep)
	fs.rev = 2
	fp2, _ := sl.Fingerprint(context.Background(), rep)
	if fp1 == fp2 {
		t.Fatal("the service's position is not in the fingerprint")
	}
}

func TestAutoLoaderFollowsTheService(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	local := &app.App{Dir: r.Dir, Source: project.SourceMain}
	lrep, _ := local.Report(context.Background())
	fs := &fakeService{rev: 1, snap: &serve.Snapshot{Epoch: "e", Rev: 1, Projects: []serve.ProjectState{{Name: "repo", Tasks: lrep.Tasks}}}}
	var up bool
	al := &autoLoader{a: local, find: func(context.Context, *app.App) *ServiceLoader {
		if !up {
			return nil
		}
		return &ServiceLoader{Client: fs, Project: "repo", Local: local}
	}}
	ctx := context.Background()
	rep, _ := al.Report(ctx)
	if rep.Conveyor {
		t.Fatal("conveyor without a service")
	}
	up = true // the service starts
	if fp, _ := al.Fingerprint(ctx, rep); fp == rep.Fingerprint {
		t.Fatal("the service starting does not change the fingerprint")
	}
	if rep, _ = al.Report(ctx); !rep.Conveyor {
		t.Fatal("no conveyor with the service up")
	}
	up = false // and stops
	if fp, _ := al.Fingerprint(ctx, rep); fp == rep.Fingerprint {
		t.Fatal("the service stopping does not change the fingerprint")
	}
	if rep, _ = al.Report(ctx); rep.Conveyor {
		t.Fatal("still conveyor after the service stopped")
	}
	// --source worktree stays local
	up = true
	wt := &autoLoader{a: &app.App{Dir: r.Dir, Source: project.SourceWorktree}, find: al.find}
	if rep, _ := wt.Report(ctx); rep.Conveyor {
		t.Fatal("conveyor with --source worktree")
	}
}

func TestServiceGoingAwayKeepsAValidColumn(t *testing.T) {
	r := testrepo.New(t)
	r.AddTasks(testrepo.Task{ID: "TM-001"})
	local := &app.App{Dir: r.Dir, Source: project.SourceMain}
	lrep, _ := local.Report(context.Background())
	fs := &fakeService{rev: 1, snap: &serve.Snapshot{Epoch: "e", Rev: 1, Projects: []serve.ProjectState{{Name: "repo", Tasks: lrep.Tasks}}}}
	sl := &ServiceLoader{Client: fs, Project: "repo", Local: local}
	crep, _ := sl.Report(context.Background())
	m := New(context.Background(), sl, crep)
	m.glamStyle = "notty"
	m = send(t, m, tea.WindowSizeMsg{Width: 100, Height: 30}) // narrow: tabs
	for i := 0; i < 7; i++ {
		m = keys(t, m, "<right>") // the empty Done column, index 7 (after Ideas)
	}
	if m.currentColumn() != "done" {
		t.Fatalf("column %s", m.currentColumn())
	}
	m = send(t, m, reportMsg{rep: lrep}) // the service stopped: the local board
	if m.currentColumn() != "done" {
		t.Fatalf("column %s after the switch", m.currentColumn())
	}
	_ = view(m) // must not panic
}
