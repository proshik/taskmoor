package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Decision is what the fixed rules say about a tool call (A13's first
// step; the dispatcher's judgement comes in a later stage).
type Decision int

const (
	// Allow runs the tool.
	Allow Decision = iota
	// Deny refuses it without asking anyone (push, publish, deploy, merges
	// into main: A11).
	Deny
	// AskPerson puts it in the inbox (level c).
	AskPerson
)

// Verdict is a decision with its reason, shown in the feed and the inbox.
type Verdict struct {
	Decision Decision
	Reason   string
}

var (
	// Commands an agent session never runs (A11): the person pushes.
	denyCommands = []*regexp.Regexp{
		regexp.MustCompile(`\bgit\s+(\S+\s+)*push\b`),
		regexp.MustCompile(`\b(npm|pnpm|yarn|cargo|gem|twine|poetry|uv|dotnet|mvn|gradle)\s+(\S+\s+)*(publish|deploy)\b`),
		regexp.MustCompile(`\bgh\s+(release\s+(create|upload|edit|delete)|pr\s+(create|merge)|repo\s+(create|delete))\b`),
		regexp.MustCompile(`\bglab\s+(release|mr\s+(create|merge))\b`),
		regexp.MustCompile(`\b(docker|podman)\s+(\S+\s+)*push\b`),
		// cluster changes, global options before the subcommand included
		regexp.MustCompile(`\bkubectl\s+(\S+\s+)*(apply|create|delete|patch|replace|edit|scale|set|rollout|annotate|label|expose|run|autoscale|cordon|uncordon|drain|taint)\b`),
		regexp.MustCompile(`\bhelm\s+(\S+\s+)*(install|upgrade|delete|uninstall|rollback)\b`),
		regexp.MustCompile(`\b(terraform|tofu)\s+(\S+\s+)*(apply|destroy|import)\b`),
		regexp.MustCompile(`\b(flyctl|fly|vercel|netlify|heroku)\s+(deploy|release)\b`),
	}
	// A secret's path: asked, never assumed harmless.
	secretPath = regexp.MustCompile(`(^|/)(\.env(\..*)?|\.netrc|\.npmrc|\.pypirc|id_[a-z0-9]+|[^/]*\.pem|[^/]*\.key|credentials(\.json)?|secrets?(\.[a-z]+)?|\.ssh(/.*)?|\.aws(/.*)?|\.kube(/.*)?|\.docker/config\.json)$`)
	// Dependency manifests: a new dependency is the person's call (C3).
	manifest  = regexp.MustCompile(`(^|/)(go\.mod|package\.json|Cargo\.toml|requirements[^/]*\.txt|pyproject\.toml|Pipfile|Gemfile|build\.gradle(\.kts)?|pom\.xml|composer\.json|mix\.exs|Package\.swift)$`)
	rmCommand = regexp.MustCompile(`(^|[;&|(]\s*|\s)(rm|rmdir|unlink|shred|trash)\s`)
	// commands that write to their path arguments, and output redirections
	writeCommand = regexp.MustCompile(`(^|[;&|(]\s*|\s)(cp|mv|tee|mkdir|touch|ln|rsync|install|chmod|chown|truncate|dd)\s`)
	// "> f", ">> f", "2> f", ">| f"; ">&1" duplicates a descriptor and is
	// not a file (its target starts with & and does not match)
	redirect = regexp.MustCompile(`(?:^|[^<>&0-9])[0-9]*>{1,2}\|?\s*([^\s;&|()<>]+)`)
	gitWrite = regexp.MustCompile(`\bgit\b.*\b(merge|rebase|reset|commit|cherry-pick|pull|am|revert|checkout|switch|branch|tag|update-ref|stash)\b`)
)

// Rules are the fixed permission rules for one session.
type Rules struct {
	Worktree   string // absolute
	MainBranch string
	// MainCheckout is the repository's main worktree, never to be written.
	MainCheckout string
	// Protected are taskmoor's own files (the service store with its API
	// token, the configuration): refused to every tool.
	Protected []string
}

// touchesProtected reports a path inside a protected directory.
func (r Rules) touchesProtected(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	abs := r.abs(p)
	if rp, err := filepath.EvalSymlinks(abs); err == nil {
		abs = rp
	}
	for _, d := range r.Protected {
		d = filepath.Clean(d)
		if rd, err := filepath.EvalSymlinks(d); err == nil {
			d = rd
		}
		if within(abs, d) {
			return d, true
		}
	}
	return "", false
}

// within reports p inside or equal to d: by name, ignoring case (macOS
// volumes usually ignore it), and by file identity of p's existing
// ancestors, whatever their spelling.
func within(p, d string) bool {
	lp, ld := strings.ToLower(p), strings.ToLower(d)
	if lp == ld || strings.HasPrefix(lp, ld+"/") {
		return true
	}
	di, err := os.Stat(d)
	if err != nil {
		return false
	}
	for a := p; ; a = filepath.Dir(a) {
		if ai, err := os.Stat(a); err == nil && os.SameFile(ai, di) {
			return true
		}
		if a == filepath.Dir(a) {
			return false
		}
	}
}

const protectedReason = "taskmoor's own files (its store and configuration) are out of an agent's reach"

// decideAll is the strictest verdict over a tool's paths: a deny if any is
// denied, else the person's if any asks.
func (r Rules) decideAll(tool string, paths []string) Verdict {
	out := Verdict{Allow, ""}
	for _, p := range paths {
		v := r.Decide(tool, map[string]any{"file_path": p})
		switch {
		case v.Decision == Deny:
			return v
		case v.Decision == AskPerson && out.Decision == Allow:
			out = v
		}
	}
	return out
}

// Decide checks one tool call. AskUserQuestion never comes here: it is a
// question, always the person's in this stage.
func (r Rules) Decide(tool string, input map[string]any) Verdict {
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if p, _ := input[k].(string); p != "" {
			if _, ok := r.touchesProtected(p); ok {
				return Verdict{Deny, protectedReason}
			}
		}
	}
	switch tool {
	case "Bash":
		cmd, _ := input["command"].(string)
		return r.command(cmd)
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		p, _ := input["file_path"].(string)
		if p == "" {
			p, _ = input["notebook_path"].(string)
		}
		return r.write(p)
	case "Read", "Glob", "Grep", "LS", "NotebookRead":
		p, _ := input["file_path"].(string)
		if p == "" {
			p, _ = input["path"].(string)
		}
		if p != "" && secretPath.MatchString(filepath.ToSlash(r.abs(p))) {
			return Verdict{AskPerson, "reads a file that may hold a secret: " + p}
		}
		return Verdict{Allow, ""}
	}
	return Verdict{Allow, ""}
}

