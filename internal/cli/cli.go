// Package cli is taskmoor's command line: subcommands, text and JSON
// output, exit codes.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/harness"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/state"
	"github.com/proshik/taskmoor/internal/text"
	"github.com/proshik/taskmoor/spec"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitProblem = 1 // validate errors, a refused transition
	ExitUsage   = 2 // usage error, or the project cannot be read
)

// Env is what a run sees of the outside world.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Dir            string // the working directory
	Now            func() time.Time
	Version        string
	// Harness overrides the harness source found from the environment.
	Harness harness.Source
	// TUI runs the board; nil means the board is not available.
	TUI func(ctx context.Context, a *app.App) error
	// OpenURL opens a link in the browser; nil prints it instead.
	OpenURL func(url string) error
}

const usage = `usage:
  taskmoor [--repo PATH] [--source main|worktree]      the TUI board
  taskmoor list     [--json]                           every task with its status
  taskmoor show ID  [--json]                           front matter, status, reasons, state, body
  taskmoor ready    [--json]                           takeable tasks, priority then id
  taskmoor validate [--json]                           all checks; exit 1 on any error
  taskmoor claim ID --branch B --worktree W [--harness H] [--session S] [--person] [--force]
  taskmoor review ID
  taskmoor release ID
  taskmoor version

The service (taskmoor serve) and its clients:
  taskmoor serve                                       run the service (127.0.0.1, foreground)
  taskmoor project add PATH [--name N] | list | remove NAME
  taskmoor run ID [--project P] [--profile NAME] [--person] [--anyway] [--json]
  taskmoor sessions [--json]                           the service's sessions
  taskmoor feed ID [--follow] [--json]                 a session's feed (ID: session or task)
  taskmoor inbox [--json]                              questions and approvals waiting for you
  taskmoor answer ASK N [N…] | --text T | --allow | --deny [--text WHY]
  taskmoor stop ID                                     stop a session; the claim stays
  taskmoor say ID TEXT…                                an instruction (resumes an ended session)
  taskmoor continue ID                                 hand the session to your terminal (prints the resume command)
  taskmoor restart ID --profile NAME                   a new session of another profile on the same claim and worktree
  taskmoor keep N [--project P]                        keep N sessions busy with ready tasks (0: off)
  taskmoor decisions [--json]                          what the dispatcher answered, and why
  taskmoor usage [--json]                              each plan's share left and the start guard
  taskmoor acceptance ID [--diff] [--json]             the work to accept: commits, diff, review, checks
  taskmoor accept ID                                   merge the task's branch into main (trial merge first)
  taskmoor override DECISION N [N…] | --text T          replace a dispatcher answer (sent to the session)
  taskmoor web [--print]                               open the board in the browser (a one-time sign-in link)

--repo and --source work before or after the subcommand. The board, list,
show, ready and claim read tasks from the main branch; validate reads the
working tree.
`

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// Run runs taskmoor with args (without the program name) and returns the
// exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	g, rest, err := globals(args)
	if err != nil {
		return runner{env: env}.fail(usagef("%v", err), wantsJSON(args))
	}
	cmd := ""
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}
	r := runner{env: env, g: g}
	switch cmd {
	case "":
		return r.board(ctx)
	case "list":
		return r.list(ctx, rest)
	case "show":
		return r.show(ctx, rest)
	case "ready":
		return r.ready(ctx, rest)
	case "validate":
		return r.validate(ctx, rest)
	case "claim":
		return r.claim(ctx, rest)
	case "review":
		return r.review(ctx, rest)
	case "release":
		return r.release(ctx, rest)
	case "serve":
		return r.serveCmd(ctx, rest)
	case "project":
		return r.projectCmd(ctx, rest)
	case "run":
		return r.runCmd(ctx, rest)
	case "sessions":
		return r.sessionsCmd(ctx, rest)
	case "feed":
		return r.feedCmd(ctx, rest)
	case "inbox":
		return r.inboxCmd(ctx, rest)
	case "answer":
		return r.answerCmd(ctx, rest)
	case "stop":
		return r.stopCmd(ctx, rest)
	case "say":
		return r.sayCmd(ctx, rest)
	case "web":
		return r.webCmd(ctx, rest)
	case "continue":
		return r.handoverCmd(ctx, rest)
	case "restart":
		return r.restartCmd(ctx, rest)
	case "keep":
		return r.keepCmd(ctx, rest)
	case "decisions":
		return r.decisionsCmd(ctx, rest)
	case "usage":
		return r.usageCmd(ctx, rest)
	case "acceptance":
		return r.acceptanceCmd(ctx, rest)
	case "accept":
		return r.acceptCmd(ctx, rest)
	case "override":
		return r.overrideCmd(ctx, rest)
	case "version":
		if len(rest) > 0 {
			return r.usageErr(usagef("version takes no arguments"))
		}
		fmt.Fprintf(env.Stdout, "taskmoor %s (format %d)\n", versionOr(env.Version), spec.Format)
		return ExitOK
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return ExitOK
	}
	return r.usageErr(usagef("unknown command %q", cmd))
}

