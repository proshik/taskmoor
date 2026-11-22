package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/gitx"
	"github.com/proshik/taskmoor/internal/schema"
	"github.com/proshik/taskmoor/internal/state"
)

// Source says where task files and tasks.toml are read from.
type Source string

const (
	// SourceMain reads the main branch's tree (`git ls-tree` + `cat-file`).
	SourceMain Source = "main"
	// SourceWorktree reads the working tree on disk.
	SourceWorktree Source = "worktree"
)

// ParseSource checks a --source value.
func ParseSource(s string) (Source, error) {
	switch Source(s) {
	case SourceMain, SourceWorktree:
		return Source(s), nil
	}
	return "", fmt.Errorf("--source must be main or worktree, not %q", s)
}

// Snapshot is everything read from a project at one moment: files, git
// facts and state files. Deriving statuses from it reads nothing more.
type Snapshot struct {
	Root      string // the worktree's top level
	Name      string // the repository's name, for the board
	CommonDir string
	StateDir  string
	Source    Source

	Config   *Config
	MainRef  string // refs/heads/<main_branch>
	MainHead string // "" when the branch does not exist

	// Tasks parsed without error, sorted by id; one per id.
	Tasks []*Task
	// Findings made while reading: tasks.toml, task files, state files.
	Findings []finding.Finding

	// Merges maps an id to its oldest `Task:` commit on the main branch's
	// first-parent history; AllMerges to all of them, newest first.
	Merges    map[string]string
	AllMerges map[string][]string
	// BadTrailers are `Task:` values that are not an id of this project.
	BadTrailers []Trailer
	// Releases maps a done task's id to the first release tag (version
	// order) containing its merge; "" for a rule = "merge" component.
	Releases map[string]string
	// AfterRelease holds the after_release facts per task id.
	AfterRelease map[string]AfterRelease

	States []state.Entry

	// OnMain is, for the worktree source, the task file names present on
	// the main branch.
	OnMain map[string]bool

	// Fingerprint was taken before the snapshot read anything else: a
	// later Fingerprint that differs means something changed since.
	Fingerprint string

	// ConfigBroken says tasks.toml could not be used (worktree source
	// only): its findings are reported and nothing needing it is checked.
	ConfigBroken bool
}

// Trailer is one `Task:` trailer value on a main-branch commit.
type Trailer struct {
	Commit, Value string
}

// AfterRelease is what git says about a task's after_release commit.
type AfterRelease struct {
	Commit    string // full hash; "" when it names no commit
	OnMain    bool
	Satisfied bool
}

// Task returns the task with id, nil when there is none.
func (s *Snapshot) Task(id string) *Task {
	i := sort.Search(len(s.Tasks), func(i int) bool { return s.Tasks[i].ID >= id })
	if i < len(s.Tasks) && s.Tasks[i].ID == id {
		return s.Tasks[i]
	}
	return nil
}

// Component returns the task's component, ok=false when it is not defined.
func (s *Snapshot) Component(t *Task) (Component, bool) {
	c, ok := s.Config.Components[t.Component]
	return c, ok
}

// Loader loads snapshots and caches release lookups between loads while
// the set of tags stays the same.
type Loader struct {
	mu       sync.Mutex
	tagsKey  string
	releases map[[2]string][]string
}

// LoadError means the project cannot be read at all.
type LoadError struct{ Err error }

func (e *LoadError) Error() string { return e.Err.Error() }
func (e *LoadError) Unwrap() error { return e.Err }

func loadErr(format string, args ...any) error {
	return &LoadError{Err: fmt.Errorf(format, args...)}
}

