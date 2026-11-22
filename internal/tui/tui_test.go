package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/finding"
	"github.com/proshik/taskmoor/internal/harness"
	"github.com/proshik/taskmoor/internal/project"
)

type fakeLoader struct {
	rep *app.Report
	fp  string
}

func (f *fakeLoader) Report(context.Context) (*app.Report, error) { return f.rep, nil }
func (f *fakeLoader) Fingerprint(context.Context, *app.Report) (string, error) {
	return f.fp, nil
}

func task(id string, prio int, lifecycle, title string) app.TaskView {
	return app.TaskView{
		ID: id, Title: title, Kind: "feature", Priority: prio, Component: "app",
		Lifecycle: lifecycle, Ready: lifecycle == "backlog", Reasons: []string{}, Touches: []string{"src/**"},
		Task: &project.Task{ID: id, Title: title, Body: "## Goal\n\nThe goal of " + id + ".\n"},
	}
}

func report(ts ...app.TaskView) *app.Report {
	return &app.Report{Project: "demo", MainBranch: "main", Source: "main", MainHead: "3f2a1c9deadbeef", HarnessSource: harness.ReportNone, Tasks: ts}
}

func baseTasks() []app.TaskView {
	t2 := task("TM-002", 1, "in_progress", "Session server")
	t2.Harness = &harness.Record{Status: "working", Source: "guild"}
	t3 := task("TM-003", 3, "backlog", "Not ready yet")
	t3.Ready = false
	t3.Reasons = []string{"dependency TM-002 is in_progress"}
	t5 := task("TM-005", 2, "review", "Waiting for review")
	t5.Harness = &harness.Record{Status: "blocked", Kind: "permission", Source: "guild"}
	t6 := task("TM-006", 4, "cancelled", "Dropped idea")
	t7 := task("TM-007", 2, "backlog", "Heavy features off by default")
	t7.NeedsPerson = true
	return []app.TaskView{task("TM-001", 2, "backlog", "First task"), t2, t3, task("TM-004", 2, "merged", "Merged one"), t5, t6, t7}
}

func newModel(t *testing.T, w, h int, ts ...app.TaskView) (Model, *fakeLoader) {
	t.Helper()
	if ts == nil {
		ts = baseTasks()
	}
	l := &fakeLoader{rep: report(ts...), fp: "fp-1"}
	l.rep.Fingerprint = "fp-1"
	m := New(context.Background(), l, l.rep)
	m.glamStyle = "notty"
	m = send(t, m, tea.WindowSizeMsg{Width: w, Height: h})
	return m, l
}

func send(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	return next.(Model)
}

// keys sends key presses: named keys in angle brackets, other runes as text.
func keys(t *testing.T, m Model, ks ...string) Model {
	t.Helper()
	for _, k := range ks {
		var msg tea.KeyPressMsg
		switch k {
		case "<left>":
			msg = tea.KeyPressMsg{Code: tea.KeyLeft}
		case "<right>":
			msg = tea.KeyPressMsg{Code: tea.KeyRight}
		case "<up>":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "<down>":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "<enter>":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "<esc>":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "<backspace>":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		default:
			r := []rune(k)[0]
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		m = send(t, m, msg)
	}
	return m
}

func view(m Model) string { return ansi.Strip(m.View().Content) }

func must(t *testing.T, v string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(v, w) {
			t.Errorf("view lacks %q:\n%s", w, v)
		}
	}
}

func mustNot(t *testing.T, v string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(v, n) {
			t.Errorf("view has %q:\n%s", n, v)
		}
	}
}

// focusedID is the id on the card drawn with the thick border.
func focusedID(t *testing.T, m Model) string {
	t.Helper()
	lines := strings.Split(view(m), "\n")
	for i, l := range lines {
		col := strings.Index(l, "┏")
		if col < 0 || i+1 >= len(lines) {
			continue
		}
		next := []rune(lines[i+1])
		start := len([]rune(l[:col])) + 1
		rest := string(next[start:])
		return strings.Fields(rest)[0]
	}
	return ""
}