func versionOr(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

// wantsJSON reports whether --json appears before a "--".
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" || a == "-json" || a == "--json=true" || a == "-json=true" {
			return true
		}
	}
	return false
}

type globalFlags struct {
	repo   string
	source string // "" = the command's default
}

// globals takes --repo and --source out of args wherever they appear
// before a "--".
func globals(args []string) (globalFlags, []string, error) {
	var g globalFlags
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || (name != "repo" && name != "source") {
			rest = append(rest, a)
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return g, nil, fmt.Errorf("--%s needs a value", name)
			}
			i++
			val = args[i]
		}
		if name == "repo" {
			g.repo = val
		} else {
			if _, err := project.ParseSource(val); err != nil {
				return g, nil, err
			}
			g.source = val
		}
	}
	return g, rest, nil
}

type runner struct {
	env Env
	g   globalFlags
}

func (r runner) app(def project.Source) *app.App {
	dir := r.g.repo
	if dir == "" {
		dir = r.env.Dir
	}
	src := def
	if r.g.source != "" {
		src = project.Source(r.g.source)
	}
	h := r.env.Harness
	if h == nil {
		h = harness.FromEnv(r.env.Getenv)
	}
	return &app.App{Dir: dir, Source: src, Harness: h, Now: r.env.Now, Version: r.env.Version, CheckOwner: true}
}

func (r runner) usageErr(err error) int {
	fmt.Fprintf(r.env.Stderr, "taskmoor: %s\n\n%s", text.Line(err.Error()), usage)
	return ExitUsage
}

// fail reports err and returns its exit code; with asJSON it also prints
// {"error": …} on stdout.
// notDurable prints the warning for a change that was made but whose
// directory sync failed; it reports whether err was that.
func (r runner) notDurable(err error) bool {
	var nd *state.NotDurableError
	if !errors.As(err, &nd) {
		return false
	}
	fmt.Fprintf(r.env.Stderr, "taskmoor: warning: %v\n", nd)
	return true
}

func (r runner) fail(err error, asJSON bool) int {
	code := ExitUsage
	var refused *app.Refused
	var ue *usageError
	switch {
	case errors.As(err, &refused):
		code = ExitProblem
		for _, reason := range refused.Reasons {
			fmt.Fprintf(r.env.Stderr, "taskmoor: refused: %s\n", text.Line(reason))
		}
	case errors.As(err, &ue):
		fmt.Fprintf(r.env.Stderr, "taskmoor: %s\n\n%s", text.Line(err.Error()), usage)
	default:
		fmt.Fprintf(r.env.Stderr, "taskmoor: %s\n", text.Line(err.Error()))
	}
	if asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": spec.Format, "error": err.Error()})
	}
	return code
}

// writeJSONLine writes one compact JSON object per line (a stream).
func writeJSONLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// parse parses flags allowing positional arguments between them. A "--"
// ends the flags: everything after it is positional.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usagef("%s: %v", fs.Name(), err)
		}
		rest := fs.Args()
		if consumed := args[:len(args)-len(rest)]; len(consumed) > 0 && consumed[len(consumed)-1] == "--" {
			return append(pos, rest...), nil // flag.Parse ate the "--"
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func (r runner) board(ctx context.Context) int {
	if r.env.TUI == nil {
		return r.usageErr(usagef("the board is not available in this build"))
	}
	a := r.app(project.SourceMain)
	if err := r.env.TUI(ctx, a); err != nil {
		return r.fail(err, false)
	}
	return ExitOK
}

func (r runner) list(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = usagef("list takes no arguments")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	rep, err := r.app(project.SourceMain).Report(ctx)
	if err != nil {
		return r.fail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, struct {
			Format int `json:"format"`
			*app.Report
			Warnings []string `json:"warnings"`
		}{spec.Format, rep, rep.Warnings()})
		return ExitOK
	}
	tw := tabwriter.NewWriter(r.env.Stdout, 0, 0, 2, ' ', 0)
	for _, t := range rep.Tasks {
		fmt.Fprintf(tw, "%s\t%s\tP%d\t%s\t%s\n", t.ID, t.Lifecycle, t.Priority, marks(t), text.Line(t.Title))
	}
	tw.Flush()
	r.warnings(rep)
	return ExitOK
}