// Load reads the project containing dir.
func (l *Loader) Load(ctx context.Context, dir string, src Source) (*Snapshot, error) {
	sch, err := schema.Load()
	if err != nil {
		return nil, err
	}
	if err := gitx.CheckVersion(ctx); err != nil {
		return nil, &LoadError{Err: err}
	}
	root, err := gitx.Toplevel(ctx, dir)
	if err != nil {
		return nil, &LoadError{Err: err}
	}
	repo := gitx.Repo{Dir: root}
	common, err := repo.CommonDir(ctx)
	if err != nil {
		return nil, &LoadError{Err: err}
	}
	snap := &Snapshot{
		Root:         root,
		Name:         repoName(root, common),
		CommonDir:    common,
		StateDir:     state.Dir(common),
		Source:       src,
		Merges:       map[string]string{},
		AllMerges:    map[string][]string{},
		Releases:     map[string]string{},
		AfterRelease: map[string]AfterRelease{},
	}

	// The main branch's name comes from tasks.toml, which (for the main
	// source) is read from the main branch: learn the name from the working
	// tree's tasks.toml, or HEAD's when the working tree's is missing or
	// unusable.
	wtData, wtErr := readLimited(filepath.Join(root, ConfigFile))
	var boot *Config
	var bootFindings []finding.Finding
	switch {
	case wtErr == nil:
		boot, bootFindings = ParseConfig(wtData)
	case !errors.Is(wtErr, os.ErrNotExist):
		// unreadable (a symlink, too big): unusable, reported below
		bootFindings = []finding.Finding{finding.Errorf(ConfigFile, 0, "cannot read: %v", wtErr)}
		wtErr = nil
	}
	usable := func(c *Config) bool { return c != nil && c.Project.MainBranch != "" }
	switch {
	case src == SourceWorktree && wtErr != nil:
		return nil, loadErr("%s not found in %s", ConfigFile, root)
	case src == SourceWorktree && !usable(boot):
		// validate reports what is wrong with the file; nothing that needs
		// the configuration is checked.
		if boot == nil {
			boot = &Config{}
		}
		snap.Config, snap.Findings, snap.ConfigBroken = boot, bootFindings, true
		if len(bootFindings) == 0 {
			snap.Findings = []finding.Finding{finding.Errorf(ConfigFile, 0, "project.main_branch is not set")}
		}
		if snap.Fingerprint, err = Fingerprint(ctx, snap); err != nil {
			return nil, &LoadError{Err: err}
		}
		files, fs, err := worktreeFiles(root)
		if err != nil {
			return nil, &LoadError{Err: err}
		}
		snap.Findings = append(snap.Findings, fs...)
		snap.parseTasks(files, sch)
		if snap.States, err = state.ReadAll(snap.StateDir, sch); err != nil {
			return nil, loadErr("read state directory: %v", err)
		}
		return snap, nil
	case src == SourceMain && !usable(boot):
		data, ok, err := repo.ShowFile(ctx, "HEAD", ConfigFile)
		if err != nil {
			return nil, &LoadError{Err: err}
		}
		if !ok {
			if wtErr == nil {
				return nil, loadErr("%s: %s", ConfigFile, firstProblem(bootFindings, "project.main_branch is not set"))
			}
			return nil, loadErr("%s not found in %s (working tree or HEAD)", ConfigFile, root)
		}
		boot, bootFindings = ParseConfig(data)
		if !usable(boot) {
			return nil, loadErr("%s at HEAD: %s", ConfigFile, firstProblem(bootFindings, "project.main_branch is not set"))
		}
	}
	snap.MainRef = "refs/heads/" + boot.Project.MainBranch
	// The fingerprint is taken before anything else is read, so a change
	// made while loading makes the next fingerprint differ from it.
	if snap.Fingerprint, err = Fingerprint(ctx, snap); err != nil {
		return nil, &LoadError{Err: err}
	}
	head, ok, err := repo.ResolveBranch(ctx, boot.Project.MainBranch)
	if err != nil {
		return nil, &LoadError{Err: err}
	}
	if ok {
		snap.MainHead = head
	}

	var files map[string][]byte // tasks/<name> → content
	switch src {
	case SourceMain:
		if snap.MainHead == "" {
			return nil, loadErr("main branch %s does not exist (project.main_branch in %s)", boot.Project.MainBranch, ConfigFile)
		}
		data, ok, err := repo.ShowFile(ctx, snap.MainHead, ConfigFile)
		if err != nil {
			return nil, &LoadError{Err: err}
		}
		if !ok {
			return nil, loadErr("%s not found on %s", ConfigFile, boot.Project.MainBranch)
		}
		cfg, fs := ParseConfig(data)
		if !usable(cfg) {
			return nil, loadErr("%s on %s: %s", ConfigFile, boot.Project.MainBranch, firstProblem(fs, "project.main_branch is not set"))
		}
		// The branch was chosen by the working tree's tasks.toml; the main
		// branch's own tasks.toml must name the same one, or a local edit
		// could point every reader and writer at another branch.
		if cfg.Project.MainBranch != boot.Project.MainBranch {
			return nil, loadErr("the working tree's %s names main branch %s, but %s's own %s names %s",
				ConfigFile, boot.Project.MainBranch, boot.Project.MainBranch, ConfigFile, cfg.Project.MainBranch)
		}
		snap.Config, snap.Findings = cfg, fs
		var tfs []finding.Finding
		files, tfs, err = mainFiles(ctx, repo, snap.MainHead)
		if err != nil {
			return nil, &LoadError{Err: err}
		}
		snap.Findings = append(snap.Findings, tfs...)
	case SourceWorktree:
		snap.Config, snap.Findings = boot, bootFindings
		var tfs []finding.Finding
		files, tfs, err = worktreeFiles(root)
		if err != nil {
			return nil, &LoadError{Err: err}
		}
		snap.Findings = append(snap.Findings, tfs...)
		if snap.MainHead != "" {
			entries, err := repo.LsTree(ctx, snap.MainHead, Dir)
			if err != nil {
				return nil, &LoadError{Err: err}
			}
			snap.OnMain = map[string]bool{}
			for _, e := range entries {
				snap.OnMain[path.Base(e.Path)] = true
			}
		}
	default:
		return nil, loadErr("unknown source %q", src)
	}
	if snap.MainHead == "" {
		snap.Findings = append(snap.Findings, finding.Errorf(ConfigFile, 0, "main branch %s does not exist (a local branch refs/heads/%s)", boot.Project.MainBranch, boot.Project.MainBranch))
	}

	snap.parseTasks(files, sch)

	if snap.MainHead != "" {
		if err := l.gitFacts(ctx, repo, snap); err != nil {
			return nil, &LoadError{Err: err}
		}
	}

	entries, err := state.ReadAll(snap.StateDir, sch)
	if err != nil {
		return nil, loadErr("read state directory: %v", err)
	}
	snap.States = entries
	return snap, nil
}

