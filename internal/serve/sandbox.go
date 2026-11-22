package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultAllowedDomains is where a sandboxed agent's commands may connect
// unless its profile says otherwise: package registries and reading code
// hosts, enough to build and test. Pushing needs credentials the sandbox
// keeps out of reach, and A11 refuses it anyway.
var DefaultAllowedDomains = []string{
	"proxy.golang.org", "sum.golang.org", "storage.googleapis.com",
	"registry.npmjs.org", "registry.yarnpkg.com",
	"pypi.org", "files.pythonhosted.org",
	"crates.io", "index.crates.io", "static.crates.io",
	"github.com", "codeload.github.com", "objects.githubusercontent.com", "raw.githubusercontent.com",
}

// MinSandboxClaude is the oldest Claude Code a confined profile runs on:
// the version the sandbox settings were checked with (stage 2b's real
// check). An older one is refused rather than trusted to honour them.
const MinSandboxClaude = "2.1.330"

// sandboxPlan is what one session's sandbox allows and denies (C13).
type sandboxPlan struct {
	Worktree  string
	GitDir    string // the worktree's own git directory (index, HEAD, logs)
	CommonDir string // the repository's git directory
	Branch    string // the task branch: the only ref the session writes
	Store     string // taskmoor serve's store (the API token)
	ConfigDir string // taskmoor's configuration
	Home      string
	Env       []string // the agent's environment (XDG and gh locations)
	Domains   []string
	// OtherRefs are the repository's branches other than the task's
	// (refs/heads/...): denied by name, so a writable location around the
	// repository (a temporary directory) cannot reopen them.
	OtherRefs []string
	// OtherWorktrees are the administrative directories of the
	// repository's other linked worktrees, denied by name too.
	OtherWorktrees []string
}

// checkedGitDir is the worktree's git directory, only when it is the one
// the repository registered for this worktree: a re-pointed .git file
// must not choose what the sandbox lets the agent write.
func checkedGitDir(wt, commonDir string) (string, error) {
	gd, err := worktreeGitDir(wt)
	if err != nil {
		return "", err
	}
	if filepath.Dir(gd) != filepath.Join(commonDir, "worktrees") {
		return "", fmt.Errorf("%s/.git names %s, not a worktree of %s", wt, gd, commonDir)
	}
	back, err := os.ReadFile(filepath.Join(gd, "gitdir"))
	if err != nil {
		return "", err
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(wt, ".git"))
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(back)))
	if want == "" || got != want {
		return "", fmt.Errorf("%s is registered for another worktree", gd)
	}
	return gd, nil
}

