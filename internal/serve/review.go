package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/gitx"
)

// Review is a session's code review by revust (A5): min(reviews.code, 2)
// rounds; each round's findings go back into the same session.
type Review struct {
	Rounds int `json:"rounds"` // the budget
	Done   int `json:"done"`   // rounds run (a failed one counts)
	// State: running, clean, findings, error.
	State    string `json:"state"`
	Findings int    `json:"findings,omitempty"`
	Archive  string `json:"archive,omitempty"` // the round's revust archive
	Report   string `json:"report,omitempty"`  // its findings, as markdown (bounded)
	Error    string `json:"error,omitempty"`
	// Unreviewed: the last round's findings went back, and the fixes after
	// them were not reviewed again (the budget is spent).
	Unreviewed bool   `json:"unreviewed,omitempty"`
	At         string `json:"at,omitempty"`
	// Tip is the commit the round reviewed; Partial: revust reported fewer
	// sources than it ran (a clean result that is not whole); SendFailed:
	// the findings did not reach the session.
	Tip        string `json:"tip,omitempty"`
	Partial    string `json:"partial,omitempty"`
	SendFailed bool   `json:"send_failed,omitempty"`
	SendError  string `json:"send_error,omitempty"`
}

// ReviewTimeout bounds one review round.
var ReviewTimeout = 90 * time.Minute

const maxReport = 24 << 10

// reviewRounds is a task's review budget (A5).
func reviewRounds(t app.TaskView) int { return min(t.Task.Reviews.Code, 2) }

// afterComplete runs when a session's turn completed and the task is in
// review: a review round if the budget has one left; otherwise the task
// waits for acceptance.
func (s *Service) afterComplete(l *live) {
	if l.app == nil {
		return
	}
	if l.s.Idea != "" {
		s.ideaSessionEnded(l) // tasks, not code: the person looks at them in Approve
		return
	}
	rep, err := l.app.Report(context.Background())
	if err != nil {
		return
	}
	t, ok := rep.Find(l.s.Task)
	if !ok {
		return
	}
	rounds := reviewRounds(t)
	tipOut, _ := gitx.Repo{Dir: l.s.Worktree}.Run(context.Background(), "rev-parse", "refs/heads/"+l.s.Branch)
	tip := strings.TrimSpace(string(tipOut))
	s.mu.Lock()
	if l.s.State != StateComplete || l.runner != nil && !l.runner.inputClosed() || l.starting {
		s.mu.Unlock()
		return // the session went on meanwhile: its next completion decides
	}
	// copy on write: snapshots and events hold the old value whole
	var rv Review
	if l.s.Review != nil {
		rv = *l.s.Review
	} else if rounds > 0 {
		rv = Review{Rounds: rounds}
	} else {
		s.mu.Unlock()
		return
	}
	// a round is due while the budget lasts and the branch's commit has no
	// clean review yet (a clean one of an older commit does not cover it)
	start := rv.State != "running" && rv.Done < rv.Rounds && !(rv.State == "clean" && rv.Tip == tip) && !s.closing
	if start {
		// revust's codex-only profile runs on Codex's plan: a low plan
		// postpones the round (it is not counted)
		if err := s.reviewGuard(); err != nil {
			start = false
			rv.State, rv.Error = "error", "postponed: "+err.Error()
		}
	}
	if !start && rv.State == "findings" {
		rv.Unreviewed = true // the last findings' fixes are not reviewed again
	}
	var cancel context.CancelFunc
	if start {
		rv.State, rv.Error, rv.At, rv.SendFailed = "running", "", stamp(s.Now()), false
		var ctx context.Context
		ctx, cancel = context.WithTimeout(context.Background(), ReviewTimeout)
		l.reviewCancel = cancel // set now: a Stop while it waits for a slot ends it
		l.reviewGen++
		gen := l.reviewGen
		s.reviews.Add(1)
		go func() { defer s.reviews.Done(); s.reviewRound(ctx, cancel, gen, l, t) }()
	}
	l.s.Review = &rv
	s.mu.Unlock()
	s.saveAndPublish(l)
}

// reviewDone records a round's end on a copy.
func (s *Service) setReview(l *live, change func(*Review)) Review {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rv Review
	if l.s.Review != nil {
		rv = *l.s.Review
	}
	change(&rv)
	l.s.Review = &rv
	return rv
}

