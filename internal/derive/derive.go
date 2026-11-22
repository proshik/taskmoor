// Package derive works out each task's lifecycle and readiness from a
// loaded snapshot. It reads nothing itself.
package derive

import (
	"fmt"
	"sort"
	"strings"

	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/state"
)

// Lifecycle values.
const (
	Backlog    = "backlog"
	Cancelled  = "cancelled"
	InProgress = state.InProgress
	Review     = state.Review
	Merged     = "merged"
	Done       = "done"
)

// Lifecycles in board order.
var Lifecycles = []string{Backlog, InProgress, Review, Merged, Done, Cancelled}

// Status is a task's derived status.
type Status struct {
	Lifecycle   string
	MergeCommit string
	ReleaseTag  string
	// State is the task's valid state file, if any; Stale says git or the
	// file's `cancelled` already decided the lifecycle, so the state file no
	// longer counts.
	State *state.File
	Stale bool
	Ready bool
	// Reasons a backlog task is not ready.
	Reasons []string
}

// Running reports whether the lifecycle holds the task's touches.
func Running(lifecycle string) bool {
	return lifecycle == InProgress || lifecycle == Review
}

// Derive returns every task's status keyed by id.
func Derive(s *project.Snapshot) map[string]*Status {
	states := ValidStates(s)
	out := make(map[string]*Status, len(s.Tasks))
	for _, t := range s.Tasks {
		st := &Status{Lifecycle: Backlog, MergeCommit: s.Merges[t.ID]}
		tag, released := s.Releases[t.ID]
		switch {
		case t.Status == Cancelled:
			st.Lifecycle = Cancelled
		case st.MergeCommit != "" && released:
			st.Lifecycle = Done
			st.ReleaseTag = tag
		case st.MergeCommit != "":
			st.Lifecycle = Merged
		}
		if f := states[t.ID]; f != nil {
			st.State = f
			if st.Lifecycle == Backlog {
				st.Lifecycle = f.Lifecycle
			} else {
				st.Stale = true
			}
		}
		out[t.ID] = st
	}
	running := runningTouches(s, out)
	unknown := UnknownReservations(s)
	for _, t := range s.Tasks {
		st := out[t.ID]
		if st.Lifecycle != Backlog {
			continue
		}
		st.Reasons = reasons(s, t, out, running)
		for _, id := range unknown {
			st.Reasons = append(st.Reasons, fmt.Sprintf("%s is running (state file) but is not a task on the main branch; its touches are unknown", id))
		}
		st.Ready = len(st.Reasons) == 0
	}
	return out
}

// UnknownReservations returns the ids of valid, running state files whose
// task is not in the snapshot: they hold touches nobody can read.
func UnknownReservations(s *project.Snapshot) []string {
	var ids []string
	for _, e := range s.States {
		if e.Valid() && s.Task(e.Name) == nil {
			ids = append(ids, e.Name)
		}
	}
	return ids
}

type runningTask struct {
	id, lifecycle string
	touches       []pattern
}

func runningTouches(s *project.Snapshot, all map[string]*Status) []runningTask {
	var rs []runningTask
	for _, o := range s.Tasks {
		if Running(all[o.ID].Lifecycle) {
			rs = append(rs, runningTask{id: o.ID, lifecycle: all[o.ID].Lifecycle, touches: patterns(o.Touches)})
		}
	}
	return rs
}

// ValidStates returns the usable state file per known task id.
func ValidStates(s *project.Snapshot) map[string]*state.File {
	m := map[string]*state.File{}
	for _, e := range s.States {
		if e.Valid() && s.Task(e.Name) != nil {
			m[e.Name] = e.File
		}
	}
	return m
}

func reasons(s *project.Snapshot, t *project.Task, all map[string]*Status, running []runningTask) []string {
	var rs []string
	for _, dep := range t.DependsOn {
		d, ok := all[dep]
		switch {
		case !ok:
			rs = append(rs, fmt.Sprintf("dependency %s does not exist", dep))
		case d.Lifecycle != Merged && d.Lifecycle != Done:
			rs = append(rs, fmt.Sprintf("dependency %s is %s", dep, d.Lifecycle))
		}
	}
	if t.AfterRelease != "" {
		comp, compOK := s.Component(t)
		ar := s.AfterRelease[t.ID]
		switch {
		case !compOK:
			rs = append(rs, fmt.Sprintf("after_release %s: component %s does not exist", t.AfterRelease, t.Component))
		case ar.Commit == "":
			rs = append(rs, fmt.Sprintf("after_release %s names no commit", t.AfterRelease))
		case !ar.OnMain:
			rs = append(rs, fmt.Sprintf("after_release %s is not on the main branch", t.AfterRelease))
		case !ar.Satisfied:
			rs = append(rs, fmt.Sprintf("after_release %s not in a %s tag", t.AfterRelease, comp.Release.PatternString()))
		}
	}
	mine := patterns(t.Touches)
	for _, o := range running {
		if o.id == t.ID {
			continue
		}
		if a, b, ok := firstOverlap(mine, o.touches); ok {
			what := b
			if a != b {
				what = a + " ~ " + b
			}
			rs = append(rs, fmt.Sprintf("touches overlap %s (%s): %s", o.id, o.lifecycle, what))
		}
	}
	return rs
}

