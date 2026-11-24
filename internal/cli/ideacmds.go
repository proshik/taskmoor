package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/text"
)

// ideaCmd is stage 8's ideas and intake: add, list, show, grill, review,
// approve, drop.
func (r runner) ideaCmd(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return r.usageErr(usagef("idea takes add, list, show, grill, review, approve or drop"))
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return r.ideaAdd(ctx, rest)
	case "list":
		return r.ideaList(ctx, rest)
	case "show":
		return r.ideaShow(ctx, rest)
	case "grill":
		return r.ideaGrill(ctx, rest)
	case "review":
		return r.ideaReview(ctx, rest)
	case "approve":
		return r.ideaApprove(ctx, rest)
	case "drop":
		return r.ideaDrop(ctx, rest)
	}
	return r.usageErr(usagef("idea takes add, list, show, grill, review, approve or drop"))
}

// ideaAdd: the idea's text is the arguments, or stdin with -.
func (r runner) ideaAdd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("idea add", flag.ContinueOnError)
	proj := fs.String("project", "", "")
	title := fs.String("title", "", "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) == 0 && *title == "" {
		err = usagef("idea add takes the idea's text (or - to read it from stdin) and --title")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	body := strings.Join(pos, " ")
	if body == "-" {
		in := r.env.Stdin
		if in == nil {
			in = os.Stdin
		}
		data, err := io.ReadAll(io.LimitReader(bufio.NewReader(in), 1<<20))
		if err != nil {
			return r.fail(err, *asJSON)
		}
		body = string(data)
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
	i, err := c.AddIdea(ctx, *proj, *title, body)
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "idea": i})
		return ExitOK
	}
	fmt.Fprintf(r.env.Stdout, "%s · %s · %s\ngrill it: taskmoor idea grill %s\n", i.ID, i.Project, text.Line(i.Title), i.ID)
	return ExitOK
}

func (r runner) ideaList(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("idea list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) > 0 {
		err = usagef("idea list takes no arguments")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	ideas, err := c.Ideas(ctx)
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		if ideas == nil {
			ideas = []serve.Idea{}
		}
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "ideas": ideas})
		return ExitOK
	}
	if len(ideas) == 0 {
		fmt.Fprintln(r.env.Stdout, "no ideas: taskmoor idea add --title T TEXT")
	}
	for _, i := range ideas {
		fmt.Fprintf(r.env.Stdout, "%s  %-9s %-12s %s\n", i.ID, i.State, i.Project, text.Line(i.Title))
	}
	return ExitOK
}

func (r runner) ideaShow(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("idea show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("idea show takes one idea id")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	i, err := c.Idea(ctx, pos[0])
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "idea": i})
		return ExitOK
	}
	w := r.env.Stdout
	fmt.Fprintf(w, "%s · %s · %s · %s\n\n%s\n", i.ID, i.Project, i.State, text.Line(i.Title), text.Block(i.Text))
	if i.Session != "" {
		fmt.Fprintf(w, "\nsession %s, branch %s, worktree %s\n", i.Session, i.Branch, i.Worktree)
	}
	if len(i.Tasks) > 0 {
		fmt.Fprintf(w, "tasks: %s\n", strings.Join(i.Tasks, ", "))
	}
	if i.Merged != "" {
		fmt.Fprintf(w, "approved: merged as %s\n", short(i.Merged))
	}
	return ExitOK
}

func (r runner) ideaGrill(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("idea grill", flag.ContinueOnError)
	prof := fs.String("profile", "", "")
	anyway := fs.Bool("anyway", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("idea grill takes one idea id")
	}
	if err != nil {
		return r.usageErr(err)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, false)
	}
	s, err := c.Grill(ctx, pos[0], serve.GrillRequest{Profile: *prof, Anyway: *anyway})
	if err != nil {
		return r.serviceFail(err, false)
	}
	fmt.Fprintf(r.env.Stdout, "%s: grilling in session %s (%s, profile %s)\n  worktree %s, branch %s\n  its questions come to the inbox (and Telegram); follow it: taskmoor feed %s --follow\n",
		pos[0], s.ID, s.Harness, s.Profile, s.Worktree, s.Branch, s.ID)
	return ExitOK
}