func TestBoardColumnsAndCards(t *testing.T) {
	m, _ := newModel(t, 150, 40)
	v := view(m)
	must(t, v,
		"taskmoor · demo · main 3f2a1c9 · 7 tasks",
		"Backlog 3", "In progress 1", "Review 1", "Merged 1", "Done 0",
		"TM-001  P2 ✓", "TM-003  P3 ·", "TM-007  P2 ✓ ⚑",
		"● working", "● blocked permission", "dependency TM-002 is in_pr…",
		"Heavy features off by", "←→ column",
	)
	mustNot(t, v, "Cancelled", "TM-006")
	if lines := strings.Split(m.View().Content, "\n"); len(lines) != 40 {
		t.Errorf("%d lines, want 40", len(lines))
	}
	for i, l := range strings.Split(v, "\n") {
		if w := ansi.StringWidth(l); w != 150 {
			t.Errorf("line %d is %d wide", i, w)
		}
	}
	// backlog sorted by priority then id: TM-001 (P2), TM-007 (P2), TM-003 (P3)
	if a, b, c := strings.Index(v, "TM-001"), strings.Index(v, "TM-007"), strings.Index(v, "TM-003"); !(a < b && b < c) {
		t.Errorf("backlog order wrong: %d %d %d", a, b, c)
	}
	if id := focusedID(t, m); id != "TM-001" {
		t.Errorf("focused %q, want TM-001", id)
	}
}

func TestNavigation(t *testing.T) {
	m, _ := newModel(t, 150, 40)
	m = keys(t, m, "j")
	if id := focusedID(t, m); id != "TM-007" {
		t.Fatalf("after j: %q", id)
	}
	m = keys(t, m, "G")
	if id := focusedID(t, m); id != "TM-003" {
		t.Fatalf("after G: %q", id)
	}
	m = keys(t, m, "g", "<right>")
	if id := focusedID(t, m); id != "TM-002" {
		t.Fatalf("after →: %q", id)
	}
	m = keys(t, m, "l", "l", "l", "l", "l") // stops at the last column
	if m.col != 4 {
		t.Fatalf("column %d", m.col)
	}
	m = keys(t, m, "h", "h", "<left>")
	if id := focusedID(t, m); id != "TM-002" {
		t.Fatalf("back in In progress: %q", id)
	}
	m = keys(t, m, "<left>") // the backlog remembers its selection
	if id := focusedID(t, m); id != "TM-001" {
		t.Fatalf("backlog selection %q", id)
	}
}

func TestFilterAndToggles(t *testing.T) {
	m, _ := newModel(t, 150, 40)
	m = keys(t, m, "/", "h", "e", "a", "v")
	v := view(m)
	must(t, v, "filter: heav", "/heav█")
	mustNot(t, v, "TM-001", "TM-002")
	must(t, v, "TM-007")
	m = keys(t, m, "<enter>")
	must(t, view(m), "filter: heav", "←→ column")
	if id := focusedID(t, m); id != "TM-007" {
		t.Fatalf("focused %q", id)
	}
	m = keys(t, m, "<esc>")
	must(t, view(m), "TM-001", "TM-002")
	mustNot(t, view(m), "filter:")

	m = keys(t, m, "/", "x", "<backspace>", "t", "m", "-", "0", "0", "3", "<esc>")
	must(t, view(m), "TM-001") // esc while typing clears

	m = keys(t, m, "r")
	v = view(m)
	must(t, v, "ready only", "Backlog 2")
	mustNot(t, v, "TM-003")
	m = keys(t, m, "r", "c")
	v = view(m)
	must(t, v, "Cancelled 1", "TM-006")
}

func TestDetailView(t *testing.T) {
	m, _ := newModel(t, 150, 40)
	m = keys(t, m, "G", "<enter>") // TM-003
	v := view(m)
	must(t, v, "TM-003  Not ready yet", "backlog · P3 · feature · app", "ready", "- dependency TM-002 is in_progress",
		"touches", "src/**", "The goal of TM-003.", "esc back")
	mustNot(t, v, "Backlog 3")
	m = keys(t, m, "q")
	must(t, view(m), "Backlog 3")
	m = keys(t, m, "<right>", "<enter>")
	must(t, view(m), "TM-002  Session server", "● working")
	m = keys(t, m, "<esc>")
	must(t, view(m), "Backlog 3")
}

