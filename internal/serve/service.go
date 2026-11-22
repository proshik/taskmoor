package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/derive"
	"github.com/proshik/taskmoor/internal/gitx"
	"github.com/proshik/taskmoor/internal/harness"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/state"
)

// Service is the running `taskmoor serve`.
type Service struct {
	Paths   Paths
	Version string
	Now     func() time.Time
	Env     []string // the base environment for agents (os.Environ())
	Home    string   // for Claude's user settings

	// Opener runs the command that shows a worktree (Finder, an editor).
	Opener func(argv []string) error

	journal *journal
	bus     *bus
	auth    *webAuth
	lock    *os.File // the store's serve.lock, held from Open to Close

	// runners counts runner goroutines until their last callback returns,
	// so Shutdown leaves nothing writing behind it.
	runners sync.WaitGroup
	// launches counts admitted launches until their runner is bound (or the
	// launch fails), so Shutdown waits for them before stopping runners.
	launches sync.WaitGroup

	mu       sync.Mutex
	closing  bool
	sessions map[string]*live
	asks     map[string]*Ask
	order    []string            // ask ids in arrival order
	owners   map[string]*os.File // owner locks of the tasks this service owns (A6)
}

// live is a session the service holds: its record and, while the process
// runs, its runner.
type live struct {
	s        *Session
	runner   *claudeRunner
	starting bool // a launch is under way: no second one
	// handingOver: a handover to the terminal is under way: no launch
	handingOver bool
	rules       Rules
	app         *app.App
	profile     Profile
	stateDir    string
	// pending maps a Claude control request id to the ask waiting on it.
	pending map[string]string
	// stopping marks a stop the person asked for.
	stopping bool
	// stmu orders a session's state changes and their state-file writes.
	stmu sync.Mutex
}

// Ask is a question or an approval waiting for the person.
type Ask struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"` // question | approval
	Session   string     `json:"session"`
	Project   string     `json:"project"`
	Task      string     `json:"task"`
	CreatedAt string     `json:"created_at"`
	Questions []Question `json:"questions,omitempty"` // kind question
	Tool      string     `json:"tool,omitempty"`      // kind approval
	// Detail is the operation as the agent will run it, in full: the
	// command, or the file and what is written.
	Detail string `json:"detail,omitempty"`
	Reason string `json:"reason,omitempty"`

	requestID string
	input     map[string]any
	opened    bool // announced: visible in snapshots and answerable
}

// Question is a normalized question (see "Normalized question").
type Question struct {
	Header      string   `json:"header,omitempty"`
	Question    string   `json:"question"`
	Options     []Option `json:"options"`
	MultiSelect bool     `json:"multi_select,omitempty"`
	// Recommended is the index of the recommended option, -1 for none.
	Recommended int `json:"recommended"`
}

// Option is one choice; Label is shown without "(Recommended)".
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	raw         string // the label as the harness sent it
}

// Answer is the person's reply to an ask.
type Answer struct {
	// Choices holds, per question in order, the chosen option labels.
	Choices [][]string `json:"choices,omitempty"`
	// Text is a free-text answer (to the first question) or a note on an
	// approval.
	Text string `json:"text,omitempty"`
	// Allow answers an approval.
	Allow bool `json:"allow,omitempty"`
}

// Refused is a request the service declines, with the reason (HTTP 409).
type Refused struct{ Msg string }

func (e *Refused) Error() string { return e.Msg }

func refusedf(format string, args ...any) error { return &Refused{Msg: fmt.Sprintf(format, args...)} }

// NotFound is an unknown project, session or ask (HTTP 404).
type NotFound struct{ Msg string }

func (e *NotFound) Error() string { return e.Msg }

// errNotOurs: the task's claim belongs to another session (released and
// claimed again since); this session must not touch it.
var errNotOurs = errors.New("the task's claim is no longer this session's")

