package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/proshik/taskmoor/internal/derive"
	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/gitx"
	"github.com/proshik/taskmoor/internal/harness"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/schema"
	"github.com/proshik/taskmoor/internal/state"
)

// Refused is a transition the task's status does not allow.
type Refused struct {
	Reasons []string
}

func (e *Refused) Error() string {
	return strings.Join(e.Reasons, "; ")
}

func refuse(format string, args ...any) error {
	return &Refused{Reasons: []string{fmt.Sprintf(format, args...)}}
}

// Writer is what state files name as their writer.
func (a *App) Writer() string {
	v := a.Version
	if v == "" {
		v = "dev"
	}
	name := a.WriterName
	if name == "" {
		name = "taskmoor"
	}
	return name + " " + v
}

// ServiceWriter is the writer name of `taskmoor serve`.
const ServiceWriter = "taskmoor serve"

// ownedByService refuses a CLI write to a task a live service owns (A6).
func (a *App) ownedByService(dir string, f *state.File) error {
	if !a.CheckOwner || f == nil {
		return nil
	}
	if state.OwnerHeld(dir, f.ID) {
		return refuse("%s is owned by a running taskmoor serve; stop or release it there", f.ID)
	}
	return nil
}

// UpdateState rewrites a task's valid, non-stale state file under the
// project lock: change edits a copy, which is then validated and published
// with rename. The writer and updated_at are set here.
func (a *App) UpdateState(ctx context.Context, id string, change func(*state.File) error) (*state.File, error) {
	sch, err := schema.Load()
	if err != nil {
		return nil, err
	}
	snap, statuses, err := a.lockedLoad(ctx)
	if err != nil {
		return nil, err
	}
	defer snap.unlock()
	e, exists, err := state.Read(snap.StateDir, id, sch)
	if err != nil {
		return nil, err
	}
	if !exists || !e.Valid() {
		return nil, refuse("%s has no valid state file", id)
	}
	if st := statuses[id]; st != nil && st.Stale {
		return nil, refuse("%s is %s; its state file is stale", id, st.Lifecycle)
	}
	nf := *e.File
	if nf.Harness != nil {
		h := *nf.Harness
		nf.Harness = &h
	}
	if err := change(&nf); err != nil {
		return nil, err
	}
	nf.UpdatedAt = state.Now(a.now())
	nf.Writer = a.Writer()
	if err := state.Replace(snap.StateDir, &nf, sch); err != nil {
		return &nf, err
	}
	return &nf, nil
}

// ClaimArgs are `taskmoor claim`'s inputs.
type ClaimArgs struct {
	ID       string
	Branch   string
	Worktree string
	Harness  string
	Session  string
	Person   bool
	Force    bool
	// Hold, when set, runs under the project lock right after the state
	// file is published (the service takes its owner lock there); an error
	// removes the file again and fails the claim.
	Hold func(stateDir string) error
}

