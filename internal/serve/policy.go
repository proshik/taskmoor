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
}

// Decide checks one tool call. AskUserQuestion never comes here: it is a
// question, always the person's in this stage.
func (r Rules) Decide(tool string, input map[string]any) Verdict {
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