func (r Rules) abs(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.Worktree, p)
	}
	return filepath.Clean(p)
}

func (r Rules) inside(p string) bool {
	rel, err := filepath.Rel(r.Worktree, r.abs(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func (r Rules) write(p string) Verdict {
	switch {
	case p == "":
		return Verdict{AskPerson, "writes a file the rules cannot see"}
	case !r.inside(p):
		return Verdict{AskPerson, "writes outside the task's worktree: " + p}
	case secretPath.MatchString(filepath.ToSlash(r.abs(p))):
		return Verdict{AskPerson, "writes a file that may hold a secret: " + p}
	case manifest.MatchString(filepath.ToSlash(p)):
		return Verdict{AskPerson, "changes a dependency manifest: " + filepath.Base(p)}
	}
	return Verdict{Allow, ""}
}

func (r Rules) command(cmd string) Verdict {
	c := strings.TrimSpace(cmd)
	for _, d := range r.Protected { // anywhere in the text: conservative
		if d != "" && strings.Contains(strings.ToLower(c), strings.ToLower(filepath.Clean(d))) {
			return Verdict{Deny, protectedReason}
		}
	}
	// two readings, the stricter wins: the simple commands that run
	// (shellparse.go), and the raw text with its data — literal heredocs,
	// quoted text — taken out, so what the reading misses is still judged
	// as before stage 11
	raw := r.commandRaw(commentsStripped(dataStripped(c)))
	stmts, err := parseShell(c)
	if err != nil {
		return stricter(raw, r.commandRaw(c)) // unreadable: the whole text, as before
	}
	return stricter(raw, r.commands(stmts))
}

// stricter is the more restrictive verdict: Deny, then AskPerson.
func stricter(a, b Verdict) Verdict {
	rank := func(v Verdict) int {
		switch v.Decision {
		case Deny:
			return 2
		case AskPerson:
			return 1
		}
		return 0
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// commentsStripped drops the shell's comments (a "#" that starts a word,
// outside quotes, to the end of its line); quoted text stays as it is.
func commentsStripped(c string) string {
	src := []rune(c)
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		r := src[i]
		switch r {
		case '\\':
			b.WriteRune(r)
			if i+1 < len(src) {
				i++
				b.WriteRune(src[i])
			}
		case '\'', '"':
			end := i + 1
			for end < len(src) && src[end] != r {
				if r == '"' && src[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(src) {
				return c // unbalanced: the text as it is
			}
			b.WriteString(string(src[i : end+1]))
			i = end
		case '#':
			if i == 0 || src[i-1] == ' ' || src[i-1] == '\t' || src[i-1] == '\n' || src[i-1] == ';' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				b.WriteRune('\n')
				continue
			}
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// gitSub is git's subcommand and its arguments, after git's own options.
func gitSub(args []string) (string, []string) {
	withArg := map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true, "--config-env": true, "--super-prefix": true, "--exec-path": false}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case withArg[a]:
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
}

// commands judges the simple commands a command runs (shellparse.go):
// what is data — a heredoc written by cat, a quoted message — is not.
func (r Rules) commands(stmts []shellStmt) Verdict {
	var all []string // every word that runs, for the checks across commands
	outsideCwd := false
	for _, st := range stmts {
		if st.cmd() == "git" {
			sub, rest := gitSub(st.args())
			switch sub {
			case "push", "send-email":
				return Verdict{Deny, "push, publish and deploy stay the person's: an agent session never runs them"}
			case "checkout", "switch":
				for _, a := range rest {
					if strings.HasPrefix(a, "-") {
						continue
					}
					if r.MainBranch != "" && a == r.MainBranch {
						return Verdict{Deny, "the task's work stays on its branch; merges into " + r.MainBranch + " happen only through Accept"}
					}
					break
				}
			}
		} else {
			t := st.text()
			for _, re := range denyCommands {
				if loc := re.FindStringIndex(t); loc != nil && loc[0] == 0 {
					return Verdict{Deny, "push, publish and deploy stay the person's: an agent session never runs them"}
				}
			}
		}
		all = append(all, st.words...)
		all = append(all, st.writes...)
		all = append(all, st.reads...)
	}
	text := strings.Join(all, " ")
	if r.MainCheckout != "" && strings.Contains(text, r.MainCheckout) && !strings.Contains(text, r.Worktree) {
		if gitWrite.MatchString(text) {
			return Verdict{Deny, "the main checkout is the person's: merges into " + r.MainBranch + " happen only through Accept"}
		}
		return Verdict{AskPerson, "names the main checkout " + r.MainCheckout}
	}
	cwdOut := map[int]bool{} // per subshell depth: a ( ) does not move its parent
	last := 0
	for _, st := range stmts {
		if st.depth < last {
			for d := range cwdOut {
				if d > st.depth {
					delete(cwdOut, d)
				}
			}
		} else if st.depth > last {
			for d := last + 1; d <= st.depth; d++ {
				cwdOut[d] = cwdOut[d-1]
			}
		}
		last = st.depth
		outsideCwd = cwdOut[st.depth]
		switch st.cmd() {
		case "cd", "pushd", "popd":
			// a relative path is relative to where it goes: outside, or not
			// known, every later relative write or delete counts as outside
			dir := ""
			for _, a := range st.args() {
				if !strings.HasPrefix(a, "-") {
					dir = a
					break
				}
			}
			switch {
			case dir == "" || st.cmd() == "popd" || strings.HasPrefix(dir, "~") || strings.HasPrefix(dir, "$"):
				outsideCwd = true
			case strings.HasPrefix(dir, "/"):
				outsideCwd = !r.inside(dir)
			case strings.HasPrefix(dir, "..") || strings.Contains(dir, "/../"):
				outsideCwd = true
			}
			cwdOut[st.depth] = outsideCwd
		}
		ts := append(append([]string{}, st.writes...), writeArgs(st)...)
		if outsideCwd {
			for _, t := range ts {
				if t != "" && !strings.HasPrefix(t, "/") && !strings.HasPrefix(t, "-") && t != "$CMD" {
					return Verdict{AskPerson, "writes outside the task's worktree (after a cd): " + t}
				}
			}
		}
		if v, ok := r.targets(ts); ok {
			return v
		}
		switch st.cmd() {
		case "rm", "rmdir", "unlink", "shred", "trash":
			for _, f := range st.args() {
				if strings.HasPrefix(f, "-") {
					continue
				}
				if outsideCwd || strings.HasPrefix(f, "~") || strings.HasPrefix(f, "..") || strings.Contains(f, "/../") || strings.HasPrefix(f, "/") && !r.inside(f) {
					return Verdict{AskPerson, fmt.Sprintf("deletes outside the task's worktree (%s)", f)}
				}
			}
		}
	}
	for _, w := range all {
		// a path in an option's value too (--env-file=.env)
		for _, v := range append([]string{w}, strings.Split(w, "=")...) {
			v = strings.Trim(v, `"'`)
			if secretPath.MatchString(v) || strings.Contains(v, ".ssh/") || strings.HasPrefix(filepath.Base(v), ".env") {
				return Verdict{AskPerson, "touches a file that may hold a secret"}
			}
		}
	}
	return Verdict{Allow, ""}
}

// writeArgs are the paths a command writes by its arguments.
func writeArgs(st shellStmt) []string {
	var plain []string
	target := ""
	args := st.args()
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t" && i+1 < len(args):
			target = args[i+1]
			i++
		case strings.HasPrefix(a, "--target-directory="):
			target = strings.TrimPrefix(a, "--target-directory=")
		case strings.HasPrefix(a, "-"):
		default:
			plain = append(plain, a)
		}
	}
	switch st.cmd() {
	case "mv": // a move also takes its sources away
		if target != "" {
			return append(plain, target)
		}
		return plain
	case "cp", "install", "rsync", "ln":
		if target != "" {
			return []string{target}
		}
		if len(plain) > 0 {
			return plain[len(plain)-1:] // the destination; the sources are read
		}
	case "tee", "mkdir", "touch", "truncate":
		return plain
	case "chmod", "chown", "chgrp":
		if len(plain) > 1 {
			return plain[1:] // after the mode or owner
		}
	case "dd":
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "of="); ok {
				return []string{p}
			}
		}
	}
	return nil
}

// targets applies the file-tool rules to paths a command writes.
func (r Rules) targets(ts []string) (Verdict, bool) {
	for _, t := range ts {
		if t == "" || strings.HasPrefix(t, "-") || t == "$CMD" {
			continue
		}
		outside := strings.HasPrefix(t, "~") || strings.HasPrefix(t, "..") || strings.Contains(t, "/../") ||
			strings.HasPrefix(t, "/") && !r.inside(t)
		switch {
		case outside && strings.HasPrefix(t, "/") && harmlessPath(t):
		case outside:
			return Verdict{AskPerson, "writes outside the task's worktree: " + t}, true
		case manifest.MatchString(filepath.ToSlash(t)):
			return Verdict{AskPerson, "changes a dependency manifest: " + filepath.Base(t)}, true
		case secretPath.MatchString(filepath.ToSlash(r.abs(t))):
			return Verdict{AskPerson, "writes a file that may hold a secret: " + t}, true
		}
	}
	return Verdict{}, false
}

// commandRaw judges a command the shell reading could not follow, on its
// text (the rules before stage 11).
func (r Rules) commandRaw(c string) Verdict {
	for _, re := range denyCommands {
		if re.MatchString(c) {
			return Verdict{Deny, "push, publish and deploy stay the person's: an agent session never runs them"}
		}
	}
	main := regexp.QuoteMeta(r.MainBranch)
	if r.MainBranch != "" && regexp.MustCompile(`\bgit\s+(\S+\s+)*(checkout|switch)\s+(-\S+\s+)*`+main+`(\s|$)`).MatchString(c) {
		return Verdict{Deny, "the task's work stays on its branch; merges into " + r.MainBranch + " happen only through Accept"}
	}
	if r.MainCheckout != "" && strings.Contains(c, r.MainCheckout) && !strings.Contains(c, r.Worktree) {
		if gitWrite.MatchString(c) {
			return Verdict{Deny, "the main checkout is the person's: merges into " + r.MainBranch + " happen only through Accept"}
		}
		return Verdict{AskPerson, "names the main checkout " + r.MainCheckout}
	}
	if v, ok := r.bashWrites(c); ok {
		return v
	}
	if rmCommand.MatchString(" " + c) {
		for _, f := range strings.Fields(c) {
			if strings.HasPrefix(f, "/") || strings.HasPrefix(f, "~") || strings.HasPrefix(f, "..") || strings.Contains(f, "/../") {
				if !strings.HasPrefix(f, "/") || !r.inside(f) {
					return Verdict{AskPerson, fmt.Sprintf("deletes outside the task's worktree (%s)", f)}
				}
			}
		}
	}
	if secretPath.MatchString(lastPathish(c)) || strings.Contains(c, ".env") || strings.Contains(c, ".ssh/") {
		return Verdict{AskPerson, "touches a file that may hold a secret"}
	}
	return Verdict{Allow, ""}
}

// lastPathish is the command's last word, the usual place of a file path.
func lastPathish(c string) string {
	f := strings.Fields(c)
	if len(f) == 0 {
		return ""
	}
	return strings.Trim(f[len(f)-1], `"'`)
}

// bashWrites finds, best effort, the paths a Bash command writes (output
// redirections, path arguments of commands that write) and applies the
// file-tool rules to them: outside the worktree, a secret, a manifest.
// Temporary directories and /dev/null are fine.
func (r Rules) bashWrites(c string) (Verdict, bool) {
	var targets []string
	for _, m := range redirect.FindAllStringSubmatch(c, -1) {
		targets = append(targets, strings.Trim(m[1], `"'`))
	}
	if writeCommand.MatchString(" " + c) {
		for _, f := range strings.Fields(c) {
			targets = append(targets, strings.Trim(f, `"'`))
		}
	}
	for _, t := range targets {
		if t == "" || strings.HasPrefix(t, "-") {
			continue
		}
		outside := strings.HasPrefix(t, "~") || strings.HasPrefix(t, "..") || strings.Contains(t, "/../") ||
			strings.HasPrefix(t, "/") && !r.inside(t)
		switch {
		case outside && strings.HasPrefix(t, "/") && harmlessPath(t):
		case outside:
			return Verdict{AskPerson, "writes outside the task's worktree: " + t}, true
		case manifest.MatchString(filepath.ToSlash(t)):
			return Verdict{AskPerson, "changes a dependency manifest: " + filepath.Base(t)}, true
		case secretPath.MatchString(filepath.ToSlash(r.abs(t))):
			return Verdict{AskPerson, "writes a file that may hold a secret: " + t}, true
		}
	}
	return Verdict{}, false
}

func harmlessPath(p string) bool {
	p = filepath.Clean(p)
	if p == "/dev/null" || p == "/dev/stdout" || p == "/dev/stderr" {
		return true
	}
	for _, tmp := range []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"} {
		tmp = filepath.Clean(tmp)
		if p == tmp || strings.HasPrefix(p, tmp+"/") {
			return true
		}
	}
	return false
}
