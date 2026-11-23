// Package gitx reads a repository by running git.
//
// The task format is defined in git's own terms (trailers, `tag --contains`,
// `--git-common-dir`), so running git gives exactly those semantics.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Repo is a repository or one of its worktrees, addressed by a directory
// inside it.
type Repo struct {
	Dir string
}

// Error is a failed git command with its stderr.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *Error) Unwrap() error { return e.Err }

// exitCode returns git's exit code, or -1 when git did not run to an exit.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// Run runs git with args in the repository and returns its stdout.
func (r Repo) Run(ctx context.Context, args ...string) ([]byte, error) {
	return r.RunInput(ctx, nil, args...)
}

// RunInput is Run with stdin.
func (r Repo) RunInput(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	full := append([]string{"-C", r.Dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = readOnlyEnv()
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), &Error{Args: args, Stderr: errb.String(), Err: err}
	}
	return out.Bytes(), nil
}

// readOnlyEnv is the environment for git: no lazy fetch of missing objects
// in a partial clone (a reader must not write the object database or need
// the network) and no optional locks (no opportunistic index refresh).
//
// Variables that point git at another repository or index are dropped, so
// the repository is always the one found from the directory taskmoor was
// given (--repo), never one an outer environment selected.
func readOnlyEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if repoVars[name] {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0")
}

var repoVars = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true,
	"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_NAMESPACE": true,
	"GIT_PREFIX": true, "GIT_INTERNAL_SUPER_PREFIX": true,
}

// MinVersion is the oldest git taskmoor runs with: GIT_NO_LAZY_FETCH
// (2.45 honours it everywhere a reader goes) and `worktree list -z` (2.36).
var MinVersion = [3]int{2, 45, 0}

var (
	versionOnce sync.Once
	versionErr  error
)

// CheckVersion fails when the git on PATH is older than MinVersion. It
// runs git once per process.
func CheckVersion(ctx context.Context) error {
	versionOnce.Do(func() {
		out, err := exec.CommandContext(ctx, "git", "version").Output()
		if err != nil {
			versionErr = fmt.Errorf("git version: %w", err)
			return
		}
		v, ok := parseVersion(string(out))
		if !ok {
			versionErr = fmt.Errorf("cannot read the git version from %q", strings.TrimSpace(string(out)))
			return
		}
		for i := range v {
			if v[i] != MinVersion[i] {
				if v[i] < MinVersion[i] {
					versionErr = fmt.Errorf("git %d.%d.%d is too old; taskmoor needs git %d.%d or newer", v[0], v[1], v[2], MinVersion[0], MinVersion[1])
				}
				return
			}
		}
	})
	return versionErr
}

func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	f := strings.Fields(s) // "git version 2.50.1 (Apple Git-155)"
	if len(f) < 3 {
		return v, false
	}
	parts := strings.SplitN(f[2], ".", 4)
	if len(parts) < 2 {
		return v, false
	}
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			if i < 2 {
				return v, false
			}
			break
		}
		v[i] = n
	}
	return v, true
}

// path runs git and returns its output as a path: only the trailing
// newline is removed, so a path ending in a space stays intact.
func (r Repo) path(ctx context.Context, args ...string) (string, error) {
	out, err := r.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(out), "\n"), nil // a CR belongs to the name
}

