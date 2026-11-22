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
	s, err := c.Run(ctx, serve.RunRequest{Project: *proj, Task: pos[0], Profile: *prof, Person: *person})
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.Task, s.State, s.Profile, bill, s.StartedAt, s.ID, text.Line(s.Title))
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
	s, err := c.Say(ctx, pos[0], strings.Join(pos[1:], " "))
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