// Open prepares the service: it takes the store's lock (one service per
// store), then recovers the sessions left by a previous run — their
// processes are gone, so they are stale.
func Open(paths Paths, version string, env []string, home string) (*Service, error) {
	// absolute: the rules and the sandbox compare them with absolute paths
	// and Claude runs in another directory
	for _, p := range []*string{&paths.Config, &paths.Store} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return nil, err
		}
		*p = abs
	}
	if err := os.MkdirAll(paths.Store, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(paths.Store, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another taskmoor serve is running for %s", paths.Store)
	}
	j, err := newJournal(paths.Store)
	if err != nil {
		lock.Close()
		return nil, err
	}
	s := &Service{Paths: paths, Version: version, Now: time.Now, Env: env, Home: home, lock: lock, Opener: runDetached,
		journal: j, bus: newBus(), sessions: map[string]*live{}, asks: map[string]*Ask{}, owners: map[string]*os.File{}}
	s.auth = newWebAuth(func() time.Time { return s.Now() }, filepath.Join(paths.Store, "web-sessions.json"))
	old, err := j.sessions()
	if err != nil {
		lock.Close()
		return nil, err
	}
	for _, rec := range old {
		l := &live{s: rec, pending: map[string]string{}}
		s.sessions[rec.ID] = l
		stale := false
		switch rec.State {
		case StatePending, StateActive, StateAwaitingInput:
			stale = true
			rec.State = StateStale
			rec.UpdatedAt = stamp(s.Now())
			_ = j.saveSession(rec)
			_, _ = j.append(rec.ID, Activity{At: rec.UpdatedAt, Type: ActError, Text: "the service restarted; the session's process is gone (say something to resume it)"})
		}
		// a session whose claim is still its own is this service's again
		if s.attach(l) != nil {
			continue
		}
		if !s.ownsClaim(l) {
			if rec.HandoverPending { // the claim already let go: only the record is left
				s.finishHandover(l)
			}
			continue
		}
		if rec.HandoverPending || rec.State == StateHandedOver {
			s.finishHandover(l) // a handover a crash interrupted
			continue
		}
		_ = s.holdOwner(l)
		switch {
		case stale:
			s.writeHarness(l, StateStale)
		case rec.State == StateComplete:
			// a crash between the session's completion and the handover
			_ = s.update(l, func(f *state.File) {
				f.Lifecycle = state.Review
				f.Harness = &state.Harness{Name: rec.Harness, Status: "done", ObservedAt: stamp(s.Now()), Session: rec.ID}
			})
		}
	}
	return s, nil
}

// Close lets go of the store lock (after Shutdown).
func (s *Service) Close() {
	if s.lock != nil {
		s.lock.Close()
		s.lock = nil
	}
}

// Config reads the configuration now (it may change between requests:
// `taskmoor project add` edits the file).
func (s *Service) Config() (*Config, error) { return LoadConfig(s.Paths.Config) }

func (s *Service) appFor(p Project) *app.App {
	return &app.App{Dir: p.Path, Source: project.SourceMain, Now: s.Now, Version: s.Version, WriterName: app.ServiceWriter}
}

func ownerKey(stateDir, task string) string { return stateDir + "\x00" + task }

func (s *Service) holdOwner(l *live) error { return s.holdOwnerIn(l.stateDir, l.s.Task) }

func (s *Service) holdOwnerIn(stateDir, task string) error {
	key := ownerKey(stateDir, task)
	s.mu.Lock()
	_, held := s.owners[key]
	s.mu.Unlock()
	if held {
		return nil
	}
	f, err := state.HoldOwner(stateDir, task)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.owners[key] = f
	s.mu.Unlock()
	return nil
}

func (s *Service) dropOwner(l *live) {
	key := ownerKey(l.stateDir, l.s.Task)
	s.mu.Lock()
	f := s.owners[key]
	delete(s.owners, key)
	s.mu.Unlock()
	if f != nil {
		f.Close()
	}
}

// ownsClaim reports whether the task's state file is this session's: its
// harness session and worktree.
func (s *Service) ownsClaim(l *live) bool {
	sch, err := schemaLoad()
	if err != nil {
		return false
	}
	e, ok, err := state.Read(l.stateDir, l.s.Task, sch)
	return err == nil && ok && e.Valid() && claimIs(e.File, l.s)
}

func claimIs(f *state.File, s *Session) bool {
	return f.Harness != nil && f.Harness.Session == s.ID && harness.SamePath(f.Worktree, s.Worktree)
}

// update rewrites the task's state file if its claim is still this
// session's.
func (s *Service) update(l *live, change func(*state.File)) error {
	_, err := l.app.UpdateState(context.Background(), l.s.Task, func(f *state.File) error {
		if !claimIs(f, l.s) {
			return errNotOurs
		}
		change(f)
		return nil
	})
	var nd *state.NotDurableError
	if errors.As(err, &nd) {
		return nil
	}
	return err
}

// RunRequest starts a session on a task.
type RunRequest struct {
	Project string `json:"project"`
	Task    string `json:"task"`
	Profile string `json:"profile,omitempty"`
	Person  bool   `json:"person,omitempty"`
}

