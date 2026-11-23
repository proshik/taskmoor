package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/proshik/taskmoor/internal/gitx"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/schema"
	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/state"
	"github.com/proshik/taskmoor/internal/text"
)

func (r runner) paths() (serve.Paths, error) { return serve.DefaultPaths(r.env.Getenv) }

// serviceFail maps a service error: a refusal is exit 1, anything else 2.
func (r runner) serviceFail(err error, asJSON bool) int {
	var ae *serve.APIError
	if errors.As(err, &ae) && ae.Refused() {
		fmt.Fprintf(r.env.Stderr, "taskmoor: refused: %s\n", text.Line(ae.Msg))
		if asJSON {
			_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "error": ae.Msg})
		}
		return ExitProblem
	}
	return r.fail(err, asJSON)
}

func (r runner) client() (*serve.Client, error) {
	p, err := r.paths()
	if err != nil {
		return nil, err
	}
	return serve.Connect(p.Store)
}

// scopedClient is client() with task-id lookups scoped to the project of
// the working directory (or --repo), when it is a registered one.
func (r runner) scopedClient(ctx context.Context) (*serve.Client, error) {
	c, err := r.client()
	if err != nil {
		return nil, err
	}
	p, err := r.paths()
	if err != nil {
		return c, nil
	}
	cfg, err := serve.LoadConfig(p.Config)
	if err != nil {
		return c, nil
	}
	if name, err := r.projectFor(ctx, cfg, false); err == nil {
		return c.Project(name), nil
	}
	return c, nil
}

func (r runner) serveCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = usagef("serve takes no arguments")
	}
	if err != nil {
		return r.fail(err, false)
	}
	p, err := r.paths()
	if err != nil {
		return r.fail(err, false)
	}
	svc, err := serve.Open(p, versionOr(r.env.Version), os.Environ(), r.env.Getenv("HOME"))
	if err != nil {
		return r.fail(err, false)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	logf := func(format string, a ...any) {
		fmt.Fprintf(r.env.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
	}
	if err := serve.Serve(ctx, svc, logf); err != nil {
		return r.fail(err, false)
	}
	return ExitOK
}

var projectNameRe = regexp.MustCompile(`[^a-z0-9-]+`)

func (r runner) webCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	printOnly := fs.Bool("print", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = usagef("web takes no arguments")
	}
	if err != nil {
		return r.fail(err, false)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, false)
	}
	u, err := c.LoginURL(ctx)
	if err != nil {
		return r.serviceFail(err, false)
	}
	if *printOnly || r.env.OpenURL == nil {
		fmt.Fprintln(r.env.Stdout, u)
		return ExitOK
	}
	if err := r.env.OpenURL(u); err != nil {
		fmt.Fprintf(r.env.Stderr, "taskmoor: could not open the browser (%v); open this one-time link:\n", err)
		fmt.Fprintln(r.env.Stdout, u)
		return ExitOK
	}
	fmt.Fprintln(r.env.Stdout, "opened the board in the browser (a one-time sign-in link)")
	return ExitOK
}

func (r runner) handoverCmd(ctx context.Context, args []string) int {
	if len(args) != 1 {
		return r.usageErr(usagef("continue takes one session or task id"))
	}
	c, err := r.scopedClient(ctx)
	if err != nil {
		return r.fail(err, false)
	}
	s, cmd, err := c.Handover(ctx, args[0])
	if err != nil {
		return r.serviceFail(err, false)
	}
	fmt.Fprintf(r.env.Stdout, "%s is yours now; continue it with:\n  %s\nthen finish with taskmoor review %s\n", s.Task, cmd, s.Task)
	return ExitOK
}

// restartCmd is "Restart with a profile" (A24).
func (r runner) restartCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	profile := fs.String("profile", "", "")
	anyway := fs.Bool("anyway", false, "")
	pos, err := parse(fs, args)
	if err == nil && (len(pos) != 1 || *profile == "") {
		err = usagef("restart takes one session or task id and --profile NAME")
	}
	if err != nil {
		return r.usageErr(err)
	}
	c, err := r.scopedClient(ctx)
	if err != nil {
		return r.fail(err, false)
	}
	s, err := c.Restart(ctx, pos[0], *profile, *anyway)
	if err != nil && s == nil {
		return r.serviceFail(err, false)
	}
	fmt.Fprintf(r.env.Stdout, "%s restarted: session %s (%s, profile %s) in %s\n", s.Task, s.ID, s.Harness, s.Profile, s.Worktree)
	if err != nil {
		fmt.Fprintf(r.env.Stderr, "taskmoor: the new session did not start: %v\n", err)
		return ExitProblem
	}
	return ExitOK
}

