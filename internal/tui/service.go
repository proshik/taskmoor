package tui

import (
	"context"
	"fmt"
	"os"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/derive"
	"github.com/proshik/taskmoor/internal/gitx"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/serve"
)

// ServiceLoader reads the board from taskmoor serve: its tasks and
// sessions for one project. The local project is still read for the
// header and for the refresh fingerprint (merges and tags do not go
// through the service's events).
type ServiceLoader struct {
	Client  ServiceClient
	Project string
	Local   *app.App
}

// ServiceClient is the part of serve.Client the board uses.
type ServiceClient interface {
	Snapshot(ctx context.Context) (*serve.Snapshot, error)
	Position(ctx context.Context) (string, int64, error)
}

// autoLoader decides at every load: the service when it runs and has the
// project registered (A21), otherwise the local project. A board started
// before the service, or one whose service stops or restarts, follows.
// With --source worktree the board stays local: the service reads tasks
// from the main branch only.
type autoLoader struct {
	a    *app.App
	find func(ctx context.Context, a *app.App) *ServiceLoader
}

func (l *autoLoader) service(ctx context.Context) *ServiceLoader {
	if l.a.Source != project.SourceMain {
		return nil
	}
	return l.find(ctx, l.a)
}

// Report implements Loader.
func (l *autoLoader) Report(ctx context.Context) (*app.Report, error) {
	if sl := l.service(ctx); sl != nil {
		if rep, err := sl.Report(ctx); err == nil {
			return rep, nil
		}
	}
	return l.a.Report(ctx)
}

// Fingerprint implements Loader; a switch between the two sources changes
// it, so the board reloads.
func (l *autoLoader) Fingerprint(ctx context.Context, last *app.Report) (string, error) {
	if sl := l.service(ctx); sl != nil {
		if fp, err := sl.Fingerprint(ctx, last); err == nil {
			return fp, nil
		}
	}
	return l.a.Fingerprint(ctx, last)
}

// serviceLoaderFor connects to the service of the default store when it
// runs and knows the project of a; nil otherwise.
func serviceLoaderFor(ctx context.Context, a *app.App) *ServiceLoader {
	paths, err := serve.DefaultPaths(os.Getenv)
	if err != nil {
		return nil
	}
	c, err := serve.Connect(paths.Store)
	if err != nil {
		return nil
	}
	cfg, err := serve.LoadConfig(paths.Config)
	if err != nil {
		return nil
	}
	top, err := gitx.Toplevel(ctx, a.Dir)
	if err != nil {
		return nil
	}
	mine, err := gitx.Repo{Dir: top}.CommonDir(ctx)
	if err != nil {
		return nil
	}
	for _, p := range cfg.Projects {
		if common, err := (gitx.Repo{Dir: p.Path}).CommonDir(ctx); err == nil && common == mine {
			return &ServiceLoader{Client: c, Project: p.Name, Local: a}
		}
	}
	return nil
}

// Report implements Loader.
func (l *ServiceLoader) Report(ctx context.Context) (*app.Report, error) {
	local, err := l.Local.Report(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := l.Client.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("taskmoor serve: %w", err)
	}
	var ps *serve.ProjectState
	for i := range snap.Projects {
		if snap.Projects[i].Name == l.Project {
			ps = &snap.Projects[i]
		}
	}
	if ps == nil {
		return nil, fmt.Errorf("taskmoor serve no longer has project %s", l.Project)
	}
	latest := map[string]serve.Session{}
	for _, s := range snap.Sessions {
		if s.Project == l.Project && s.StartedAt >= latest[s.Task].StartedAt {
			latest[s.Task] = s
		}
	}
	// the service's view of each task, with the local task file for the
	// detail view
	rep := *local
	rep.Conveyor = true
	rep.HarnessSource = "taskmoor serve"
	rep.Tasks = nil
	for _, t := range ps.Tasks {
		if lt, ok := local.Find(t.ID); ok {
			t.Task = lt.Task
		}
		if t.Task == nil {
			continue // not readable locally: nothing to show in detail
		}
		t.Column = conveyorColumn(t, latest[t.ID])
		rep.Tasks = append(rep.Tasks, t)
	}
	// the ideas under way (stage 8): shown, their work is the board's
	for _, i := range snap.Ideas {
		if i.Project != l.Project || (i.State != serve.IdeaNew && i.State != serve.IdeaGrilling && i.State != serve.IdeaProposed) {
			continue
		}
		if i.ProjectPath != "" && i.ProjectPath != ps.Path {
			continue // an idea of an earlier checkout of the same name
		}
		body := i.Text
		if i.Branch != "" {
			body += "\n\n---\nbranch " + i.Branch + ", worktree " + i.Worktree
		}
		rep.Tasks = append(rep.Tasks, app.TaskView{ID: i.ID, Title: i.Title, Lifecycle: i.State, Column: colIdeas,
			Reasons: []string{}, Touches: i.Tasks, Task: &project.Task{ID: i.ID, Title: i.Title, Body: body}})
	}
	if rep.Tasks == nil {
		rep.Tasks = []app.TaskView{}
	}
	epoch, rev := snap.Epoch, snap.Rev
	rep.Fingerprint = local.Fingerprint + fmt.Sprintf("\x00serve %s %d", epoch, rev)
	return &rep, nil
}

// Fingerprint implements Loader: the local project's plus the service's
// position.
func (l *ServiceLoader) Fingerprint(ctx context.Context, last *app.Report) (string, error) {
	fp, err := l.Local.Fingerprint(ctx, last)
	if err != nil {
		return "", err
	}
	epoch, rev, err := l.Client.Position(ctx)
	if err != nil {
		return "", fmt.Errorf("taskmoor serve: %w", err)
	}
	return fp + fmt.Sprintf("\x00serve %s %d", epoch, rev), nil
}

func conveyorColumn(t app.TaskView, s serve.Session) string {
	switch t.Lifecycle {
	case derive.Backlog:
		if t.Ready {
			return colReady
		}
		return derive.Backlog
	case derive.InProgress:
		if s.State == serve.StateAwaitingInput {
			return colNeeds
		}
		return colRunning
	}
	return t.Lifecycle
}
