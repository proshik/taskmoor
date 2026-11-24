package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/gitx"
	"github.com/proshik/taskmoor/internal/project"
)

// Ideas and intake (stage 8, v2: C8, C9, A22). A raw idea lives in the
// service's store, outside git. Grilling it is an agent session of its own
// on an intake/<id> branch in its own worktree: the agent asks the person
// through the same forms as any question (never the dispatcher: an idea is
// the person's), then writes the agreed tasks as files under tasks/ and
// commits them. Approve merges that branch into the local main, as Accept
// merges a task, once the branch holds only task files that validate; the
// grilling's transcript stays in the store.

// Idea states.
const (
	IdeaNew      = "new"
	IdeaGrilling = "grilling"
	IdeaProposed = "proposed" // the grilling ended with tasks committed
	IdeaApproved = "approved"
	IdeaDropped  = "dropped"
)

// Idea is a raw idea and its way to tasks.
type Idea struct {
	ID          string `json:"id"`
	Project     string `json:"project"`
	ProjectPath string `json:"project_path,omitempty"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	State       string `json:"state"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	// Session is the grilling session; Branch, Worktree and Base its git.
	Session  string `json:"session,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Base     string `json:"base,omitempty"`
	// Tasks are the ids the branch adds, as last seen.
	Tasks  []string `json:"tasks,omitempty"`
	Merged string   `json:"merged,omitempty"`
	// Approving is an Approve under way: written before main moves, so a
	// crash between the two is finished at the next start (or Approve).
	Approving *Approval `json:"approving,omitempty"`
}

// Approval is the commit an Approve lands on main and what it rests on.
type Approval struct {
	Commit     string   `json:"commit"`
	Tip        string   `json:"tip"`
	Main       string   `json:"main"`
	MainBranch string   `json:"main_branch"`
	Tasks      []string `json:"tasks"`
}

const maxIdeaText = 64 << 10

func (s *Service) ideasDir() string { return filepath.Join(s.Paths.Store, "ideas") }

// loadIdeas reads the store's ideas (Open).
func (s *Service) loadIdeas() error {
	files, err := filepath.Glob(filepath.Join(s.ideasDir(), "*.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var i Idea
		if err := json.Unmarshal(data, &i); err != nil || i.ID == "" {
			continue // not an idea record
		}
		s.ideas[i.ID] = &i
	}
	return nil
}

// saveIdea writes an idea; s.mu is not held.
func (s *Service) saveIdea(i *Idea) error {
	if err := os.MkdirAll(s.ideasDir(), 0o700); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(s.ideasDir(), i.ID+".json"), i)
}

// changeIdea changes an idea under s.mu, saves and publishes it.
func (s *Service) changeIdea(id string, change func(*Idea) error) (*Idea, error) {
	// one change at a time from copy to store: a stale copy never
	// overwrites a newer state
	s.ideaSave.Lock()
	defer s.ideaSave.Unlock()
	s.mu.Lock()
	i := s.ideas[id]
	if i == nil {
		s.mu.Unlock()
		return nil, &NotFound{Msg: fmt.Sprintf("no idea %q (taskmoor idea list)", id)}
	}
	cp := *i
	if err := change(&cp); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	cp.UpdatedAt = stamp(s.Now())
	s.mu.Unlock()
	if err := s.saveIdea(&cp); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.ideas[id] = &cp
	s.mu.Unlock()
	s.bus.publish("idea", cp)
	return &cp, nil
}

// Ideas lists the ideas, newest first.
func (s *Service) Ideas() []Idea {
	s.mu.Lock()
	out := make([]Idea, 0, len(s.ideas))
	for _, i := range s.ideas {
		out = append(out, *i)
	}
	s.mu.Unlock()
	sort.Slice(out, func(a, b int) bool {
		if out[a].CreatedAt != out[b].CreatedAt {
			return out[a].CreatedAt > out[b].CreatedAt
		}
		return out[a].ID > out[b].ID
	})
	return out
}

// IdeaByID is one idea.
func (s *Service) IdeaByID(id string) (*Idea, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.ideas[id]
	if i == nil {
		return nil, &NotFound{Msg: fmt.Sprintf("no idea %q (taskmoor idea list)", id)}
	}
	cp := *i
	return &cp, nil
}

// AddIdea keeps a raw idea for a project.
func (s *Service) AddIdea(projectName, title, text string) (*Idea, error) {
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	p := cfg.ProjectByName(projectName)
	if p == nil {
		return nil, &NotFound{Msg: fmt.Sprintf("no project %q (taskmoor project list)", projectName)}
	}
	text = strings.TrimSpace(text)
	title = strings.TrimSpace(title)
	if text == "" && title == "" {
		return nil, refusedf("the idea is empty")
	}
	if len(text) > maxIdeaText {
		return nil, refusedf("the idea is longer than %d KB: keep the raw idea short, the grilling adds the rest", maxIdeaText>>10)
	}
	if title == "" {
		title = clip(strings.SplitN(text, "\n", 2)[0], 80)
	}
	title = clip(strings.Join(strings.Fields(title), " "), 120)
	now := stamp(s.Now())
	i := &Idea{ID: "idea-" + newID()[:8], Project: p.Name, ProjectPath: p.Path, Title: title, Text: text,
		State: IdeaNew, CreatedAt: now, UpdatedAt: now}
	if err := s.saveIdea(i); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.ideas[i.ID] = i
	cp := *i
	s.mu.Unlock()
	s.bus.publish("idea", cp)
	return &cp, nil
}

// GrillRequest starts the grilling of an idea.
type GrillRequest struct {
	Profile string `json:"profile,omitempty"`
	Anyway  bool   `json:"anyway,omitempty"`
}

// Grill makes the idea's intake branch and worktree and starts the agent
// that grills the idea with the person and writes its tasks.
func (s *Service) Grill(ctx context.Context, id string, req GrillRequest) (*Session, error) {
	if s.isClosing() {
		return nil, refusedf("taskmoor serve is stopping")
	}
	s.ideaMu.Lock()
	defer s.ideaMu.Unlock()
	idea, err := s.IdeaByID(id)
	if err != nil {
		return nil, err
	}
	if idea.State == IdeaGrilling && s.grillNeverStarted(ctx, idea) {
		// its first launch failed (no conversation opened, nothing made):
		// it starts again from the beginning
		if idea, err = s.changeIdea(id, func(x *Idea) error {
			if x.State != IdeaGrilling || x.Session != idea.Session {
				return refusedf("%s changed meanwhile", id)
			}
			x.State = IdeaNew
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if idea.State != IdeaNew {
		if idea.Session != "" && (idea.State == IdeaGrilling || idea.State == IdeaProposed) {
			return nil, refusedf("%s is %s already: say something to its session %s to go on", id, idea.State, idea.Session)
		}
		return nil, refusedf("%s is %s", id, idea.State)
	}
	if idea.Branch != "" {
		// a Grill that stopped half way: its leftovers go if untouched
		if err := s.clearGrill(ctx, idea); err != nil {
			return nil, err
		}
	}
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	p := cfg.ProjectByName(idea.Project)
	if p == nil || (idea.ProjectPath != "" && p.Path != idea.ProjectPath) {
		return nil, refusedf("project %s is no longer the one the idea was added to", idea.Project)
	}
	a := s.appFor(*p)
	rep, err := a.Report(ctx)
	if err != nil {
		return nil, err
	}
	// routed as a design task: a profile named in tasks.toml for design,
	// else the default
	prof, err := chooseProfile(cfg, rep.Snapshot.Config, app.TaskView{ID: id, Kind: "design", Task: &project.Task{}}, req.Profile)
	if err != nil {
		return nil, err
	}
	if err := s.preflight(*prof, rep.Snapshot.Root); err != nil {
		return nil, err
	}
	if err := s.guard(*prof, false, req.Anyway); err != nil {
		return nil, err
	}
	branch := "intake/" + id
	root := rep.Snapshot.Root
	wt := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-"+id)
	if _, err := os.Lstat(wt); err == nil {
		return nil, refusedf("%s already exists; the service does not reuse or remove worktrees (A7)", wt)
	}
	repo := gitx.Repo{Dir: root}
	if _, ok, err := repo.ResolveBranch(ctx, branch); err != nil {
		return nil, err
	} else if ok {
		return nil, refusedf("branch %s already exists", branch)
	}
	base := rep.Snapshot.MainHead
	sid := newUUID()
	// the intent first, durably: a crash after this leaves what the next
	// Grill can recognise and clear
	if _, err := s.changeIdea(id, func(i *Idea) error {
		if i.State != IdeaNew {
			return refusedf("%s changed meanwhile", id)
		}
		i.Session, i.Branch, i.Worktree, i.Base = sid, branch, wt, base
		return nil
	}); err != nil {
		return nil, err
	}
	if _, err := repo.Run(ctx, "worktree", "add", "-q", "-b", branch, wt, base); err != nil {
		return nil, err
	}
	undo := func(err error) error {
		if kept := undoWorktree(repo, wt, branch, base); kept != "" {
			err = fmt.Errorf("%w (%s)", err, kept)
		}
		return err
	}
	if err := s.preflight(*prof, wt); err != nil {
		return nil, undo(err)
	}
	now := stamp(s.Now())
	rec := &Session{ID: sid, Project: p.Name, Idea: id, Title: idea.Title, Profile: prof.Name, Harness: prof.Harness,
		Model: prof.Model, Worktree: wt, Branch: branch, State: StatePending, StartedAt: now, UpdatedAt: now,
		ProjectPath: p.Path, Confined: prof.Confined(), Isolation: prof.Isolation()}
	if prof.Harness == "claude" {
		rec.Resume = fmt.Sprintf("cd %s && claude --resume %s", shellQuote(wt), sid)
	}
	l := &live{s: rec, app: a, profile: *prof, pending: map[string]string{}, stateDir: rep.Snapshot.StateDir,
		rules: Rules{Worktree: wt, MainBranch: rep.MainBranch, MainCheckout: root, Protected: s.protected()}}
	plan, err := s.sandboxFor(l)
	if err != nil {
		return nil, undo(err)
	}
	if err := s.reserve(l); err != nil {
		return nil, undo(err)
	}
	fail := func(err error) (*Session, error) {
		s.unreserve(l)
		s.mu.Lock()
		delete(s.sessions, sid)
		s.mu.Unlock()
		_ = s.journal.remove(sid)
		return nil, undo(err)
	}
	s.mu.Lock()
	s.sessions[sid] = l
	s.mu.Unlock()
	if err := s.journal.saveSession(rec); err != nil {
		return fail(err)
	}
	if _, err := s.changeIdea(id, func(i *Idea) error {
		if i.State != IdeaNew || i.Session != sid {
			return refusedf("%s changed meanwhile", id)
		}
		i.State = IdeaGrilling
		return nil
	}); err != nil {
		return fail(err)
	}
	s.publishSession(l)
	exe, _ := os.Executable()
	if err := s.launch(l, false, grillAssignment(idea, rep.Snapshot, wt, branch, exe), plan); err != nil {
		return s.snapshotSession(sid), err
	}
	return s.snapshotSession(sid), nil
}

// grillAssignment is the first message of a grilling session.
func grillAssignment(i *Idea, snap *project.Snapshot, wt, branch, exe string) string {
	main := snap.Config.Project.MainBranch
	if exe == "" {
		exe = "taskmoor"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are turning a raw idea of the person's into tasks for the project %s, in the git worktree %s on branch %s.\n\n", snap.Name, wt, branch)
	fmt.Fprintf(&b, "The idea, \"%s\":\n\n%s\n\n", i.Title, i.Text)
	b.WriteString("How to work:\n")
	b.WriteString("1. Read the repository first: its instructions (CLAUDE.md, AGENTS.md, .claude/rules/; they may not be loaded for you), README, docs, tasks/tasks.toml and some existing tasks/*.md, so you never ask what they already answer.\n")
	b.WriteString("2. Then grill the idea with the person, through AskUserQuestion only, from the main conversation (not a subagent): one decision per question, 2–4 concrete options, the recommended one first and labelled \"(Recommended)\", each option's description saying what it trades off. Ask what would change what gets built: why they want it and what success looks like, goals and non-goals, what is cut, how it is checked, limits, and how it splits into tasks and in which order. What you can decide yourself, decide, and say so in the task.\n")
	b.WriteString("3. When it is settled, write the tasks as new files in tasks/, in this repository's task format (follow the existing files and tasks/tasks.toml): the next free ids with the project's prefix, status backlog, depends_on between the new tasks where the order matters, touches, Goal, Acceptance and Limits with what was agreed, and a Context section with a short summary of the grilling. Keep each task one agent's work.\n")
	fmt.Fprintf(&b, "4. Check them with `%s --repo %s --source worktree validate` and fix every error.\n", shellQuote(exe), shellQuote(wt))
	fmt.Fprintf(&b, "5. Commit only files under tasks/, on this branch. Change nothing else; never push, publish or merge: the person looks at the tasks and approves them into %s.\n", main)
	b.WriteString("6. Finish with a short summary: the tasks you wrote and the decisions they rest on.\n")
	return b.String()
}

// clearGrill removes what a Grill that stopped half way left: its session
// record and, if they are untouched (at base, clean), its worktree and
// branch; refused when they changed. ideaMu is held.
func (s *Service) clearGrill(ctx context.Context, i *Idea) error {
	s.mu.Lock()
	l := s.sessions[i.Session]
	s.mu.Unlock()
	if l != nil {
		s.mu.Lock()
		busy := l.runner != nil || l.starting
		s.mu.Unlock()
		if busy {
			return refusedf("%s's session %s is running", i.ID, i.Session)
		}
		s.mu.Lock()
		l.s.Abandoned = true
		s.mu.Unlock()
		s.saveAndPublish(l)
	}
	repo := gitx.Repo{Dir: i.ProjectPath}
	_, wtErr := os.Lstat(i.Worktree)
	_, branchThere, err := repo.ResolveBranch(ctx, i.Branch)
	if err != nil {
		return err
	}
	switch {
	case wtErr == nil:
		if ok, why := untouchedIntake(ctx, repo, i); !ok {
			return refusedf("%s's last Grill stopped half way and %s; look at it, remove it, then Grill again", i.ID, why)
		}
		if kept := undoWorktree(repo, i.Worktree, i.Branch, i.Base); kept != "" {
			return refusedf("%s's last Grill stopped half way and %s; look at them, remove them, then Grill again", i.ID, kept)
		}
	case branchThere:
		if _, err := repo.Run(ctx, "update-ref", "-d", "refs/heads/"+i.Branch, i.Base); err != nil {
			return refusedf("%s's last Grill stopped half way and its branch %s moved; remove it, then Grill again", i.ID, i.Branch)
		}
	}
	_, err = s.changeIdea(i.ID, func(x *Idea) error {
		if x.State != IdeaNew {
			return refusedf("%s changed meanwhile", x.ID)
		}
		x.Session, x.Branch, x.Worktree, x.Base = "", "", "", ""
		return nil
	})
	return err
}

// ownsIdea says whether the idea is still grilled by this session.
func (s *Service) ownsIdea(l *live) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.ideas[l.s.Idea]
	return i != nil && i.Session == l.s.ID && (i.State == IdeaGrilling || i.State == IdeaProposed)
}

// ideaSessionEnded: the grilling's turn finished; with tasks committed the
// idea is proposed.
func (s *Service) ideaSessionEnded(l *live) {
	if !s.ownsIdea(l) {
		return
	}
	ctx := context.Background()
	repo := gitx.Repo{Dir: l.s.Worktree}
	tip := revOf(ctx, repo, "refs/heads/"+l.s.Branch)
	mainTip := revOf(ctx, repo, "refs/heads/"+l.rules.MainBranch)
	var ids []string
	commits := 0
	if tip != "" && mainTip != "" {
		if n, err := repo.Run(ctx, "rev-list", "--count", mainTip+".."+tip); err == nil {
			fmt.Sscan(strings.TrimSpace(string(n)), &commits)
		}
		if base, err := repo.MergeBase(ctx, mainTip, tip); err == nil {
			_, briefs, _ := s.tasksAt(ctx, l.rules.MainCheckout, base, tip)
			for _, b := range briefs {
				ids = append(ids, b.ID)
			}
		}
	}
	_, _ = s.changeIdea(l.s.Idea, func(i *Idea) error {
		if i.Session != l.s.ID || (i.State != IdeaGrilling && i.State != IdeaProposed) {
			return refusedf("not this session's")
		}
		i.Tasks = ids
		if commits > 0 {
			i.State = IdeaProposed
		}
		return nil
	})
}

func revOf(ctx context.Context, repo gitx.Repo, ref string) string {
	out, err := repo.Run(ctx, "rev-parse", "-q", "--verify", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TaskBrief is a task an intake branch adds.
type TaskBrief struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	File  string `json:"file"`
}

// tasksAt reads the project at the commits base and tip, as main would be
// at each: the tasks tip adds over base and tip's validation findings.
// What is read is committed: nothing on disk (an ignored file, an edit)
// counts.
func (s *Service) tasksAt(ctx context.Context, root, base, tip string) ([]TaskBrief, []TaskBrief, []finding.Finding) {
	at := func(c string) (*app.Report, error) {
		a := &app.App{Dir: root, Source: project.SourceMain, At: c, Now: s.Now, Version: s.Version, WriterName: app.ServiceWriter}
		return a.Report(ctx)
	}
	baseRep, err := at(base)
	if err != nil {
		return nil, nil, []finding.Finding{finding.Errorf("", 0, "the base %s cannot be read: %s", short12(base), err.Error())}
	}
	tipRep, err := at(tip)
	if err != nil {
		return nil, nil, []finding.Finding{finding.Errorf("", 0, "the intake's tasks cannot be read: %s", err.Error())}
	}
	var all, added []TaskBrief
	for _, t := range tipRep.Tasks {
		b := TaskBrief{ID: t.ID, Title: t.Title, File: t.File}
		all = append(all, b)
		if _, ok := baseRep.Find(t.ID); !ok {
			added = append(added, b)
		}
	}
	return all, added, tipRep.Findings
}

// changedPaths lists what tip changes against base, from the two trees
// (NUL-separated, no rename detection, submodules included, no diff
// configuration applied), with each path's new mode ("000000": deleted).
func changedPaths(ctx context.Context, repo gitx.Repo, base, tip string) ([][2]string, error) {
	out, err := repo.Run(ctx, "-c", "diff.ignoreSubmodules=none", "diff-tree", "-r", "-z", "--no-renames", "--ignore-submodules=none", "--raw", base, tip)
	if err != nil {
		return nil, err
	}
	var paths [][2]string
	fields := strings.Split(string(out), "\x00")
	for k := 0; k+1 < len(fields); k += 2 {
		meta := fields[k]
		if meta == "" {
			break
		}
		// ":oldmode newmode oldsha newsha status"
		parts := strings.Fields(strings.TrimPrefix(meta, ":"))
		if len(parts) < 5 {
			return nil, fmt.Errorf("unexpected diff-tree output %q", meta)
		}
		paths = append(paths, [2]string{fields[k+1], parts[1]})
	}
	return paths, nil
}

// intakePath says why a changed path cannot be approved ("" if it can):
// only task files directly in tasks/ and tasks/tasks.toml, regular files,
// none deleted.
func intakePath(p, mode string) string {
	dir, name := path.Split(p)
	switch {
	case mode == "000000":
		return "deleted"
	case mode != "100644" && mode != "100755":
		return "not a regular file (mode " + mode + ")"
	case dir != project.Dir+"/":
		return "not in tasks/"
	case name == "tasks.toml" || project.IsTaskFile(name):
		return ""
	}
	return "not a task file"
}

// IdeaReview is what Approve would land on main.
type IdeaReview struct {
	Idea       Idea        `json:"idea"`
	MainBranch string      `json:"main_branch"`
	Base       string      `json:"base"`
	Main       string      `json:"main"`
	Tip        string      `json:"tip"`
	Commits    []string    `json:"commits"`
	Stat       string      `json:"stat"`
	Diff       string      `json:"diff"`
	DiffCut    bool        `json:"diff_cut,omitempty"`
	Dirty      string      `json:"dirty,omitempty"`
	Tasks      []TaskBrief `json:"tasks"`
	// Outside are the changes that cannot be approved, with why.
	Outside  []string `json:"outside,omitempty"`
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// MainMoved: main has commits the branch lacks; Approve sends it back
	// to be brought up to date first.
	MainMoved bool   `json:"main_moved,omitempty"`
	Result    string `json:"result,omitempty"` // the agent's summary
	Ready     bool   `json:"ready"`
	Why       string `json:"why,omitempty"`
}

// ideaLive is an idea's session, checked to belong to the project the idea
// was added to (the same checkout and git directory).
func (s *Service) ideaLive(ctx context.Context, i *Idea) (*live, error) {
	if i.Session == "" {
		return nil, refusedf("%s has not been grilled yet", i.ID)
	}
	s.mu.Lock()
	l := s.sessions[i.Session]
	s.mu.Unlock()
	if l == nil {
		return nil, refusedf("%s's session %s is gone", i.ID, i.Session)
	}
	if err := s.attach(l); err != nil {
		return nil, err
	}
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	if p := cfg.ProjectByName(i.Project); p == nil || p.Path != i.ProjectPath {
		return nil, refusedf("project %s is no longer the checkout %s the idea was added to", i.Project, i.ProjectPath)
	}
	common := func(dir string) string {
		out, err := gitx.Repo{Dir: dir}.Run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return ""
		}
		p, _ := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
		return p
	}
	if c := common(i.Worktree); c == "" || c != common(l.rules.MainCheckout) {
		return nil, refusedf("%s's worktree %s is not of the repository at %s", i.ID, i.Worktree, l.rules.MainCheckout)
	}
	return l, nil
}

// ReviewIdea is the intake branch as Approve would land it.
func (s *Service) ReviewIdea(ctx context.Context, id string) (*IdeaReview, error) {
	// an Approve's intent is reconciled only under ideaMu (Open, Approve,
	// Drop): a review never clears one under way
	i, err := s.IdeaByID(id)
	if err != nil {
		return nil, err
	}
	l, err := s.ideaLive(ctx, i)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	running := grillingRuns(l)
	s.mu.Unlock()
	return s.reviewIdea(ctx, i, l, running), nil
}

// grillingRuns says whether the grilling's agent may still act: starting,
// completing, or a runner attached that has not finished, unless it is
// only exiting after a completed turn. s.mu is held.
func grillingRuns(l *live) bool {
	if l.starting || l.completing {
		return true
	}
	r := l.runner
	if r == nil {
		return false
	}
	select {
	case <-r.finished():
		return false
	default:
	}
	// a runner being stopped is still running until it has exited
	return !r.inputClosed() || l.s.State != StateComplete
}

func (s *Service) reviewIdea(ctx context.Context, i *Idea, l *live, running bool) *IdeaReview {
	out := &IdeaReview{Idea: *i, MainBranch: l.rules.MainBranch, Commits: []string{}, Tasks: []TaskBrief{}}
	not := func(why string) *IdeaReview { out.Ready, out.Why = false, why; return out }
	repo := gitx.Repo{Dir: l.rules.MainCheckout}
	out.Tip = revOf(ctx, repo, "refs/heads/"+l.s.Branch)
	out.Main = revOf(ctx, repo, "refs/heads/"+l.rules.MainBranch)
	if out.Tip == "" {
		return not("branch " + l.s.Branch + " is gone")
	}
	base, err := repo.MergeBase(ctx, out.Main, out.Tip)
	if err != nil {
		return not("the branch has no merge base with " + l.rules.MainBranch)
	}
	out.Base = base
	out.MainMoved = base != out.Main
	if log, err := repo.Run(ctx, "log", "--reverse", "--format=%h %s", base+".."+out.Tip); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
			if line != "" {
				out.Commits = append(out.Commits, line)
			}
		}
	}
	// git's own diff of the committed bytes: no external diff driver or
	// text conversion of the person's config may hide a change
	if st, err := repo.Run(ctx, "diff", "--no-ext-diff", "--no-textconv", "--stat", base, out.Tip); err == nil {
		out.Stat = strings.TrimRight(string(st), "\n")
	}
	d, cut, derr := repo.RunLimited(ctx, maxDiff, "diff", "--no-ext-diff", "--no-textconv", base, out.Tip)
	if derr == nil {
		out.Diff, out.DiffCut = string(d), cut
	}
	paths, perr := changedPaths(ctx, repo, base, out.Tip)
	for _, p := range paths {
		if why := intakePath(p[0], p[1]); why != "" {
			out.Outside = append(out.Outside, fmt.Sprintf("%q: %s", p[0], why))
		}
	}
	st, serr := (gitx.Repo{Dir: l.s.Worktree}).Run(ctx, "status", "--porcelain", "--untracked-files=all")
	if serr == nil {
		out.Dirty = strings.TrimRight(string(st), "\n")
	}
	// what is validated is the commit Approve would land: the intake's
	// tree on main's tip, its only parent (so a reference to an intake
	// commit, which main never gets, does not pass); with main moved, the
	// intake itself, for the send-back
	candidate := out.Tip
	if !out.MainMoved {
		c, err := repo.RunInput(ctx, strings.NewReader("Intake candidate\n"), "commit-tree", out.Tip+"^{tree}", "-p", out.Main)
		if err != nil {
			return not("the commit to land cannot be made: " + err.Error())
		}
		candidate = strings.TrimSpace(string(c))
	}
	_, added, findings := s.tasksAt(ctx, l.rules.MainCheckout, base, candidate)
	if added != nil {
		out.Tasks = added
	}
	for _, f := range findings {
		if f.Severity == finding.Error {
			out.Errors = append(out.Errors, f.String())
		} else {
			out.Warnings = append(out.Warnings, f.String())
		}
	}
	if acts, err := s.journal.activities(l.s.ID); err == nil {
		for k := len(acts) - 1; k >= 0; k-- {
			if acts[k].Type == ActResult && acts[k].Tool == "" {
				out.Result = acts[k].Text
				break
			}
		}
	}
	switch {
	case i.State == IdeaApproved:
		return not("already approved")
	case i.State == IdeaDropped:
		return not("dropped")
	case running:
		return not("the grilling is still running: answer its questions, or wait for its summary")
	case perr != nil:
		return not("the branch's changes cannot be listed: " + perr.Error())
	case derr != nil:
		return not("the diff cannot be shown: " + derr.Error())
	case serr != nil:
		return not("the worktree's status cannot be read: " + serr.Error())
	case len(out.Commits) == 0:
		return not("nothing is committed on " + l.s.Branch + " yet")
	case len(out.Outside) > 0:
		return not("the branch changes what an intake may not (" + strings.Join(out.Outside, "; ") + "): only task files in tasks/ and tasks/tasks.toml")
	case len(out.Tasks) == 0:
		return not("the branch adds no task")
	case out.Dirty != "":
		return not("the worktree has uncommitted changes: " + firstLine(out.Dirty, 200))
	case len(out.Errors) > 0 && !out.MainMoved:
		// with main moved, Approve sends the branch back first and the
		// tasks are validated again after
		return not(fmt.Sprintf("the tasks do not validate (%d error(s))", len(out.Errors)))
	}
	out.Ready = true
	return out
}

// ApproveResult is what Approve did.
type ApproveResult struct {
	Merged string `json:"merged,omitempty"`
	// SentBack: main had moved, so the session was asked to bring the
	// branch up to date first; nothing was merged.
	SentBack bool  `json:"sent_back,omitempty"`
	Idea     *Idea `json:"idea"`
}

// holdIdeaSession admits Approve or Drop to an idea's session: refused
// while it runs or starts; held, no Say or resume starts until release.
func (s *Service) holdIdeaSession(l *live, what string) (release func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case l.handingOver || l.starting:
		return nil, refusedf("session %s is starting or changing hands; try again", l.s.ID)
	case what == "approve" && grillingRuns(l):
		return nil, refusedf("the grilling is still running: answer its questions, or wait for its summary")
	}
	l.handingOver = true
	return func() {
		s.mu.Lock()
		l.handingOver = false
		s.mu.Unlock()
	}, nil
}

// Approve lands the intake's tasks, at tip (the commit the person looked
// at), on the local main as one commit (C9): its tree is tip's, its parent
// main's tip; the intake's own history stays on its branch, so nothing it
// committed and later removed reaches main (C8). Push stays the person's.
func (s *Service) Approve(ctx context.Context, id, tip, main string) (*ApproveResult, error) {
	if tip == "" || main == "" {
		return nil, refusedf("Approve needs the commits you looked at (tip and main, from the review)")
	}
	s.ideaMu.Lock()
	defer s.ideaMu.Unlock()
	s.acceptMu.Lock() // one change of a main at a time
	defer s.acceptMu.Unlock()
	if s.isClosing() {
		return nil, refusedf("taskmoor serve is stopping")
	}
	if err := s.reconcileApproval(ctx, id); err != nil {
		return nil, err
	}
	i, err := s.IdeaByID(id)
	if err != nil {
		return nil, err
	}
	l, err := s.ideaLive(ctx, i)
	if err != nil {
		return nil, err
	}
	release, err := s.holdIdeaSession(l, "approve")
	if err != nil {
		return nil, err
	}
	held := true
	defer func() {
		if held {
			release()
		}
	}()
	rv := s.reviewIdea(ctx, i, l, false)
	if !rv.Ready {
		return nil, refusedf("%s cannot be approved now: %s", id, rv.Why)
	}
	if rv.Tip != tip {
		return nil, refusedf("%s moved since you looked (now %s, you saw %s): look again before Approve", l.s.Branch, short12(rv.Tip), short12(tip))
	}
	if rv.Main != main {
		// a main moved back or on since the review: what lands would not
		// be what was shown
		return nil, refusedf("%s is at %s now, not %s as you saw it: look again before Approve", rv.MainBranch, short12(rv.Main), short12(main))
	}
	if rv.MainMoved {
		// what was validated is the branch: main's new tasks may clash
		// with it, so the session brings it up to date first
		if l.s.State == StateHandedOver {
			return nil, refusedf("%s has moved since the grilling began: merge it into %s in your terminal, check the ids, validate and commit, then Approve", rv.MainBranch, l.s.Branch)
		}
		msg := fmt.Sprintf("Approve found that %s has moved since this branch was made. Merge %s into your branch (git merge %s), "+
			"give your new tasks ids that do not clash with the tasks there (main's own tasks keep theirs), update depends_on, validate again, commit, and finish with a short summary.",
			rv.MainBranch, rv.MainBranch, rv.MainBranch)
		release()
		held = false
		if err := s.say(ctx, l.s.ID, msg, false, true); err != nil {
			return nil, fmt.Errorf("%s has moved, and sending the branch back failed: %w", rv.MainBranch, err)
		}
		s.addActivity(l, Activity{Type: ActResponse, Text: "approve: " + rv.MainBranch + " moved; sent back to bring the branch up to date", By: "person"})
		return &ApproveResult{SentBack: true, Idea: i}, nil
	}
	root := l.rules.MainCheckout
	_, mainTip, err := checkoutReady(ctx, root, rv.MainBranch, l.s.Branch, tip)
	if err != nil {
		return nil, err
	}
	if mainTip != rv.Main {
		return nil, refusedf("%s moved while approving (now %s, looked at %s): look again", rv.MainBranch, short12(mainTip), short12(rv.Main))
	}
	treeOut, err := gitx.Repo{Dir: root}.Run(ctx, "rev-parse", tip+"^{tree}")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rv.Tasks))
	for _, t := range rv.Tasks {
		ids = append(ids, t.ID)
	}
	// the idea's id, not its title: the raw text stays in the store (C8)
	msg := fmt.Sprintf("Intake %s\n\nTasks: %s\nIdea: %s\n", id, strings.Join(ids, ", "), id)
	landed, err := landTree(ctx, root, rv.MainBranch, mainTip, strings.TrimSpace(string(treeOut)), []string{mainTip}, msg, "taskmoor approve "+id,
		func(commit string) error {
			// the intent, before main moves: a crash after this is finished
			// by the next start or Approve
			_, err := s.changeIdea(id, func(x *Idea) error {
				x.Approving = &Approval{Commit: commit, Tip: tip, Main: mainTip, MainBranch: rv.MainBranch, Tasks: ids}
				return nil
			})
			return err
		})
	if err != nil {
		_, _ = s.changeIdea(id, func(x *Idea) error { x.Approving = nil; return nil })
		return nil, err
	}
	done, err := s.changeIdea(id, func(x *Idea) error {
		x.State, x.Merged, x.Tasks, x.Approving = IdeaApproved, landed, ids, nil
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("landed as %s, but the idea's record could not be written (the next start finishes it): %w", short12(landed), err)
	}
	s.addActivity(l, Activity{Type: ActResponse, By: "person",
		Text: fmt.Sprintf("approved: %s landed on %s as %s (push is yours); the worktree %s and the branch %s stay",
			strings.Join(ids, ", "), rv.MainBranch, short12(landed), l.s.Worktree, l.s.Branch)})
	return &ApproveResult{Merged: landed, Idea: done}, nil
}

// reconcileApproval finishes or clears an Approve a crash cut off; ideaMu
// is held (or the service is opening). Its commit on main: approved. Main
// still at the old tip with the checkout's index at the commit's tree (cut
// off between read-tree and update-ref): main is moved to it, finishing
// what the person approved. Main at the old tip with the index untouched,
// or main moved elsewhere: the intent goes. Anything else, or git unable
// to say, keeps the intent and reports it.
func (s *Service) reconcileApproval(ctx context.Context, id string) error {
	i, err := s.IdeaByID(id)
	if err != nil {
		return err
	}
	ap := i.Approving
	if ap == nil {
		return nil
	}
	repo := gitx.Repo{Dir: i.ProjectPath}
	mainRef := "refs/heads/" + ap.MainBranch
	cur, err := repo.Run(ctx, "rev-parse", "-q", "--verify", mainRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("%s's Approve was cut off and %s cannot be read now (%v): it is finished when it can", id, ap.MainBranch, err)
	}
	mainNow := strings.TrimSpace(string(cur))
	on, err := repo.IsAncestor(ctx, ap.Commit, mainNow)
	if err != nil {
		return fmt.Errorf("%s's Approve was cut off and git cannot tell whether %s is on %s (%v): it is finished when it can", id, short12(ap.Commit), ap.MainBranch, err)
	}
	finish := on
	if !on && mainNow == ap.Main {
		idx, err := repo.Run(ctx, "write-tree")
		want, werr := repo.Run(ctx, "rev-parse", ap.Commit+"^{tree}")
		old, oerr := repo.Run(ctx, "rev-parse", ap.Main+"^{tree}")
		switch {
		case err != nil || werr != nil || oerr != nil:
			return fmt.Errorf("%s's Approve was cut off and %s's index cannot be compared now: look at it; it is finished when it can", id, i.ProjectPath)
		case strings.TrimSpace(string(idx)) == strings.TrimSpace(string(want)):
			if _, err := repo.Run(ctx, "update-ref", "-m", "taskmoor approve "+id+" (finished)", mainRef, ap.Commit, ap.Main); err != nil {
				return fmt.Errorf("%s's Approve was cut off after the checkout moved, and %s could not be moved to it: %v", id, ap.MainBranch, err)
			}
			finish = true
		case strings.TrimSpace(string(idx)) != strings.TrimSpace(string(old)):
			return refusedf("%s's Approve was cut off and %s's index is neither the old %s nor the approved commit: look at it (the approved commit is %s)", id, i.ProjectPath, ap.MainBranch, short12(ap.Commit))
		}
	}
	_, err = s.changeIdea(id, func(x *Idea) error {
		if x.Approving == nil || x.Approving.Commit != ap.Commit {
			return nil
		}
		if finish {
			x.State, x.Merged, x.Tasks = IdeaApproved, ap.Commit, ap.Tasks
		}
		x.Approving = nil
		return nil
	})
	return err
}

// reconcileIdeas finishes the Approves a crash cut off (Open).
func (s *Service) reconcileIdeas() {
	for _, i := range s.Ideas() {
		if i.Approving != nil {
			_ = s.reconcileApproval(context.Background(), i.ID)
		}
	}
}

// DropIdea gives an idea up: its grilling stops; its branch and worktree,
// if any, stay (A7).
func (s *Service) DropIdea(id string) (*Idea, error) {
	s.ideaMu.Lock()
	defer s.ideaMu.Unlock()
	ctx := context.Background()
	if err := s.reconcileApproval(ctx, id); err != nil {
		return nil, err
	}
	i, err := s.IdeaByID(id)
	if err != nil {
		return nil, err
	}
	if i.State == IdeaApproved || i.State == IdeaDropped {
		return nil, refusedf("%s is %s already", id, i.State)
	}
	if i.Session != "" {
		s.mu.Lock()
		l := s.sessions[i.Session]
		s.mu.Unlock()
		if l != nil {
			// held: no Say or resume starts the grilling again meanwhile
			release, err := s.holdIdeaSession(l, "drop")
			if err != nil {
				return nil, err
			}
			defer release()
			s.mu.Lock()
			r := l.runner
			l.stopping = true
			s.mu.Unlock()
			if r != nil {
				r.stop(5 * time.Second)
				<-r.finished()
			}
		}
	}
	return s.changeIdea(id, func(x *Idea) error {
		if x.State == IdeaApproved {
			return refusedf("%s was approved meanwhile", id)
		}
		x.State = IdeaDropped
		return nil
	})
}

// untouchedIntake says whether an intake worktree is as Grill made it: a
// worktree of this repository, on its intake branch, at the base, clean.
func untouchedIntake(ctx context.Context, repo gitx.Repo, i *Idea) (bool, string) {
	list, err := repo.Run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return false, "the repository's worktrees cannot be listed"
	}
	want, _ := filepath.EvalSymlinks(i.Worktree)
	registered := false
	for _, line := range strings.Split(string(list), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			if q, _ := filepath.EvalSymlinks(p); q == want {
				registered = true
			}
		}
	}
	if !registered {
		return false, i.Worktree + " is not a worktree of this repository"
	}
	wt := gitx.Repo{Dir: i.Worktree}
	if h, err := wt.Run(ctx, "symbolic-ref", "-q", "HEAD"); err != nil || strings.TrimSpace(string(h)) != "refs/heads/"+i.Branch {
		return false, i.Worktree + " is not on " + i.Branch + " any more"
	}
	if revOf(ctx, wt, "HEAD") != i.Base {
		return false, i.Branch + " has commits"
	}
	if st, err := wt.Run(ctx, "status", "--porcelain", "--ignored", "--untracked-files=all"); err != nil || strings.TrimSpace(string(st)) != "" {
		return false, i.Worktree + " has changes"
	}
	return true, ""
}

// grillNeverStarted: a grilling whose session is not running and whose
// intake is untouched, with no conversation to resume (an ACP agent that
// never opened one, or a session that ended in an error before it did).
func (s *Service) grillNeverStarted(ctx context.Context, i *Idea) bool {
	s.mu.Lock()
	l := s.sessions[i.Session]
	idle := l != nil && l.runner == nil && !l.starting && !l.handingOver
	noConversation := l != nil && (l.s.Harness != "claude" && l.s.AgentSession == "" ||
		l.s.State == StateError && l.s.Harness == "claude")
	s.mu.Unlock()
	if !idle || !noConversation {
		return false
	}
	ok, _ := untouchedIntake(ctx, gitx.Repo{Dir: i.ProjectPath}, i)
	return ok
}
