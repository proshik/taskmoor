package serve

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/state"
)

// Restart is "Restart with a profile" (A24): an operation on the existing
// claim, not a new Run. The runner stops; the state file, worktree,
// branch and their changes stay; a new session of the chosen profile
// starts in the same worktree with the task's assignment and a note on
// the work already there; the claim's harness record moves to it.
func (s *Service) Restart(ctx context.Context, id, profile string) (*Session, error) {
	l, err := s.byID(id)
	if err != nil {
		return nil, err
	}
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	prof := cfg.Profile(profile)
	if prof == nil {
		return nil, refusedf("no profile %q in the service's configuration", profile)
	}
	// everything about the successor that can refuse is checked while the
	// session still runs
	if err := s.attach(l); err != nil {
		return nil, err
	}
	if err := s.preflight(*prof, l.s.Worktree); err != nil {
		return nil, err
	}
	l.sayMu.Lock() // a Say in flight finishes first; later ones see handingOver
	s.mu.Lock()
	switch {
	case s.closing:
		s.mu.Unlock()
		l.sayMu.Unlock()
		return nil, refusedf("taskmoor serve is stopping")
	case l.starting || l.handingOver:
		s.mu.Unlock()
		l.sayMu.Unlock()
		return nil, refusedf("session %s is starting or changing hands; try again in a moment", l.s.ID)
	case l.s.State == StateHandedOver:
		s.mu.Unlock()
		l.sayMu.Unlock()
		return nil, refusedf("session %s is handed over to the terminal", l.s.ID)
	}
	l.handingOver = true // no Say, Handover or second Restart meanwhile
	r := l.runner
	l.stopping = true
	s.mu.Unlock()
	l.sayMu.Unlock()
	defer func() {
		s.mu.Lock()
		l.handingOver = false
		s.mu.Unlock()
	}()
	if r != nil {
		r.stop(5 * time.Second)
		<-r.finished()
	}
	if !s.ownsClaim(l) {
		return nil, refusedf("%s's claim is not this session's any more", l.s.Task)
	}
	rep, err := l.app.Report(ctx)
	if err != nil {
		return nil, err
	}
	t, ok := rep.Find(l.s.Task)
	if !ok {
		return nil, refusedf("%s is no longer a task of %s", l.s.Task, l.s.Project)
	}
	nid := newUUID()
	now := stamp(s.Now())
	rec := &Session{ID: nid, Project: l.s.Project, Task: l.s.Task, Title: l.s.Title, Profile: prof.Name, Harness: prof.Harness,
		Model: taskModel(t, prof), Worktree: l.s.Worktree, Branch: l.s.Branch, State: StatePending, StartedAt: now, UpdatedAt: now,
		ProjectPath: l.s.ProjectPath, Confined: prof.Confined(), Isolation: prof.Isolation(), RestartOf: l.s.ID}
	if prof.Harness == "claude" {
		rec.Resume = fmt.Sprintf("cd %s && claude --resume %s", shellQuote(l.s.Worktree), nid)
	}
	nl := &live{s: rec, app: l.app, profile: *prof, pending: map[string]string{}, stateDir: l.stateDir, rules: l.rules}
	plan, err := s.sandboxFor(nl)
	if err != nil {
		return nil, err
	}
	if err := s.reserve(nl); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sessions[nid] = nl
	s.mu.Unlock()
	undo := func(err error) (*Session, error) {
		s.unreserve(nl)
		s.mu.Lock()
		delete(s.sessions, nid)
		s.mu.Unlock()
		_ = s.journal.remove(nid)
		return nil, err
	}
	if err := s.journal.saveSession(rec); err != nil {
		return undo(err)
	}
	// the claim moves: the harness record names the new session (A24); the
	// predecessor says so first, so a crash between the two leaves it
	// pointing at a successor Open can check
	s.mu.Lock()
	l.s.RestartedAs = nid
	s.mu.Unlock()
	if err := s.journal.saveSession(l.s); err != nil {
		s.mu.Lock()
		l.s.RestartedAs = ""
		s.mu.Unlock()
		return undo(err)
	}
	err = s.update(l, func(f *state.File) {
		f.Lifecycle = state.InProgress
		f.Harness = &state.Harness{Name: prof.Harness, Status: "unknown", ObservedAt: now, Session: nid}
	})
	if err != nil {
		if errors.Is(err, errNotOurs) {
			err = refusedf("%s's claim changed meanwhile; nothing restarted", l.s.Task)
		}
		s.mu.Lock()
		l.s.RestartedAs = ""
		s.mu.Unlock()
		_ = s.journal.saveSession(l.s)
		return undo(err)
	}
	s.addActivity(l, Activity{Type: ActResponse, Text: fmt.Sprintf("restarted with profile %s as session %s", prof.Name, nid), By: "person"})
	s.mu.Lock()
	l.s.State = StateStopped
	l.s.RestartedAs = nid
	s.mu.Unlock()
	s.saveAndPublish(l)
	s.publishSession(nl)
	first := assignment(t, rep.Snapshot, l.s.Worktree, l.s.Branch) + "\n\n" + restartNote
	if err := s.launch(nl, false, first, plan); err != nil {
		return s.snapshotSession(nid), err
	}
	return s.snapshotSession(nid), nil
}

