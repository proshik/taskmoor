package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/proshik/taskmoor/internal/gitx"
	"github.com/proshik/taskmoor/internal/state"
)

// StateAccepted: the task's branch is merged into main (Accept, A8).
const StateAccepted = "accepted"

// Acceptance is what the person looks at before Accept: the work against
// its base, the agent's last word, the review.
type Acceptance struct {
	Session *Session `json:"session"`
	Base    string   `json:"base"` // the merge base with main
	Tip     string   `json:"tip"`  // the branch's commit: what Accept merges
	// MainBranch is the project's main branch (main, trunk, …).
	MainBranch string   `json:"main_branch"`
	Main       string   `json:"main"`    // main's tip
	Commits    []string `json:"commits"` // oneline, oldest first
	Stat       string   `json:"stat"`
	Diff       string   `json:"diff"`
	// DiffCut: the diff is longer than shown.
	DiffCut bool `json:"diff_cut,omitempty"`
	// Dirty lists uncommitted changes in the worktree (they are not merged).
	Dirty  string  `json:"dirty,omitempty"`
	Checks string  `json:"checks,omitempty"` // the task file's Checks section
	Result string  `json:"result,omitempty"` // the agent's last summary
	Review *Review `json:"review,omitempty"`
	// Ready: Accept can run now; otherwise Why says what waits.
	Ready bool   `json:"ready"`
	Why   string `json:"why,omitempty"`
}

const maxDiff = 256 << 10

// Acceptance builds the acceptance view of a session.
func (s *Service) Acceptance(ctx context.Context, id string) (*Acceptance, error) {
	l, err := s.byID(id)
	if err != nil {
		return nil, err
	}
	if err := s.attach(l); err != nil {
		return nil, err
	}
	repo := gitx.Repo{Dir: l.s.Worktree}
	mainRef := "refs/heads/" + l.rules.MainBranch
	// one commit of the branch for the whole view: what Accept will merge
	tipOut, err := repo.Run(ctx, "rev-parse", "-q", "--verify", "refs/heads/"+l.s.Branch+"^{commit}")
	if err != nil {
		return nil, refusedf("branch %s is gone", l.s.Branch)
	}
	tip := strings.TrimSpace(string(tipOut))
	base, err := repo.MergeBase(ctx, mainRef, tip)
	if err != nil {
		return nil, fmt.Errorf("the branch has no merge base with %s: %w", l.rules.MainBranch, err)
	}
	mainTip, _ := repo.Run(ctx, "rev-parse", mainRef)
	out := &Acceptance{Session: s.snapshotSession(l.s.ID), Base: base, Main: strings.TrimSpace(string(mainTip)),
		Tip: tip, MainBranch: l.rules.MainBranch, Commits: []string{}}
	if log, err := repo.Run(ctx, "log", "--reverse", "--format=%h %s", base+".."+tip); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
			if line != "" {
				out.Commits = append(out.Commits, line)
			}
		}
	}
	if st, err := repo.Run(ctx, "diff", "--stat", base, tip); err == nil {
		out.Stat = strings.TrimRight(string(st), "\n")
	}
	if d, cut, err := repo.RunLimited(ctx, maxDiff, "diff", base, out.Tip); err == nil {
		out.Diff, out.DiffCut = string(d), cut // git's output is not held whole
	}
	if st, err := repo.Run(ctx, "status", "--porcelain"); err == nil {
		out.Dirty = strings.TrimRight(string(st), "\n")
	}
	if rep, err := l.app.Report(ctx); err == nil {
		if t, ok := rep.Find(l.s.Task); ok {
			out.Checks = section(t.Task.Body, "Checks")
		}
	}
	if acts, err := s.journal.activities(l.s.ID); err == nil {
		for i := len(acts) - 1; i >= 0; i-- {
			if acts[i].Type == ActResult && acts[i].Tool == "" {
				out.Result = acts[i].Text
				break
			}
		}
	}
	s.mu.Lock()
	if l.s.Review != nil {
		r := *l.s.Review
		out.Review = &r
	}
	s.mu.Unlock()
	out.Ready, out.Why = s.acceptable(l)
	return out, nil
}

