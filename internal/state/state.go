// Package state reads and writes the shared state files,
// `<git-common-dir>/tasks/state/<ID>.json`.
package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/schema"
)

// Lifecycle values a state file may hold.
const (
	InProgress = "in_progress"
	Review     = "review"
)

// File is one state file.
type File struct {
	Format    int      `json:"format"`
	ID        string   `json:"id"`
	Lifecycle string   `json:"lifecycle"`
	Worktree  string   `json:"worktree"`
	Branch    string   `json:"branch"`
	Base      string   `json:"base"`
	StartedAt string   `json:"started_at"`
	UpdatedAt string   `json:"updated_at"`
	Writer    string   `json:"writer"`
	Harness   *Harness `json:"harness,omitempty"`
}

// Harness is a state file's harness record.
type Harness struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Kind       string `json:"kind,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	ObservedAt string `json:"observed_at"`
	Session    string `json:"session,omitempty"`
}

// Dir returns the state directory for a git common directory.
func Dir(commonDir string) string {
	return filepath.Join(commonDir, "tasks", "state")
}

// Path returns the state file path of a task.
func Path(dir, id string) string {
	return filepath.Join(dir, id+".json")
}

// Now formats t as the format's time: UTC RFC 3339 with seconds and Z.
func Now(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// Entry is one *.json file found in the state directory.
type Entry struct {
	Path string
	// Name is the file name without .json.
	Name string
	// File is nil when the file is unreadable, not JSON or fails the schema.
	File *File
	// Problem says why File is nil, or that the file's id differs from its
	// name (then File is set but must not be used).
	Problem string
}

// Valid reports whether the entry is usable as state: parsed, schema-valid
// and named after its id.
func (e Entry) Valid() bool { return e.File != nil && e.Problem == "" }

// ReadAll reads every <name>.json in dir. A missing directory is no state.
func ReadAll(dir string, sch *schema.Schemas) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, de := range des {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		// A FIFO, socket or symlink named <ID>.json is reported, not read
		// (read would block or follow it), and release can remove it.
		p := filepath.Join(dir, name)
		entries = append(entries, read(p, strings.TrimSuffix(name, ".json"), sch))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// Read reads one task's state file; ok=false when there is none.
func Read(dir, id string, sch *schema.Schemas) (Entry, bool, error) {
	p := Path(dir, id)
	if _, err := os.Lstat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	return read(p, id, sch), true, nil
}

// MaxSize is the largest state file a reader reads.
const MaxSize = 64 << 10

func read(p, name string, sch *schema.Schemas) Entry {
	e := Entry{Path: p, Name: name}
	data, err := readRegular(p)
	if err != nil {
		e.Problem = fmt.Sprintf("unreadable: %v", err)
		return e
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		e.Problem = fmt.Sprintf("not JSON: %v", err)
		return e
	}
	if vs := schema.Validate(sch.State, value); len(vs) > 0 {
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = v.String()
		}
		e.Problem = "fails state.schema.json: " + strings.Join(parts, "; ")
		return e
	}
	// Decode the validated value, not the raw bytes: the schema accepts
	// numbers like 1.0 for an integer, which a Go int would refuse.
	norm, err := json.Marshal(integers(value))
	if err != nil {
		e.Problem = fmt.Sprintf("cannot decode: %v", err)
		return e
	}
	var f File
	if err := json.Unmarshal(norm, &f); err != nil {
		e.Problem = fmt.Sprintf("cannot decode: %v", err)
		return e
	}
	e.File = &f
	if f.ID != name {
		e.Problem = fmt.Sprintf("id %s does not equal the file name %s.json", f.ID, name)
	}
	return e
}

// readRegular reads a regular file of at most MaxSize bytes without
// following a symlink or blocking on a FIFO.
func readRegular(p string) ([]byte, error) {
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
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	if info.Size() > MaxSize {
		return nil, fmt.Errorf("%d bytes, more than %d", info.Size(), MaxSize)
	}
	return io.ReadAll(io.LimitReader(f, MaxSize+1))
}

// integers turns every integral JSON number (1.0, 1e0) into an integer
// literal, exactly; other values are kept.
func integers(v any) any {
	switch x := v.(type) {
	case json.Number:
		var r big.Rat
		if _, ok := r.SetString(string(x)); ok && r.IsInt() {
			return json.Number(r.Num().String())
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = integers(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = integers(e)
		}
		return x
	}
	return v
}

// Finding renders an entry's problem.
func (e Entry) Finding(sev finding.Severity) finding.Finding {
	return finding.Finding{Severity: sev, File: e.Path, Message: "state file " + e.Problem + "; treated as no state"}
}

// ErrExists is returned by Create when the task already has a state file.
var ErrExists = errors.New("a state file already exists")

// Encode validates f against the schema and returns its bytes.
func Encode(f *File, sch *schema.Schemas) ([]byte, error) {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > MaxSize {
		return nil, fmt.Errorf("the new state file is %d bytes; readers take at most %d", len(data), MaxSize)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if vs := schema.Validate(sch.State, value); len(vs) > 0 {
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = v.String()
		}
		return nil, fmt.Errorf("the new state file fails state.schema.json: %s", strings.Join(parts, "; "))
	}
	return data, nil
}

// Create publishes a new state file atomically and only if none exists:
// the bytes go to a temporary file in the same directory, are fsynced, and
// are published with link(2), which fails when the target exists.
func Create(dir string, f *File, sch *schema.Schemas) error {
	return publish(dir, f, sch, func(tmp, target string) error {
		if err := os.Link(tmp, target); err != nil {
			if errors.Is(err, os.ErrExist) {
				return ErrExists
			}
			return err
		}
		return nil
	})
}

// Replace publishes a new version of a state file with rename(2).
func Replace(dir string, f *File, sch *schema.Schemas) error {
	return publish(dir, f, sch, os.Rename)
}

func publish(dir string, f *File, sch *schema.Schemas, put func(tmp, target string) error) error {
	data, err := Encode(f, sch)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := tempFile(dir, f.ID)
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // after link the temp name is an extra link; after rename it is gone
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if beforePublish != nil {
		beforePublish()
	}
	if err := put(name, Path(dir, f.ID)); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return &NotDurableError{Err: err}
	}
	return nil
}

// beforePublish is a test hook run after the temporary file is complete
// and before it is published.
var beforePublish func()

// NotDurableError means a change was published (other readers see it) but
// syncing the directory failed, so it may not survive a crash. The caller
// treats the change as made and says so.
type NotDurableError struct{ Err error }

func (e *NotDurableError) Error() string {
	return fmt.Sprintf("the change is written, but syncing the state directory failed (it may not survive a crash): %v", e.Err)
}

func (e *NotDurableError) Unwrap() error { return e.Err }

func tempFile(dir, id string) (*os.File, error) {
	for range 10 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		p := filepath.Join(dir, "."+id+"."+hex.EncodeToString(b[:])+".tmp")
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, errors.New("cannot create a temporary state file")
}

// Remove deletes a task's state file; os.ErrNotExist when there is none.
func Remove(dir, id string) error {
	if err := os.Remove(Path(dir, id)); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return &NotDurableError{Err: err}
	}
	return nil
}

// syncDir fsyncs a directory; a variable so tests can make it fail.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Lock takes the per-project lock, flock(2) on <dir>/.lock, shared with
// every writer that starts tasks. It waits until the lock is free or ctx
// is done.
func Lock(ctx context.Context, dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, err
	}
	delay := 5 * time.Millisecond
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("waiting for the lock %s: %w", f.Name(), ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, 100*time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
