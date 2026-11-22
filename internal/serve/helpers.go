package serve

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
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
		return pick(t.Task.Profile, t.ID+"'s profile") // an explicit profile wins (A16)
	}
	name, from := cfg.DefaultProfile, "default_profile"
	switch {
	case pc != nil && pc.Routing[t.Kind] != "":
		name, from = pc.Routing[t.Kind], "tasks.toml routing."+t.Kind
	case pc != nil && pc.Routing["default"] != "":
		name, from = pc.Routing["default"], "tasks.toml routing.default"
	}
	// harness is a preference (A16): the routed profile if it is of that
	// harness, else the first configured one that is, else the route
	if h := t.Task.Harness; h != "" && h != "any" {
		if p := cfg.Profile(name); p != nil && p.Harness == h {
			return p, nil
		}
		for i := range cfg.Profiles {
			if cfg.Profiles[i].Harness == h {
				return &cfg.Profiles[i], nil
			}
		}
	}
	return pick(name, from)
}

// taskModel is the model a task runs with: its own `model` over the
// profile's (C10).
func taskModel(t app.TaskView, prof *Profile) string {
	if t.Task.Model != "" {
		return t.Task.Model
	}
	return prof.Model
}

// codexProvider reads model_provider from Codex's configuration.
func codexProvider(home, codexHome string) string {
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	var c struct {
		ModelProvider string `toml:"model_provider"`
	}
	if _, err := toml.DecodeFile(filepath.Join(codexHome, "config.toml"), &c); err != nil {
		return ""
	}
	return c.ModelProvider
}

// ProfileHarnessWarnings are A16's validate warnings: a task whose profile
// runs another harness than the one it names.
func ProfileHarnessWarnings(cfg *Config, tasks []app.TaskView) []string {
	var out []string
	for _, t := range tasks {
		h := t.Task.Harness
		if t.Task.Profile == "" || h == "" || h == "any" {
			continue
		}
		if p := cfg.Profile(t.Task.Profile); p != nil && p.Harness != h {
			out = append(out, fmt.Sprintf("%s: warning: profile %s runs %s, harness says %s; the profile wins", t.File, p.Name, p.Harness, h))
		}
	}
	return out
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
	lists := map[string][]string{} // the raw labels per question (ACP forms)
	var said []string
	if ans.Text != "" && len(ans.Choices) == 0 {
		for _, q := range a.Questions[1:] {
			if !q.Optional {
				return nil, "", refusedf("the ask has %d questions: answer each one (choices), not one text", len(a.Questions))
			}
		}
		answers[a.Questions[0].Question] = ans.Text
		if a.Questions[0].Secret {
			said = append(said, "(secret answer, not recorded)")
		} else {
			said = append(said, ans.Text)
		}
	} else {
		// trailing optional questions may be left out
		for len(ans.Choices) < len(a.Questions) && a.Questions[len(ans.Choices)].Optional {
			ans.Choices = append(ans.Choices, nil)
		}
		if len(ans.Choices) != len(a.Questions) {
			return nil, "", refusedf("the ask has %d question(s); %d answer(s) given", len(a.Questions), len(ans.Choices))
		}
		for i, q := range a.Questions {
			var labels []string
			switch {
			case q.FreeText: // typed, taken as written
				for _, c := range ans.Choices[i] {
					if strings.TrimSpace(c) != "" {
						labels = append(labels, c)
					}
				}
				if len(labels) == 0 && !q.Optional {
					return nil, "", refusedf("no answer to %q", q.Question)
				}
			case len(ans.Choices[i]) == 0 && q.Optional:
			default:
				var err error
				if labels, err = matchOptions(q, ans.Choices[i]); err != nil {
					return nil, "", err
				}
			}
			if len(labels) == 0 {
				continue
			}
			answers[q.Question] = strings.Join(labels, ", ")
			lists[q.Question] = labels
			if q.Secret {
				said = append(said, "(secret answer, not recorded)")
			} else {
				said = append(said, strings.Join(labels, ", "))
			}
		}
	}
	input := map[string]any{}
	for k, v := range a.input {
		input[k] = v
	}
	input["answers"] = answers
	if len(lists) > 0 {
		input["answerLists"] = lists
	}
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