// acceptable says whether Accept can run now.
func (s *Service) acceptable(l *live) (bool, string) {
	s.mu.Lock()
	// a runner whose conversation ended is only exiting
	running := l.runner != nil && !l.runner.inputClosed() || l.starting || l.handingOver || l.completing
	st := l.s.State
	rv := l.s.Review
	s.mu.Unlock()
	switch {
	case st == StateAccepted:
		return false, "already accepted"
	case st == StateHandedOver:
		return false, "continued in the terminal: finish it there with taskmoor review"
	case running:
		return false, "the agent is still working"
	case rv != nil && rv.State == "running":
		return false, "the code review is running"
	case rv != nil && rv.State == "findings" && !rv.Unreviewed && !rv.SendFailed:
		return false, "the review's findings are going back to the session"
	case !s.ownsClaim(l):
		return false, l.s.Task + "'s claim is not this session's"
	}
	if f, err := s.lifecycle(l); err != nil || f != state.Review {
		return false, fmt.Sprintf("%s is %s, not in review", l.s.Task, f)
	}
	rep, err := l.app.Report(context.Background())
	if err != nil {
		return false, err.Error()
	}
	t, ok := rep.Find(l.s.Task)
	switch {
	case !ok:
		return false, l.s.Task + " is no longer a task"
	case t.Lifecycle != state.Review:
		return false, fmt.Sprintf("%s is %s already (its claim is stale)", l.s.Task, t.Lifecycle)
	case reviewRounds(t) > 0 && rv == nil:
		return false, "the code review has not started yet"
	}
	return true, ""
}

func (s *Service) lifecycle(l *live) (string, error) {
	sch, err := schemaLoad()
	if err != nil {
		return "", err
	}
	e, ok, err := state.Read(l.stateDir, l.s.Task, sch)
	if err != nil || !ok {
		return "", errors.New("no state file")
	}
	return e.File.Lifecycle, nil
}