// pattern is a touches pattern with its literal prefix split once.
type pattern struct {
	raw     string
	segs    []string
	partial bool
}

func patterns(ps []string) []pattern {
	out := make([]pattern, len(ps))
	for i, p := range ps {
		segs, partial := literal(p)
		out[i] = pattern{raw: p, segs: segs, partial: partial}
	}
	return out
}

func firstOverlap(as, bs []pattern) (a, b string, ok bool) {
	for _, x := range as {
		for _, y := range bs {
			if overlap(x.segs, x.partial, y.segs, y.partial) {
				return x.raw, y.raw, true
			}
		}
	}
	return "", "", false
}

// FirstOverlap returns the first pair of overlapping patterns, a from as
// and b from bs.
func FirstOverlap(as, bs []string) (a, b string, ok bool) {
	return firstOverlap(patterns(as), patterns(bs))
}

// literal splits a pattern's literal prefix (the part before its first `*`,
// `?` or `[`) into path segments. partial says the prefix ends inside its
// last segment, which then matches by string prefix.
func literal(p string) (segs []string, partial bool) {
	i := strings.IndexAny(p, "*?[")
	prefix := p
	if i >= 0 {
		prefix = p[:i]
		partial = prefix != "" && !strings.HasSuffix(prefix, "/")
	}
	// Empty segments (a doubled or trailing slash) name nothing: src//a.go
	// is src/a.go. A partial last segment stays partial.
	for _, seg := range strings.Split(prefix, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	if len(segs) == 0 {
		return nil, false
	}
	return segs, partial
}

// Overlap reports whether two touches patterns overlap: one literal prefix
// is a prefix of the other, compared segment by segment; a partial last
// segment matches by string prefix (`src/ro*` overlaps `src/root/a.rs`).
func Overlap(a, b string) bool {
	sa, pa := literal(a)
	sb, pb := literal(b)
	return overlap(sa, pa, sb, pb)
}

func overlap(sa []string, pa bool, sb []string, pb bool) bool {
	if len(sa) > len(sb) {
		sa, sb, pa, pb = sb, sa, pb, pa
	}
	if len(sa) == 0 {
		return true
	}
	last := len(sa) - 1
	for i := 0; i < last; i++ {
		if sa[i] != sb[i] {
			return false
		}
	}
	x, y := sa[last], sb[last]
	switch {
	case len(sa) == len(sb) && pa && pb:
		return strings.HasPrefix(x, y) || strings.HasPrefix(y, x)
	case pa:
		return strings.HasPrefix(y, x)
	case len(sa) == len(sb) && pb:
		return strings.HasPrefix(x, y)
	default:
		return x == y
	}
}

// Cycles returns the dependency cycles among known tasks, each as the ids
// along the cycle with the first repeated at the end, smallest id first.
func Cycles(tasks []*project.Task) [][]string {
	deps := map[string][]string{}
	for _, t := range tasks {
		deps[t.ID] = t.DependsOn
	}
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var cycles [][]string
	seen := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		color[id] = grey
		stack = append(stack, id)
		for _, d := range deps[id] {
			if _, known := deps[d]; !known || d == id {
				continue
			}
			switch color[d] {
			case white:
				visit(d)
			case grey:
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				cyc := rotate(append([]string(nil), stack[i:]...))
				if key := strings.Join(cyc, " "); !seen[key] {
					seen[key] = true
					cycles = append(cycles, append(cyc, cyc[0]))
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
	}
	ids := make([]string, 0, len(deps))
	for id := range deps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if color[id] == white {
			visit(id)
		}
	}
	return cycles
}

func rotate(c []string) []string {
	min := 0
	for i := range c {
		if c[i] < c[min] {
			min = i
		}
	}
	return append(c[min:], c[:min]...)
}