func TestDetailScrollsAndRerendersOnResize(t *testing.T) {
	long := task("TM-001", 1, "backlog", "Long")
	var b strings.Builder
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, "Paragraph line %d of a long body.\n\n", i)
	}
	long.Task.Body = b.String()
	m, _ := newModel(t, 100, 20, long)
	m = keys(t, m, "<enter>")
	must(t, view(m), "Paragraph line 0")
	mustNot(t, view(m), "Paragraph line 30")
	m = keys(t, m, "G")
	must(t, view(m), "Paragraph line 79")
	m = keys(t, m, "g")
	must(t, view(m), "Paragraph line 0")
	m = send(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
	for i, l := range strings.Split(view(m), "\n") {
		if w := ansi.StringWidth(l); w != 60 {
			t.Errorf("line %d is %d wide after resize", i, w)
		}
	}
}

func TestNarrowLayout(t *testing.T) {
	m, _ := newModel(t, 80, 30)
	v := view(m)
	must(t, v, "[Backlog 3]", "In progress 1", "TM-001", "TM-007")
	mustNot(t, v, "TM-002  P1")
	m = keys(t, m, "<right>")
	v = view(m)
	must(t, v, "[In progress 1]", "TM-002  P1", "● working")
	mustNot(t, v, "TM-001  P2")
}

func TestSelectionFollowsTaskAcrossRefresh(t *testing.T) {
	m, l := newModel(t, 150, 40)
	m = keys(t, m, "j") // TM-007 in Backlog
	if id := focusedID(t, m); id != "TM-007" {
		t.Fatalf("focused %q", id)
	}
	ts := baseTasks()
	for i := range ts {
		if ts[i].ID == "TM-007" {
			ts[i].Lifecycle = "in_progress"
			ts[i].Harness = &harness.Record{Status: "idle", Source: "state"}
		}
	}
	l.rep, l.fp = report(ts...), "fp-2"
	l.rep.Fingerprint = "fp-2"

	// a tick asks for the fingerprint; a changed one reloads
	next, cmd := m.Update(tickMsg{})
	m = next.(Model)
	msg := firstOf[fingerprintMsg](t, cmd)
	next, cmd = m.Update(msg)
	m = next.(Model)
	if !m.loading || cmd == nil {
		t.Fatal("a changed fingerprint did not start a reload")
	}
	m = send(t, m, cmd())
	if m.fp != "fp-2" || m.loading {
		t.Fatalf("fp %q loading %v", m.fp, m.loading)
	}
	if id := focusedID(t, m); id != "TM-007" {
		t.Fatalf("selection did not follow: %q", id)
	}
	if m.currentColumn() != "in_progress" {
		t.Fatalf("column %s", m.currentColumn())
	}
	must(t, view(m), "In progress 2", "● idle")

	// the same fingerprint does not reload
	next, cmd = m.Update(fingerprintMsg{fp: "fp-2"})
	if next.(Model).loading || cmd != nil {
		t.Fatal("an unchanged fingerprint reloaded")
	}
}

// firstOf runs cmd (unpacking a batch) and returns the first message of
// type T; tea.Tick commands are skipped.
func firstOf[T any](t *testing.T, cmd tea.Cmd) T {
	t.Helper()
	var zero T
	if cmd == nil {
		t.Fatal("no command")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c == nil {
				continue
			}
			done := make(chan tea.Msg, 1)
			go func() { done <- c() }()
			select {
			case got := <-done:
				if v, ok := got.(T); ok {
					return v
				}
			case <-timeoutCh():
			}
		}
		t.Fatalf("no %T in batch", zero)
	}
	if v, ok := msg.(T); ok {
		return v
	}
	t.Fatalf("got %T, want %T", msg, zero)
	return zero
}

func TestHelpAndQuit(t *testing.T) {
	m, _ := newModel(t, 150, 40)
	m = keys(t, m, "?")
	must(t, view(m), "taskmoor — help", "filter: id, title, component", "back (ctrl+c quits)")
	m = keys(t, m, "?")
	must(t, view(m), "Backlog 3")
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil || fmt.Sprintf("%T", cmd()) != "tea.QuitMsg" {
		t.Fatal("q does not quit the board")
	}
}