// section returns a markdown section's text ("## Name" up to the next).
func section(body, name string) string {
	lines := strings.Split(body, "\n")
	var out []string
	in := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			if in {
				break
			}
			in = strings.EqualFold(strings.TrimSpace(ln[3:]), name)
			continue
		}
		if in {
			out = append(out, ln)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// AcceptResult says what Accept did.
type AcceptResult struct {
	Merged string `json:"merged,omitempty"` // the merge commit
	// Conflicted: the trial merge found conflicts (Conflicts names them
	// when git does); the task went back to its session to merge main in.
	Conflicted bool     `json:"conflicted,omitempty"`
	Conflicts  []string `json:"conflicts,omitempty"`
	Session    *Session `json:"session"`
}

// Accept merges the task's branch into main (A8), exactly the commit the
// person looked at (tip; "" takes the branch as it is now):
//   - nothing runs while the person's checkout is in a merge, a rebase, a
//     cherry-pick or a revert, or is not on main (never switched, never
//     aborted);
//   - a trial merge of the two commits (`git merge-tree --write-tree`,
//     which writes no worktree); a conflict sends the task back;
//   - a path the merge writes that exists in the checkout untracked or
//     ignored refuses it (git's ort strategy would overwrite an ignored
//     file whatever --no-overwrite-ignore says);
//   - the merge: --no-ff --commit --no-squash --no-log, the Task trailer
//     in its own last paragraph; a merge this call started and git left
//     unfinished is aborted;
//   - the result is checked: on main, parents main's old tip and the task
//     commit, the trailer readable; only then does the claim go.
func (s *Service) Accept(ctx context.Context, id, tip string) (*AcceptResult, error) {
	l, err := s.byID(id)
	if err != nil {
		return nil, err
	}
	if err := s.attach(l); err != nil {
		return nil, err
	}
	if tip == "" {
		return nil, refusedf("Accept needs the commit you looked at (tip, from the acceptance view)")
	}
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	if s.isClosing() {
		return nil, refusedf("taskmoor serve is stopping")
	}
	if ok, why := s.acceptable(l); !ok {
		return nil, refusedf("%s cannot be accepted now: %s", l.s.Task, why)
	}
	// the session is held: no Say, Release, Restart or handover meanwhile
	s.mu.Lock()
	if l.handingOver || l.starting {
		s.mu.Unlock()
		return nil, refusedf("session %s is changing hands; try again", l.s.ID)
	}
	l.handingOver = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		l.handingOver = false
		s.mu.Unlock()
	}()
	root, mainRef := l.rules.MainCheckout, "refs/heads/"+l.rules.MainBranch
	main := gitx.Repo{Dir: root}
	rev := func(ref string) string {
		out, err := main.Run(ctx, "rev-parse", "-q", "--verify", ref+"^{commit}")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	cur := rev("refs/heads/" + l.s.Branch)
	switch {
	case cur == "":
		return nil, refusedf("branch %s is gone", l.s.Branch)
	case cur != tip:
		return nil, refusedf("%s moved since you looked (now %s, you saw %s): look again before Accept", l.s.Branch, short12(cur), short12(tip))
	}
	for _, op := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		if rev(op) != "" {
			return nil, refusedf("%s is in the middle of a %s (%s): finish or abort it yourself, then Accept", root, strings.ToLower(strings.TrimSuffix(op, "_HEAD")), op)
		}
	}
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if p, err := main.Run(ctx, "rev-parse", "--git-path", d); err == nil {
			path := strings.TrimSpace(string(p))
			if !filepath.IsAbs(path) {
				path = filepath.Join(root, path)
			}
			if _, err := os.Stat(path); err == nil {
				return nil, refusedf("%s is in the middle of a rebase: finish or abort it yourself, then Accept", root)
			}
		}
	}
	head, err := main.Run(ctx, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) != mainRef {
		return nil, refusedf("%s is not on %s (it is on %s); taskmoor never switches your checkout: switch it, then Accept",
			root, l.rules.MainBranch, strings.TrimSpace(string(head)))
	}
	mainTip := rev("HEAD")
	if _, err := main.Run(ctx, "merge-base", "--is-ancestor", cur, mainTip); err == nil {
		return nil, refusedf("nothing to merge: %s's commits are already in %s", l.s.Branch, l.rules.MainBranch)
	}
	// the trial, of the two commits themselves
	tout, terr := main.Run(ctx, "merge-tree", "--write-tree", "--name-only", "--no-messages", mainTip, cur)
	if terr != nil {
		if gitx.ExitCode(terr) != 1 {
			return nil, fmt.Errorf("the trial merge failed: %v", terr)
		}
		var files []string
		for _, f := range strings.Split(strings.TrimSpace(string(tout)), "\n")[1:] { // after the tree id
			if f != "" {
				files = append(files, f)
			}
		}
		where := "conflicts"
		if len(files) > 0 {
			where = "conflicts in " + strings.Join(files, ", ")
		}
		msg := fmt.Sprintf("Accept found that your branch does not merge cleanly into %s: %s. "+
			"Merge %s into your branch (git merge %s), resolve the conflicts, run the checks again, commit, and finish with a short summary.",
			l.rules.MainBranch, where, l.rules.MainBranch, l.rules.MainBranch)
		s.mu.Lock()
		l.handingOver = false // Say needs the session
		s.mu.Unlock()
		// the person's Accept is the confirmation: below the plan's
		// threshold too (anyway)
		if err := s.say(ctx, l.s.ID, msg, false, true); err != nil {
			return nil, fmt.Errorf("the branch does not merge cleanly (%s), and sending it back failed: %w", where, err)
		}
		s.addActivity(l, Activity{Type: ActResponse, Text: "accept: " + where + " with " + l.rules.MainBranch + "; sent back", By: "person"})
		return &AcceptResult{Conflicted: true, Conflicts: files, Session: s.snapshotSession(l.s.ID)}, nil
	}
	tree := strings.TrimSpace(strings.SplitN(string(tout), "\n", 2)[0])
	// nothing the merge writes may land on, under or over a file git does
	// not track here (untracked or ignored): git's own checks do not
	// protect ignored files, so taskmoor checks every path itself
	if hit, err := untrackedInTheWay(ctx, main, root, mainTip, tree); err != nil {
		return nil, fmt.Errorf("checking the checkout: %w", err)
	} else if len(hit) > 0 {
		return nil, refusedf("the merge would write over files in %s that git does not track (untracked or ignored): %s; move them, then Accept", root, strings.Join(hit, ", "))
	}
	// the merge commit itself: the trial's tree, the two commits as parents,
	// the exact message; no strategy, merge option, merge.log or hook of the
	// person's config can change what goes in
	msg := fmt.Sprintf("Merge %s: %s\n\nTask: %s\n", l.s.Branch, l.s.Title, l.s.Task)
	out, err := main.RunInput(ctx, strings.NewReader(msg), "commit-tree", tree, "-p", mainTip, "-p", cur)
	if err != nil {
		return nil, fmt.Errorf("making the merge commit: %w", err)
	}
	merged := strings.TrimSpace(string(out))
	// the checkout: the index and the files move from main's tip to the
	// merge; read-tree refuses when the person's changes are in the way
	if h, _ := main.Run(ctx, "symbolic-ref", "-q", "HEAD"); strings.TrimSpace(string(h)) != mainRef {
		return nil, refusedf("%s left %s meanwhile; nothing was merged", root, l.rules.MainBranch)
	}
	if _, err := main.Run(ctx, "read-tree", "-m", "-u", mainTip, merged); err != nil {
		return nil, refusedf("git refused to update %s (your changes are in the way; nothing was merged): %v", root, err)
	}
	// main moves only from the tip that was checked (compare and swap)
	if _, err := main.Run(ctx, "update-ref", "-m", "taskmoor accept "+l.s.Task, mainRef, merged, mainTip); err != nil {
		_, rerr := main.Run(context.WithoutCancel(ctx), "read-tree", "-m", "-u", merged, mainTip) // the files back
		state := "the files are back as they were"
		if rerr != nil {
			state = "the files could not be put back (" + rerr.Error() + "): look at " + root
		}
		return nil, refusedf("%s moved meanwhile, so nothing was merged (%s): %v", l.rules.MainBranch, state, err)
	}
	if !hasTrailer(ctx, main, merged, l.s.Task) {
		return nil, fmt.Errorf("merged as %s, but its message has no readable `Task: %s` trailer: look at it; the task keeps its claim", short12(merged), l.s.Task)
	}
	// the task is merged: its state file goes; worktree and branch stay
	_, rerr := l.app.ReleaseIf(context.WithoutCancel(ctx), l.s.Task, func(f *state.File) bool { return claimIs(f, l.s) })
	var nd *state.NotDurableError
	if rerr != nil && !errors.As(rerr, &nd) {
		s.addActivity(l, Activity{Type: ActError, Text: "merged, but the state file stayed: " + rerr.Error()})
	} else {
		s.dropOwner(l)
	}
	s.mu.Lock()
	l.s.State = StateAccepted
	l.s.Merged = merged
	s.mu.Unlock()
	s.addActivity(l, Activity{Type: ActResponse, By: "person",
		Text: fmt.Sprintf("accepted: %s merged into %s as %s (push is yours); the worktree %s and the branch %s stay",
			short12(cur), l.rules.MainBranch, short12(merged), l.s.Worktree, l.s.Branch)})
	s.saveAndPublish(l)
	s.bus.publish("tasks", map[string]string{"project": l.s.Project})
	return &AcceptResult{Merged: merged, Session: s.snapshotSession(l.s.ID)}, nil
}