// marks is the short status column: readiness, needs a person, harness,
// staleness.
func marks(t app.TaskView) string {
	var m []string
	if t.Lifecycle == "backlog" {
		if t.Ready {
			m = append(m, "✓")
		} else {
			m = append(m, "·")
		}
	}
	if t.NeedsPerson {
		m = append(m, "⚑")
	}
	if t.Harness != nil {
		h := t.Harness.Status
		if t.Harness.Kind != "" {
			h += "/" + t.Harness.Kind
		}
		m = append(m, "●"+h)
	}
	if t.State != nil && t.State.Stale {
		m = append(m, "stale")
	}
	if len(m) == 0 {
		return "-"
	}
	return strings.Join(m, " ")
}

func (r runner) warnings(rep *app.Report) {
	for _, w := range rep.Warnings() {
		fmt.Fprintf(r.env.Stderr, "%s\n", text.Line(w))
	}
	if rep.HarnessSource == harness.ReportGuildUnavailable {
		fmt.Fprintln(r.env.Stderr, "warning: Guild: unavailable; harness status from state files")
	}
}

func (r runner) ready(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("ready", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = usagef("ready takes no arguments")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	rep, err := r.app(project.SourceMain).Report(ctx)
	if err != nil {
		return r.fail(err, *asJSON)
	}
	ready := rep.Ready()
	if *asJSON {
		_ = writeJSON(r.env.Stdout, struct {
			Format int            `json:"format"`
			Tasks  []app.TaskView `json:"tasks"`
		}{spec.Format, ready})
		return ExitOK
	}
	tw := tabwriter.NewWriter(r.env.Stdout, 0, 0, 2, ' ', 0)
	for _, t := range ready {
		flag := ""
		if t.NeedsPerson {
			flag = " ⚑ needs a person"
		}
		fmt.Fprintf(tw, "%s\tP%d\t%s%s\n", t.ID, t.Priority, text.Line(t.Title), flag)
	}
	tw.Flush()
	return ExitOK
}

func (r runner) show(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("show takes one task id")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	rep, err := r.app(project.SourceMain).Report(ctx)
	if err != nil {
		return r.fail(err, *asJSON)
	}
	t, ok := rep.Find(pos[0])
	if !ok {
		return r.fail(fmt.Errorf("%s: no such task", pos[0]), *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, struct {
			Format      int           `json:"format"`
			FrontMatter *project.Task `json:"front_matter"`
			app.TaskView
			Body string `json:"body"`
		}{spec.Format, t.Task, t, t.Task.Body})
		return ExitOK
	}
	w := r.env.Stdout
	fmt.Fprintf(w, "%s  %s\n", t.ID, text.Line(t.Title))
	fmt.Fprintf(w, "lifecycle:  %s\n", t.Lifecycle)
	if t.Lifecycle == "backlog" {
		if t.Ready {
			fmt.Fprintf(w, "ready:      yes\n")
		} else {
			fmt.Fprintf(w, "ready:      no\n")
			for _, reason := range t.Reasons {
				fmt.Fprintf(w, "  - %s\n", text.Line(reason))
			}
		}
	}
	fmt.Fprintf(w, "kind:       %s\npriority:   P%d\ncomponent:  %s\nfile:       %s\n", t.Kind, t.Priority, t.Component, t.File)
	if t.NeedsPerson {
		fmt.Fprintf(w, "needs a person\n")
	}
	if len(t.DependsOn) > 0 {
		fmt.Fprintf(w, "depends_on:\n")
		for _, d := range t.DependsOn {
			lc := "unknown task"
			if dv, ok := rep.Find(d); ok {
				lc = dv.Lifecycle
			}
			fmt.Fprintf(w, "  - %s (%s)\n", d, lc)
		}
	}
	fmt.Fprintf(w, "touches:\n")
	for _, p := range t.Touches {
		fmt.Fprintf(w, "  - %s\n", text.Line(p))
	}
	if t.Task.AfterRelease != "" {
		fmt.Fprintf(w, "after_release: %s\n", t.Task.AfterRelease)
	}
	if t.Task.Profile != "" || t.Task.Model != "" {
		fmt.Fprintf(w, "profile: %s  model: %s\n", t.Task.Profile, text.Line(t.Task.Model))
	}
	if t.MergeCommit != "" {
		fmt.Fprintf(w, "merge:      %s\n", t.MergeCommit)
	}
	if t.ReleaseTag != "" {
		fmt.Fprintf(w, "release:    %s\n", t.ReleaseTag)
	}
	if s := t.State; s != nil {
		stale := ""
		if s.Stale {
			stale = " (stale)"
		}
		fmt.Fprintf(w, "state:      %s%s\n  branch:   %s\n  worktree: %s\n  base:     %s\n  started:  %s\n  updated:  %s by %s\n",
			s.Lifecycle, stale, text.Line(s.Branch), text.Line(s.Worktree), s.Base, s.StartedAt, s.UpdatedAt, text.Line(s.Writer))
	}
	if h := t.Harness; h != nil {
		fmt.Fprintf(w, "harness:    %s %s", text.Line(h.Name), h.Status)
		if h.Kind != "" {
			fmt.Fprintf(w, " (%s)", h.Kind)
		}
		fmt.Fprintf(w, " [%s]", h.Source)
		if h.Note != "" {
			fmt.Fprintf(w, " — %s", text.Line(h.Note))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "\n%s", text.Block(t.Task.Body))
	return ExitOK
}

func (r runner) validate(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = usagef("validate takes no arguments")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	rep, err := r.app(project.SourceWorktree).Report(ctx)
	if err != nil {
		return r.fail(err, *asJSON)
	}
	// A16: the service's profiles, when it is configured here
	if paths, err := serve.DefaultPaths(r.env.Getenv); err == nil {
		if _, statErr := os.Stat(paths.Config); statErr == nil {
			if cfg, err := serve.LoadConfig(paths.Config); err == nil {
				for _, w := range serve.ProfileHarnessWarnings(cfg, rep.Tasks) {
					file, msg, _ := strings.Cut(w, ": warning: ")
					rep.Findings = append(rep.Findings, finding.Warnf(file, 0, "%s", msg))
				}
			}
		}
	}
	errs, warns := finding.Count(rep.Findings)
	if *asJSON {
		fs := rep.Findings
		if fs == nil {
			fs = []finding.Finding{}
		}
		_ = writeJSON(r.env.Stdout, map[string]any{
			"format": spec.Format, "source": rep.Source, "findings": fs, "errors": errs, "warnings": warns,
		})
	} else {
		for _, f := range rep.Findings {
			fmt.Fprintln(r.env.Stdout, text.Line(f.String()))
		}
		fmt.Fprintf(r.env.Stdout, "%s, %s (%d tasks, source %s)\n", plural(errs, "error"), plural(warns, "warning"), len(rep.Tasks), rep.Source)
	}
	if errs > 0 {
		return ExitProblem
	}
	return ExitOK
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func (r runner) claim(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	var c app.ClaimArgs
	fs.StringVar(&c.Branch, "branch", "", "")
	fs.StringVar(&c.Worktree, "worktree", "", "")
	fs.StringVar(&c.Harness, "harness", "", "")
	fs.StringVar(&c.Session, "session", "", "")
	fs.BoolVar(&c.Person, "person", false, "")
	fs.BoolVar(&c.Force, "force", false, "")
	pos, err := parse(fs, args)
	switch {
	case err != nil:
	case len(pos) != 1:
		err = usagef("claim takes one task id")
	case c.Branch == "" || c.Worktree == "":
		err = usagef("claim needs --branch and --worktree")
	case c.Session != "" && c.Harness == "":
		err = usagef("--session needs --harness")
	}
	if err != nil {
		return r.fail(err, false)
	}
	c.ID = pos[0]
	f, err := r.app(project.SourceMain).Claim(ctx, c)
	if err != nil && !r.notDurable(err) {
		return r.fail(err, false)
	}
	fmt.Fprintf(r.env.Stdout, "%s claimed: in_progress on %s in %s (base %s)\n", f.ID, f.Branch, f.Worktree, short(f.Base))
	return ExitOK
}

func (r runner) review(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("review takes one task id")
	}
	if err != nil {
		return r.fail(err, false)
	}
	f, changed, err := r.app(project.SourceMain).Review(ctx, pos[0])
	if err != nil && !r.notDurable(err) {
		return r.fail(err, false)
	}
	if changed {
		fmt.Fprintf(r.env.Stdout, "%s: review\n", f.ID)
	} else {
		fmt.Fprintf(r.env.Stdout, "%s: already review\n", f.ID)
	}
	return ExitOK
}

func (r runner) release(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("release takes one task id")
	}
	if err != nil {
		return r.fail(err, false)
	}
	after, err := r.app(project.SourceMain).Release(ctx, pos[0])
	var refused *app.Refused
	if errors.As(err, &refused) && strings.Contains(refused.Error(), "owned by a running taskmoor serve") {
		if code, ok := r.releaseViaService(ctx, pos[0]); ok {
			return code
		}
	}
	if err != nil && !r.notDurable(err) {
		return r.fail(err, false)
	}
	switch after {
	case "":
		fmt.Fprintf(r.env.Stdout, "%s: state file removed\n", pos[0])
	case "backlog":
		fmt.Fprintf(r.env.Stdout, "%s: state file removed; back to backlog\n", pos[0])
	default:
		fmt.Fprintf(r.env.Stdout, "%s: stale state file removed; still %s\n", pos[0], after)
	}
	return ExitOK
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
