package serve

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/schema"
)

func schemaLoad() (*schema.Schemas, error) { return schema.Load() }

// chooseProfile: the person's choice at Run → the task's profile →
// tasks.toml's routing for the kind → routing.default → the service's
// default (C10).
func chooseProfile(cfg *Config, pc *project.Config, t app.TaskView, asked string) (*Profile, error) {
	pick := func(name, from string) (*Profile, error) {
		if p := cfg.Profile(name); p != nil {
			return p, nil
		}
		return nil, refusedf("%s names profile %q, which is not in the service's configuration", from, name)
	}
	switch {
	case asked != "":
		return pick(asked, "--profile")
	case t.Task.Profile != "":
		return pick(t.Task.Profile, t.ID+"'s profile")
	case pc != nil && pc.Routing[t.Kind] != "":
		return pick(pc.Routing[t.Kind], "tasks.toml routing."+t.Kind)
	case pc != nil && pc.Routing["default"] != "":
		return pick(pc.Routing["default"], "tasks.toml routing.default")
	}
	return pick(cfg.DefaultProfile, "default_profile")
}

// assignment is the first message of a session: what to do and the rules
// it works under.
func assignment(t app.TaskView, snap *project.Snapshot, wt, branch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working on task %s, \"%s\", in the git worktree %s on branch %s.\n\n", t.ID, t.Title, wt, branch)
	b.WriteString("Rules for this session:\n")
	b.WriteString("- Work only in this worktree, on this branch. Commit your work on the branch as you go.\n")
	fmt.Fprintf(&b, "- Never push, publish or deploy, and never merge into %s: the person does that after accepting the work.\n", snap.Config.Project.MainBranch)
	b.WriteString("- Work to the Acceptance list, run what Checks says, keep to Limits. Tick the Acceptance boxes in the task file on your branch as they are met.\n")
	b.WriteString("- If you need a decision, ask with AskUserQuestion from the main conversation (not from a subagent), with the recommended option first, labelled \"(Recommended)\".\n")
	b.WriteString("- When the work is finished and committed, say so in a short final summary.\n")
	b.WriteString("- Read the repository's own instructions first, if it has them: CLAUDE.md, AGENTS.md and .claude/rules/ (they may not be loaded for you).\n\n")
	fmt.Fprintf(&b, "The task file (%s):\n\n", t.File)
	if data, err := os.ReadFile(filepath.Join(wt, t.File)); err == nil {
		b.Write(data)
	} else {
		b.WriteString(t.Task.Body)
	}
	return b.String()
}

// start launches the runner for a session and sends its first message.

// answerInput builds AskUserQuestion's updatedInput: the original input
// plus answers {question: label} (F14); several labels are joined with ", ".
func answerInput(a *Ask, ans Answer) (map[string]any, string, error) {
	answers := map[string]any{}
	var said []string
	if ans.Text != "" && len(ans.Choices) == 0 {
		answers[a.Questions[0].Question] = ans.Text
		said = append(said, ans.Text)
	} else {
		if len(ans.Choices) != len(a.Questions) {
			return nil, "", refusedf("the ask has %d question(s); %d answer(s) given", len(a.Questions), len(ans.Choices))
		}
		for i, q := range a.Questions {
			labels, err := matchOptions(q, ans.Choices[i])
			if err != nil {
				return nil, "", err
			}
			answers[q.Question] = strings.Join(labels, ", ")
			said = append(said, strings.Join(labels, ", "))
		}
	}
	input := map[string]any{}
	for k, v := range a.input {
		input[k] = v
	}
	input["answers"] = answers
	return input, strings.Join(said, "; "), nil
}

// matchOptions resolves choices (labels or 1-based numbers) to the raw
// labels the harness sent.
func matchOptions(q Question, choices []string) ([]string, error) {
	if len(choices) == 0 {
		return nil, refusedf("no answer to %q", q.Question)
	}
	if len(choices) > 1 && !q.MultiSelect {
		return nil, refusedf("%q takes one answer", q.Question)
	}
	var out []string
	for _, c := range choices {
		found := ""
		var n int
		if _, err := fmt.Sscanf(c, "%d", &n); err == nil && fmt.Sprint(n) == c && n >= 1 && n <= len(q.Options) {
			found = q.Options[n-1].raw
		}
		for _, o := range q.Options {
			if found == "" && (strings.EqualFold(o.Label, c) || strings.EqualFold(o.raw, c)) {
				found = o.raw
			}
		}
		if found == "" {
			return nil, refusedf("%q is not an option of %q", c, q.Question)
		}
		out = append(out, found)
	}
	return out, nil
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func shellQuote(p string) string {
	if p != "" && strings.IndexFunc(p, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) < 0 {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