// reviewRound runs one revust round on the task branch and sends its
// findings back into the session.
func (s *Service) reviewRound(ctx context.Context, cancel context.CancelFunc, gen uint64, l *live, t app.TaskView) {
	defer func() {
		cancel()
		s.mu.Lock()
		if l.reviewGen == gen { // a next round's cancel is not ours to clear
			l.reviewCancel = nil
		}
		s.mu.Unlock()
	}()
	select {
	case s.reviewSem <- struct{}{}:
	case <-ctx.Done():
		s.setReview(l, func(r *Review) { r.State, r.Error = "error", "stopped before it ran" })
		s.saveAndPublish(l)
		return
	}
	defer func() { <-s.reviewSem }()
	// the session may have gone on while the round waited
	s.mu.Lock()
	working := l.s.State != StateComplete || l.runner != nil && !l.runner.inputClosed() || l.starting
	s.mu.Unlock()
	if working {
		s.setReview(l, func(r *Review) {
			r.State, r.Error = "error", "the session went on before the review ran: its next completion decides"
		})
		s.saveAndPublish(l)
		return
	}
	// the plan may have gone low while the round waited for its slot
	if err := s.reviewGuard(); err != nil {
		s.setReview(l, func(r *Review) { r.State, r.Error = "error", "postponed: "+err.Error() })
		s.saveAndPublish(l)
		return
	}
	// the session may have moved on while the round waited
	s.mu.Lock()
	gone := l.s.State == StateStopped || l.s.State == StateHandedOver || l.s.RestartedAs != "" || l.s.State == StateAccepted
	s.mu.Unlock()
	if gone || !s.ownsClaim(l) {
		s.setReview(l, func(r *Review) { r.State, r.Error = "error", "the session moved on before the review ran" })
		s.saveAndPublish(l)
		return
	}
	rv0 := s.setReview(l, func(*Review) {})
	round := rv0.Done + 1
	s.addActivity(l, Activity{Type: ActAction, Tool: "review", Text: fmt.Sprintf("code review round %d of %d (revust)", round, rv0.Rounds)})
	s.agentUp() // revust's agents are agents too: the Mac stays awake (A19)
	res := s.runRevust(ctx, l, t, round, rv0.Rounds)
	s.agentDown()
	if res.state == "postponed" { // not run, not counted
		s.setReview(l, func(r *Review) { r.State, r.Error = "error", "postponed: "+res.err })
		s.saveAndPublish(l)
		return
	}
	rv := s.setReview(l, func(r *Review) {
		r.Done = round
		r.State, r.Findings, r.Archive, r.Report, r.Error, r.Tip, r.Partial = res.state, res.findings, res.archive, res.report, res.err, res.tip, res.partial
	})
	s.mu.Lock()
	stopped := l.s.State == StateStopped || l.s.State == StateHandedOver || l.s.RestartedAs != ""
	s.mu.Unlock()
	s.saveAndPublish(l)
	switch res.state {
	case "clean":
		text := fmt.Sprintf("review round %d: no findings; the task waits for acceptance", round)
		if res.partial != "" {
			text += " (partial: " + res.partial + ")"
		}
		s.addActivity(l, Activity{Type: ActResult, Tool: "review", Text: text})
	case "error":
		s.addActivity(l, Activity{Type: ActError, Tool: "review", Text: fmt.Sprintf("review round %d failed: %s", round, res.err)})
	case "findings":
		s.addActivity(l, Activity{Type: ActResult, Tool: "review", Text: fmt.Sprintf("review round %d: %d finding(s), sent back to the session", round, res.findings)})
		if stopped || ctx.Err() != nil {
			s.setReview(l, func(r *Review) { r.SendFailed = true })
			s.saveAndPublish(l)
			return // the person took the session elsewhere: the findings wait in the acceptance view
		}
		msg := fmt.Sprintf("Code review round %d of %d found the issues below. Check each against the code; fix what is right "+
			"(with a test where it fits), say briefly why for what is not, commit on the branch, and finish with a short summary.\n\n%s",
			round, rv.Rounds, res.report)
		// the findings resume the agent: an automatic start (it waits on a low plan)
		if err := s.say(context.Background(), l.s.ID, msg, true, false); err != nil {
			s.setReview(l, func(r *Review) { r.SendFailed, r.SendError = true, err.Error() })
			s.saveAndPublish(l)
			s.addActivity(l, Activity{Type: ActError, Text: "the review's findings did not reach the session: " + err.Error()})
		}
	}
}

