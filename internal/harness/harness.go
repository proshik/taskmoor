// Package harness reports what the program running a task is doing: from
// Guild when taskmoor runs inside it, otherwise from the state file's
// harness record.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/proshik/taskmoor/internal/state"
)

// Statuses are the format's harness words.
var Statuses = []string{"idle", "working", "blocked", "done", "error", "unknown", "exited"}

// Record sources.
const (
	SourceGuild = "guild"
	SourceState = "state"
	SourceNone  = "none"
)

// Report-level harness sources (`harness_source` in JSON).
const (
	ReportGuild            = "guild"
	ReportNone             = "none"
	ReportGuildUnavailable = "guild-unavailable"
)

// Record is a task's harness status.
type Record struct {
	Name       string `json:"name,omitempty"`
	Status     string `json:"status"`
	Kind       string `json:"kind,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
	Session    string `json:"session,omitempty"`
	Source     string `json:"source"`
	// Raw is Guild's own word when it is not one taskmoor knows.
	Raw string `json:"raw,omitempty"`
	// Note explains an unknown status.
	Note string `json:"note,omitempty"`
}

// Agent is one entry of `guild agent list --json`.
type Agent struct {
	Session  string `json:"session"`
	Harness  string `json:"harness"`
	Worktree string `json:"worktree"`
	Status   string `json:"status"`
	Kind     string `json:"kind"`
	Task     bool   `json:"task"`
	Open     bool   `json:"open"`
	Exited   bool   `json:"exited"`
}

// Source lists the agents a harness host knows about.
type Source interface {
	Agents(ctx context.Context) ([]Agent, error)
}

// Runner runs a program and returns its stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs programs with os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return out, fmt.Errorf("%s: %w: %s", name, err, ee.Stderr)
	}
	return out, err
}

// Guild asks `guild agent list --json`.
type Guild struct {
	Program string
	Run     Runner
	Timeout time.Duration
}

// FromEnv returns the Guild source when GUILD_SOCKET is set and guild is on
// PATH; nil otherwise.
func FromEnv(getenv func(string) string) Source {
	if getenv("GUILD_SOCKET") == "" {
		return nil
	}
	prog, err := exec.LookPath("guild")
	if err != nil {
		return nil
	}
	return &Guild{Program: prog, Run: ExecRunner, Timeout: 2 * time.Second}
}

// Agents implements Source.
func (g *Guild) Agents(ctx context.Context) ([]Agent, error) {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := g.Run(ctx, g.Program, "agent", "list", "--json")
	if ctx.Err() != nil {
		return nil, fmt.Errorf("guild agent list: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("guild agent list: %w", err)
	}
	var reply struct {
		Agents []Agent `json:"agents"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		return nil, fmt.Errorf("guild agent list: %w", err)
	}
	return reply.Agents, nil
}

// MapStatus maps Guild's wire words to the format's.
func MapStatus(a Agent) (status, kind, raw string) {
	if a.Exited {
		return "exited", "", ""
	}
	switch a.Status {
	case "idle", "working", "done", "unknown":
		return a.Status, "", ""
	case "waiting":
		return "blocked", a.Kind, ""
	case "failed":
		return "error", "", ""
	}
	return "unknown", "", a.Status
}

// Fingerprint summarises an agent list (or its failure) for the refresh
// loop; "" without a source.
func Fingerprint(src Source, agents []Agent, err error) string {
	if src == nil {
		return ""
	}
	if err != nil {
		return "\x00guild unavailable"
	}
	var b strings.Builder
	b.WriteString("\x00guild\n")
	for _, a := range agents {
		fmt.Fprintf(&b, "%s %s %s %s %v %v %v\n", a.Session, a.Worktree, a.Status, a.Kind, a.Exited, a.Task, a.Open)
	}
	return b.String()
}

// Resolve returns a harness record for every running task (in_progress or
// review, given with its state file), the report-level source and the
// fingerprint of the agent list it used.
func Resolve(ctx context.Context, src Source, running map[string]*state.File, now time.Time) (map[string]Record, string, string) {
	out := map[string]Record{}
	reportSource := ReportNone
	var agents []Agent
	var agentsErr error
	guildOK := false
	if src != nil {
		agents, agentsErr = src.Agents(ctx)
		if err := agentsErr; err != nil {
			reportSource = ReportGuildUnavailable
		} else {
			reportSource = ReportGuild
			guildOK = true
		}
	}
	for id, f := range running {
		if guildOK {
			if rec, ok := match(agents, f, now); ok {
				out[id] = rec
				continue
			}
		}
		if f.Harness != nil {
			h := f.Harness
			out[id] = Record{Name: h.Name, Status: h.Status, Kind: h.Kind, ExitCode: h.ExitCode,
				ObservedAt: h.ObservedAt, Session: h.Session, Source: SourceState}
			continue
		}
		out[id] = Record{Status: "unknown", Source: SourceNone, Note: "no harness reported"}
	}
	return out, reportSource, Fingerprint(src, agents, agentsErr)
}

func match(agents []Agent, f *state.File, now time.Time) (Record, bool) {
	var found []Agent
	if f.Harness != nil && f.Harness.Session != "" {
		for _, a := range agents {
			if a.Session == f.Harness.Session {
				found = append(found, a)
			}
		}
	} else {
		want := samePath(f.Worktree)
		for _, a := range agents {
			if a.Worktree != "" && samePath(a.Worktree) == want {
				found = append(found, a)
			}
		}
		found = narrow(found, func(a Agent) bool { return a.Task })
		found = narrow(found, func(a Agent) bool { return a.Open })
	}
	switch len(found) {
	case 0:
		return Record{}, false
	case 1:
		a := found[0]
		status, kind, raw := MapStatus(a)
		return Record{Name: a.Harness, Status: status, Kind: kind, Raw: raw, Session: a.Session,
			ObservedAt: state.Now(now), Source: SourceGuild}, true
	}
	return Record{Status: "unknown", Source: SourceGuild, ObservedAt: state.Now(now),
		Note: "several sessions in the worktree"}, true
}

// narrow keeps the agents matching keep when there are several and at
// least one matches.
func narrow(as []Agent, keep func(Agent) bool) []Agent {
	if len(as) < 2 {
		return as
	}
	var kept []Agent
	for _, a := range as {
		if keep(a) {
			kept = append(kept, a)
		}
	}
	if len(kept) == 0 {
		return as
	}
	return kept
}

// samePath cleans a path and resolves its symlinks when it exists.
func samePath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// SamePath reports whether two paths name the same directory after
// cleaning and resolving symlinks.
func SamePath(a, b string) bool { return samePath(a) == samePath(b) }