func firstProblem(fs []finding.Finding, fallback string) string {
	for _, f := range fs {
		if f.Severity == finding.Error {
			return f.Message
		}
	}
	return fallback
}

// readLimited reads a regular file of at most gitx.MaxBlob bytes.
func readLimited(p string) ([]byte, error) {
	// O_NONBLOCK: opening a FIFO must not hang a reader; O_NOFOLLOW: a
	// symlink is not the file git would commit.
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if info.Size() > gitx.MaxBlob {
		return nil, fmt.Errorf("%s is %d bytes, more than %d", p, info.Size(), gitx.MaxBlob)
	}
	return io.ReadAll(io.LimitReader(f, gitx.MaxBlob+1))
}

func repoName(root, common string) string {
	if filepath.Base(common) == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	return filepath.Base(root)
}

func mainFiles(ctx context.Context, repo gitx.Repo, rev string) (map[string][]byte, []finding.Finding, error) {
	entries, err := repo.LsTree(ctx, rev, Dir)
	if err != nil {
		return nil, nil, err
	}
	var hashes []string
	var fs []finding.Finding
	names := map[string]string{}
	for _, e := range entries {
		name := path.Base(e.Path)
		if !IsTaskFile(name) {
			continue
		}
		if !e.Regular() {
			fs = append(fs, finding.Errorf(Dir+"/"+name, 0, "not a regular file (mode %s); not read", e.Mode))
			continue
		}
		hashes = append(hashes, e.Hash)
		names[name] = e.Hash
	}
	blobs, err := repo.CatBlobs(ctx, hashes)
	if err != nil {
		return nil, nil, err
	}
	files := make(map[string][]byte, len(names))
	for name, h := range names {
		files[name] = blobs[h]
	}
	return files, fs, nil
}

func worktreeFiles(root string) (map[string][]byte, []finding.Finding, error) {
	des, err := os.ReadDir(filepath.Join(root, Dir))
	if err != nil {
		return nil, nil, err
	}
	files := map[string][]byte{}
	var fs []finding.Finding
	total := 0
	for _, de := range des {
		if de.IsDir() || !IsTaskFile(de.Name()) {
			continue
		}
		if !de.Type().IsRegular() {
			fs = append(fs, finding.Errorf(Dir+"/"+de.Name(), 0, "not a regular file; not read"))
			continue
		}
		data, err := readLimited(filepath.Join(root, Dir, de.Name()))
		if err != nil {
			return nil, nil, err
		}
		if total += len(data); total > gitx.MaxTotal {
			return nil, nil, fmt.Errorf("task files are more than %d bytes together", gitx.MaxTotal)
		}
		files[de.Name()] = data
	}
	return files, fs, nil
}

func (s *Snapshot) parseTasks(files map[string][]byte, sch *schema.Schemas) {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	byID := map[string]*Task{}
	for _, name := range names {
		t, fs := ParseTask(Dir+"/"+name, files[name], sch)
		s.Findings = append(s.Findings, fs...)
		if t == nil {
			continue
		}
		if prev, dup := byID[t.ID]; dup {
			s.Findings = append(s.Findings, finding.Errorf(t.File, t.Line("/id"), "id %s is also used by %s", t.ID, prev.File))
			continue
		}
		byID[t.ID] = t
		s.Tasks = append(s.Tasks, t)
	}
	sort.Slice(s.Tasks, func(i, j int) bool { return s.Tasks[i].ID < s.Tasks[j].ID })
}