// Run creates the task's worktree and branch, records the session, claims
// the task and starts its agent with the task as the assignment.
func (s *Service) Run(ctx context.Context, req RunRequest) (*Session, error) {
	if s.isClosing() {
		return nil, refusedf("taskmoor serve is stopping")
	}
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	p := cfg.ProjectByName(req.Project)
	if p == nil {
		return nil, &NotFound{Msg: fmt.Sprintf("no project %q (taskmoor project list)", req.Project)}
	}
	a := s.appFor(*p)
	rep, err := a.Report(ctx)
	if err != nil {
		return nil, err
	}
	t, ok := rep.Find(req.Task)
	if !ok {
		return nil, &NotFound{Msg: fmt.Sprintf("%s: no such task in %s", req.Task, p.Name)}
	}
	if t.Lifecycle != derive.Backlog {
		return nil, refusedf("%s is %s, not backlog", t.ID, t.Lifecycle)
	}
	if !t.Ready {
		return nil, refusedf("%s is not ready: %s", t.ID, strings.Join(t.Reasons, "; "))
	}
	if t.NeedsPerson && !req.Person {
		return nil, refusedf("%s needs a person: run it with --person when you start or permit it (A4)", t.ID)
	}
	prof, err := chooseProfile(cfg, rep.Snapshot.Config, t, req.Profile)
	if err != nil {
		return nil, err
	}
	if err := s.preflight(*prof, rep.Snapshot.Root); err != nil {
		return nil, err
	}

	// worktree and branch (A23)
	slug := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(t.File), t.ID+"-"), ".md")
	branch := "task/" + t.ID + "-" + slug
	root := rep.Snapshot.Root
	wt := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-"+strings.ToLower(t.ID))
	if _, err := os.Lstat(wt); err == nil {
		return nil, refusedf("%s already exists; the service does not reuse or remove worktrees (A7)", wt)
	}
	repo := gitx.Repo{Dir: root}
	if _, ok, err := repo.ResolveBranch(ctx, branch); err != nil {
		return nil, err
	} else if ok {
		return nil, refusedf("branch %s already exists; release or rename it first", branch)
	}
	base := rep.Snapshot.MainHead
	if _, err := repo.Run(ctx, "worktree", "add", "-q", "-b", branch, wt, base); err != nil {
		return nil, err
	}
	id := newUUID()
	now := stamp(s.Now())
	if err := s.preflight(*prof, wt); err != nil { // the worktree's own settings
		if kept := undoWorktree(repo, wt, branch, base); kept != "" {
			err = fmt.Errorf("%w (%s)", err, kept)
		}
		return nil, err
	}
	rec := &Session{ID: id, Project: p.Name, Task: t.ID, Title: t.Title, Profile: prof.Name, Harness: prof.Harness,
		Model: prof.Model, Worktree: wt, Branch: branch, State: StatePending, StartedAt: now, UpdatedAt: now,
		Resume: fmt.Sprintf("cd %s && claude --resume %s", shellQuote(wt), id), ProjectPath: p.Path, Confined: prof.Confined()}
	l := &live{s: rec, app: a, profile: *prof, pending: map[string]string{}, stateDir: rep.Snapshot.StateDir,
		rules: Rules{Worktree: wt, MainBranch: rep.MainBranch, MainCheckout: root, Protected: s.protected()}}
	plan, err := s.sandboxFor(l) // refused here, nothing is claimed yet
	if err != nil {
		if kept := undoWorktree(repo, wt, branch, base); kept != "" {
			err = fmt.Errorf("%w (%s)", err, kept)
		}
		return nil, err
	}
	// the launch is reserved before anything is published, so Release and
	// Shutdown see it from the start
	if err := s.reserve(l); err != nil {
		_ = undoWorktree(repo, wt, branch, base)
		return nil, err
	}
	fail := func(err error) (*Session, error) {
		s.unreserve(l)
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		_ = s.journal.remove(id)
		if kept := undoWorktree(repo, wt, branch, base); kept != "" {
			err = fmt.Errorf("%w (%s)", err, kept)
		}
		return nil, err
	}
	s.mu.Lock()
	s.sessions[id] = l
	s.mu.Unlock()
	// the session is recorded before the claim, so a claim never exists
	// without a session that can be found, stopped and released
	if err := s.journal.saveSession(rec); err != nil {
		return fail(err)
	}
	hold := func(stateDir string) error { return s.holdOwnerIn(stateDir, t.ID) }
	if _, err := a.Claim(ctx, app.ClaimArgs{ID: t.ID, Branch: branch, Worktree: wt, Harness: prof.Harness, Session: id, Person: req.Person, Hold: hold}); err != nil {
		var nd *state.NotDurableError
		if !errors.As(err, &nd) {
			var ref *app.Refused
			if errors.As(err, &ref) {
				return fail(refusedf("%s", ref.Error()))
			}
			return fail(err)
		}
	}
	s.publishSession(l)
	if err := s.launch(l, false, assignment(t, rep.Snapshot, wt, branch), plan); err != nil {
		return s.snapshotSession(id), err
	}
	return s.snapshotSession(id), nil
}

