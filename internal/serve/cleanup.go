package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/proshik/taskmoor/internal/gitx"
)

// Cleaning up after the merge (stage 12). The service never removes a
// worktree on its own (A7); the person can ask it to, for work that is in
// main: an accepted task's (its branch's tip is in main) or an approved
// idea's (its branch is at the commit Approve landed). Cleanup deletes
// nothing in a worktree: it moves the whole directory, with one rename,
// into the repository's trash beside it (`.<repo>-taskmoor-trash`), so
// whatever git cannot see there — a nested repository, an ignored file,
// an edit git's caches hide — is still there; it forgets the worktree's
// registration and deletes the branch, whose work is in main. Emptying the
// trash is a separate, explicit request. A worktree with a change git
// shows is kept, as is anything whose identity it cannot establish.

// CleanupItem is one worktree cleaned up, or kept and why.
type CleanupItem struct {
	Project  string `json:"project"`
	Kind     string `json:"kind"` // task or idea
	ID       string `json:"id"`   // the task's or the idea's
	Session  string `json:"session,omitempty"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	// Removed: the worktree is in the trash and the branch gone (with a
	// dry run: would be); WorktreeRemoved: the worktree is moved, the
	// branch stays (Kept says why; a later cleanup tries it again).
	Removed         bool   `json:"removed"`
	WorktreeRemoved bool   `json:"worktree_removed,omitempty"`
	Trash           string `json:"trash,omitempty"` // where the worktree went
	Kept            string `json:"kept,omitempty"`  // why it (or its branch) stays
}

// cleanupCandidate is what one cleanup needs.
type cleanupCandidate struct {
	item     CleanupItem
	root     string // the main checkout
	main     string // its main branch
	merged   string // the commit the work is in
	approved string // an idea's approved intake commit ("" if not recorded)
	idea     bool
	l        *live
	err      string // why it cannot even be looked at
}

// Cleanup cleans up one accepted task's or approved idea's worktree (id:
// a session, a task id or an idea id), or with all every one of the
// project ("" for all); dryRun only says what it would do.
func (s *Service) Cleanup(ctx context.Context, id, project string, all, dryRun bool) ([]CleanupItem, error) {
	if (id == "") == !all {
		return nil, refusedf("name one task, session or idea, or ask for all of them")
	}
	s.acceptMu.Lock() // no Accept, Approve, Run or Grill touches these meanwhile
	defer s.acceptMu.Unlock()
	cands, err := s.cleanupCandidates(id, project)
	if err != nil {
		return nil, err
	}
	out := []CleanupItem{}
	for _, c := range cands {
		if c.err != "" {
			it := c.item
			it.Kept = c.err
			out = append(out, it)
			continue
		}
		out = append(out, s.cleanupOne(ctx, c, dryRun))
	}
	return out, nil
}

func (s *Service) cleanupCandidates(id, project string) ([]cleanupCandidate, error) {
	if strings.HasPrefix(id, "idea-") {
		i, err := s.IdeaByID(id)
		if err != nil {
			return nil, err
		}
		if i.State != IdeaApproved || i.CleanedUp || i.Branch == "" {
			return nil, refusedf("%s is %s: only an approved idea's worktree is cleaned up", id, ideaWord(i))
		}
		return []cleanupCandidate{s.ideaCandidate(i)}, nil
	}
	if id != "" {
		l, err := s.lookup(id, project)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		st, cleaned := l.s.State, l.s.CleanedUp
		s.mu.Unlock()
		if st != StateAccepted || cleaned || l.s.Idea != "" {
			return nil, refusedf("session %s is %s: only an accepted task's worktree is cleaned up", l.s.ID, stateOrCleaned(st, cleaned))
		}
		return []cleanupCandidate{s.sessionCandidate(l)}, nil
	}
	s.mu.Lock()
	var ls []*live
	for _, l := range s.sessions {
		if l.s.State == StateAccepted && !l.s.CleanedUp && l.s.Idea == "" && (project == "" || l.s.Project == project) {
			ls = append(ls, l)
		}
	}
	s.mu.Unlock()
	sort.Slice(ls, func(a, b int) bool { return ls[a].s.Task < ls[b].s.Task })
	var out []cleanupCandidate
	for _, l := range ls {
		out = append(out, s.sessionCandidate(l)) // one it cannot look at says so
	}
	for _, i := range s.Ideas() {
		if i.State == IdeaApproved && !i.CleanedUp && i.Branch != "" && (project == "" || i.Project == project) {
			out = append(out, s.ideaCandidate(&i))
		}
	}
	return out, nil
}

func ideaWord(i *Idea) string {
	if i.CleanedUp {
		return "cleaned up already"
	}
	return i.State
}

func stateOrCleaned(st string, cleaned bool) string {
	if cleaned {
		return "cleaned up already"
	}
	return st
}

// projectRoot is the project's checkout and main branch now, if it is
// still the checkout the work was done in ("" and why, if not).
func (s *Service) projectRoot(name, path string) (string, string, string) {
	cfg, err := s.Config()
	if err != nil {
		return "", "", err.Error()
	}
	p := cfg.ProjectByName(name)
	switch {
	case p == nil:
		return "", "", "project " + name + " is no longer registered"
	case path != "" && p.Path != path:
		return "", "", "project " + name + " is now the checkout " + p.Path + ", not " + path + " where this was done"
	}
	rep, err := s.appFor(*p).Report(context.Background())
	if err != nil {
		return "", "", err.Error()
	}
	return rep.Snapshot.Root, rep.MainBranch, ""
}

func (s *Service) sessionCandidate(l *live) cleanupCandidate {
	c := cleanupCandidate{item: CleanupItem{Project: l.s.Project, Kind: "task", ID: l.s.Task, Session: l.s.ID, Worktree: l.s.Worktree, Branch: l.s.Branch},
		merged: l.s.Merged, l: l}
	c.root, c.main, c.err = s.projectRoot(l.s.Project, l.s.ProjectPath)
	return c
}

func (s *Service) ideaCandidate(i *Idea) cleanupCandidate {
	s.mu.Lock()
	l := s.sessions[i.Session]
	s.mu.Unlock()
	c := cleanupCandidate{item: CleanupItem{Project: i.Project, Kind: "idea", ID: i.ID, Session: i.Session, Worktree: i.Worktree, Branch: i.Branch},
		merged: i.Merged, approved: i.ApprovedTip, idea: true, l: l}
	c.root, c.main, c.err = s.projectRoot(i.Project, i.ProjectPath)
	return c
}

// noReplace reads the history as it is: no replacement object may make
// a commit look merged.
var noReplace = []string{"-c", "core.useReplaceRefs=false"}

func isAncestor(ctx context.Context, repo gitx.Repo, a, b string) (bool, error) {
	_, err := repo.Run(ctx, append(append([]string{}, noReplace...), "merge-base", "--is-ancestor", a, b)...)
	if err == nil {
		return true, nil
	}
	if gitx.ExitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// cleanupOne moves one worktree to the trash and deletes its branch, if
// its work is in main.
func (s *Service) cleanupOne(ctx context.Context, c cleanupCandidate, dryRun bool) CleanupItem {
	it := c.item
	keep := func(why string) CleanupItem { it.Kept = why; return it }
	repo := gitx.Repo{Dir: c.root}
	branchRef := "refs/heads/" + it.Branch
	if c.l != nil {
		s.mu.Lock()
		busy := c.l.runner != nil || c.l.starting || c.l.handingOver
		s.mu.Unlock()
		if busy {
			return keep("its session is running")
		}
	}
	// no other session — a later run of the same task — uses the branch or
	// the path
	s.mu.Lock()
	for _, o := range s.sessions {
		if o.s.ID == it.Session || o.s.CleanedUp || o.s.Project != it.Project || o.s.State == StateAccepted {
			continue
		}
		if o.s.Branch == it.Branch || filepath.Clean(o.s.Worktree) == filepath.Clean(it.Worktree) {
			s.mu.Unlock()
			return keep("session " + o.s.ID + " (" + o.s.State + ") uses its branch or worktree now")
		}
	}
	s.mu.Unlock()
	if c.merged == "" {
		return keep("no merge is recorded")
	}
	if out, err := repo.Run(ctx, "symbolic-ref", "-q", branchRef); err == nil && strings.TrimSpace(string(out)) != "" {
		return keep(it.Branch + " is a symbolic ref (to " + strings.TrimSpace(string(out)) + ")")
	}
	tip := revOf(ctx, repo, branchRef)
	mainTip := revOf(ctx, repo, "refs/heads/"+c.main)
	if mainTip == "" {
		return keep("the main branch " + c.main + " cannot be read")
	}
	// every checkout of the branch, and any rebase or bisect that holds it
	wts, err := repo.Worktrees(ctx)
	if err != nil {
		return keep("the repository's worktrees cannot be listed: " + err.Error())
	}
	common := commonDir(ctx, repo)
	if common == "" {
		return keep("the repository's git directory cannot be read")
	}
	own := false
	for _, w := range wts {
		if w.Branch == branchRef {
			if !samePath(w.Path, it.Worktree) {
				return keep(it.Branch + " is checked out at " + w.Path + " too")
			}
			own = true
		}
		if held := branchHeldBy(ctx, w.Path, branchRef); held != "" {
			return keep(it.Branch + " is in use at " + w.Path + " (" + held + ")")
		}
	}
	if tip == "" {
		if exists(it.Worktree) {
			return keep("its branch " + it.Branch + " is gone; the worktree stays for you to look at")
		}
		if !dryRun {
			s.markCleaned(c) // both gone already
		}
		it.Removed = true
		return it
	}
	// the branch's work is in main (replacement objects off)
	if c.idea {
		switch {
		case c.approved != "" && tip != c.approved:
			return keep("its branch moved after the approval (" + short12(tip) + ")")
		case c.approved == "":
			// approved before the approved commit was recorded: only a tip in
			// main (an approval by merge) proves nothing is lost
			if on, err := isAncestor(ctx, repo, tip, mainTip); err != nil || !on {
				return keep("it was approved before taskmoor recorded the approved commit, and " + it.Branch + " is not in " + c.main + ": remove it by hand once you have looked")
			}
		}
		if on, err := isAncestor(ctx, repo, c.merged, mainTip); err != nil || !on {
			return keep("the approved commit " + short12(c.merged) + " is not in " + c.main)
		}
	} else if on, err := isAncestor(ctx, repo, tip, mainTip); err != nil || !on {
		return keep(it.Branch + " has commits that are not in " + c.main)
	}
	admin := ""
	if exists(it.Worktree) {
		if !own {
			return keep(it.Worktree + " is not checked out on " + it.Branch)
		}
		var why string
		admin, why = ownLinkedWorktree(it.Worktree, common)
		if why != "" {
			return keep(why)
		}
		st, err := gitx.Repo{Dir: it.Worktree}.Run(ctx, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
		if err != nil {
			return keep("its status cannot be read: " + err.Error())
		}
		if changed := strings.TrimSpace(string(st)); changed != "" {
			return keep("it has changes not in git: " + firstLine(changed, 200))
		}
	}
	trash := trashDir(c.root)
	if dryRun {
		it.Removed = true // would be
		if exists(it.Worktree) {
			it.Trash = trash
		}
		return it
	}
	if exists(it.Worktree) {
		// the whole directory, as it is, in one rename: nothing in it is
		// deleted; then git's record of it
		if err := os.MkdirAll(trash, 0o700); err != nil {
			return keep("the trash cannot be made: " + err.Error())
		}
		dest := filepath.Join(trash, filepath.Base(it.Worktree)+"-"+time.Now().UTC().Format("20060102T150405"))
		if err := os.Rename(it.Worktree, dest); err != nil {
			return keep("it could not be moved to the trash: " + err.Error())
		}
		it.Trash = dest
		if err := os.RemoveAll(admin); err != nil { // git's metadata of it only
			it.Kept = "moved to the trash, but git's record of it stays (" + err.Error() + "): git worktree prune"
		}
	}
	it.WorktreeRemoved = true
	// the branch, from the commit checked, only while main is where it was
	// checked: one ref transaction, following no symbolic ref
	tx := fmt.Sprintf("option no-deref\nverify %s %s\noption no-deref\ndelete %s %s\n", "refs/heads/"+c.main, mainTip, branchRef, tip)
	if _, err := repo.RunInput(ctx, strings.NewReader(tx), "update-ref", "--no-deref", "--stdin"); err != nil {
		it.Kept = "the worktree is in the trash, but the branch stays (" + firstLine(err.Error(), 200) + "); cleanup tries it again"
		return it
	}
	s.markCleaned(c)
	it.Removed = true
	return it
}

// commonDir is the repository's git directory (absolute).
func commonDir(ctx context.Context, repo gitx.Repo) string {
	out, err := repo.Run(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ownLinkedWorktree checks that path is, on disk, a linked worktree of
// the repository at common: a real directory (not a symbolic link), whose
// .git file points into common/worktrees, whose entry there points back.
// It returns that entry (git's metadata of the worktree).
func ownLinkedWorktree(path, common string) (string, string) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", path + " is not a directory of its own (a link, or gone)"
	}
	data, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return "", path + " has no .git file of a linked worktree"
	}
	gd, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return "", path + "'s .git is not a linked worktree's"
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(path, gd)
	}
	base, _ := filepath.EvalSymlinks(filepath.Join(common, "worktrees"))
	real, _ := filepath.EvalSymlinks(gd)
	if base == "" || real == "" || filepath.Dir(real) != base {
		return "", path + " belongs to another repository"
	}
	back, err := os.ReadFile(filepath.Join(real, "gitdir"))
	if err != nil || !samePath(strings.TrimSpace(string(back)), filepath.Join(path, ".git")) {
		return "", "git's record of " + path + " does not point back to it"
	}
	return real, ""
}

// branchHeldBy names a git operation under way in the worktree at path
// that holds the branch (a rebase, a bisect), "" if none.
func branchHeldBy(ctx context.Context, path, branchRef string) string {
	gd, err := gitx.Repo{Dir: path}.Run(ctx, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(gd))
	for _, f := range []string{"rebase-merge/head-name", "rebase-apply/head-name", "BISECT_START"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(data))
		if v == branchRef || "refs/heads/"+v == branchRef {
			return strings.SplitN(f, "/", 2)[0] + " under way"
		}
	}
	return ""
}

// trashDir is the repository's trash, beside its checkout (the same
// volume as its worktrees, so a move is one rename).
func trashDir(root string) string {
	return filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+"-taskmoor-trash")
}

// EmptyTrash deletes what cleanup moved to the trash, for the project
// ("" for all): the person's explicit request.
func (s *Service) EmptyTrash(project string) ([]string, error) {
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, p := range cfg.Projects {
		if project != "" && p.Name != project {
			continue
		}
		dir := trashDir(p.Path)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return gone, err
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return gone, err
			}
			gone = append(gone, filepath.Join(dir, e.Name()))
		}
		_ = os.Remove(dir)
	}
	return gone, nil
}

func samePath(a, b string) bool {
	ea, err1 := filepath.EvalSymlinks(a)
	eb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ea == eb
}

func (s *Service) markCleaned(c cleanupCandidate) {
	if c.idea {
		_, _ = s.changeIdea(c.item.ID, func(x *Idea) error { x.CleanedUp = true; return nil })
	}
	if c.l != nil { // the task's session, or the idea's grilling
		s.mu.Lock()
		c.l.s.CleanedUp = true
		s.mu.Unlock()
		s.saveAndPublish(c.l)
		s.addActivity(c.l, Activity{Type: ActResponse, By: "person", Text: "cleaned up: the worktree " + c.item.Worktree + " is in the trash and the branch " + c.item.Branch + " is deleted (the work is in main)"})
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