// keepCmd is "Keep N busy" (A4).
func (r runner) keepCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("keep", flag.ContinueOnError)
	proj := fs.String("project", "", "")
	pos, err := parse(fs, args)
	n := 0
	if err == nil {
		if len(pos) != 1 {
			err = usagef("keep takes the number of sessions to keep busy (0 turns it off)")
		} else if _, e := fmt.Sscanf(pos[0], "%d", &n); e != nil || fmt.Sprint(n) != pos[0] {
			err = usagef("keep: %q is not a number", pos[0])
		}
	}
	if err != nil {
		return r.usageErr(err)
	}
	if *proj == "" {
		p, err := r.paths()
		if err != nil {
			return r.fail(err, false)
		}
		cfg, err := serve.LoadConfig(p.Config)
		if err != nil {
			return r.fail(err, false)
		}
		if *proj, err = r.projectFor(ctx, cfg, true); err != nil {
			return r.usageErr(err)
		}
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, false)
	}
	if err := c.Keep(ctx, *proj, n); err != nil {
		return r.serviceFail(err, false)
	}
	if n == 0 {
		fmt.Fprintf(r.env.Stdout, "%s: keep busy is off; running sessions go on\n", *proj)
	} else {
		fmt.Fprintf(r.env.Stdout, "%s: the service keeps %d session(s) busy with ready tasks (priority then id, never needs_person)\n", *proj, n)
	}
	return ExitOK
}

func (r runner) projectCmd(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return r.usageErr(usagef("project takes add, list or remove"))
	}
	p, err := r.paths()
	if err != nil {
		return r.fail(err, false)
	}
	if args[0] == "add" || args[0] == "remove" {
		unlock, err := serve.LockConfig(p.Config)
		if err != nil {
			return r.fail(err, false)
		}
		defer unlock()
	}
	cfg, err := serve.LoadConfig(p.Config)
	if err != nil {
		return r.fail(err, false)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		fs := flag.NewFlagSet("project add", flag.ContinueOnError)
		name := fs.String("name", "", "")
		pos, err := parse(fs, rest)
		if err == nil && len(pos) != 1 {
			err = usagef("project add takes one repository path")
		}
		if err != nil {
			return r.fail(err, false)
		}
		dir := pos[0]
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(r.env.Dir, dir)
		}
		root, err := gitx.Toplevel(ctx, dir)
		if err != nil {
			return r.fail(fmt.Errorf("%s is not in a git repository: %w", dir, err), false)
		}
		if *name == "" {
			*name = strings.Trim(projectNameRe.ReplaceAllString(strings.ToLower(filepath.Base(root)), "-"), "-")
			if *name == "" || (*name)[0] < 'a' || (*name)[0] > 'z' {
				*name = "p-" + *name
			}
		}
		if cfg.ProjectByName(*name) != nil {
			return r.fail(&usageError{msg: fmt.Sprintf("a project named %s is already registered", *name)}, false)
		}
		for _, pr := range cfg.Projects {
			if pr.Path == root {
				return r.fail(&usageError{msg: fmt.Sprintf("%s is already registered as %s", root, pr.Name)}, false)
			}
		}
		cfg.Projects = append(cfg.Projects, serve.Project{Name: *name, Path: root})
		// validate before replacing the file, so a bad name leaves the
		// working configuration as it was
		if err := cfg.Check(p.Config); err != nil {
			return r.fail(&usageError{msg: err.Error()}, false)
		}
		if err := cfg.Save(p.Config); err != nil {
			return r.fail(err, false)
		}
		fmt.Fprintf(r.env.Stdout, "project %s: %s\n", *name, root)
	case "list":
		tw := tabwriter.NewWriter(r.env.Stdout, 0, 0, 2, ' ', 0)
		for _, pr := range cfg.Projects {
			fmt.Fprintf(tw, "%s\t%s\n", pr.Name, pr.Path)
		}
		tw.Flush()
	case "remove":
		if len(rest) != 1 {
			return r.usageErr(usagef("project remove takes one project name"))
		}
		kept := cfg.Projects[:0]
		found := false
		for _, pr := range cfg.Projects {
			if pr.Name == rest[0] {
				found = true
				continue
			}
			kept = append(kept, pr)
		}
		if !found {
			return r.fail(&usageError{msg: fmt.Sprintf("no project %s", rest[0])}, false)
		}
		cfg.Projects = kept
		if err := cfg.Save(p.Config); err != nil {
			return r.fail(err, false)
		}
		fmt.Fprintf(r.env.Stdout, "project %s removed from the configuration (nothing in the repository changed)\n", rest[0])
	default:
		return r.usageErr(usagef("unknown project command %q", sub))
	}
	return ExitOK
}