func (r runner) ideaReview(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("idea review", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	diff := fs.Bool("diff", false, "")
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("idea review takes one idea id")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	rv, err := c.ReviewIdea(ctx, pos[0])
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	if *asJSON {
		_ = writeJSON(r.env.Stdout, map[string]any{"format": 1, "review": rv})
		return ExitOK
	}
	w := r.env.Stdout
	fmt.Fprintf(w, "%s · %s · branch %s, base %s\n", rv.Idea.ID, text.Line(rv.Idea.Title), rv.Idea.Branch, short(rv.Base))
	fmt.Fprintln(w, "\ntasks it adds:")
	if len(rv.Tasks) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, t := range rv.Tasks {
		fmt.Fprintf(w, "  %s  %s  (%s)\n", t.ID, text.Line(t.Title), t.File)
	}
	fmt.Fprintln(w, "\ncommits:")
	for _, cm := range rv.Commits {
		fmt.Fprintf(w, "  %s\n", text.Line(cm))
	}
	if rv.Stat != "" {
		fmt.Fprintf(w, "\n%s\n", text.Block(rv.Stat))
	}
	for _, e := range rv.Errors {
		fmt.Fprintf(w, "error: %s\n", text.Line(e))
	}
	for _, e := range rv.Warnings {
		fmt.Fprintf(w, "warning: %s\n", text.Line(e))
	}
	if rv.MainMoved {
		fmt.Fprintf(w, "\n%s has moved since the branch was made: Approve sends it back to be brought up to date first\n", rv.MainBranch)
	}
	if rv.Result != "" {
		fmt.Fprintf(w, "\nthe agent's summary:\n%s\n", text.Block(rv.Result))
	}
	if *diff {
		fmt.Fprintf(w, "\n%s\n", text.Block(rv.Diff))
		if rv.DiffCut {
			fmt.Fprintf(w, "\n(the diff is cut at 256 KiB: see it whole with git diff %s %s in %s)\n", rv.Base, rv.Tip, rv.Idea.ProjectPath)
		}
	} else {
		fmt.Fprintln(w, "\n(--diff shows the diff)")
	}
	if rv.Ready {
		// the full commit: Approve lands exactly the one looked at
		fmt.Fprintf(w, "\nready: taskmoor idea approve %s --tip %s --main %s lands exactly this on %s (push stays yours)\n", rv.Idea.ID, rv.Tip, rv.Main, rv.MainBranch)
	} else {
		fmt.Fprintf(w, "\nnot ready: %s\n", text.Line(rv.Why))
	}
	return ExitOK
}

func (r runner) ideaApprove(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("idea approve", flag.ContinueOnError)
	tip := fs.String("tip", "", "")
	main := fs.String("main", "", "")
	pos, err := parse(fs, args)
	if err == nil && (len(pos) != 1 || *tip == "" || *main == "") {
		err = usagef("idea approve takes one idea id, --tip COMMIT and --main COMMIT (taskmoor idea review ID prints the command)")
	}
	if err != nil {
		return r.usageErr(err)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, false)
	}
	rv, err := c.ReviewIdea(ctx, pos[0])
	if err != nil {
		return r.serviceFail(err, false)
	}
	// sent as given: the service lands only that exact commit
	res, err := c.Approve(ctx, pos[0], *tip, *main)
	if err != nil {
		return r.serviceFail(err, false)
	}
	if res.SentBack {
		fmt.Fprintf(r.env.Stdout, "%s: %s has moved; sent back to its session to bring the branch up to date, then look again\n", pos[0], rv.MainBranch)
		return ExitProblem
	}
	fmt.Fprintf(r.env.Stdout, "%s approved: %s merged into %s as %s; push is yours. The worktree and the branch stay.\n",
		pos[0], strings.Join(res.Idea.Tasks, ", "), rv.MainBranch, short(res.Merged))
	return ExitOK
}

func (r runner) ideaDrop(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("idea drop", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err == nil && len(pos) != 1 {
		err = usagef("idea drop takes one idea id")
	}
	if err != nil {
		return r.usageErr(err)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, false)
	}
	i, err := c.DropIdea(ctx, pos[0])
	if err != nil {
		return r.serviceFail(err, false)
	}
	msg := i.ID + " dropped"
	if i.Branch != "" {
		msg += fmt.Sprintf("; its branch %s and worktree %s stay", i.Branch, i.Worktree)
	}
	fmt.Fprintln(r.env.Stdout, msg)
	return ExitOK
}
