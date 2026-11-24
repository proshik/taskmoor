package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/text"
)

// cleanupCmd moves merged work's worktrees to the trash and deletes their
// branches (stage 12): one (a task, session or idea id) or all of them;
// --empty-trash deletes what the trash holds.
func (r runner) cleanupCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	proj := fs.String("project", "", "")
	dry := fs.Bool("dry-run", false, "")
	empty := fs.Bool("empty-trash", false, "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parse(fs, args)
	switch {
	case err != nil:
	case len(pos) > 1:
		err = usagef("cleanup takes at most one id (a task, session or idea)")
	case len(pos) == 1 && strings.TrimSpace(pos[0]) == "":
		err = usagef("cleanup was given an empty id: name one, or give none for all")
	case *empty && len(pos) > 0:
		err = usagef("--empty-trash takes no id")
	}
	if err != nil {
		return r.fail(err, *asJSON)
	}
	c, err := r.client()
	if err != nil {
		return r.fail(err, *asJSON)
	}
	w := r.env.Stdout
	if *empty {
		gone, err := c.EmptyTrash(ctx, *proj)
		if err != nil {
			return r.serviceFail(err, *asJSON)
		}
		if *asJSON {
			if gone == nil {
				gone = []string{}
			}
			_ = writeJSON(w, map[string]any{"format": 1, "emptied": gone})
			return ExitOK
		}
		if len(gone) == 0 {
			fmt.Fprintln(w, "the trash is empty")
		}
		for _, g := range gone {
			fmt.Fprintf(w, "deleted %s\n", g)
		}
		return ExitOK
	}
	id := ""
	if len(pos) == 1 {
		id = pos[0]
	}
	items, err := c.Cleanup(ctx, id, *proj, id == "", *dry)
	if err != nil {
		return r.serviceFail(err, *asJSON)
	}
	kept := 0
	for _, it := range items {
		if !it.Removed {
			kept++
		}
	}
	status := ExitOK
	if kept > 0 {
		status = ExitProblem // something stays: the same in both formats
	}
	if *asJSON {
		if items == nil {
			items = []serve.CleanupItem{}
		}
		_ = writeJSON(w, map[string]any{"format": 1, "items": items})
		return status
	}
	if len(items) == 0 {
		fmt.Fprintln(w, "nothing to clean up: no accepted task or approved idea has a worktree or branch left")
	}
	for _, it := range items {
		switch {
		case it.Removed && *dry:
			fmt.Fprintf(w, "would clean  %s %s: %s to the trash, branch %s deleted\n", it.Kind, it.ID, it.Worktree, it.Branch)
		case it.WorktreeRemoved && !it.Removed:
			fmt.Fprintf(w, "worktree only %s %s: %s\n", it.Kind, it.ID, text.Line(it.Kept))
		case it.Removed:
			fmt.Fprintf(w, "cleaned      %s %s: branch %s deleted", it.Kind, it.ID, it.Branch)
			if it.Trash != "" {
				fmt.Fprintf(w, ", worktree in %s", it.Trash)
			}
			fmt.Fprintln(w)
		default:
			fmt.Fprintf(w, "kept         %s %s: %s\n", it.Kind, it.ID, text.Line(it.Kept))
		}
	}
	if !*dry && kept < len(items) {
		fmt.Fprintln(w, "(nothing was deleted from the worktrees: taskmoor cleanup --empty-trash deletes the trash)")
	}
	return status
}