// projectFor finds the registered project of the working directory (or
// --repo): the one sharing its git common dir; with one project, that one.
func (r runner) projectFor(ctx context.Context, cfg *serve.Config, onlyOne bool) (string, error) {
	dir := r.g.repo
	if dir == "" {
		dir = r.env.Dir
	}
	if top, err := gitx.Toplevel(ctx, dir); err == nil {
		mine, err := gitx.Repo{Dir: top}.CommonDir(ctx)
		if err == nil {
			for _, p := range cfg.Projects {
				if c, err := (gitx.Repo{Dir: p.Path}).CommonDir(ctx); err == nil && c == mine {
					return p.Name, nil
				}
			}
		}
	}
	if onlyOne && len(cfg.Projects) == 1 {
		return cfg.Projects[0].Name, nil
	}
	return "", usagef("which project? pass --project (taskmoor project list)")
}

func (r runner) runCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	proj := fs.String("project", "", "")
	prof := fs.String("profile", "", "")
	person := fs.Bool("person", false, "")
	anyway := fs.Bool("anyway", false, "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("run takes one task id")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	if *proj == "" {
		p, err := r.paths()
		if err != nil {
			return r.fail(err, *asJSON)
		}
		cfg, err := serve.LoadConfig(p.Config)
		if err != nil {
			return r.fail(err, *asJSON)
		}
		if *proj, err = r.projectFor(ctx, cfg, true); err != nil {
			return r.fail(err, *asJSON)
		}
	}
	s, err := c.Run(ctx, serve.RunRequest{Project: *proj, Task: pos[0], Profile: *prof, Person: *person, Anyway: *anyway})
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "session": s})
		return ExitOK
	}
	fmt.Fprintf(r.env.Stdout, "%s started: session %s (%s, profile %s)\n  worktree %s, branch %s\n  follow it: taskmoor feed %s --follow\n",
		s.Task, s.ID, s.Harness, s.Profile, s.Worktree, s.Branch, s.ID)
	return ExitOK
}

func (r runner) sessionsCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	if _, err := parse(fs, args); err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "sessions": snap.Sessions})
		return ExitOK
	}
	tw := tabwriter.NewWriter(r.env.Stdout, 0, 0, 2, ' ', 0)
	for _, s := range snap.Sessions {
		bill := s.Billing
		if bill == "" {
			bill = "-"
		}
		box := "UNCONFINED"
		switch {
		case s.Confined:
			box = "sandboxed"
		case s.Isolation == "codex":
			box = "codex-sandbox"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Task, s.State, s.Profile, bill, box, s.StartedAt, s.ID, text.Line(s.Title))
	}
	tw.Flush()
	return ExitOK
}