type revustResult struct {
	state    string
	findings int
	archive  string
	report   string
	err      string
	tip      string
	partial  string
}

var sourceDelivery = regexp.MustCompile(`Source delivery: (\d+) / (\d+) reported`)

var revustName = regexp.MustCompile(`[^a-z0-9-]+`)

// runRevust prepares and runs one revust round headless: the archives live
// in the service store (never in the agent's worktree), the subject is the
// branch against its merge base with main.
func (s *Service) runRevust(ctx context.Context, l *live, t app.TaskView, round, rounds int) revustResult {
	cfg, err := s.Config()
	if err != nil {
		return revustResult{state: "error", err: err.Error()}
	}
	prog := cfg.ReviewCommand
	if prog == "" {
		prog = "revust"
	}
	path, err := exec.LookPath(prog)
	if err != nil {
		return revustResult{state: "error", err: prog + " is not on the service's PATH"}
	}
	profile := cfg.ReviewProfile
	if profile == "" {
		profile = "codex-only"
	}
	dir := filepath.Join(s.Paths.Store, "reviews")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return revustResult{state: "error", err: err.Error()}
	}
	task := strings.Trim(revustName.ReplaceAllString(strings.ToLower(l.s.Project+"-"+l.s.Task+"-"+l.s.ID[:8]), "-"), "-")
	run := fmt.Sprintf("r%d-%s", round, newID()[:6]) // never an archive a cut-off round left
	repo := gitx.Repo{Dir: l.s.Worktree}
	// the branch's commit, named: what Accept would merge, whatever the
	// worktree's HEAD is
	tipOut, err := repo.Run(ctx, "rev-parse", "refs/heads/"+l.s.Branch)
	if err != nil {
		return revustResult{state: "error", err: "branch " + l.s.Branch + ": " + err.Error()}
	}
	tip := strings.TrimSpace(string(tipOut))
	base, err := repo.MergeBase(ctx, "refs/heads/"+l.rules.MainBranch, tip)
	if err != nil {
		return revustResult{state: "error", err: "no merge base with " + l.rules.MainBranch + ": " + err.Error()}
	}
	out, err := exec.CommandContext(ctx, path, "new", "--task", task, "--run", run, "--tasks-dir", dir).Output()
	if err != nil {
		return revustResult{state: "error", err: "revust new: " + err.Error()}
	}
	var paths struct {
		InputDir string `json:"input_dir"`
		RoundDir string `json:"round_dir"`
	}
	if json.Unmarshal(lastJSON(out), &paths) != nil || paths.InputDir == "" {
		return revustResult{state: "error", err: "revust new gave no input directory"}
	}
	scope := fmt.Sprintf("# Scope\n\nReview, round %d of %d, of the work on branch `%s` for task %s, \"%s\": "+
		"commit %s against its merge base with `%s`, %s.\n\nThe task file (%s) is the contract: its Goal, "+
		"Acceptance and Limits.\n\nCommands: `git log --oneline %s..%s`, `git diff %s %s`.\n"+
		"Report only defects that matter, with a concrete failure scenario.\n",
		round, rounds, l.s.Branch, t.ID, t.Title, tip, l.rules.MainBranch, base, t.File, base, tip, base, tip)
	if err := os.WriteFile(filepath.Join(paths.InputDir, "scope.md"), []byte(scope), 0o600); err != nil {
		return revustResult{state: "error", err: err.Error()}
	}
	_ = os.WriteFile(filepath.Join(paths.InputDir, "goal.md"), []byte(t.Task.Body), 0o600)
	// the plan, once more, right before the providers start
	if err := s.reviewGuard(); err != nil {
		return revustResult{state: "postponed", err: err.Error()}
	}
	cmd := exec.CommandContext(ctx, path, "--task", task, "--run", run, "--profile", profile, "--no-tui",
		"--tasks-dir", dir, "--workdir", l.s.Worktree)
	cmd.Dir = dir
	cmd.Env = agentEnv(s.Env, false)
	// a stop asks revust to end: it cleans up its providers on SIGTERM
	// (SIGKILL would leave them running); WaitDelay then kills
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	var stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &bytes.Buffer{}, n: 1 << 20}
	cmd.Stderr = &limitedWriter{w: &stderr, n: 16 << 10}
	err = cmd.Run()
	archive := paths.RoundDir
	code := 0
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		return revustResult{state: "error", archive: archive, err: err.Error()}
	}
	if ctx.Err() != nil {
		return revustResult{state: "error", archive: archive, err: "the review was stopped or ran out of time"}
	}
	// the report decides, not only the exit code: read under the round's
	// context, bounded; an unreadable report is an error, never clean
	view := exec.CommandContext(ctx, path, "view", "--archive", archive, "--no-tui", "--markdown")
	view.Cancel = func() error { return view.Process.Signal(syscall.SIGTERM) }
	view.WaitDelay = 10 * time.Second
	var mdBuf bytes.Buffer
	view.Stdout = &limitedWriter{w: &mdBuf, n: 4 << 20}
	if err := view.Run(); err != nil || ctx.Err() != nil {
		return revustResult{state: "error", archive: archive, tip: tip, err: fmt.Sprintf("revust exit %d, and its report could not be read: %v", code, err)}
	}
	md := mdBuf.Bytes()
	partial := ""
	if m := sourceDelivery.FindStringSubmatch(string(md)); m != nil && m[1] != m[2] {
		partial = m[1] + " of " + m[2] + " sources reported"
	} else if head, _, _ := strings.Cut(string(md), "## Active findings"); strings.Contains(strings.ToLower(head), "partial") {
		partial = "revust reports a partial result"
	}
	switch code {
	case 0:
		return revustResult{state: "clean", archive: archive, tip: tip, partial: partial}
	case 1:
		report := activeFindings(string(md))
		if !strings.Contains(report, "### ") {
			return revustResult{state: "error", archive: archive, tip: tip, err: "revust reported findings, but none could be read from its report"}
		}
		if len(report) > maxReport {
			report = report[:maxReport] + "\n\n(cut; the whole report: revust view --archive " + archive + ")"
		}
		return revustResult{state: "findings", findings: strings.Count(report, "\n### "), archive: archive, report: report, tip: tip, partial: partial}
	}
	return revustResult{state: "error", archive: archive, err: fmt.Sprintf("revust exit %d: %s", code, lastLines(stderr.String(), 2))}
}

