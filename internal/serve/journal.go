package serve

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session states (Linear's agent session states, plus stopped).
const (
	StatePending       = "pending"
	StateActive        = "active"
	StateAwaitingInput = "awaitingInput"
	StateComplete      = "complete"
	StateError         = "error"
	StateStale         = "stale"
	StateStopped       = "stopped" // stopped by the person; the claim is kept (A7)
)

// Activity types.
const (
	ActThought     = "thought"
	ActAction      = "action"
	ActElicitation = "elicitation"
	ActResponse    = "response"
	ActError       = "error"
	ActPrompt      = "prompt"
	ActResult      = "result"
)

// Session is one agent session the service runs.
type Session struct {
	ID        string `json:"id"` // the harness's own session id (claude --session-id)
	Project   string `json:"project"`
	Task      string `json:"task"`
	Title     string `json:"title"`
	Profile   string `json:"profile"`
	Harness   string `json:"harness"`
	Model     string `json:"model,omitempty"`
	Worktree  string `json:"worktree"`
	Branch    string `json:"branch"`
	State     string `json:"state"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"`
	// Billing is "subscription", "api" or "" before the harness said.
	Billing  string  `json:"billing,omitempty"`
	ExitCode *int    `json:"exit_code,omitempty"`
	CostUSD  float64 `json:"cost_usd,omitempty"`
	Error    string  `json:"error,omitempty"`
	// Resume is the command that continues the conversation in a terminal.
	Resume string `json:"resume"`
}

// Activity is one entry of a session's feed.
type Activity struct {
	Seq  int    `json:"seq"`
	At   string `json:"at"`
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Tool string `json:"tool,omitempty"`
	Ask  string `json:"ask,omitempty"` // the ask an elicitation opened or answered
	By   string `json:"by,omitempty"`  // who answered: person, rules
}

// journal stores sessions: <store>/sessions/<id>.json (the session record,
// replaced whole) and <id>.jsonl (the activities, appended).
type journal struct {
	dir string
	mu  sync.Mutex
}

func newJournal(store string) (*journal, error) {
	dir := filepath.Join(store, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &journal{dir: dir}, nil
}

func (j *journal) saveSession(s *Session) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(j.dir, s.ID+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// remove deletes a session's record and feed (a Run that did not start).
func (j *journal) remove(id string) error {
	err1 := os.Remove(filepath.Join(j.dir, id+".json"))
	err2 := os.Remove(filepath.Join(j.dir, id+".jsonl"))
	for _, err := range []error{err1, err2} {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (j *journal) append(id string, a Activity) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(j.dir, id+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

func (j *journal) activities(id string) ([]Activity, error) {
	f, err := os.Open(filepath.Join(j.dir, id+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Activity
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var a Activity
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			continue // a torn last line after a crash
		}
		out = append(out, a)
	}
	return out, sc.Err()
}

// sessions loads every session record.
func (j *journal) sessions() ([]*Session, error) {
	des, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, de := range des {
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(j.dir, de.Name()))
		if err != nil {
			return nil, err
		}
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("session %s: %w", de.Name(), err)
		}
		out = append(out, &s)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].StartedAt < out[k].StartedAt })
	return out, nil
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