func (r runner) feedCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("feed", flag.ContinueOnError)
	follow := fs.Bool("follow", false, "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("feed takes one session or task id")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.scopedClient(ctx)
	if err != nil {
		return r.fail(err, *asJSON)
	}
	if !*follow {
		s, acts, err := c.Feed(ctx, pos[0])
		if err != nil {
			return r.serviceFail(err, *asJSON)
		}
		if *asJSON {
			_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "session": s, "activities": acts})
			return ExitOK
		}
		r.printHeader(*s)
		for _, a := range acts {
			r.printActivity(a)
		}
		return ExitOK
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	id := pos[0]
	printed := 0 // activities shown so far (the feed only grows)
	first := true
	for {
		// position first, then the feed: nothing falls between the two
		snap, err := c.Snapshot(ctx)
		if err != nil {
			return r.serviceFail(err, *asJSON)
		}
		s, acts, err := c.Feed(ctx, id)
		if err != nil {
			return r.serviceFail(err, *asJSON)
		}
		id = s.ID // stay on this session even if the task gets a newer one
		if first {
			if !*asJSON {
				r.printHeader(*s)
			}
			first = false
		}
		for _, a := range acts[min(printed, len(acts)):] {
			r.emitActivity(a, *asJSON)
		}
		printed = len(acts)
		if ended(s.State) {
			r.emitEnd(s.State, *asJSON)
			return ExitOK
		}
		reset := false
		err = c.Events(ctx, snap.Epoch, snap.Rev, func(ev serve.Event) bool {
			switch ev.Type {
			case "reset":
				reset = true
				return false
			case "activity":
				m, _ := ev.Data.(map[string]any)
				if m["session"] != id {
					return true
				}
				r.emitActivity(activityFrom(m["activity"]), *asJSON)
				printed++
			case "session":
				m, _ := ev.Data.(map[string]any)
				if m["id"] == id {
					if st, _ := m["state"].(string); ended(st) {
						r.emitEnd(st, *asJSON)
						return false
					}
				}
			}
			return true
		})
		switch {
		case ctx.Err() != nil:
			return ExitOK
		case reset:
			continue // missed events: read the feed again from where it was
		case err != nil:
			return r.fail(fmt.Errorf("the event stream broke: %w", err), *asJSON)
		default:
			return ExitOK
		}
	}
}

func (r runner) printHeader(s serve.Session) {
	fmt.Fprintf(r.env.Stdout, "%s · %s · %s · %s\n", s.Task, text.Line(s.Title), s.State, s.Worktree)
}

func (r runner) emitActivity(a serve.Activity, asJSON bool) {
	if asJSON {
		_ = writeJSONLine(r.env.Stdout, map[string]any{"format": 1, "activity": a})
		return
	}
	r.printActivity(a)
}

func (r runner) emitEnd(state string, asJSON bool) {
	if asJSON {
		_ = writeJSONLine(r.env.Stdout, map[string]any{"format": 1, "ended": state})
		return
	}
	fmt.Fprintf(r.env.Stdout, "— session %s\n", state)
}

func ended(st string) bool {
	switch st {
	case serve.StateComplete, serve.StateError, serve.StateStopped, serve.StateStale, serve.StateHandedOver:
		return true
	}
	return false
}

func activityFrom(v any) serve.Activity {
	m, _ := v.(map[string]any)
	str := func(k string) string { s, _ := m[k].(string); return s }
	seq, _ := m["seq"].(float64)
	return serve.Activity{Seq: int(seq), At: str("at"), Type: str("type"), Text: str("text"), Tool: str("tool"), Ask: str("ask"), By: str("by")}
}

func (r runner) printActivity(a serve.Activity) {
	at := a.At
	if t, err := time.Parse("2006-01-02T15:04:05Z", a.At); err == nil {
		at = t.Local().Format("15:04:05")
	}
	what := a.Type
	if a.Tool != "" {
		what += " " + a.Tool
	}
	if a.By != "" && a.By != "person" {
		what += " (" + a.By + ")"
	}
	extra := ""
	if a.Type == serve.ActElicitation && a.Ask != "" {
		extra = "  → taskmoor inbox; answer " + a.Ask
	}
	lines := strings.Split(strings.TrimRight(text.Block(a.Text), "\n"), "\n")
	fmt.Fprintf(r.env.Stdout, "%s  %-12s %s%s\n", at, what, lines[0], extra)
	for _, l := range lines[1:] {
		fmt.Fprintf(r.env.Stdout, "%s  %s\n", strings.Repeat(" ", len(at)+12), l)
	}
}

func (r runner) inboxCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	if _, err := parse(fs, args); err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "asks": snap.Asks})
		return ExitOK
	}
	if len(snap.Asks) == 0 {
		fmt.Fprintln(r.env.Stdout, "nothing waits for you")
		return ExitOK
	}
	w := r.env.Stdout
	for _, a := range snap.Asks {
		fmt.Fprintf(w, "%s  %s  %s  %s\n", a.ID, a.Task, a.Kind, a.CreatedAt)
		if a.Kind == "approval" {
			fmt.Fprintf(w, "  %s:\n", a.Tool)
			for _, l := range strings.Split(text.Block(a.Detail), "\n") {
				fmt.Fprintf(w, "    %s\n", l)
			}
			fmt.Fprintf(w, "  why it asks: %s\n  answer: taskmoor answer %s --allow | --deny [--text WHY]\n", text.Line(a.Reason), a.ID)
			continue
		}
		for qi, q := range a.Questions {
			head := text.Line(q.Question)
			if q.Header != "" {
				head = "[" + text.Line(q.Header) + "] " + head
			}
			if q.MultiSelect {
				head += " (several allowed)"
			}
			fmt.Fprintf(w, "  %d. %s\n", qi+1, head)
			for oi, o := range q.Options {
				mark := " "
				if oi == q.Recommended {
					mark = "*"
				}
				desc := ""
				if o.Description != "" {
					desc = " — " + text.Line(o.Description)
				}
				fmt.Fprintf(w, "     %s %d) %s%s\n", mark, oi+1, text.Line(o.Label), desc)
			}
		}
		fmt.Fprintf(w, "  answer: taskmoor answer %s N [N…]   (one per question; * is recommended; --text for your own words)\n", a.ID)
	}
	return ExitOK
}

