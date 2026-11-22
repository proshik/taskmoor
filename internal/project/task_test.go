package project_test

import (
	"strings"
	"testing"

	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/schema"
	"github.com/proshik/taskmoor/internal/testrepo"
)

func parse(t *testing.T, name, content string) (*project.Task, []finding.Finding) {
	t.Helper()
	sch, err := schema.Load()
	if err != nil {
		t.Fatal(err)
	}
	return project.ParseTask("tasks/"+name, []byte(content), sch)
}

func one(t *testing.T, fs []finding.Finding, want string) finding.Finding {
	t.Helper()
	for _, f := range fs {
		if strings.Contains(f.Message, want) {
			return f
		}
	}
	t.Fatalf("no finding containing %q in %v", want, fs)
	return finding.Finding{}
}

func TestParseTaskValid(t *testing.T) {
	src := testrepo.TaskFile(testrepo.Task{ID: "TM-007", DependsOn: []string{"TM-006"}, NeedsPerson: true,
		Extra: "profile: claude-main\nmodel: opus\n", Body: "## Goal\n\nBody.\n"})
	tk, fs := parse(t, "TM-007-task.md", src)
	if len(fs) != 0 || tk == nil {
		t.Fatalf("findings %v", fs)
	}
	if tk.ID != "TM-007" || tk.Priority != 2 || !tk.NeedsPerson || tk.DependsOn[0] != "TM-006" || tk.Profile != "claude-main" || tk.Model != "opus" {
		t.Fatalf("task %+v", tk)
	}
	if !strings.HasPrefix(tk.Body, "\n## Goal") {
		t.Fatalf("body %q", tk.Body)
	}
}

func TestParseTaskYAMLRefusals(t *testing.T) {
	base := testrepo.TaskFile(testrepo.Task{ID: "TM-001"})
	cases := []struct {
		name, from, to, want string
		line                 int
	}{
		{"unquoted date", `created: "2026-11-07"`, `created: 2026-11-07`, "unquoted date; quote it", 13},
		{"all-digit commit", "---\n\n", "", "", 0}, // replaced below
		{"duplicate key", "kind: feature\n", "kind: feature\nkind: fix\n", `duplicate key "kind"`, 5},
		{"anchor", "harness: any", "harness: &h any", "anchors and aliases are not allowed", 11},
		{"anchor on a key", "harness: any", "&k harness: any", "anchor &k", 11},
		{"merge key", "reviews: {spec: 0, code: 2}", "base: &b {spec: 0, code: 2}\nreviews:\n  <<: *b", "anchor", 12},
	}
	for _, c := range cases {
		if c.name == "all-digit commit" {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(base, c.from, c.to, 1)
			if src == base {
				t.Fatalf("fixture did not change")
			}
			tk, fs := parse(t, "TM-001-task.md", src)
			if tk != nil {
				t.Fatalf("parsed despite %s", c.name)
			}
			f := one(t, fs, c.want)
			if c.line != 0 && f.Line != c.line {
				t.Errorf("line %d, want %d (%s)", f.Line, c.line, f)
			}
		})
	}
	t.Run("all-digit commit", func(t *testing.T) {
		src := strings.Replace(base, "---\n\n", "after_release: 12345678\n---\n\n", 1)
		tk, fs := parse(t, "TM-001-task.md", src)
		if tk != nil {
			t.Fatal("parsed an all-digit after_release")
		}
		f := one(t, fs, "/after_release")
		if !strings.Contains(f.Message, "want string") {
			t.Errorf("message %q", f.Message)
		}
		if f.Line != 14 {
			t.Errorf("line %d, want 14", f.Line)
		}
	})
	t.Run("alias", func(t *testing.T) {
		src := strings.Replace(base, "harness: any", "harness: *x", 1)
		_, fs := parse(t, "TM-001-task.md", src)
		if len(fs) == 0 {
			t.Fatal("no finding for an undefined alias")
		}
	})
}