// Claim starts a task: it writes the task's state file with lifecycle
// in_progress. The readiness re-check and the publish run under the
// per-project lock.
func (a *App) Claim(ctx context.Context, args ClaimArgs) (*state.File, error) {
	if !filepath.IsAbs(args.Worktree) {
		return nil, refuse("--worktree %s is not an absolute path", args.Worktree)
	}
	sch, err := schema.Load()
	if err != nil {
		return nil, err
	}
	snap, statuses, err := a.lockedLoad(ctx)
	if err != nil {
		return nil, err
	}
	defer snap.unlock()
	t := snap.Task(args.ID)
	if t == nil {
		return nil, refuse("%s: no such task", args.ID)
	}
	if _, exists, err := state.Read(snap.StateDir, t.ID, sch); err != nil {
		return nil, err
	} else if exists {
		return nil, refuse("%s already has a state file (%s); another writer owns the task", t.ID, state.Path(snap.StateDir, t.ID))
	}
	st := statuses[t.ID]
	if st.Lifecycle != derive.Backlog {
		return nil, refuse("%s is %s, not backlog", t.ID, st.Lifecycle)
	}
	if !st.Ready && !args.Force {
		return nil, &Refused{Reasons: append([]string{t.ID + " is not ready"}, st.Reasons...)}
	}
	// Statuses rest on tasks.toml (components, release rules); with errors
	// in it on main, readiness cannot be trusted, --force or not.
	for _, f := range snap.Findings {
		if f.File == project.ConfigFile && f.Severity == finding.Error {
			return nil, refuse("%s on the main branch has errors (taskmoor validate --source main): %s", project.ConfigFile, f.Message)
		}
	}
	if t.NeedsPerson && !args.Person {
		return nil, refuse("%s needs a person: pass --person when a person started it or permitted this run", t.ID)
	}

	repo := gitx.Repo{Dir: snap.Root}
	wts, err := repo.Worktrees(ctx)
	if err != nil {
		return nil, err
	}
	branchRef := args.Branch
	if !strings.HasPrefix(branchRef, "refs/heads/") {
		branchRef = "refs/heads/" + branchRef
	}
	found := false
	for _, w := range wts {
		if harness.SamePath(w.Path, args.Worktree) {
			if w.Prunable {
				return nil, refuse("worktree %s is gone (git worktree list marks it prunable)", args.Worktree)
			}
			if w.Branch != branchRef {
				have := strings.TrimPrefix(w.Branch, "refs/heads/")
				if have == "" {
					have = "a detached HEAD"
				}
				return nil, refuse("worktree %s has %s checked out, not %s", args.Worktree, have, strings.TrimPrefix(branchRef, "refs/heads/"))
			}
			found = true
			break
		}
	}
	if !found {
		return nil, refuse("%s is not a worktree of this repository (git worktree list)", args.Worktree)
	}
	// The listing is git's record; the directory itself must be there with
	// the branch checked out.
	if info, err := os.Stat(args.Worktree); err != nil || !info.IsDir() {
		return nil, refuse("worktree %s does not exist", args.Worktree)
	}
	if head, err := gitx.HeadBranch(ctx, args.Worktree); err != nil || head != branchRef {
		return nil, refuse("worktree %s does not have %s checked out", args.Worktree, strings.TrimPrefix(branchRef, "refs/heads/"))
	}
	// …and belong to this repository, not another checkout put at the
	// registered path.
	if common, err := (gitx.Repo{Dir: args.Worktree}).CommonDir(ctx); err != nil || !harness.SamePath(common, snap.CommonDir) {
		return nil, refuse("%s is not a worktree of this repository (its git common dir differs)", args.Worktree)
	}
	if !args.Force {
		// The branch must start from a main branch that holds the merged
		// dependencies (spec: "Dependencies and parallel work").
		for _, dep := range t.DependsOn {
			merge := snap.Merges[dep]
			if merge == "" {
				continue
			}
			in, err := repo.IsAncestor(ctx, merge, branchRef)
			if err != nil {
				return nil, err
			}
			if !in {
				return nil, refuse("%s does not contain the merge of dependency %s (%s); start it from the current main branch", strings.TrimPrefix(branchRef, "refs/heads/"), dep, merge[:8])
			}
		}
	}
	base, err := repo.MergeBase(ctx, branchRef, snap.MainHead)
	if err != nil {
		return nil, err
	}

	now := state.Now(a.now())
	f := &state.File{
		Format:    1,
		ID:        t.ID,
		Lifecycle: state.InProgress,
		Worktree:  filepath.Clean(args.Worktree),
		Branch:    strings.TrimPrefix(branchRef, "refs/heads/"),
		Base:      base,
		StartedAt: now,
		UpdatedAt: now,
		Writer:    a.Writer(),
	}
	if args.Harness != "" {
		f.Harness = &state.Harness{Name: args.Harness, Status: "unknown", ObservedAt: now, Session: args.Session}
	}
	var created error
	if err := state.Create(snap.StateDir, f, sch); err != nil {
		if errors.Is(err, state.ErrExists) {
			return nil, refuse("%s already has a state file; another writer owns the task", t.ID)
		}
		var nd *state.NotDurableError
		if !errors.As(err, &nd) {
			return nil, err
		}
		created = err // published, durability unknown
	}
	if args.Hold != nil {
		if err := args.Hold(snap.StateDir); err != nil {
			_ = state.Remove(snap.StateDir, t.ID)
			return nil, err
		}
	}
	return f, created
}

// Review hands a task over: its state file's lifecycle becomes review.
// Already review is a no-op (changed=false).
func (a *App) Review(ctx context.Context, id string) (f *state.File, changed bool, err error) {
	sch, err := schema.Load()
	if err != nil {
		return nil, false, err
	}
	snap, statuses, err := a.lockedLoad(ctx)
	if err != nil {
		return nil, false, err
	}
	defer snap.unlock()
	t := snap.Task(id)
	if t == nil {
		return nil, false, refuse("%s: no such task", id)
	}
	e, exists, err := state.Read(snap.StateDir, id, sch)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, refuse("%s has no state file; claim it first", id)
	}
	if !e.Valid() {
		return nil, false, refuse("%s's state file is invalid (%s); release it and claim again", id, e.Problem)
	}
	if err := a.ownedByService(snap.StateDir, e.File); err != nil {
		return nil, false, err
	}
	if st := statuses[id]; st.Stale {
		return nil, false, refuse("%s is %s; its state file is stale (release it)", id, st.Lifecycle)
	}
	if e.File.Lifecycle == state.Review {
		return e.File, false, nil
	}
	nf := *e.File
	nf.Lifecycle = state.Review
	nf.UpdatedAt = state.Now(a.now())
	nf.Writer = a.Writer()
	if err := state.Replace(snap.StateDir, &nf, sch); err != nil {
		return &nf, true, err // a NotDurableError: published, durability unknown
	}
	return &nf, true, nil
}