// refFormat is the repository's ref backend: "files" or "reftable".
func refFormat(commonDir string) (string, error) {
	out, err := exec.Command("git", "--git-dir", commonDir, "config", "--get", "extensions.refStorage").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return "files", nil // unset: the files backend
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// programPath is the absolute executable of a profile, the same for the
// version probe and the launch (a relative command would otherwise be
// found from the worktree at launch).
func programPath(prof Profile) (string, error) {
	prog := prof.Command
	if prog == "" {
		prog = "claude"
	}
	p, err := exec.LookPath(prog)
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// managedSettingsFiles are where Claude Code reads managed settings.
var managedSettingsFiles = func() []string {
	dir := "/etc/claude-code"
	if runtime.GOOS == "darwin" {
		dir = "/Library/Application Support/ClaudeCode"
	}
	files := []string{filepath.Join(dir, "managed-settings.json")}
	more, _ := filepath.Glob(filepath.Join(dir, "managed-settings.d", "*.json"))
	return append(files, more...)
}

// managedSandboxOff names a managed settings file that turns the sandbox
// off: it outranks taskmoor's settings, so a confined run would not be.
func managedSandboxOff() string {
	for _, f := range managedSettingsFiles() {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m struct {
			Sandbox struct {
				Enabled *bool `json:"enabled"`
			} `json:"sandbox"`
		}
		if json.Unmarshal(data, &m) == nil && m.Sandbox.Enabled != nil && !*m.Sandbox.Enabled {
			return f
		}
	}
	return ""
}

// branchRefs lists a repository's branches.
func branchRefs(commonDir string) ([]string, error) {
	out, err := exec.Command("git", "--git-dir", commonDir, "for-each-ref", "--format=%(refname)", "refs/heads/").Output()
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref: %v", err)
	}
	return strings.Fields(string(out)), nil
}

// worktreeGitDir reads the gitdir a linked worktree's .git file names.
func worktreeGitDir(wt string) (string, error) {
	data, err := os.ReadFile(filepath.Join(wt, ".git"))
	if err != nil {
		return "", err
	}
	d, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok {
		return "", fmt.Errorf("%s/.git is not a linked worktree's", wt)
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(wt, d)
	}
	return filepath.Clean(d), nil
}

// credentialPaths are the person's credentials a build never needs.
func credentialPaths(home string, env []string) []string {
	var out []string
	for _, rel := range []string{
		".ssh", ".aws", ".gnupg", ".config/gh", ".config/glab-cli", ".config/git/credentials", ".netrc", ".npmrc", ".pypirc",
		".docker", ".kube", ".git-credentials", ".claude.json", ".claude/.credentials.json", ".config/gcloud", ".azure",
		"Library/Keychains", "Library/Application Support/glab-cli",
	} {
		out = append(out, filepath.Join(home, rel))
	}
	if c := envValue(env, "CLAUDE_CONFIG_DIR"); c != "" {
		out = append(out, filepath.Join(c, ".credentials.json"))
	}
	if g := envValue(env, "GLAB_CONFIG_DIR"); g != "" {
		out = append(out, g)
	}
	// relocated: git's XDG credential store, the GitHub CLI's directory
	if x := envValue(env, "XDG_CONFIG_HOME"); x != "" {
		out = append(out, filepath.Join(x, "git", "credentials"), filepath.Join(x, "gh"), filepath.Join(x, "glab-cli"))
	}
	if g := envValue(env, "GH_CONFIG_DIR"); g != "" {
		out = append(out, g)
	}
	return out
}

// writableCaches are the build caches commands may write besides the
// worktree and temporary directories.
func writableCaches(home string) []string {
	var out []string
	for _, rel := range []string{"Library/Caches", ".cache", "go/pkg/mod", ".npm", ".cargo/registry", ".cargo/git", ".gradle/caches", ".m2/repository"} {
		out = append(out, filepath.Join(home, rel))
	}
	return out
}

// pathRules are Claude permission rules for absolute paths (//abs/**):
// the directory itself and everything under it.
func pathRules(tool string, paths []string) []string {
	var out []string
	for _, p := range paths {
		p = escapeGlob(filepath.Clean(p))
		out = append(out, tool+"(/"+p+")", tool+"(/"+p+"/**)")
	}
	return out
}

// escapeGlob makes a literal path of a rule's pattern (gitignore syntax:
// a backslash quotes the next character).
func escapeGlob(p string) string {
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`\*?[]{}!()`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sessionSettings is the --settings JSON of a session: ask rules (every
// command, read, edit and fetch comes to the host), deny rules for
// taskmoor's own files (which also keep them out of Grep's and Glob's
// recursive searches) and, when confined, the sandbox.
func sessionSettings(p *sandboxPlan, protected []string) string {
	deny := append(pathRules("Read", protected), pathRules("Edit", protected)...)
	if p == nil {
		// unconfined: off, also when the person's settings turn it on
		return mustJSON(map[string]any{
			"permissions": map[string]any{"ask": askTools, "deny": deny},
			"sandbox":     map[string]any{"enabled": false, "autoAllowBashIfSandboxed": false},
		})
	}
	creds := credentialPaths(p.Home, p.Env)
	deny = append(deny, pathRules("Read", creds)...)
	deny = append(deny, pathRules("Edit", creds)...)
	tmp := []string{os.TempDir(), "/tmp", "/private/tmp", "/private/var/folders"}
	// git: objects (content-addressed, they change no branch), the
	// worktree's own directory, and only the task branch's ref, its lock
	// and its log; other branches (main among them), hooks, config and
	// taskmoor's tasks/ stay read-only
	ref := filepath.Join(p.CommonDir, "refs", "heads", filepath.FromSlash(p.Branch))
	log := filepath.Join(p.CommonDir, "logs", "refs", "heads", filepath.FromSlash(p.Branch))
	allowWrite := []string{p.Worktree, filepath.Join(p.CommonDir, "objects"), p.GitDir, ref, ref + ".lock", log}
	allowWrite = append(allowWrite, tmp...)
	allowWrite = append(allowWrite, writableCaches(p.Home)...)
	denyWrite := []string{
		filepath.Join(p.Worktree, ".git"), // re-pointed, it would hand git a config of the agent's
		filepath.Join(p.CommonDir, "hooks"), filepath.Join(p.CommonDir, "config"),
		filepath.Join(p.CommonDir, "tasks"), filepath.Join(p.CommonDir, "info"),
		filepath.Join(p.GitDir, "config.worktree"),
		// the main checkout's own files and the packed refs
		filepath.Join(p.CommonDir, "HEAD"), filepath.Join(p.CommonDir, "index"),
		filepath.Join(p.CommonDir, "packed-refs"), filepath.Join(p.CommonDir, "packed-refs.lock"),
		filepath.Join(p.CommonDir, "logs", "HEAD"), filepath.Join(p.CommonDir, "config.worktree"),
	}
	for _, w := range p.OtherWorktrees {
		if filepath.Clean(w) != filepath.Clean(p.GitDir) {
			denyWrite = append(denyWrite, w)
		}
	}
	for _, r := range p.OtherRefs {
		if r == "refs/heads/"+p.Branch {
			continue
		}
		f := filepath.Join(p.CommonDir, filepath.FromSlash(r))
		denyWrite = append(denyWrite, f, f+".lock", filepath.Join(p.CommonDir, "logs", filepath.FromSlash(r)))
	}
	denyWrite = append(denyWrite, protected...)
	denyRead := append(append([]string(nil), protected...), creds...)
	domains := p.Domains
	if domains == nil {
		domains = DefaultAllowedDomains
	}
	return mustJSON(map[string]any{
		"permissions":     map[string]any{"ask": askTools, "deny": deny},
		"disableAllHooks": true, // hooks run outside the sandbox
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,  // no sandbox, no session
			"autoAllowBashIfSandboxed": false, // the host still sees every command
			"allowUnsandboxedCommands": false, // no dangerouslyDisableSandbox escape
			"filesystem": map[string]any{
				"allowWrite": allowWrite,
				"denyWrite":  denyWrite,
				"denyRead":   denyRead,
			},
			"network": map[string]any{
				"allowedDomains":  domains,
				"strictAllowlist": true, // other hosts are refused, not asked
			},
		},
	})
}

var askTools = []string{"Bash", "Read", "Write", "Edit", "MultiEdit", "NotebookEdit", "WebFetch"}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// claudeVersionOK checks that prog is at least MinSandboxClaude.
func claudeVersionOK(prog string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prog, "--version")
	cmd.Env = env
	cmd.WaitDelay = time.Second // a child that keeps the output open does not hold it
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%s --version: %v", prog, err)
	}
	got := strings.Fields(string(out))
	if len(got) == 0 || versionLess(got[0], MinSandboxClaude) {
		return fmt.Errorf("Claude Code %s is older than %s, the oldest a confined profile runs on: update it (claude update)",
			strings.TrimSpace(string(out)), MinSandboxClaude)
	}
	return nil
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
