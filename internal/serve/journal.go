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
	StateStopped       = "stopped"    // stopped by the person; the claim is kept (A7)
	StateHandedOver    = "handedOver" // continued in the person's terminal; the service let go
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
	ID      string `json:"id"` // the harness's own session id (claude --session-id)
	Project string `json:"project"`
	Task    string `json:"task"`
	// Idea is the idea a grilling session grills (stage 8); Task is ""
	// then.
	Idea      string `json:"idea,omitempty"`
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
	// Confined: the agent's commands run in Claude's sandbox (C13).
	Confined bool `json:"confined"`
	// Isolation: "sandbox", "codex" (Codex's workspace-write) or "".
	Isolation string `json:"isolation"`
	// ChosenBy says the profile was the dispatcher's suggestion, and why
	// (stage 13); empty: routed or picked by the person.
	ChosenBy string `json:"chosen_by,omitempty"`
	// AgentSession is an ACP agent's own session id (Codex's thread,
	// OpenCode's session), for resuming and the terminal; Claude's is ID.
	AgentSession string `json:"agent_session,omitempty"`
	// RestartOf / RestartedAs link the sessions of a Restart (A24).
	RestartOf   string `json:"restart_of,omitempty"`
	RestartedAs string `json:"restarted_as,omitempty"`
	// Abandoned: a Restart's successor that never got the claim.
	Abandoned bool `json:"abandoned,omitempty"`
	// Review is the code review (A5); Merged the merge commit of Accept.
	Review *Review `json:"review,omitempty"`
	Merged string  `json:"merged,omitempty"`
	// CleanedUp: its worktree and branch were removed after the merge
	// (stage 12).
	CleanedUp bool `json:"cleaned_up,omitempty"`
	// ProjectPath is the registered repository the session belongs to: a
	// project name reused for another repository does not inherit it.
	ProjectPath string `json:"project_path,omitempty"`
	// HandoverPending: a handover to the terminal started and has not
	// finished letting go of the claim (Open finishes it).
	HandoverPending bool `json:"handover_pending,omitempty"`
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
	dir    string
	mu     sync.Mutex
	saveMu sync.Mutex     // session records are written one at a time, in order
	seq    map[string]int // session → the last activity number written
}

func newJournal(store string) (*journal, error) {
	dir := filepath.Join(store, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &journal{dir: dir, seq: map[string]int{}}, nil
}

func (j *journal) saveSession(s *Session) error {
	j.saveMu.Lock() // saves land in the order they are made
	defer j.saveMu.Unlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(j.dir, s.ID+".json"), append(data, '\n'))
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

// append writes an activity with the next number of its session (1, 2,
// …; a feed's lines in order) and returns it numbered.
func (j *journal) append(id string, a Activity) (Activity, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	n, ok := j.seq[id]
	if !ok {
		existing, err := j.activitiesLocked(id)
		if err != nil {
			return a, err
		}
		for _, e := range existing { // the highest number, not the count
			n = max(n, e.Seq)
		}
	}
	a.Seq = n + 1
	p := filepath.Join(j.dir, id+".jsonl")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return a, err
	}
	defer f.Close()
	// a torn last line (a crash mid-write) is closed first, so the new
	// record stays a line of its own
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return a, err
			}
		}
	}
	data, err := json.Marshal(a)
	if err != nil {
		return a, err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return a, err
	}
	j.seq[id] = a.Seq
	return a, nil
}

func (j *journal) activities(id string) ([]Activity, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.activitiesLocked(id)
}

// activitiesLocked reads a feed; lines without a number (older journals)
// are numbered by position.
func (j *journal) activitiesLocked(id string) ([]Activity, error) {
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
		if a.Seq == 0 { // a journal from before numbering: by position
			a.Seq = len(out) + 1
			if len(out) > 0 && out[len(out)-1].Seq >= a.Seq {
				a.Seq = out[len(out)-1].Seq + 1
			}
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