func (r runner) answerCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	allow := fs.Bool("allow", false, "")
	deny := fs.Bool("deny", false, "")
	txt := fs.String("text", "", "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	switch {
	case err != nil:
	case len(pos) == 0:
		err = usagef("answer takes an ask id, then the choices")
	case *allow && *deny:
		err = usagef("--allow or --deny, not both")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	ans := serve.Answer{Text: *txt, Allow: *allow}
	if !*allow && !*deny {
		for _, ch := range pos[1:] {
			// "1,3" picks several options of one question
			ans.Choices = append(ans.Choices, strings.Split(ch, ","))
		}
		if len(ans.Choices) == 0 && *txt == "" {
			return r.fail(usagef("give a choice per question, --text, or --allow/--deny"), *asJSON)
		}
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	if err := c.Answer(ctx, pos[0], ans); err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "answered": pos[0]})
		return ExitOK
	}
	fmt.Fprintf(r.env.Stdout, "answered %s\n", pos[0])
	return ExitOK
}

// acceptanceCmd shows what Accept would merge (A8).
func (r runner) acceptanceCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("acceptance", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	diff := fs.Bool("diff", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("acceptance takes one session or task id")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.scopedClient(ctx)
	if err != nil {
		return r.fail(err, *asJSON)
	}
	a, err := c.Acceptance(ctx, pos[0])
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "acceptance": a})
		return ExitOK
	}
	w := r.env.Stdout
	fmt.Fprintf(w, "%s · %s · branch %s, base %s\n", a.Session.Task, text.Line(a.Session.Title), a.Session.Branch, short(a.Base))
	fmt.Fprintf(w, "\ncommits:\n")
	for _, cm := range a.Commits {
		fmt.Fprintf(w, "  %s\n", text.Line(cm))
	}
	fmt.Fprintf(w, "\n%s\n", text.Block(a.Stat))
	if a.Dirty != "" {
		fmt.Fprintf(w, "\nuncommitted in the worktree (not merged):\n%s\n", text.Block(a.Dirty))
	}
	if rv := a.Review; rv != nil {
		fmt.Fprintf(w, "\nreview: %d of %d round(s), %s", rv.Done, rv.Rounds, rv.State)
		if rv.Partial != "" {
			fmt.Fprintf(w, " (partial: %s)", rv.Partial)
		}
		if rv.Tip != "" {
			fmt.Fprintf(w, ", of %s", short(rv.Tip))
		}
		if rv.Unreviewed {
			fmt.Fprint(w, " (the last findings' fixes are not reviewed again)")
		}
		if rv.Error != "" {
			fmt.Fprintf(w, ": %s", text.Line(rv.Error))
		}
		fmt.Fprintln(w)
		if rv.Archive != "" {
			fmt.Fprintf(w, "  revust view --archive %s\n", rv.Archive)
		}
	} else {
		fmt.Fprintln(w, "\nreview: none (reviews.code is 0)")
	}
	if a.Checks != "" {
		fmt.Fprintf(w, "\nchecks:\n%s\n", text.Block(a.Checks))
	}
	if a.Result != "" {
		fmt.Fprintf(w, "\nthe agent's summary:\n%s\n", text.Block(a.Result))
	}
	if *diff {
		fmt.Fprintf(w, "\n%s\n", text.Block(a.Diff))
		if a.DiffCut {
			fmt.Fprintf(w, "\n(the diff is cut at 256 KiB: see it whole with git diff %s %s in %s)\n", short(a.Base), short(a.Tip), a.Session.Worktree)
		}
	} else if a.DiffCut {
		fmt.Fprintln(w, "\n(the diff is longer than 256 KiB: --diff shows the first part)")
	}
	if a.Ready {
		fmt.Fprintf(w, "\nready: taskmoor accept %s --tip %s merges exactly this into %s (push stays yours)\n", a.Session.Task, short(a.Tip), a.MainBranch)
	} else {
		fmt.Fprintf(w, "\nnot ready: %s\n", text.Line(a.Why))
	}
	return ExitOK
}