// ReleaseIf is Release for a writer that owns a claim: the state file is
// removed only if keep says it is still that writer's, checked under the
// project lock.
func (a *App) ReleaseIf(ctx context.Context, id string, keep func(*state.File) bool) (string, error) {
	snap, statuses, err := a.lockedLoad(ctx)
	if err != nil {
		return "", err
	}
	defer snap.unlock()
	sch, err := schema.Load()
	if err != nil {
		return "", err
	}
	e, exists, err := state.Read(snap.StateDir, id, sch)
	if err != nil {
		return "", err
	}
	if !exists || !e.Valid() || !keep(e.File) {
		return "", refuse("%s's state file is not this session's", id)
	}
	after := derive.Backlog
	if st := statuses[id]; st != nil && (st.Stale || !derive.Running(st.Lifecycle)) {
		after = st.Lifecycle
	}
	if err := state.Remove(snap.StateDir, id); err != nil {
		return after, err
	}
	return after, nil
}

// Release removes a task's state file: from in_progress or review the task
// goes back to backlog; for a stale file the lifecycle stays what git or the
// main branch says. It also removes an invalid file. It returns the
// lifecycle the task has afterwards ("" for an id that is not a task).
func (a *App) Release(ctx context.Context, id string) (string, error) {
	snap, statuses, err := a.lockedLoad(ctx)
	if err != nil {
		return "", err
	}
	defer snap.unlock()
	if snap.Task(id) == nil && !project.TaskIDRe.MatchString(id) {
		return "", refuse("%s: no such task", id)
	}
	sch, err := schema.Load()
	if err != nil {
		return "", err
	}
	if e, exists, err := state.Read(snap.StateDir, id, sch); err == nil && exists && e.File != nil {
		if err := a.ownedByService(snap.StateDir, e.File); err != nil {
			return "", err
		}
	}
	after := ""
	if st := statuses[id]; st != nil {
		after = derive.Backlog
		if st.Stale || !derive.Running(st.Lifecycle) {
			after = st.Lifecycle
		}
	}
	if err := state.Remove(snap.StateDir, id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", refuse("%s has no state file", id)
		}
		return after, err
	}
	return after, nil
}

type lockedSnapshot struct {
	*project.Snapshot
	unlock func()
}

func (a *App) lockedLoad(ctx context.Context) (lockedSnapshot, map[string]*derive.Status, error) {
	root, dir, err := a.stateDir(ctx)
	if err != nil {
		return lockedSnapshot{}, nil, err
	}
	unlock, err := state.Lock(ctx, dir)
	if err != nil {
		return lockedSnapshot{}, nil, err
	}
	// Writes always read the main branch, whatever --source says: the
	// readiness re-check and needs_person must not depend on a local edit.
	// Load the repository resolved before locking (not --repo again, which
	// may be a symlink that changed meanwhile), and check it is the one
	// whose lock is held.
	snap, err := a.loader.Load(ctx, root, project.SourceMain)
	if err != nil {
		unlock()
		return lockedSnapshot{}, nil, err
	}
	if snap.StateDir != dir {
		unlock()
		return lockedSnapshot{}, nil, fmt.Errorf("the repository changed while waiting for the lock (%s, now %s)", dir, snap.StateDir)
	}
	statuses := derive.Derive(snap)
	return lockedSnapshot{Snapshot: snap, unlock: unlock}, statuses, nil
}

// stateDir resolves the repository once: its top level (symlinks
// resolved) and its state directory.
func (a *App) stateDir(ctx context.Context) (root, dir string, err error) {
	if err := gitx.CheckVersion(ctx); err != nil {
		return "", "", &project.LoadError{Err: err}
	}
	top, err := gitx.Toplevel(ctx, a.Dir)
	if err != nil {
		return "", "", &project.LoadError{Err: err}
	}
	if r, err := filepath.EvalSymlinks(top); err == nil {
		top = r
	}
	common, err := gitx.Repo{Dir: top}.CommonDir(ctx)
	if err != nil {
		return "", "", &project.LoadError{Err: err}
	}
	return top, state.Dir(common), nil
}