const restartNote = "Note: another agent session already worked on this task in this worktree. " +
	"Its commits are on the branch and its uncommitted changes may still be in the worktree: " +
	"look at `git log` and `git status` first and continue from there; do not start over."

// keeper is "Keep N busy" (A4): per project, the service starts ready tasks
// with their routed profile until N sessions of the project are under way.
type keeper struct {
	mu     sync.Mutex
	failed map[string]time.Time // project/task → when its start failed
	errs   map[string]string    // project → the last failure, for the board
}

// KeepRetry is how long a task whose start failed is skipped.
var KeepRetry = 10 * time.Minute

// SetKeepBusy sets N for a project (0 turns it off).
func (s *Service) SetKeepBusy(project string, n int) error {
	if n < 0 || n > 32 {
		return refusedf("keep busy takes 0 to 32 sessions, not %d", n)
	}
	unlock, err := LockConfig(s.Paths.Config)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	found := false
	for i := range cfg.Projects {
		if cfg.Projects[i].Name == project {
			cfg.Projects[i].KeepBusy = n
			found = true
		}
	}
	if !found {
		return &NotFound{Msg: "no project " + project}
	}
	if err := cfg.Save(s.Paths.Config); err != nil {
		return err
	}
	s.keep.mu.Lock()
	delete(s.keep.errs, project)
	s.keep.mu.Unlock()
	s.bus.publish("tasks", map[string]string{"project": project, "keep": fmt.Sprint(n)})
	go s.fill(context.Background())
	return nil
}

// busy counts a project's sessions under way: starting, working or
// waiting for the person.
func (s *Service) busy(project, path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.sessions {
		if l.s.Project != project || (l.s.ProjectPath != "" && l.s.ProjectPath != path) {
			continue
		}
		switch {
		case l.starting, l.runner != nil:
			n++
		case l.s.State == StatePending || l.s.State == StateActive || l.s.State == StateAwaitingInput:
			n++
		}
	}
	return n
}

var fillMu sync.Mutex

// fill starts ready tasks for every project that keeps sessions busy. The
// readiness is derived again after every claim (A4); needs_person tasks
// are never started; a task whose start failed waits KeepRetry.
func (s *Service) fill(ctx context.Context) {
	if !fillMu.TryLock() {
		return // a fill is under way; it reads the configuration itself
	}
	defer fillMu.Unlock()
	cfg, err := s.Config()
	if err != nil {
		return
	}
	for _, p := range cfg.Projects {
		for !s.isClosing() {
			// the target as it is now: a Keep 0 during the pass stops it
			now, err := s.Config()
			if err != nil {
				break
			}
			cur := now.ProjectByName(p.Name)
			if cur == nil || cur.Path != p.Path || cur.KeepBusy <= 0 || s.busy(p.Name, p.Path) >= cur.KeepBusy {
				break
			}
			rep, err := s.appFor(p).Report(ctx)
			if err != nil {
				s.keepError(p.Name, err.Error())
				break
			}
			t, ok := s.nextReady(p.Name, rep.Ready())
			if !ok {
				break
			}
			if _, err := s.Run(ctx, RunRequest{Project: p.Name, Task: t.ID, auto: true}); err != nil {
				s.keep.mu.Lock()
				s.keep.failed[p.Name+"/"+t.ID] = time.Now()
				s.keep.mu.Unlock()
				s.keepError(p.Name, fmt.Sprintf("%s did not start: %v", t.ID, err))
			}
		}
	}
}

func (s *Service) nextReady(project string, ready []app.TaskView) (app.TaskView, bool) {
	s.keep.mu.Lock()
	defer s.keep.mu.Unlock()
	for _, t := range ready { // priority then id
		if t.NeedsPerson {
			continue // A4: never without the person
		}
		if at, ok := s.keep.failed[project+"/"+t.ID]; ok && time.Since(at) < KeepRetry {
			continue
		}
		return t, true
	}
	return app.TaskView{}, false
}

func (s *Service) keepError(project, msg string) {
	s.keep.mu.Lock()
	changed := s.keep.errs[project] != msg
	s.keep.errs[project] = msg
	s.keep.mu.Unlock()
	if changed {
		s.bus.publish("tasks", map[string]string{"project": project, "keep_error": firstLine(msg, 200)})
	}
}

func (s *Service) keepErrorOf(project string) string {
	s.keep.mu.Lock()
	defer s.keep.mu.Unlock()
	return s.keep.errs[project]
}