func short12(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// stopReview cancels a running review (Stop, Release, Restart, handover).
func (s *Service) stopReview(l *live) {
	s.mu.Lock()
	c := l.reviewCancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

// hasTrailer: the commit's message carries exactly `Task: <id>` as git
// reads trailers.
func hasTrailer(ctx context.Context, repo gitx.Repo, commit, id string) bool {
	body, err := repo.Run(ctx, "log", "-1", "--format=%B", commit)
	if err != nil {
		return false
	}
	tr, err := repo.RunInput(ctx, strings.NewReader(string(body)), "interpret-trailers", "--parse")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(tr), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Task") && strings.TrimSpace(v) == id {
			return true
		}
	}
	return false
}

// untrackedInTheWay lists what in root git does not track at mainTip and
// a move from mainTip to tree would write over: a changed path that is an
// untracked file, an untracked file where a changed path's directory goes,
// or untracked files inside a directory a changed path replaces.
func untrackedInTheWay(ctx context.Context, repo gitx.Repo, root, mainTip, tree string) ([]string, error) {
	list, err := repo.Run(ctx, "ls-tree", "-r", "-z", "--name-only", "--full-tree", mainTip)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, p := range strings.Split(string(list), "\x00") {
		if p != "" {
			tracked[p] = true
		}
	}
	changed, err := repo.Run(ctx, "diff", "--name-only", "-z", "--no-renames", mainTip, tree)
	if err != nil {
		return nil, err
	}
	var hit []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			hit = append(hit, p)
		}
	}
	walked := 0
	for _, p := range strings.Split(string(changed), "\x00") {
		if p == "" {
			continue
		}
		// an ancestor that is a file (not a directory) where a directory goes
		for d := filepath.Dir(p); d != "." && d != "/"; d = filepath.Dir(d) {
			if fi, err := os.Lstat(filepath.Join(root, d)); err == nil && !fi.IsDir() && !tracked[filepath.ToSlash(d)] {
				add(filepath.ToSlash(d))
			}
		}
		fi, err := os.Lstat(filepath.Join(root, p))
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			if !tracked[p] {
				add(p)
			}
			continue
		}
		// a directory the change replaces: everything in it must be tracked
		err = filepath.WalkDir(filepath.Join(root, p), func(path string, de os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if walked++; walked > 20000 {
				return errors.New("too many files to check")
			}
			if de.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			if !tracked[filepath.ToSlash(rel)] {
				add(filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(hit) > 10 {
		hit = append(hit[:10], "…")
	}
	return hit, nil
}