// undoWorktree removes the worktree and branch a refused Run just made,
// only while they hold nothing new: the branch still at base and the
// worktree clean. It returns what was kept and why, "" when all went.
func undoWorktree(repo gitx.Repo, wt, branch, base string) string {
	ctx := context.Background()
	tip, ok, _ := repo.ResolveBranch(ctx, branch)
	status, err := gitx.Repo{Dir: wt}.Run(ctx, "status", "--porcelain", "--ignored", "--untracked-files=all")
	if !ok || tip != base || err != nil || len(strings.TrimSpace(string(status))) > 0 {
		return fmt.Sprintf("kept %s and branch %s: they changed since they were made", wt, branch)
	}
	if _, err := repo.Run(ctx, "worktree", "remove", wt); err != nil {
		return fmt.Sprintf("kept %s: %v", wt, err)
	}
	// delete the branch only if it still points at base (atomic check)
	if _, err := repo.Run(ctx, "update-ref", "-d", "refs/heads/"+branch, base); err != nil {
		return fmt.Sprintf("kept branch %s: %v", branch, err)
	}
	return ""
}

// preflight refuses a launch that cannot run or would bill an API key
// (A30), for the directory Claude starts in.
func (s *Service) preflight(prof Profile, dir string) error {
	if !runnable[prof.Harness] {
		return refusedf("profile %s runs %s; this stage of taskmoor serve runs only claude profiles", prof.Name, prof.Harness)
	}
	prog := prof.Command
	if prog == "" {
		prog = "claude"
	}
	path, err := programPath(prof)
	if err != nil {
		return refusedf("%s is not on the service's PATH: install Claude Code and sign in (claude /login) first", prog)
	}
	if prof.Confined() {
		if err := claudeVersionOK(path, s.Env); err != nil {
			return refusedf("profile %s runs in the sandbox: %v", prof.Name, err)
		}
		if f := managedSandboxOff(); f != "" {
			return refusedf("profile %s runs in the sandbox, but the managed Claude settings %s turn it off", prof.Name, f)
		}
		return nil // the person's and the repository's settings are not loaded (init still reports the billing)
	}
	if !prof.APIBilling {
		if f, what := apiKeyInSettings(s.Home, envValue(s.Env, "CLAUDE_CONFIG_DIR"), dir); f != "" {
			return refusedf("%s sets %s, so Claude would bill an API key; profile %s does not allow API billing (A30)", f, what, prof.Name)
		}
	}
	return nil
}

func (s *Service) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

func (s *Service) refreshProfile(l *live) error {
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	prof := cfg.Profile(l.s.Profile)
	if prof == nil {
		return refusedf("profile %s is no longer configured", l.s.Profile)
	}
	l.profile = *prof
	return nil
}

// sandboxFor is the sandbox of a launch under the session's current
// profile, or nil when the profile runs unconfined.
func (s *Service) sandboxFor(l *live) (*sandboxPlan, error) {
	if !l.profile.Confined() {
		return nil, nil
	}
	common := filepath.Dir(filepath.Dir(l.stateDir))
	gd, err := checkedGitDir(l.s.Worktree, common)
	if err != nil {
		return nil, refusedf("the sandbox needs the worktree's own git directory: %v", err)
	}
	if f, err := refFormat(common); err != nil || f != "files" {
		return nil, refusedf("the sandbox protects branches only with git's files ref backend (this repository: %q %v)", f, err)
	}
	refs, err := branchRefs(common)
	if err != nil {
		return nil, fmt.Errorf("the sandbox needs the repository's branches: %w", err)
	}
	others, _ := filepath.Glob(filepath.Join(common, "worktrees", "*"))
	return &sandboxPlan{Worktree: l.s.Worktree, GitDir: gd, CommonDir: common,
		Branch: l.s.Branch, Store: s.Paths.Store, ConfigDir: filepath.Dir(s.Paths.Config), Home: s.Home,
		Env: s.Env, Domains: l.profile.AllowedDomains, OtherRefs: refs, OtherWorktrees: others}, nil
}

// protected are the service's own files: no agent reads or writes them.
func (s *Service) protected() []string {
	return []string{s.Paths.Store, filepath.Dir(s.Paths.Config)}
}

// reserve admits one launch of a session: refused while the service is
// stopping, while the session runs or another launch of it is under way.
func (s *Service) reserve(l *live) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closing:
		return refusedf("taskmoor serve is stopping")
	case l.runner != nil || l.starting:
		return refusedf("session %s is already running", l.s.ID)
	case l.handingOver || l.s.State == StateHandedOver:
		return refusedf("session %s is handed over to the terminal", l.s.ID)
	}
	l.starting = true
	s.launches.Add(1)
	return nil
}

func (s *Service) unreserve(l *live) {
	s.mu.Lock()
	l.starting = false
	s.mu.Unlock()
	s.launches.Done()
}

