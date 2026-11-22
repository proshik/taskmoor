// Package app joins loading, derivation, harness status and state writes
// into the operations the CLI and the TUI share.
package app

import (
	"context"
	"sort"
	"time"

	"github.com/proshik/taskmoor/internal/derive"
	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/harness"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/state"
	"github.com/proshik/taskmoor/internal/validate"
)

// App is one project opened with fixed options.
type App struct {
	Dir     string
	Source  project.Source
	Harness harness.Source // nil outside Guild
	Now     func() time.Time
	Version string
	// WriterName is the program named in state files' writer field;
	// "taskmoor" when empty ("taskmoor serve" for the service).
	WriterName string
	// CheckOwner makes review and release refuse a task a live
	// `taskmoor serve` owns (A6); the CLI sets it, the service does not.
	CheckOwner bool

	loader project.Loader
}

// Report is a derived view of the whole project: `list --json` and the
// board both show it.
type Report struct {
	Snapshot *project.Snapshot         `json:"-"`
	Statuses map[string]*derive.Status `json:"-"`
	Findings []finding.Finding         `json:"-"`

	Project       string `json:"project"`
	MainBranch    string `json:"main_branch"`
	MainHead      string `json:"main_head"`
	Source        string `json:"source"`
	HarnessSource string `json:"harness_source"`
	// Fingerprint summarises what this report read (see App.Fingerprint).
	Fingerprint string     `json:"-"`
	Tasks       []TaskView `json:"tasks"`
}

// TaskView is one task as `list --json` prints it.
type TaskView struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Kind        string          `json:"kind"`
	Priority    int             `json:"priority"`
	Component   string          `json:"component"`
	File        string          `json:"file"`
	Lifecycle   string          `json:"lifecycle"`
	NeedsPerson bool            `json:"needs_person"`
	DependsOn   []string        `json:"depends_on"`
	Touches     []string        `json:"touches"`
	Ready       bool            `json:"ready"`
	Reasons     []string        `json:"reasons"`
	MergeCommit string          `json:"merge_commit,omitempty"`
	ReleaseTag  string          `json:"release_tag,omitempty"`
	State       *StateView      `json:"state,omitempty"`
	Harness     *harness.Record `json:"harness,omitempty"`

	Task *project.Task `json:"-"`
}

// StateView is a state file with its staleness.
type StateView struct {
	*state.File
	Stale bool `json:"stale"`
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// LoadSnapshot reads the project without deriving anything.
func (a *App) LoadSnapshot(ctx context.Context) (*project.Snapshot, error) {
	return a.loader.Load(ctx, a.Dir, a.Source)
}

// Load reads the project and derives statuses, without harness status.
func (a *App) Load(ctx context.Context) (*project.Snapshot, map[string]*derive.Status, error) {
	snap, err := a.loader.Load(ctx, a.Dir, a.Source)
	if err != nil {
		return nil, nil, err
	}
	return snap, derive.Derive(snap), nil
}

// Report loads, derives, validates and asks the harness source.
func (a *App) Report(ctx context.Context) (*Report, error) {
	snap, statuses, err := a.Load(ctx)
	if err != nil {
		return nil, err
	}
	running := map[string]*state.File{}
	for id, st := range statuses {
		if derive.Running(st.Lifecycle) && st.State != nil {
			running[id] = st.State
		}
	}
	records, hsrc, hfp := harness.Resolve(ctx, a.Harness, running, a.now())
	r := &Report{
		Snapshot:      snap,
		Statuses:      statuses,
		Findings:      validate.Run(snap, statuses),
		Project:       snap.Name,
		MainBranch:    snap.Config.Project.MainBranch,
		MainHead:      snap.MainHead,
		Source:        string(snap.Source),
		HarnessSource: hsrc,
		Fingerprint:   snap.Fingerprint + hfp,
	}
	for _, t := range snap.Tasks {
		st := statuses[t.ID]
		v := TaskView{
			ID: t.ID, Title: t.Title, Kind: t.Kind, Priority: t.Priority, Component: t.Component,
			File: t.File, Lifecycle: st.Lifecycle, NeedsPerson: t.NeedsPerson,
			DependsOn: t.DependsOn, Touches: t.Touches, Ready: st.Ready, Reasons: st.Reasons,
			MergeCommit: st.MergeCommit, ReleaseTag: st.ReleaseTag, Task: t,
		}
		if v.Reasons == nil {
			v.Reasons = []string{}
		}
		if st.State != nil {
			v.State = &StateView{File: st.State, Stale: st.Stale}
		}
		if rec, ok := records[t.ID]; ok {
			rec := rec
			v.Harness = &rec
		}
		r.Tasks = append(r.Tasks, v)
	}
	if r.Tasks == nil {
		r.Tasks = []TaskView{}
	}
	return r, nil
}

// Warnings renders the report's findings as text lines.
func (r *Report) Warnings() []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.String())
	}
	return out
}

// Ready returns the ready tasks, priority then id.
func (r *Report) Ready() []TaskView {
	var out []TaskView
	for _, t := range r.Tasks {
		if t.Ready {
			out = append(out, t)
		}
	}
	SortByPriority(out)
	if out == nil {
		out = []TaskView{}
	}
	return out
}

// SortByPriority orders tasks by priority, then id.
func SortByPriority(ts []TaskView) {
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].Priority != ts[j].Priority {
			return ts[i].Priority < ts[j].Priority
		}
		return ts[i].ID < ts[j].ID
	})
}

// Find returns the task view with id.
func (r *Report) Find(id string) (TaskView, bool) {
	for _, t := range r.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return TaskView{}, false
}

// Fingerprint summarises what a reload would read now, in the form of
// Report.Fingerprint (project.Fingerprint, plus the agent list inside
// Guild). A value that differs from the shown report's means a reload.
func (a *App) Fingerprint(ctx context.Context, last *Report) (string, error) {
	fp, err := project.Fingerprint(ctx, last.Snapshot)
	if err != nil {
		return "", err
	}
	if a.Harness != nil {
		agents, err := a.Harness.Agents(ctx)
		fp += harness.Fingerprint(a.Harness, agents, err)
	}
	return fp, nil
}
