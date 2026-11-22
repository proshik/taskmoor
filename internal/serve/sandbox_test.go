package serve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type settingsJSON struct {
	Permissions     struct{ Ask, Deny []string }
	DisableAllHooks bool
	Sandbox         struct {
		Enabled                                                               *bool
		FailIfUnavailable, AutoAllowBashIfSandboxed, AllowUnsandboxedCommands bool
		Filesystem                                                            struct{ AllowWrite, DenyWrite, DenyRead []string }
		Network                                                               struct {
			AllowedDomains  []string
			StrictAllowlist bool
		}
	}
}

func decodeSettings(t *testing.T, s string) settingsJSON {
	t.Helper()
	var out settingsJSON
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSandboxSettings(t *testing.T) {
	p := &sandboxPlan{Worktree: "/w/repo-tm-001", GitDir: "/w/repo/.git/worktrees/repo-tm-001", CommonDir: "/w/repo/.git",
		Branch: "task/TM-001-x", Store: "/h/.local/state/taskmoor", ConfigDir: "/h/.config/taskmoor", Home: "/h",
		Env: []string{"XDG_CONFIG_HOME=/x", "GH_CONFIG_DIR=/g"}, OtherRefs: []string{"refs/heads/main", "refs/heads/task/TM-001-x"}}
	protected := []string{p.Store, p.ConfigDir}
	s := decodeSettings(t, sessionSettings(p, protected))
	sb := s.Sandbox
	if sb.Enabled == nil || !*sb.Enabled || !sb.FailIfUnavailable || sb.AutoAllowBashIfSandboxed || sb.AllowUnsandboxedCommands ||
		!sb.Network.StrictAllowlist || !s.DisableAllHooks {
		t.Fatalf("%+v", s)
	}
	for _, want := range []string{"/w/repo-tm-001", "/w/repo/.git/objects", "/w/repo/.git/worktrees/repo-tm-001",
		"/w/repo/.git/refs/heads/task/TM-001-x", "/w/repo/.git/refs/heads/task/TM-001-x.lock",
		"/w/repo/.git/logs/refs/heads/task/TM-001-x", "/h/go/pkg/mod"} {
		if !slices.Contains(sb.Filesystem.AllowWrite, want) {
			t.Errorf("allowWrite lacks %s", want)
		}
	}
	// not the whole git directory: main and the other branches stay
	for _, not := range []string{"/w/repo/.git", "/w/repo/.git/refs", "/w/repo/.git/refs/heads"} {
		if slices.Contains(sb.Filesystem.AllowWrite, not) {
			t.Errorf("allowWrite has %s", not)
		}
	}
	if slices.Contains(sb.Filesystem.DenyWrite, "/w/repo/.git/refs/heads/task/TM-001-x") {
		t.Error("the task's own branch is denied")
	}
	for _, want := range []string{"/w/repo-tm-001/.git", "/w/repo/.git/hooks", "/w/repo/.git/config", "/w/repo/.git/tasks",
		"/w/repo/.git/worktrees/repo-tm-001/config.worktree", "/h/.local/state/taskmoor",
		"/w/repo/.git/refs/heads/main", "/w/repo/.git/refs/heads/main.lock", "/w/repo/.git/logs/refs/heads/main",
		"/w/repo/.git/HEAD", "/w/repo/.git/index", "/w/repo/.git/packed-refs"} {
		if !slices.Contains(sb.Filesystem.DenyWrite, want) {
			t.Errorf("denyWrite lacks %s", want)
		}
	}
	for _, want := range []string{"/h/.local/state/taskmoor", "/h/.config/taskmoor", "/h/.ssh", "/h/.aws", "/h/.config/gh",
		"/h/.claude.json", "/h/.config/git/credentials", "/x/git/credentials", "/x/gh", "/g"} {
		if !slices.Contains(sb.Filesystem.DenyRead, want) {
			t.Errorf("denyRead lacks %s", want)
		}
	}
	// the file tools: taskmoor's files and the credentials, recursively
	for _, want := range []string{"Read(//h/.local/state/taskmoor/**)", "Edit(//h/.config/taskmoor/**)", "Read(//h/.ssh/**)", "Read(//g/**)"} {
		if !slices.Contains(s.Permissions.Deny, want) {
			t.Errorf("deny lacks %s", want)
		}
	}
	if !slices.Equal(sb.Network.AllowedDomains, DefaultAllowedDomains) || !slices.Contains(s.Permissions.Ask, "Read") {
		t.Fatalf("%v %v", sb.Network.AllowedDomains, s.Permissions.Ask)
	}
	// a profile's list replaces the default; an empty one allows no host
	p.Domains = []string{}
	if s := decodeSettings(t, sessionSettings(p, protected)); len(s.Sandbox.Network.AllowedDomains) != 0 {
		t.Fatalf("%v", s.Sandbox.Network.AllowedDomains)
	}
	// unconfined: the sandbox off (also against the person's settings),
	// taskmoor's files still denied to the file tools
	u := decodeSettings(t, sessionSettings(nil, protected))
	if u.Sandbox.Enabled == nil || *u.Sandbox.Enabled || u.Sandbox.AutoAllowBashIfSandboxed ||
		!slices.Contains(u.Permissions.Deny, "Read(//h/.local/state/taskmoor/**)") || slices.Contains(u.Permissions.Deny, "Read(//h/.ssh/**)") {
		t.Fatalf("%+v", u)
	}
}

func TestConfinedArgsLoadOnlyTaskmoorSettings(t *testing.T) {
	conf := claudeArgs(claudeStart{SessionID: "s", Sandbox: &sandboxPlan{Home: "/h"}})
	if !slices.Contains(conf, "--setting-sources=") || !slices.Contains(conf, "--strict-mcp-config") {
		t.Fatalf("%v", conf)
	}
	open := claudeArgs(claudeStart{SessionID: "s"})
	if slices.Contains(open, "--setting-sources=") {
		t.Fatalf("%v", open)
	}
}

func TestWorktreeGitDir(t *testing.T) {
	wt := t.TempDir()
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /r/.git/worktrees/x\n"), 0o644)
	if d, err := worktreeGitDir(wt); err != nil || d != "/r/.git/worktrees/x" {
		t.Fatalf("%q %v", d, err)
	}
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: ../r/.git/worktrees/x\n"), 0o644)
	if d, _ := worktreeGitDir(wt); d != filepath.Join(filepath.Dir(wt), "r/.git/worktrees/x") {
		t.Fatalf("%q", d)
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{{"2.1.329", "2.1.330", true}, {"2.1.330", "2.1.330", false}, {"2.2.0", "2.1.330", false}, {"2.1.1000", "2.1.330", false}, {"x", "2.1.330", true}} {
		if versionLess(c.a, c.b) != c.less {
			t.Errorf("%s < %s", c.a, c.b)
		}
	}
}

func TestProtectedPaths(t *testing.T) {
	home := t.TempDir()
	store := filepath.Join(home, "state", "taskmoor")
	_ = os.MkdirAll(store, 0o700)
	_ = os.WriteFile(filepath.Join(store, "token"), []byte("x"), 0o600)
	r := Rules{Worktree: "/w/repo-tm-001", Protected: []string{store, "/h/.config/taskmoor"}}
	upper := strings.Replace(filepath.Join(store, "token"), "taskmoor", "TaskMoor", 1)
	for _, c := range []struct {
		tool  string
		input map[string]any
	}{
		{"Read", map[string]any{"file_path": filepath.Join(store, "token")}},
		{"Read", map[string]any{"file_path": upper}}, // a case-insensitive volume: the same file
		{"Grep", map[string]any{"path": "/h/.config/taskmoor"}},
		{"Write", map[string]any{"file_path": "/h/.config/taskmoor/config.toml"}},
		{"Bash", map[string]any{"command": "cat " + filepath.Join(store, "token")}},
		{"Bash", map[string]any{"command": "cat " + upper}},
	} {
		if v := r.Decide(c.tool, c.input); v.Decision != Deny {
			t.Errorf("%s %v: %v", c.tool, c.input, v)
		}
	}
	if v := r.Decide("Read", map[string]any{"file_path": "/w/repo-tm-001/main.go"}); v.Decision != Allow {
		t.Errorf("worktree read: %v", v)
	}
}

func TestSandboxDeniesOtherWorktreesAndEscapesRules(t *testing.T) {
	p := &sandboxPlan{Worktree: "/w/a", GitDir: "/w/r/.git/worktrees/a", CommonDir: "/w/r/.git", Branch: "task/A", Home: "/h",
		Env:            []string{"GLAB_CONFIG_DIR=/gl", "CLAUDE_CONFIG_DIR=/cc"},
		OtherWorktrees: []string{"/w/r/.git/worktrees/a", "/w/r/.git/worktrees/b"}}
	s := decodeSettings(t, sessionSettings(p, []string{"/st/[x]"}))
	deny := s.Sandbox.Filesystem.DenyWrite
	if !slices.Contains(deny, "/w/r/.git/worktrees/b") || slices.Contains(deny, "/w/r/.git/worktrees/a") || !slices.Contains(deny, "/w/r/.git/config.worktree") {
		t.Fatalf("%v", deny)
	}
	for _, want := range []string{"/gl", "/cc/.credentials.json", "/h/.claude/.credentials.json", "/h/Library/Application Support/glab-cli"} {
		if !slices.Contains(s.Sandbox.Filesystem.DenyRead, want) {
			t.Errorf("denyRead lacks %s", want)
		}
	}
	if !slices.Contains(s.Permissions.Deny, `Read(//st/\[x\]/**)`) {
		t.Fatalf("%v", s.Permissions.Deny[:2])
	}
}