// acceptCmd is Accept (A8): of the commit the person looked at, named by
// --tip as `taskmoor acceptance` prints it.
func (r runner) acceptCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	tip := fs.String("tip", "", "")
	pos, err := parse(fs, args)
	if err == nil && (len(pos) != 1 || *tip == "") {
		err = usagef("accept takes one session or task id and --tip COMMIT (taskmoor acceptance ID shows both)")
	}
	if err != nil {
		return r.usageErr(err)
	}
	c, err := r.scopedClient(ctx)
	if err != nil {
		return r.fail(err, false)
	}
	a, err := c.Acceptance(ctx, pos[0])
	if err != nil {
		return r.serviceFail(err, false)
	}
	if !strings.HasPrefix(a.Tip, *tip) || len(*tip) < 7 {
		fmt.Fprintf(r.env.Stderr, "taskmoor: %s is at %s now, not %s: look at it again (taskmoor acceptance %s)\n", a.Session.Branch, short(a.Tip), *tip, pos[0])
		return ExitProblem
	}
	res, err := c.Accept(ctx, pos[0], a.Tip)
	if err != nil {
		return r.serviceFail(err, false)
	}
	if res.Conflicted {
		where := "conflicts"
		if len(res.Conflicts) > 0 {
			where += " in " + strings.Join(res.Conflicts, ", ")
		}
		fmt.Fprintf(r.env.Stdout, "%s: %s with %s: sent back to its session to merge %s into its branch\n", res.Session.Task, where, a.MainBranch, a.MainBranch)
		return ExitProblem
	}
	fmt.Fprintf(r.env.Stdout, "%s merged into %s as %s; push is yours. The worktree and the branch stay.\n", res.Session.Task, a.MainBranch, short(res.Merged))
	return ExitOK
}

// usageCmd shows the plans' usage and the guard (stage 6).
func (r runner) usageCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	if _, err := parse(fs, args); err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	us, th, running, err := c.Usage(ctx)
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "usage": us, "threshold": th, "agents_running": running})
		return ExitOK
	}
	if len(us) == 0 {
		fmt.Fprintln(r.env.Stdout, "no subscription plan read yet (claude and codex profiles without API billing are read every 5 minutes)")
	}
	for _, u := range us {
		left := "unknown"
		if u.Left >= 0 {
			left = fmt.Sprintf("%d%% left", u.Left)
		}
		fmt.Fprintf(r.env.Stdout, "%s: %s", u.Harness, left)
		for _, w := range u.Windows {
			fmt.Fprintf(r.env.Stdout, " · %s %d%%", w.Name, w.Left)
			if w.ResetsAt != "" {
				fmt.Fprintf(r.env.Stdout, " (resets %s)", w.ResetsAt)
			}
		}
		if u.Error != "" {
			fmt.Fprintf(r.env.Stdout, " · last read failed: %s", text.Line(u.Error))
		}
		fmt.Fprintln(r.env.Stdout)
	}
	if th > 0 {
		fmt.Fprintf(r.env.Stdout, "guard: below %d%% left, automatic starts wait and manual ones need --anyway\n", th)
	} else {
		fmt.Fprintln(r.env.Stdout, "guard: off (usage_threshold = 0)")
	}
	if running {
		fmt.Fprintln(r.env.Stdout, "agents running: the Mac is held awake (keep the lid open or on power)")
	}
	return ExitOK
}