// launch starts the runner of a reserved session and sends its first
// message; the reservation ends either way.
func (s *Service) launch(l *live, resume bool, first string, plan *sandboxPlan) error {
	prog, err := programPath(l.profile)
	if err != nil {
		s.unreserve(l)
		return err
	}
	s.mu.Lock()
	l.s.Confined = plan != nil // saved with the pending state below
	s.mu.Unlock()
	if resume {
		// a fresh state for this run, written before it starts, so a stop
		// before its init, or a restart, is not read as the previous run's
		// completion
		l.stmu.Lock()
		s.mu.Lock()
		l.s.State = StatePending
		s.mu.Unlock()
		err := s.saveAndPublishErr(l)
		l.stmu.Unlock()
		if err != nil {
			s.unreserve(l)
			return fmt.Errorf("the session record could not be written: %w", err)
		}
		s.writeHarness(l, StatePending)
	}
	h := &sessionHost{s: s, l: l}
	s.runners.Add(1)
	_, err = startClaude(claudeStart{Program: prog, Dir: l.s.Worktree, SessionID: l.s.ID, Resume: resume,
		Model: l.profile.Model, APIBilling: l.profile.APIBilling, Env: s.Env, Sandbox: plan, Protected: s.protected(),
		SettingsFile: filepath.Join(s.Paths.Store, "sessions", l.s.ID+".settings.json")}, h)
	s.unreserve(l)
	if err != nil {
		s.runners.Done()
		s.addActivity(l, Activity{Type: ActError, Text: err.Error()})
		s.setState(l, StateError, err.Error())
		return err
	}
	s.addActivity(l, Activity{Type: ActPrompt, Text: firstLine(first, 400), By: "person"})
	return h.r.send(first)
}

func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == name {
			return v
		}
	}
	return ""
}