// activeFindings is the "Active findings" part of revust's markdown.
func activeFindings(md string) string {
	start := strings.Index(md, "## Active findings")
	if start < 0 {
		return strings.TrimSpace(md)
	}
	rest := md[start:]
	if end := strings.Index(rest[1:], "\n## "); end >= 0 {
		rest = rest[:end+1]
	}
	return "\n" + strings.TrimSpace(rest)
}

// lastJSON is the last JSON object in out (revust prints its paths).
func lastJSON(out []byte) []byte {
	i := bytes.LastIndex(out, []byte("\n{"))
	if i < 0 {
		i = bytes.IndexByte(out, '{')
	} else {
		i++
	}
	if i < 0 {
		return nil
	}
	return out[i:]
}

// reviewRunning: a round of this session is under way.
func (s *Service) reviewRunning(l *live) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return l.s.Review != nil && l.s.Review.State == "running"
}

// reviewGuard is the usage guard of an automatic review round: on the
// plans of revust's review profile.
func (s *Service) reviewGuard() error {
	cfg, err := s.Config()
	if err != nil {
		return nil
	}
	for _, h := range reviewHarnesses(cfg) {
		if err := s.guardPlan(h, true, false); err != nil {
			return err
		}
	}
	return nil
}

// resendFindings delivers findings a low plan kept from the session.
func (s *Service) resendFindings(l *live) {
	s.mu.Lock()
	rv := l.s.Review
	s.mu.Unlock()
	if rv == nil || rv.Report == "" {
		return
	}
	msg := fmt.Sprintf("Code review round %d of %d found the issues below. Check each against the code; fix what is right "+
		"(with a test where it fits), say briefly why for what is not, commit on the branch, and finish with a short summary.\n\n%s",
		rv.Done, rv.Rounds, rv.Report)
	if err := s.say(context.Background(), l.s.ID, msg, true, false); err != nil {
		s.setReview(l, func(r *Review) { r.SendError = err.Error() })
		return
	}
	s.setReview(l, func(r *Review) { r.SendFailed, r.SendError = false, "" })
	s.saveAndPublish(l)
	s.addActivity(l, Activity{Type: ActResult, Tool: "review", Text: fmt.Sprintf("review round %d: the findings went to the session now that the plan allows it", rv.Done)})
}