func TestParseTaskSchemaAndNames(t *testing.T) {
	base := testrepo.TaskFile(testrepo.Task{ID: "TM-001"})
	t.Run("misspelt key", func(t *testing.T) {
		_, fs := parse(t, "TM-001-task.md", strings.Replace(base, "priority:", "priorty:", 1))
		one(t, fs, "priorty")
	})
	t.Run("touches lookahead pattern", func(t *testing.T) {
		_, fs := parse(t, "TM-001-task.md", strings.Replace(base, `"src/tm-001/**"`, `"../outside"`, 1))
		f := one(t, fs, "/touches/0")
		if f.Line != 10 {
			t.Errorf("line %d, want 10", f.Line)
		}
	})
	t.Run("bad profile", func(t *testing.T) {
		_, fs := parse(t, "TM-001-task.md", strings.Replace(base, "---\n\n", "profile: Claude Main\n---\n\n", 1))
		one(t, fs, "/profile")
	})
	t.Run("id differs from file name", func(t *testing.T) {
		tk, fs := parse(t, "TM-002-task.md", base)
		if tk == nil {
			t.Fatal("a mismatched id still parses")
		}
		one(t, fs, "does not equal the file name's prefix TM-002")
	})
	t.Run("file name shape", func(t *testing.T) {
		_, fs := parse(t, "TM-001_Task.md", base)
		one(t, fs, "file name is not <ID>-<slug>.md")
	})
	t.Run("no front matter", func(t *testing.T) {
		_, fs := parse(t, "TM-001-task.md", "# just text\n")
		one(t, fs, "no front matter")
	})
	t.Run("unclosed front matter", func(t *testing.T) {
		_, fs := parse(t, "TM-001-task.md", "---\nid: TM-001\n")
		one(t, fs, "no front matter")
	})
}

func TestParseConfig(t *testing.T) {
	cfg, fs := project.ParseConfig([]byte(testrepo.DefaultConfig + "\n[routing]\ndefault = \"codex-default\"\nfeature = \"claude-main\"\n"))
	if len(fs) != 0 {
		t.Fatalf("findings %v", fs)
	}
	if cfg.Routing["feature"] != "claude-main" || cfg.Components["app"].Release.PatternString() != "v*" {
		t.Fatalf("config %+v", cfg)
	}

	bad := `format = 2

[project]
id_prefix = "tm"

[components.App]
paths = []
release = { rule = "tag" }

[components.docs]
paths = ["docs/**"]
release = { rule = "merge", pattern = "x" }

[components.other]
paths = ["x"]
release = { rule = "nightly", pattern = "x" }

[routing]
default = "Bad Name"
bugfix = "codex"

[extra]
key = 1
`
	_, fs = project.ParseConfig([]byte(bad))
	for _, want := range []string{
		"format = 2", `project.id_prefix "tm"`, "project.main_branch is not set",
		`component name "App"`, "component App: paths is empty", `rule "tag" needs a pattern`,
		`rule "merge" takes no pattern`, `release.rule "nightly"`,
		`routing.default: profile name "Bad Name"`, `routing.bugfix: not "default" or a task kind`,
		`unknown key "extra"`,
	} {
		one(t, fs, want)
	}
	f := one(t, fs, "format = 2")
	if f.Line != 1 {
		t.Errorf("format line %d", f.Line)
	}

	_, fs = project.ParseConfig([]byte(strings.Replace(testrepo.DefaultConfig, `main_branch = "main"`, "main_branch = \"main\"\nMain_Branch = \"trunk\"", 1)))
	one(t, fs, `unknown key "project.Main_Branch"`)
	_, fs = project.ParseConfig([]byte(strings.Replace(testrepo.DefaultConfig, `rule = "merge"`, `Rule = "merge"`, 1)))
	one(t, fs, `unknown key "components.docs.release.Rule"`)

	cfg, fs = project.ParseConfig([]byte("format = 1\nid_prefix = = 2\n"))
	if cfg != nil || len(fs) != 1 || fs[0].Line != 2 {
		t.Fatalf("parse error: %v %v", cfg, fs)
	}
}