// Stop ends a running session; the claim, worktree and branch stay (A7).
func (s *Service) Stop(id string) error {
	l, err := s.byID(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	r := l.runner
	l.stopping = true
	s.mu.Unlock()
	if r == nil {
		return refusedf("session %s is not running", l.s.ID)
	}
	r.stop(5 * time.Second)
	return nil
}

// Say sends an instruction: to the running process, or by resuming the
// conversation when it has ended.
func (s *Service) Say(ctx context.Context, id, text string) error {
	if strings.TrimSpace(text) == "" {
		return refusedf("the instruction is empty")
	}
	l, err := s.byID(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	r := l.runner
	s.mu.Unlock()
	if r != nil && !r.inputClosed() {
		s.addActivity(l, Activity{Type: ActPrompt, Text: text, By: "person"})
		return r.send(text)
	}
	if r != nil {
		// the conversation ended and its process is on its way out
		select {
		case <-r.done:
		case <-time.After(15 * time.Second):
			return refusedf("session %s is still ending; try again", l.s.ID)
		}
	}
	if err := s.attach(l); err != nil {
		return err
	}
	if err := s.reserve(l); err != nil {
		return err
	}
	// a resume runs under the profile as it is now (read while this
	// launch holds the session, so no runner reads it meanwhile)
	if err := s.refreshProfile(l); err != nil {
		s.unreserve(l)
		return err
	}
	// everything that can refuse runs before the task's state changes
	if err := s.preflight(l.profile, l.s.Worktree); err != nil {
		s.unreserve(l)
		return err
	}
	if !s.ownsClaim(l) {
		s.unreserve(l)
		return refusedf("%s's claim now belongs to another session (or is gone); this one cannot resume", l.s.Task)
	}
	plan, err := s.sandboxFor(l) // before the task's state changes
	if err != nil {
		s.unreserve(l)
		return err
	}
	if err := s.holdOwner(l); err != nil {
		s.unreserve(l)
		return refusedf("%s", err.Error())
	}
	// work resumes on this session's claim only: a task handed over for
	// review goes back to in_progress
	if err := s.update(l, func(f *state.File) { f.Lifecycle = state.InProgress }); err != nil {
		s.unreserve(l)
		if errors.Is(err, errNotOurs) {
			return refusedf("%s's claim now belongs to another session; this one cannot resume", l.s.Task)
		}
		var ref *app.Refused
		if errors.As(err, &ref) {
			return refusedf("%s", ref.Error())
		}
		return err
	}
	return s.launch(l, true, text, plan)
}

// Release stops the session if it runs, removes the task's state file
// (back to backlog, or the stale file cleared) and lets go of the task:
// the handover of A6.
func (s *Service) Release(ctx context.Context, id string) (string, error) {
	l, err := s.byID(id)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if l.starting {
		s.mu.Unlock()
		return "", refusedf("session %s is starting; try again in a moment", l.s.ID)
	}
	r := l.runner
	l.stopping = true
	s.mu.Unlock()
	if r != nil {
		r.stop(5 * time.Second)
		<-r.done
	}
	if err := s.attach(l); err != nil {
		return "", err
	}
	// removed only if, under the project lock, the claim is still this
	// session's: a second release cannot remove a newer claim
	after, err := l.app.ReleaseIf(ctx, l.s.Task, func(f *state.File) bool { return claimIs(f, l.s) })
	var nd *state.NotDurableError
	if err != nil && !errors.As(err, &nd) {
		var ref *app.Refused
		if errors.As(err, &ref) {
			return "", refusedf("%s", ref.Error())
		}
		return "", err
	}
	s.dropOwner(l)
	s.addActivity(l, Activity{Type: ActResponse, Text: "released: the state file is removed (" + after + ")", By: "person"})
	s.mu.Lock()
	l.s.State = StateStopped
	s.mu.Unlock()
	s.saveAndPublish(l)
	return after, nil
}

// attach fills in what a session loaded from the journal needs to run.
func (s *Service) attach(l *live) error {
	if l.app != nil {
		return nil
	}
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	p := cfg.ProjectByName(l.s.Project)
	if p == nil {
		return refusedf("project %s is no longer registered", l.s.Project)
	}
	prof := cfg.Profile(l.s.Profile)
	if prof == nil {
		return refusedf("profile %s is no longer configured", l.s.Profile)
	}
	a := s.appFor(*p)
	rep, err := a.Report(context.Background())
	if err != nil {
		return err
	}
	l.app, l.profile, l.stateDir = a, *prof, rep.Snapshot.StateDir
	l.rules = Rules{Worktree: l.s.Worktree, MainBranch: rep.MainBranch, MainCheckout: rep.Snapshot.Root, Protected: s.protected()}
	return nil
}

// AnswerAsk delivers the person's answer.
func (s *Service) AnswerAsk(id string, ans Answer) error {
	s.mu.Lock()
	a := s.asks[id]
	var l *live
	if a != nil {
		l = s.sessions[a.Session]
	}
	r := (*claudeRunner)(nil)
	if l != nil {
		r = l.runner
	}
	s.mu.Unlock()
	if a == nil || l == nil || !a.opened {
		return &NotFound{Msg: fmt.Sprintf("no pending ask %q", id)}
	}
	if r == nil {
		return refusedf("session %s is not running any more; the ask cannot be answered", a.Session)
	}
	// Build the reply, take the ask out and record the answer first, then
	// let the agent go on: what it does next must come after the answer.
	var reply func() error
	var text string
	switch a.Kind {
	case "question":
		input, summary, e := answerInput(a, ans)
		if e != nil {
			return e
		}
		reply, text = func() error { return r.allow(a.requestID, input) }, summary
	default:
		if ans.Allow {
			reply, text = func() error { return r.allow(a.requestID, a.input) }, "allowed"
		} else {
			msg := "The person declined this."
			if ans.Text != "" {
				msg += " " + ans.Text
			}
			reply, text = func() error { return r.deny(a.requestID, msg) }, "declined"
			if ans.Text != "" {
				text += ": " + ans.Text
			}
		}
	}
	if !s.take(a) {
		return &NotFound{Msg: fmt.Sprintf("ask %s was already answered or withdrawn", id)}
	}
	s.resolve(l, a, text, "person")
	return reply()
}

// take removes a pending ask; false when another answer took it first.
func (s *Service) take(a *Ask) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.asks[a.ID] != a {
		return false
	}
	delete(s.asks, a.ID)
	return true
}

// byID finds a session by its id, or by a task id within project ("" for
// any project, refused when two projects have a session for that task id).
func (s *Service) byID(id string) (*live, error) { return s.lookup(id, "") }

func (s *Service) lookup(id, project string) (*live, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.sessions[id]; l != nil {
		return l, nil
	}
	// a task id names its latest session
	var best *live
	projects := map[string]bool{}
	for _, l := range s.sessions {
		if l.s.Task != id || (project != "" && l.s.Project != project) {
			continue
		}
		projects[l.s.Project] = true
		if best == nil || l.s.StartedAt > best.s.StartedAt || l.s.StartedAt == best.s.StartedAt && l.s.ID > best.s.ID {
			best = l
		}
	}
	if len(projects) > 1 {
		names := make([]string, 0, len(projects))
		for p := range projects {
			names = append(names, p)
		}
		sort.Strings(names)
		return nil, refusedf("%s has sessions in %s: name the project or use the session id", id, strings.Join(names, ", "))
	}
	if best != nil {
		return best, nil
	}
	return nil, &NotFound{Msg: fmt.Sprintf("no session %q", id)}
}

// SessionID resolves a session id or a project's task id.
func (s *Service) SessionID(id, project string) (string, error) {
	l, err := s.lookup(id, project)
	if err != nil {
		return "", err
	}
	return l.s.ID, nil
}

func (s *Service) snapshotSession(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.sessions[id]; l != nil {
		c := *l.s
		return &c
	}
	return nil
}

// Feed returns a session's activities.
func (s *Service) Feed(id string) ([]Activity, error) {
	l, err := s.byID(id)
	if err != nil {
		return nil, err
	}
	return s.journal.activities(l.s.ID)
}

// Snapshot is everything a client shows.
type Snapshot struct {
	Epoch    string         `json:"epoch"`
	Rev      int64          `json:"rev"`
	Profiles []ProfileView  `json:"profiles"`
	Projects []ProjectState `json:"projects"`
	Sessions []Session      `json:"sessions"`
	Asks     []Ask          `json:"asks"`
}

// ProfileView is a profile as clients show it.
type ProfileView struct {
	Name       string `json:"name"`
	Harness    string `json:"harness"`
	Model      string `json:"model,omitempty"`
	APIBilling bool   `json:"api_billing,omitempty"`
	Runnable   bool   `json:"runnable"`
	Confined   bool   `json:"confined"`
}

// ProjectState is a registered project with its tasks.
type ProjectState struct {
	Name  string         `json:"name"`
	Path  string         `json:"path"`
	Error string         `json:"error,omitempty"`
	Tasks []app.TaskView `json:"tasks"`
	// Routes is the profile Run would choose per task id (C10), or the
	// reason there is none.
	Routes map[string]Route `json:"routes"`
}

// Route is the routed profile of a task.
type Route struct {
	Profile string `json:"profile,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Snapshot reads every project now and the sessions and asks held.
func (s *Service) Snapshot(ctx context.Context) (*Snapshot, error) {
	// the position first: an event during the reads below makes the
	// client's stream start with a reset, so it reads again
	epoch, rev := s.bus.position()
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	out := &Snapshot{Epoch: epoch, Rev: rev, Profiles: []ProfileView{}, Projects: []ProjectState{}, Sessions: []Session{}, Asks: []Ask{}}
	for _, p := range cfg.Profiles {
		out.Profiles = append(out.Profiles, ProfileView{Name: p.Name, Harness: p.Harness, Model: p.Model, APIBilling: p.APIBilling, Runnable: runnable[p.Harness], Confined: p.Confined()})
	}
	for _, p := range cfg.Projects {
		ps := ProjectState{Name: p.Name, Path: p.Path, Tasks: []app.TaskView{}, Routes: map[string]Route{}}
		if rep, err := s.appFor(p).Report(ctx); err != nil {
			ps.Error = err.Error()
		} else {
			ps.Tasks = rep.Tasks
			for _, t := range rep.Tasks {
				if t.Lifecycle != derive.Backlog {
					continue
				}
				if prof, err := chooseProfile(cfg, rep.Snapshot.Config, t, ""); err != nil {
					ps.Routes[t.ID] = Route{Error: err.Error()}
				} else {
					ps.Routes[t.ID] = Route{Profile: prof.Name}
				}
			}
		}
		out.Projects = append(out.Projects, ps)
	}
	s.mu.Lock()
	for _, l := range s.sessions {
		out.Sessions = append(out.Sessions, *l.s)
	}
	for _, id := range s.order {
		if a := s.asks[id]; a != nil && a.opened {
			out.Asks = append(out.Asks, *a)
		}
	}
	s.mu.Unlock()
	sort.Slice(out.Sessions, func(i, j int) bool { return out.Sessions[i].StartedAt > out.Sessions[j].StartedAt })
	return out, nil
}

// Shutdown refuses new launches, stops every running session (their claims
// stay; on the next start they are stale and can be resumed) and waits for
// every runner's last callback.
func (s *Service) Shutdown() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.launches.Wait() // every admitted launch has bound its runner or failed
	s.mu.Lock()
	var rs []*claudeRunner
	for _, l := range s.sessions {
		if l.runner != nil {
			l.stopping = true
			rs = append(rs, l.runner)
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range rs {
		wg.Add(1)
		go func() { defer wg.Done(); r.stop(3 * time.Second) }()
	}
	wg.Wait()
	s.runners.Wait()
	s.mu.Lock()
	for k, f := range s.owners {
		f.Close()
		delete(s.owners, k)
	}
	s.mu.Unlock()
}

func (s *Service) addActivity(l *live, a Activity) {
	s.mu.Lock()
	if a.At == "" {
		a.At = stamp(s.Now())
	}
	sid := l.s.ID
	s.mu.Unlock()
	a, _ = s.journal.append(sid, a) // numbers it: Seq orders and identifies it
	s.bus.publish("activity", map[string]any{"session": sid, "activity": a})
}

// publishSession publishes a copy taken under the lock.
func (s *Service) publishSession(l *live) {
	s.mu.Lock()
	rec := *l.s
	s.mu.Unlock()
	s.bus.publish("session", rec)
}

func (s *Service) saveAndPublish(l *live) { _ = s.saveAndPublishErr(l) }

func (s *Service) saveAndPublishErr(l *live) error {
	s.mu.Lock()
	l.s.UpdatedAt = stamp(s.Now())
	rec := *l.s
	s.mu.Unlock()
	err := s.journal.saveSession(&rec)
	s.bus.publish("session", rec)
	return err
}

// setState changes a session's state and the state file's harness record
// (the mapping under "Session journal").
func (s *Service) setState(l *live, st, errText string) {
	l.stmu.Lock()
	defer l.stmu.Unlock()
	s.setStateLocked(l, st, errText)
}

func (s *Service) setStateLocked(l *live, st, errText string) {
	s.mu.Lock()
	l.s.State = st
	if errText != "" {
		l.s.Error = errText
	}
	s.mu.Unlock()
	s.saveAndPublish(l)
	s.writeHarness(l, st)
}

func harnessStatus(st string, kind string) (string, string) {
	switch st {
	case StateActive:
		return "working", ""
	case StateAwaitingInput:
		return "blocked", kind
	case StateComplete:
		return "done", ""
	case StateError:
		return "error", ""
	case StateStopped:
		return "exited", ""
	}
	return "unknown", ""
}

func (s *Service) writeHarness(l *live, st string) {
	if l.app == nil {
		return
	}
	s.mu.Lock()
	kind := ""
	if st == StateAwaitingInput {
		kind = "question"
		for _, aid := range l.pending {
			if a := s.asks[aid]; a != nil && a.Kind == "approval" {
				kind = "permission"
			}
		}
	}
	var code *int
	if l.s.ExitCode != nil {
		c := *l.s.ExitCode
		code = &c
	}
	sid, harnessName := l.s.ID, l.s.Harness
	s.mu.Unlock()
	status, kind := harnessStatus(st, kind)
	err := s.update(l, func(f *state.File) {
		f.Harness = &state.Harness{Name: harnessName, Status: status, Kind: kind, ObservedAt: stamp(s.Now()), Session: sid}
		if status == "exited" && code != nil {
			f.Harness.ExitCode = code
		}
	})
	switch {
	case err == nil:
	case errors.Is(err, errNotOurs):
		// another session's claim now: leave it alone
	default:
		s.addActivity(l, Activity{Type: ActError, Text: "the state file's harness record was not updated: " + err.Error()})
	}
}

// complete ends a session's work and hands the task over (the plan's
// `review`): the harness record and the lifecycle change in one write, so
// no crash leaves "done" without "review".
func (s *Service) complete(l *live) {
	l.stmu.Lock()
	defer l.stmu.Unlock()
	s.mu.Lock()
	l.s.State = StateComplete
	sid, name := l.s.ID, l.s.Harness
	s.mu.Unlock()
	s.saveAndPublish(l)
	if l.app == nil {
		return
	}
	err := s.update(l, func(f *state.File) {
		f.Lifecycle = state.Review
		f.Harness = &state.Harness{Name: name, Status: "done", ObservedAt: stamp(s.Now()), Session: sid}
	})
	if err != nil && !errors.Is(err, errNotOurs) {
		s.addActivity(l, Activity{Type: ActError, Text: "not moved to review: " + err.Error()})
	}
}

// openAsk registers an ask, marks the session waiting and only then makes
// the ask known, all in the order replies and withdrawals see them.
func (s *Service) openAsk(l *live, a *Ask) {
	l.stmu.Lock()
	defer l.stmu.Unlock()
	s.mu.Lock()
	a.ID = newID()
	a.Session, a.Project, a.Task = l.s.ID, l.s.Project, l.s.Task
	a.CreatedAt = stamp(s.Now())
	s.asks[a.ID] = a
	s.order = append(s.order, a.ID)
	l.pending[a.requestID] = a.ID
	copyA := *a
	s.mu.Unlock()
	s.setStateLocked(l, StateAwaitingInput, "")
	text := a.Detail
	if a.Kind == "question" && len(a.Questions) > 0 {
		text = a.Questions[0].Question
	}
	s.addActivity(l, Activity{Type: ActElicitation, Text: text, Tool: a.Tool, Ask: a.ID})
	copyA.opened = true
	s.bus.publish("ask", copyA)
	s.mu.Lock()
	a.opened = true // only now can it be seen in a snapshot and answered
	s.mu.Unlock()
}

func (s *Service) resolve(l *live, a *Ask, text, by string) {
	s.mu.Lock()
	delete(s.asks, a.ID)
	delete(l.pending, a.requestID)
	for i, id := range s.order {
		if id == a.ID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if text != "" {
		s.addActivity(l, Activity{Type: ActResponse, Text: text, Ask: a.ID, By: by})
	}
	s.bus.publish("ask_resolved", map[string]string{"id": a.ID})
	// back to work, unless asks remain or the session has moved on
	l.stmu.Lock()
	defer l.stmu.Unlock()
	s.mu.Lock()
	back := l.s.State == StateAwaitingInput && len(l.pending) == 0 && l.runner != nil
	s.mu.Unlock()
	if back {
		s.setStateLocked(l, StateActive, "")
	}
}