// IsProjectID reports whether v is an id with the project's prefix.
func (s *Snapshot) IsProjectID(v string) bool {
	return TaskIDRe.MatchString(v) && strings.HasPrefix(v, s.Config.Project.IDPrefix+"-")
}

func (l *Loader) gitFacts(ctx context.Context, repo gitx.Repo, s *Snapshot) error {
	commits, err := repo.TaskTrailers(ctx, s.MainHead)
	if err != nil {
		return err
	}
	for _, c := range commits { // newest first
		seen := map[string]bool{}
		for _, v := range c.Values {
			if seen[v] {
				continue
			}
			seen[v] = true
			if !s.IsProjectID(v) {
				s.BadTrailers = append(s.BadTrailers, Trailer{Commit: c.Hash, Value: v})
				continue
			}
			s.AllMerges[v] = append(s.AllMerges[v], c.Hash)
			s.Merges[v] = c.Hash // the last assignment is the oldest
		}
	}

	refs, err := repo.RefsFingerprint(ctx, s.MainRef)
	if err != nil {
		return err
	}
	tags := tagsPart(refs)

	for _, t := range s.Tasks {
		comp, ok := s.Component(t)
		if merge, merged := s.Merges[t.ID]; merged && ok {
			switch comp.Release.Rule {
			case RuleMerge:
				s.Releases[t.ID] = ""
			case RuleTag:
				found, err := l.tagsContaining(ctx, repo, tags, merge, comp.Release.PatternString())
				if err != nil {
					return err
				}
				if len(found) > 0 {
					s.Releases[t.ID] = found[0]
				}
			}
		}
		if t.AfterRelease == "" {
			continue
		}
		var ar AfterRelease
		commit, exists, err := repo.ResolveCommit(ctx, t.AfterRelease)
		if err != nil {
			return err
		}
		if exists {
			ar.Commit = commit
			if ar.OnMain, err = repo.IsAncestor(ctx, commit, s.MainHead); err != nil {
				return err
			}
			if ok && ar.OnMain {
				switch comp.Release.Rule {
				case RuleMerge:
					ar.Satisfied = true
				case RuleTag:
					found, err := l.tagsContaining(ctx, repo, tags, commit, comp.Release.PatternString())
					if err != nil {
						return err
					}
					ar.Satisfied = len(found) > 0
				}
			}
		}
		s.AfterRelease[t.ID] = ar
	}
	return nil
}

// tagsPart keeps the refs/tags lines of a refs fingerprint.
func tagsPart(refs string) string {
	var b strings.Builder
	for _, l := range strings.Split(refs, "\n") {
		if strings.Contains(l, " refs/tags/") {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (l *Loader) tagsContaining(ctx context.Context, repo gitx.Repo, tagsKey, commit, pattern string) ([]string, error) {
	l.mu.Lock()
	if l.tagsKey != tagsKey || l.releases == nil {
		l.tagsKey = tagsKey
		l.releases = map[[2]string][]string{}
	}
	if v, ok := l.releases[[2]string{commit, pattern}]; ok {
		l.mu.Unlock()
		return v, nil
	}
	l.mu.Unlock()
	tags, err := repo.TagsContaining(ctx, commit, pattern)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.tagsKey == tagsKey {
		l.releases[[2]string{commit, pattern}] = tags
	}
	l.mu.Unlock()
	return tags, nil
}

// Fingerprint is a cheap summary of what a reload would read: the main
// branch and tag refs, the state files' names, sizes and mtimes, and with
// the worktree source the task files' names, sizes and mtimes.
func Fingerprint(ctx context.Context, s *Snapshot) (string, error) {
	repo := gitx.Repo{Dir: s.Root}
	var refArgs []string
	if s.MainRef != "" {
		refArgs = append(refArgs, s.MainRef)
	}
	refs, err := repo.RefsFingerprint(ctx, refArgs...)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(refs)
	b.WriteString("\x00state\n")
	listDir(&b, s.StateDir, func(n string) bool { return strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") })
	if s.Source == SourceWorktree {
		b.WriteString("\x00tasks\n")
		listDir(&b, filepath.Join(s.Root, Dir), func(n string) bool { return IsTaskFile(n) || n == "tasks.toml" })
	}
	return b.String(), nil
}

func listDir(b *strings.Builder, dir string, keep func(string) bool) {
	des, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(b, "error %v\n", errors.Unwrap(err) != nil)
		return
	}
	for _, de := range des {
		if !keep(de.Name()) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(b, "%s %d %d\n", de.Name(), info.Size(), info.ModTime().UnixNano())
	}
}