// decisionsCmd lists the dispatcher's decisions (C3).
func (r runner) decisionsCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("decisions", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = usagef("decisions takes no arguments")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	ds, err := c.Decisions(ctx)
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "decisions": ds})
		return ExitOK
	}
	if len(ds) == 0 {
		fmt.Fprintln(r.env.Stdout, "the dispatcher has decided nothing yet")
		return ExitOK
	}
	for _, d := range ds {
		q := ""
		if len(d.Questions) > 0 {
			q = d.Questions[0].Question
		}
		fmt.Fprintf(r.env.Stdout, "%s  %s  %s  level %s, %s: %s\n", d.ID, d.At, d.Task, d.Level, d.AnsweredBy, text.Line(q))
		if len(d.Answer) > 0 {
			var parts []string
			for _, a := range d.Answer {
				if len(a) > 0 {
					parts = append(parts, strings.Join(a, ", "))
				}
			}
			fmt.Fprintf(r.env.Stdout, "  answer: %s\n", text.Line(strings.Join(parts, "; ")))
		}
		if d.Citation != "" {
			fmt.Fprintf(r.env.Stdout, "  citation: %s\n", text.Line(d.Citation))
		}
		fmt.Fprintf(r.env.Stdout, "  reason: %s\n", text.Line(d.Reason))
		if d.Overridden != nil {
			fmt.Fprintf(r.env.Stdout, "  overridden %s: %s\n", d.Overridden.At, text.Line(d.Overridden.Answer))
		} else if d.AnsweredBy == "dispatcher" {
			fmt.Fprintf(r.env.Stdout, "  override: taskmoor override %s N [N…] | --text T\n", d.ID)
		}
	}
	return ExitOK
}

// overrideCmd replaces a dispatcher answer with the person's (C3).
func (r runner) overrideCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("override", flag.ContinueOnError)
	txt := fs.String("text", "", "")
	pos, err := parse(fs, args)
	if err == nil && (len(pos) == 0 || len(pos) == 1 && *txt == "") {
		err = usagef("override takes a decision id, then a choice per question or --text")
	}
	if err != nil {
		return r.usageErr(err)
	}
	ans := serve.Answer{Text: *txt}
	for _, ch := range pos[1:] {
		ans.Choices = append(ans.Choices, strings.Split(ch, ","))
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, false)
	}
	d, err := c.Override(ctx, pos[0], ans)
	if err != nil {
		return r.serviceFail(err, false)
	}
	fmt.Fprintf(r.env.Stdout, "overridden: %s gets the person's answer (%s) as an instruction\n", d.Task, text.Line(d.Overridden.Answer))
	return ExitOK
}

func (r runner) stopCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("stop takes one session or task id")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.scopedClient(ctx)
	if err != nil {
		return r.fail(err, *asJSON)
	}
	s, err := c.Stop(ctx, pos[0])
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "session": s})
		return ExitOK
	}
	fmt.Fprintf(r.env.Stdout, "%s: %s; the claim, worktree and branch stay (resume: taskmoor say %s …)\n", s.Task, s.State, s.ID)
	return ExitOK
}

func (r runner) sayCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("say", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	anyway := fs.Bool("anyway", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) < 2 {
		err = usagef("say takes a session or task id and the instruction (put -- before an instruction that starts with -)")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.scopedClient(ctx)
	if err != nil {
		return r.fail(err, *asJSON)
	}
	s, err := c.Say(ctx, pos[0], strings.Join(pos[1:], " "), *anyway)
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "session": s})
		return ExitOK
	}
	fmt.Fprintf(r.env.Stdout, "%s: sent (%s)\n", s.Task, s.State)
	return ExitOK
}

// releaseViaService hands a task the live service owns back through it
// (A6): by the exact session the state file names, to the service of this
// store, which checks that session's claim before removing anything.
// ok=false when that service is not reachable or does not know the
// session: the local refusal stands.
func (r runner) releaseViaService(ctx context.Context, id string) (code int, ok bool) {
	a := r.app(project.SourceMain)
	snap, err := a.LoadSnapshot(ctx)
	if err != nil {
		return 0, false
	}
	sch, err := schema.Load()
	if err != nil {
		return 0, false
	}
	e, exists, err := state.Read(snap.StateDir, id, sch)
	if err != nil || !exists || !e.Valid() || e.File.Harness == nil || e.File.Harness.Session == "" {
		return 0, false
	}
	c, err := r.client()
	if err != nil {
		return 0, false
	}
	s, after, err := c.Release(ctx, e.File.Harness.Session)
	if err != nil {
		var ae *serve.APIError
		if errors.As(err, &ae) && ae.Status == 404 {
			return 0, false
		}
		return r.serviceFail(err, false), true
	}
	fmt.Fprintf(r.env.Stdout, "%s: released through taskmoor serve (session %s stopped); now %s\n", s.Task, s.ID, after)
	return ExitOK, true
}