func (r Repo) line(ctx context.Context, args ...string) (string, error) {
	out, err := r.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Toplevel returns the root of the worktree containing dir.
func Toplevel(ctx context.Context, dir string) (string, error) {
	return Repo{Dir: dir}.path(ctx, "rev-parse", "--show-toplevel")
}

// CommonDir returns the absolute git common directory, the same from every
// worktree of the repository.
func (r Repo) CommonDir(ctx context.Context) (string, error) {
	d, err := r.path(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Clean(d), nil
}

// ResolveCommit returns the full hash of rev as a commit, or ok=false when it
// names no commit.
func (r Repo) ResolveCommit(ctx context.Context, rev string) (hash string, ok bool, err error) {
	out, err := r.Run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		if exitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(string(out)), true, nil
}

// ResolveBranch returns the commit of the local branch refs/heads/<name>,
// exactly that ref (not a revision expression); ok=false when it does not
// exist or the name is not a valid branch name.
func (r Repo) ResolveBranch(ctx context.Context, name string) (hash string, ok bool, err error) {
	out, err := r.Run(ctx, "show-ref", "--verify", "--hash", "--end-of-options", "refs/heads/"+name)
	if err != nil {
		if c := exitCode(err); c == 1 || c == 128 {
			return "", false, nil
		}
		return "", false, err
	}
	return r.ResolveCommit(ctx, strings.TrimSpace(string(out)))
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := r.Run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// MergeBase returns the best common ancestor of a and b.
func (r Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	return r.line(ctx, "merge-base", a, b)
}

// TreeEntry is one entry of `git ls-tree`.
type TreeEntry struct {
	Mode, Type, Hash, Path string
}

// Regular reports whether the entry is a regular file (not a symlink,
// submodule or tree).
func (e TreeEntry) Regular() bool {
	return e.Type == "blob" && (e.Mode == "100644" || e.Mode == "100755")
}

func pathDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// LsTree lists the entries directly inside dir (a path relative to the root,
// without a trailing slash) at rev. A missing directory is an empty list.
func (r Repo) LsTree(ctx context.Context, rev, dir string) ([]TreeEntry, error) {
	args := []string{"ls-tree", "-z", "--end-of-options", rev}
	if dir != "" {
		args = append(args, dir+"/")
	}
	out, err := r.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			return nil, fmt.Errorf("git ls-tree: unexpected record %q", rec)
		}
		f := strings.Fields(meta)
		if len(f) != 3 {
			return nil, fmt.Errorf("git ls-tree: unexpected record %q", rec)
		}
		entries = append(entries, TreeEntry{Mode: f[0], Type: f[1], Hash: f[2], Path: path})
	}
	return entries, nil
}

// Limits on what a reader loads from git, so a huge file cannot exhaust
// memory before it is validated.
const (
	MaxBlob  = 1 << 20  // one task file or tasks.toml
	MaxTotal = 64 << 20 // all task files together
)

// TooBigError is a blob over MaxBlob, or blobs over MaxTotal together.
type TooBigError struct {
	Hash string
	Size int64
}

func (e *TooBigError) Error() string {
	if e.Hash == "" {
		return fmt.Sprintf("task files are %d bytes together, more than %d", e.Size, MaxTotal)
	}
	return fmt.Sprintf("object %s is %d bytes, more than %d", e.Hash, e.Size, MaxBlob)
}

// CatBlobs reads blobs by hash with one `git cat-file --batch`, streaming
// its output and checking each object's size before reading it.
func (r Repo) CatBlobs(ctx context.Context, hashes []string) (map[string][]byte, error) {
	if len(hashes) == 0 {
		return map[string][]byte{}, nil
	}
	var in bytes.Buffer
	for _, h := range hashes {
		in.WriteString(h)
		in.WriteByte('\n')
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"cat-file", "--batch"}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Dir}, args...)...)
	cmd.Env = readOnlyEnv()
	cmd.Stdin = &in
	var errb bytes.Buffer
	cmd.Stderr = &errb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, &Error{Args: args, Err: err}
	}
	blobs, readErr := readBatch(bufio.NewReader(stdout), hashes)
	if readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, readErr
	}
	if waitErr != nil {
		return nil, &Error{Args: args, Stderr: errb.String(), Err: waitErr}
	}
	return blobs, nil
}

func readBatch(br *bufio.Reader, hashes []string) (map[string][]byte, error) {
	blobs := make(map[string][]byte, len(hashes))
	var total int64
	for range hashes {
		header, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file: short output: %w", err)
		}
		f := strings.Fields(header)
		if len(f) == 2 && f[1] == "missing" {
			return nil, fmt.Errorf("git cat-file: object %s is missing locally (a partial clone is not fetched by readers)", f[0])
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("git cat-file: unexpected header %q", header)
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("git cat-file: unexpected header %q", header)
		}
		if size > MaxBlob {
			return nil, &TooBigError{Hash: f[0], Size: size}
		}
		if total += size; total > MaxTotal {
			return nil, &TooBigError{Size: total}
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(br, data); err != nil {
			return nil, fmt.Errorf("git cat-file: short object %s: %w", f[0], err)
		}
		if _, err := br.ReadByte(); err != nil { // the newline after the object
			return nil, fmt.Errorf("git cat-file: short output: %w", err)
		}
		blobs[f[0]] = data
	}
	return blobs, nil
}

// ShowFile reads path at rev; ok=false when rev has no such file.
//
// The entry must be a regular file: a symlink's blob is its target's name,
// not the file's content.
func (r Repo) ShowFile(ctx context.Context, rev, path string) (data []byte, ok bool, err error) {
	entries, err := r.LsTree(ctx, rev, pathDir(path))
	if err != nil {
		return nil, false, err
	}
	hash := ""
	for _, e := range entries {
		if e.Path == path {
			if !e.Regular() {
				return nil, false, fmt.Errorf("%s at %s is not a regular file (mode %s)", path, short(rev), e.Mode)
			}
			hash = e.Hash
		}
	}
	if hash == "" {
		return nil, false, nil
	}
	blobs, err := r.CatBlobs(ctx, []string{hash})
	if err != nil {
		return nil, false, err
	}
	return blobs[hash], true, nil
}