func TestGuildIndicator(t *testing.T) {
	m, l := newModel(t, 150, 40)
	l.rep.HarnessSource = harness.ReportGuildUnavailable
	m.rep = l.rep
	must(t, view(m), "Guild ○", "Guild: unavailable")
	m.rep.HarnessSource = harness.ReportGuild
	must(t, view(m), "Guild ●")
}

func TestScrollingColumn(t *testing.T) {
	var ts []app.TaskView
	for i := 1; i <= 12; i++ {
		ts = append(ts, task(fmt.Sprintf("TM-%03d", i), 2, "backlog", "Task"))
	}
	m, _ := newModel(t, 150, 21, ts...) // (21-3)/6 = 3 cards per column
	v := view(m)
	must(t, v, "TM-001", "TM-003", "↓")
	mustNot(t, v, "TM-004")
	m = keys(t, m, "j", "j", "j", "j")
	v = view(m)
	must(t, v, "TM-005", "↑↓")
	mustNot(t, v, "TM-001")
	if id := focusedID(t, m); id != "TM-005" {
		t.Fatalf("focused %q", id)
	}
}

func timeoutCh() <-chan time.Time { return time.After(200 * time.Millisecond) }

func TestReloadWhenDataChangesAndChangesBack(t *testing.T) {
	// The shown report read a claim (fingerprint "claimed"); the claim is
	// released before the first tick, so the fingerprint now is back to
	// what it was before the claim. Comparing with the report's own
	// fingerprint still reloads.
	m, l := newModel(t, 150, 40)
	m.rep.Fingerprint, m.fp = "claimed", "claimed"
	l.fp = "before-claim"
	next, cmd := m.Update(tickMsg{})
	m = next.(Model)
	msg := firstOf[fingerprintMsg](t, cmd)
	next, cmd = m.Update(msg)
	if !next.(Model).loading || cmd == nil {
		t.Fatal("no reload")
	}
}

func TestOnePollAtATime(t *testing.T) {
	m, _ := newModel(t, 150, 40)
	next, _ := m.Update(tickMsg{})
	m = next.(Model)
	if !m.polling {
		t.Fatal("not polling after a tick")
	}
	_, cmd := m.Update(tickMsg{}) // the first poll has not answered yet
	if cmd == nil {
		t.Fatal("the tick is not rescheduled")
	}
	if _, ok := cmd().(tea.BatchMsg); ok {
		t.Fatal("a second poll started while the first runs")
	}
}

func TestBoardShowsProblems(t *testing.T) {
	m, l := newModel(t, 150, 40)
	l.rep.Findings = []finding.Finding{finding.Errorf("/x/TM-009.json", 0, "state file not JSON")}
	must(t, view(m), "⚠ 1 problems (?)")
	m = send(t, m, tea.WindowSizeMsg{Width: 40, Height: 30})
	must(t, view(m), "⚠ 1 problems (?)")
	m = send(t, m, tea.WindowSizeMsg{Width: 150, Height: 40})
	m = keys(t, m, "?")
	must(t, view(m), "validate --source main", "state file not JSON")
}

func TestShrinkingKeepsSelectionVisible(t *testing.T) {
	var ts []app.TaskView
	for i := 1; i <= 8; i++ {
		ts = append(ts, task(fmt.Sprintf("TM-%03d", i), 2, "backlog", "Task"))
	}
	m, _ := newModel(t, 150, 51, ts...) // 8 cards fit
	m = keys(t, m, "G")
	m = send(t, m, tea.WindowSizeMsg{Width: 150, Height: 21}) // 3 fit
	if id := focusedID(t, m); id != "TM-008" {
		t.Fatalf("focused %q after shrinking, want TM-008 visible", id)
	}
}

func TestNarrowTabsKeepTheActiveOne(t *testing.T) {
	m, _ := newModel(t, 40, 30)
	m = keys(t, m, "l", "l", "l", "l") // Done
	v := view(m)
	must(t, v, "[Done 0]", "‹")
	m = keys(t, m, "h", "h", "h", "h")
	must(t, view(m), "[Backlog 3]", "›")
}