// TrailerCommit is a first-parent commit and its `Task:` trailer values.
type TrailerCommit struct {
	Hash   string
	Values []string
}

// TaskTrailers returns every commit on ref's first-parent history that
// carries a `Task:` trailer, newest first. The trailers are git's own
// parsing (`%(trailers)`), so "the last paragraph" means what
// `git interpret-trailers` means.
func (r Repo) TaskTrailers(ctx context.Context, ref string) ([]TrailerCommit, error) {
	out, err := r.Run(ctx, "log", "--first-parent",
		// one value per line: an unfolded value cannot hold a newline, so a
		// value is never split into two
		"--format=%H%x1f%(trailers:key=Task,valueonly,unfold,separator=%x0a)%x00",
		"--end-of-options", ref, "--")
	if err != nil {
		return nil, err
	}
	var commits []TrailerCommit
	for _, rec := range strings.Split(string(out), "\x00") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		hash, vals, ok := strings.Cut(rec, "\x1f")
		if !ok {
			return nil, fmt.Errorf("git log: unexpected record %q", rec)
		}
		vals = strings.TrimRight(vals, "\n")
		if vals == "" {
			continue
		}
		var values []string
		for _, v := range strings.Split(vals, "\n") {
			if v = strings.TrimSpace(v); v != "" {
				values = append(values, v)
			}
		}
		if len(values) > 0 {
			commits = append(commits, TrailerCommit{Hash: hash, Values: values})
		}
	}
	return commits, nil
}

// TagsContaining returns the tags matching pattern that contain commit, in
// version order.
func (r Repo) TagsContaining(ctx context.Context, commit, pattern string) ([]string, error) {
	out, err := r.Run(ctx, "tag", "--list", "--sort=version:refname", "--contains", commit, "--", pattern)
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			tags = append(tags, l)
		}
	}
	return tags, nil
}

// RefsFingerprint is the objects of the given refs and of every tag, in one
// call: it changes when the main branch moves or a tag is added, moved or
// removed.
func (r Repo) RefsFingerprint(ctx context.Context, refs ...string) (string, error) {
	args := append([]string{"for-each-ref", "--format=%(objectname) %(refname)"}, refs...)
	args = append(args, "refs/tags")
	out, err := r.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Worktree is one entry of `git worktree list --porcelain`.
type Worktree struct {
	Path   string
	Head   string
	Branch string // full ref, e.g. refs/heads/main; empty when detached
	// Prunable is set when git knows the worktree's directory is gone.
	Prunable bool
}

// HeadBranch returns the full ref the worktree at dir has checked out, ""
// when HEAD is detached.
func HeadBranch(ctx context.Context, dir string) (string, error) {
	out, err := Repo{Dir: dir}.Run(ctx, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		if exitCode(err) == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Worktrees lists the repository's worktrees.
func (r Repo) Worktrees(ctx context.Context) ([]Worktree, error) {
	out, err := r.Run(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var list []Worktree
	var cur *Worktree
	for _, f := range strings.Split(string(out), "\x00") {
		switch {
		case f == "":
			if cur != nil {
				list = append(list, *cur)
				cur = nil
			}
		case strings.HasPrefix(f, "worktree "):
			if cur != nil {
				list = append(list, *cur)
			}
			cur = &Worktree{Path: strings.TrimPrefix(f, "worktree ")}
		case cur == nil:
		case strings.HasPrefix(f, "HEAD "):
			cur.Head = strings.TrimPrefix(f, "HEAD ")
		case strings.HasPrefix(f, "branch "):
			cur.Branch = strings.TrimPrefix(f, "branch ")
		case f == "prunable" || strings.HasPrefix(f, "prunable "):
			cur.Prunable = true
		}
	}
	if cur != nil {
		list = append(list, *cur)
	}
	return list, nil
}

// ExitCode is git's exit code in an error of Run, or -1.
func ExitCode(err error) int { return exitCode(err) }

// RunLimited is Run keeping at most limit bytes of stdout (the rest is
// read and dropped, so git does not block); cut reports a longer output.
func (r Repo) RunLimited(ctx context.Context, limit int, args ...string) (out []byte, cut bool, err error) {
	full := append([]string{"-C", r.Dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = readOnlyEnv()
	lw := &limitBuffer{n: limit}
	var errb bytes.Buffer
	cmd.Stdout = lw
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return lw.b.Bytes(), lw.cut, &Error{Args: args, Stderr: errb.String(), Err: err}
	}
	return lw.b.Bytes(), lw.cut, nil
}

type limitBuffer struct {
	b   bytes.Buffer
	n   int
	cut bool
}

func (l *limitBuffer) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
			l.cut = true
		} else {
			l.b.Write(p)
		}
	} else if len(p) > 0 {
		l.cut = true
	}
	return len(p), nil
}
