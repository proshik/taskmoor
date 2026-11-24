// Package tui is taskmoor's board: a Bubble Tea model with a board of
// lifecycle columns, a task detail view, a filter and live refresh.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/derive"
	"github.com/proshik/taskmoor/internal/harness"
	"github.com/proshik/taskmoor/internal/text"
)

// Loader is what the board reads: app.App in the program, a fake in tests.
type Loader interface {
	Report(ctx context.Context) (*app.Report, error)
	Fingerprint(ctx context.Context, last *app.Report) (string, error)
}

// Interval is the refresh poll period.
const Interval = 2 * time.Second

const (
	cardHeight   = 6
	minColWidth  = 24
	headerHeight = 2 // title line + column names
	footerHeight = 1
)

var columnNames = map[string]string{
	derive.Backlog:    "Backlog",
	derive.InProgress: "In progress",
	derive.Review:     "Review",
	derive.Merged:     "Merged",
	derive.Done:       "Done",
	derive.Cancelled:  "Cancelled",
	colReady:          "Ready",
	colRunning:        "Running",
	colNeeds:          "Needs you",
	colIdeas:          "Ideas",
}

// The conveyor's own columns (A21), when the board reads taskmoor serve.
const (
	colReady   = "ready"
	colRunning = "running"
	colNeeds   = "needs"
	colIdeas   = "ideas" // the service's ideas (stage 8), read-only here
)

// Messages.
type (
	reportMsg struct {
		rep *app.Report
		err error
	}
	tickMsg        struct{}
	fingerprintMsg struct {
		fp  string
		err error
	}
)

// Model is the board.
type Model struct {
	ctx      context.Context
	loader   Loader
	interval time.Duration

	width, height int
	rep           *app.Report
	fp            string
	err           error
	loading       bool
	polling       bool

	col       int               // index into columns()
	selected  map[string]string // column → selected task id
	lastIndex map[string]int    // column → index of the last selection
	offset    map[string]int    // column → first visible card

	filter        string
	filtering     bool
	readyOnly     bool
	showCancelled bool
	help          bool

	detail     *detail
	glamStyle  string
	renderers  map[int]*glamour.TermRenderer
	tickActive bool
}

type detail struct {
	id    string
	vp    viewport.Model
	width int
}

// New makes a board over an already loaded report.
func New(ctx context.Context, loader Loader, rep *app.Report) Model {
	m := Model{
		ctx:       ctx,
		loader:    loader,
		interval:  Interval,
		width:     120,
		height:    40,
		rep:       rep,
		fp:        rep.Fingerprint,
		selected:  map[string]string{},
		lastIndex: map[string]int{},
		offset:    map[string]int{},
		glamStyle: "dark",
		renderers: map[int]*glamour.TermRenderer{},
	}
	m.fixSelection("")
	return m
}

// Run loads the project and runs the board until the person quits. When
// taskmoor serve runs and has the project registered, the board is its
// client and shows the conveyor's columns (A21).
func Run(ctx context.Context, a *app.App) error {
	loader := &autoLoader{a: a, find: serviceLoaderFor}
	rep, err := loader.Report(ctx)
	if err != nil {
		return err
	}
	m := New(ctx, loader, rep)
	_, err = tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil // stopped by a signal: a normal end
	}
	return err
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.tick(), tea.RequestBackgroundColor)
}

func (m Model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) fingerprintCmd() tea.Cmd {
	rep, loader, ctx := m.rep, m.loader, m.ctx
	return func() tea.Msg {
		fp, err := loader.Fingerprint(ctx, rep)
		return fingerprintMsg{fp: fp, err: err}
	}
}

func (m Model) loadCmd() tea.Cmd {
	loader, ctx := m.loader, m.ctx
	return func() tea.Msg {
		rep, err := loader.Report(ctx)
		return reportMsg{rep: rep, err: err}
	}
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.fixSelection(m.currentID()) // fewer cards per column: keep the selection in view
		if m.detail != nil {
			m.openDetail(m.detail.id, true)
		}
		return m, nil
	case tea.BackgroundColorMsg:
		style := "light"
		if msg.IsDark() {
			style = "dark"
		}
		if style != m.glamStyle {
			m.glamStyle = style
			m.renderers = map[int]*glamour.TermRenderer{}
			if m.detail != nil {
				m.openDetail(m.detail.id, true)
			}
		}
		return m, nil
	case tickMsg:
		// one poll at a time: a slow one is not stacked with the next
		if m.loading || m.polling {
			return m, m.tick()
		}
		m.polling = true
		return m, tea.Batch(m.fingerprintCmd(), m.tick())
	case fingerprintMsg:
		m.polling = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// m.fp is what the shown report read; any difference reloads
		if msg.fp == m.fp || m.loading {
			return m, nil
		}
		m.loading = true
		return m, m.loadCmd()
	case reportMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.applyReport(msg.rep)
		return m, nil
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	if m.detail != nil {
		var cmd tea.Cmd
		m.detail.vp, cmd = m.detail.vp.Update(msg)
		return m, cmd
	}
	return m, nil
}

// applyReport takes a new report; the selection follows the task id.
func (m *Model) applyReport(rep *app.Report) {
	cur := m.currentID()
	prevCol := m.currentColumn()
	m.rep, m.fp = rep, rep.Fingerprint
	// the set of columns may change (the service came or went): keep the
	// column by name, or a valid one
	cols := m.columns()
	m.col = min(m.col, len(cols)-1)
	for i, c := range cols {
		if c == prevCol {
			m.col = i
		}
	}
	m.fixSelection(cur)
	if m.detail != nil {
		if _, ok := rep.Find(m.detail.id); ok {
			m.openDetail(m.detail.id, true)
		} else {
			m.detail = nil
		}
	}
}

func (m Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	if m.help {
		switch k {
		case "?", "esc", "q", "enter":
			m.help = false
		}
		return m, nil
	}
	if m.filtering {
		switch k {
		case "enter":
			m.filtering = false
		case "esc":
			m.filtering = false
			m.filter = ""
		case "backspace":
			if r := []rune(m.filter); len(r) > 0 {
				m.filter = string(r[:len(r)-1])
			}
		default:
			if msg.Text != "" {
				m.filter += msg.Text
			}
		}
		m.fixSelection(m.currentID())
		return m, nil
	}
	if k == "?" {
		m.help = true
		return m, nil
	}
	if k == "R" {
		if m.loading {
			return m, nil
		}
		m.loading = true
		return m, m.loadCmd()
	}
	if m.detail != nil {
		return m.detailKey(k, msg)
	}
	return m.boardKey(k)
}

func (m Model) detailKey(k string, msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	vp := &m.detail.vp
	switch k {
	case "esc", "q":
		m.detail = nil
	case "up", "k":
		vp.ScrollUp(1)
	case "down", "j":
		vp.ScrollDown(1)
	case "pgup":
		vp.PageUp()
	case "pgdown", "space":
		vp.PageDown()
	case "g", "home":
		vp.GotoTop()
	case "G", "end":
		vp.GotoBottom()
	default:
		var cmd tea.Cmd
		*vp, cmd = vp.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m Model) boardKey(k string) (tea.Model, tea.Cmd) {
	cols := m.columns()
	switch k {
	case "q":
		return m, tea.Quit
	case "left", "h":
		if m.col > 0 {
			m.col--
		}
	case "right", "l":
		if m.col < len(cols)-1 {
			m.col++
		}
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "pgup":
		m.moveSelection(-m.cardsPerColumn())
	case "pgdown":
		m.moveSelection(m.cardsPerColumn())
	case "g", "home":
		m.moveSelection(-1 << 30)
	case "G", "end":
		m.moveSelection(1 << 30)
	case "enter":
		if id := m.currentID(); id != "" {
			m.openDetail(id, false)
		}
	case "esc":
		m.filter = ""
		m.fixSelection(m.currentID())
	case "/":
		m.filtering = true
	case "r":
		m.readyOnly = !m.readyOnly
		m.fixSelection(m.currentID())
	case "c":
		cur := m.currentID()
		m.showCancelled = !m.showCancelled
		if m.col >= len(m.columns()) {
			m.col = len(m.columns()) - 1
		}
		m.fixSelection(cur)
	}
	return m, nil
}

// columns are the lifecycles shown, in order.
func (m Model) columns() []string {
	cols := []string{derive.Backlog, derive.InProgress, derive.Review, derive.Merged, derive.Done}
	if m.rep != nil && m.rep.Conveyor {
		cols = []string{colIdeas, derive.Backlog, colReady, colRunning, colNeeds, derive.Review, derive.Merged, derive.Done}
	}
	if m.showCancelled {
		cols = append(cols, derive.Cancelled)
	}
	return cols
}

// items returns a column's tasks after the filter and the ready toggle,
// priority then id.
func (m Model) items(col string) []app.TaskView {
	if m.rep == nil {
		return nil
	}
	f := strings.ToLower(m.filter)
	var out []app.TaskView
	for _, t := range m.rep.Tasks {
		key := t.Lifecycle
		if t.Column != "" {
			key = t.Column
		}
		if key != col {
			continue
		}
		if col == derive.Backlog && m.readyOnly && !t.Ready {
			continue
		}
		if f != "" && !strings.Contains(strings.ToLower(t.ID+"\x00"+t.Title+"\x00"+t.Component), f) {
			continue
		}
		out = append(out, t)
	}
	app.SortByPriority(out)
	return out
}

func (m Model) currentColumn() string {
	cols := m.columns()
	if m.col < 0 || m.col >= len(cols) {
		return cols[0]
	}
	return cols[m.col]
}

func (m Model) currentID() string {
	return m.selected[m.currentColumn()]
}

func indexOf(items []app.TaskView, id string) int {
	for i, t := range items {
		if t.ID == id {
			return i
		}
	}
	return -1
}

// fixSelection keeps every column's selection on a shown task. follow is
// the task selected before a change: when it moved to another shown
// column, the board moves with it.
func (m *Model) fixSelection(follow string) {
	cols := m.columns()
	m.col = min(max(m.col, 0), len(cols)-1)
	if follow != "" {
		for i, c := range cols {
			if indexOf(m.items(c), follow) >= 0 {
				m.col = i
				m.selected[c] = follow
				break
			}
		}
	}
	for _, c := range cols {
		items := m.items(c)
		if len(items) == 0 {
			delete(m.selected, c)
			m.offset[c] = 0
			continue
		}
		i := indexOf(items, m.selected[c])
		if i < 0 {
			i = min(max(m.lastIndex[c], 0), len(items)-1)
		}
		m.selected[c] = items[i].ID
		m.lastIndex[c] = i
		m.scrollTo(c, i)
	}
}

func (m *Model) moveSelection(delta int) {
	c := m.currentColumn()
	items := m.items(c)
	if len(items) == 0 {
		return
	}
	i := indexOf(items, m.selected[c])
	if i < 0 {
		i = 0
	}
	i = min(max(i+delta, 0), len(items)-1)
	m.selected[c] = items[i].ID
	m.lastIndex[c] = i
	m.scrollTo(c, i)
}

func (m *Model) scrollTo(c string, i int) {
	n := m.cardsPerColumn()
	off := m.offset[c]
	if i < off {
		off = i
	}
	if i >= off+n {
		off = i - n + 1
	}
	m.offset[c] = max(off, 0)
}

func (m Model) cardsPerColumn() int {
	return max((m.height-headerHeight-footerHeight)/cardHeight, 1)
}

func (m Model) narrow() bool {
	return m.width < len(m.columns())*minColWidth
}

// View implements tea.Model.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "taskmoor"
	return v
}

func (m Model) render() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	var body string
	switch {
	case m.help:
		body = m.renderHelp()
	case m.detail != nil:
		body = m.renderDetail()
	default:
		body = m.renderBoard()
	}
	lines := strings.Split(body, "\n")
	if len(lines) > m.height-footerHeight {
		lines = lines[:m.height-footerHeight]
	}
	for len(lines) < m.height-footerHeight {
		lines = append(lines, "")
	}
	for i, l := range lines {
		lines[i] = fit(l, m.width)
	}
	lines = append(lines, fit(m.renderFooter(), m.width))
	return strings.Join(lines, "\n")
}

// fit truncates or pads a line to exactly w cells.
func fit(s string, w int) string {
	if ansi.StringWidth(s) > w {
		return ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", w-ansi.StringWidth(s))
}

var (
	dim          = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bold         = lipgloss.NewStyle().Bold(true)
	accent       = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	statusColors = map[string]string{
		"working": "12", "blocked": "11", "error": "9",
		"done": "10", "idle": "10", "unknown": "8", "exited": "8",
	}
)

func (m Model) title() string {
	r := m.rep
	head := r.MainHead
	if len(head) > 7 {
		head = head[:7]
	}
	n := 0
	for _, t := range r.Tasks {
		if t.Column != colIdeas { // ideas have a column, not a count among tasks
			n++
		}
	}
	parts := []string{bold.Render("taskmoor"), r.Project, r.MainBranch + " " + head, fmt.Sprintf("%d tasks", n)}
	switch r.HarnessSource {
	case harness.ReportGuild:
		parts = append(parts, "Guild "+lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("●"))
	case harness.ReportGuildUnavailable:
		parts = append(parts, "Guild "+dim.Render("○"))
	case "taskmoor serve":
		parts = append(parts, "serve "+lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("●"))
	}
	left := strings.Join(parts, " · ")
	var right []string
	if n := len(r.Findings); n > 0 {
		right = append(right, errStyle.Render(fmt.Sprintf("⚠ %d problems (?)", n)))
	}
	if m.readyOnly {
		right = append(right, "ready only")
	}
	if m.filter != "" || m.filtering {
		right = append(right, "filter: "+m.filter)
	}
	if len(right) == 0 {
		return left
	}
	rs := strings.Join(right, "  ")
	if ansi.StringWidth(rs) > m.width {
		rs = ansi.Truncate(rs, m.width, "…")
	}
	// the right side stays; the project facts give way
	room := m.width - ansi.StringWidth(rs) - 2
	if room < 1 {
		return rs
	}
	if ansi.StringWidth(left) > room {
		left = ansi.Truncate(left, room, "…")
	}
	return left + strings.Repeat(" ", m.width-ansi.StringWidth(left)-ansi.StringWidth(rs)) + rs
}

func (m Model) renderFooter() string {
	switch {
	case m.filtering:
		return "/" + m.filter + "█  enter keep  esc clear"
	case m.err != nil:
		return errStyle.Render("error: " + m.err.Error())
	case m.help:
		return "? esc close"
	case m.detail != nil:
		return dim.Render("↑↓ scroll  pgup/pgdown page  g/G top/bottom  R refresh  esc back  ? help")
	}
	keys := "←→ column  ↑↓ task  enter open  / filter  r ready only  c cancelled  ? help  q quit"
	if m.rep != nil && m.rep.Conveyor {
		keys = "←→ column  ↑↓ task  enter open  / filter  c cancelled  ? help  q quit  ·  run, answer: taskmoor web"
	}
	if m.rep != nil && m.rep.HarnessSource == harness.ReportGuildUnavailable {
		return dim.Render("Guild: unavailable  ·  " + keys)
	}
	return dim.Render(keys)
}

func (m Model) renderBoard() string {
	cols := m.columns()
	var b strings.Builder
	b.WriteString(m.title())
	b.WriteByte('\n')
	n := m.cardsPerColumn()
	if m.narrow() {
		b.WriteString(m.tabs(cols))
		b.WriteByte('\n')
		b.WriteString(strings.Join(m.renderColumn(m.currentColumn(), m.width, n, true), "\n"))
		return b.String()
	}
	gap := 1
	cw := (m.width - gap*(len(cols)-1)) / len(cols)
	var names []string
	columns := make([][]string, len(cols))
	for i, c := range cols {
		label := fmt.Sprintf(" %s %d", columnNames[c], len(m.items(c)))
		if more := m.scrollMarks(c, n); more != "" {
			label += " " + more
		}
		if i == m.col {
			label = accent.Render(label)
		}
		names = append(names, fit(label, cw))
		columns[i] = m.renderColumn(c, cw, n, i == m.col)
	}
	b.WriteString(strings.Join(names, strings.Repeat(" ", gap)))
	rows := 0
	for _, c := range columns {
		rows = max(rows, len(c))
	}
	for r := 0; r < rows; r++ {
		b.WriteByte('\n')
		parts := make([]string, len(columns))
		for i, c := range columns {
			line := ""
			if r < len(c) {
				line = c[r]
			}
			parts[i] = fit(line, cw)
		}
		b.WriteString(strings.Join(parts, strings.Repeat(" ", gap)))
	}
	return b.String()
}

// tabs renders the column names as tabs that fit the width, always
// including the active one; ‹ and › mark tabs cut off to either side.
func (m Model) tabs(cols []string) string {
	labels := make([]string, len(cols))
	for i, c := range cols {
		label := fmt.Sprintf("%s %d", columnNames[c], len(m.items(c)))
		if i == m.col {
			labels[i] = "[" + label + "]"
		} else {
			labels[i] = " " + label + " "
		}
	}
	width := func(lo, hi int) int { // lo..hi inclusive, with separators and marks
		w := 4
		for i := lo; i <= hi; i++ {
			w += ansi.StringWidth(labels[i]) + 1
		}
		return w
	}
	lo, hi := m.col, m.col
	for {
		grew := false
		if hi+1 < len(labels) && width(lo, hi+1) <= m.width {
			hi++
			grew = true
		}
		if lo > 0 && width(lo-1, hi) <= m.width {
			lo--
			grew = true
		}
		if !grew {
			break
		}
	}
	var parts []string
	if lo > 0 {
		parts = append(parts, "‹")
	}
	for i := lo; i <= hi; i++ {
		if i == m.col {
			parts = append(parts, accent.Render(labels[i]))
		} else {
			parts = append(parts, labels[i])
		}
	}
	if hi < len(labels)-1 {
		parts = append(parts, "›")
	}
	return strings.Join(parts, " ")
}

func (m Model) scrollMarks(c string, n int) string {
	total := len(m.items(c))
	off := m.offset[c]
	var s string
	if off > 0 {
		s += "↑"
	}
	if off+n < total {
		s += "↓"
	}
	return s
}

func (m Model) renderColumn(c string, w, n int, active bool) []string {
	items := m.items(c)
	off := m.offset[c]
	var lines []string
	if len(items) == 0 {
		return []string{dim.Render(" (none)")}
	}
	for i := off; i < len(items) && i < off+n; i++ {
		sel := items[i].ID == m.selected[c]
		lines = append(lines, card(items[i], w, sel && active, sel)...)
	}
	return lines
}

// card draws a task as a 6-line box w cells wide.
func card(t app.TaskView, w int, focused, selected bool) []string {
	inner := max(w-2, 4)
	tl, tr, bl, br, h, v := "┌", "┐", "└", "┘", "─", "│"
	if focused {
		tl, tr, bl, br, h, v = "┏", "┓", "┗", "┛", "━", "┃"
	} else if selected {
		tl, tr, bl, br = "╭", "╮", "╰", "╯"
	}
	marks := fmt.Sprintf("P%d", t.Priority)
	if t.Column == colIdeas {
		marks = t.Lifecycle // the idea's state
	}
	if t.Lifecycle == derive.Backlog {
		if t.Ready {
			marks += " ✓"
		} else {
			marks += " ·"
		}
	}
	if t.NeedsPerson {
		marks += " ⚑"
	}
	first := t.ID + "  " + marks
	t1, t2 := wrap2(text.Line(t.Title), inner)
	content := []string{bold.Render(first), t1, t2, statusLine(t, inner)}
	border := func(s string) string {
		if focused {
			return accent.Render(s)
		}
		return s
	}
	out := []string{border(tl + strings.Repeat(h, inner) + tr)}
	for _, c := range content {
		out = append(out, border(v)+fit(c, inner)+border(v))
	}
	out = append(out, border(bl+strings.Repeat(h, inner)+br))
	return out
}

// statusLine is a card's last line: the harness badge for a running task,
// the merge or release for a finished one, the first reason for an unready
// backlog task.
func statusLine(t app.TaskView, w int) string {
	var parts []string
	if t.Harness != nil {
		parts = append(parts, badge(*t.Harness))
	}
	if t.State != nil && t.State.Stale {
		parts = append(parts, errStyle.Render("stale"))
	}
	switch t.Lifecycle {
	case derive.Done:
		if t.ReleaseTag != "" {
			parts = append(parts, dim.Render(t.ReleaseTag))
		} else {
			parts = append(parts, dim.Render(shortHash(t.MergeCommit)))
		}
	case derive.Merged:
		parts = append(parts, dim.Render(shortHash(t.MergeCommit)))
	case derive.Backlog:
		if !t.Ready && len(t.Reasons) > 0 {
			parts = append(parts, dim.Render(text.Line(t.Reasons[0])))
		}
	}
	return ansi.Truncate(strings.Join(parts, " "), w, "…")
}

func badge(h harness.Record) string {
	s := "● " + h.Status
	if h.Kind != "" {
		s += " " + h.Kind
	}
	color := statusColors[h.Status]
	if color == "" {
		color = "8"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(s)
}

func shortHash(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

// wrap2 word-wraps s into at most two lines of w cells, the second cut
// with an ellipsis when s is longer.
func wrap2(s string, w int) (string, string) {
	words := strings.Fields(s)
	var lines []string
	cur := ""
	for i, word := range words {
		next := word
		if cur != "" {
			next = cur + " " + word
		}
		if ansi.StringWidth(next) <= w || cur == "" {
			cur = next
			continue
		}
		lines = append(lines, cur)
		if len(lines) == 1 {
			rest := strings.Join(words[i:], " ")
			return ansi.Truncate(lines[0], w, "…"), ansi.Truncate(rest, w, "…")
		}
	}
	return ansi.Truncate(cur, w, "…"), ""
}

func (m *Model) openDetail(id string, keepOffset bool) {
	t, ok := m.rep.Find(id)
	if !ok {
		return
	}
	h := max(m.height-2-footerHeight, 1)
	off := 0
	if keepOffset && m.detail != nil {
		off = m.detail.vp.YOffset()
	}
	vp := viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(h))
	vp.SetContent(m.facts(t) + "\n" + m.renderBody(text.Block(t.Task.Body)))
	vp.SetYOffset(off)
	m.detail = &detail{id: id, vp: vp, width: m.width}
}

func (m *Model) renderBody(body string) string {
	w := max(m.width-2, 20)
	r, ok := m.renderers[w]
	if !ok {
		var err error
		r, err = glamour.NewTermRenderer(glamour.WithStandardStyle(m.glamStyle), glamour.WithWordWrap(w))
		if err != nil {
			return body
		}
		m.renderers[w] = r
	}
	out, err := r.Render(body)
	if err != nil {
		return body
	}
	return out
}

func (m Model) facts(t app.TaskView) string {
	var b strings.Builder
	line := func(label, value string) {
		fmt.Fprintf(&b, "%s %s\n", dim.Render(fmt.Sprintf("%-13s", label)), value)
	}
	if t.Column == colIdeas {
		line("idea", t.Lifecycle+" — grill, review and approve it on the board or with taskmoor idea")
		if len(t.Touches) > 0 {
			line("tasks", text.Line(strings.Join(t.Touches, ", ")))
		}
		return b.String()
	}
	if len(t.DependsOn) > 0 {
		var ds []string
		for _, d := range t.DependsOn {
			lc := "unknown"
			if dv, ok := m.rep.Find(d); ok {
				lc = dv.Lifecycle
			}
			ds = append(ds, fmt.Sprintf("%s (%s)", d, lc))
		}
		line("depends_on", strings.Join(ds, ", "))
	}
	if t.Lifecycle == derive.Backlog {
		if t.Ready {
			line("ready", "yes")
		} else {
			line("ready", "no")
			for _, r := range t.Reasons {
				fmt.Fprintf(&b, "%s - %s\n", strings.Repeat(" ", 13), text.Line(r))
			}
		}
	}
	line("touches", text.Line(strings.Join(t.Touches, ", ")))
	if a := t.Task.AfterRelease; a != "" {
		line("after_release", a)
	}
	if t.Task.Profile != "" || t.Task.Model != "" {
		line("profile", text.Line(strings.TrimSpace(t.Task.Profile+" "+t.Task.Model)))
	}
	if s := t.State; s != nil {
		st := s.Lifecycle
		if s.Stale {
			st += " (stale)"
		}
		line("state", st)
		line("branch", text.Line(s.Branch))
		line("worktree", text.Line(s.Worktree))
		line("base", shortHash(s.Base))
		line("started", s.StartedAt)
	}
	if t.MergeCommit != "" {
		line("merge", shortHash(t.MergeCommit))
	}
	if t.ReleaseTag != "" {
		line("release", t.ReleaseTag)
	}
	return b.String()
}

func (m Model) renderDetail() string {
	t, ok := m.rep.Find(m.detail.id)
	if !ok {
		return ""
	}
	head := bold.Render(t.ID) + "  " + text.Line(t.Title)
	meta := []string{t.Lifecycle, fmt.Sprintf("P%d", t.Priority), t.Kind, t.Component}
	if t.NeedsPerson {
		meta = append(meta, "⚑ needs a person")
	}
	if t.Harness != nil {
		meta = append(meta, badge(*t.Harness))
	}
	return head + "\n" + dim.Render(strings.Join(meta, " · ")) + "\n" + m.detail.vp.View()
}

func (m Model) renderHelp() string {
	rows := [][3]string{
		{"key", "board", "detail"},
		{"←/h →/l", "column", "—"},
		{"↑/k ↓/j", "task", "scroll"},
		{"pgup pgdown", "page", "page"},
		{"g G", "first / last", "top / bottom"},
		{"enter", "open detail", "—"},
		{"esc", "clear filter", "back"},
		{"/", "filter: id, title, component", "—"},
		{"r", "backlog: ready only", "—"},
		{"c", "cancelled column", "—"},
		{"R", "refresh now", "refresh now"},
		{"?", "this help", "this help"},
		{"q ctrl+c", "quit", "back (ctrl+c quits)"},
	}
	var b strings.Builder
	b.WriteString(bold.Render("taskmoor — help") + "\n\n")
	for i, r := range rows {
		l := fmt.Sprintf("  %-13s %-30s %s", r[0], r[1], r[2])
		if i == 0 {
			l = dim.Render(l)
		}
		b.WriteString(l + "\n")
	}
	b.WriteString("\nCards: ✓ ready  · not ready  ⚑ needs a person  ● harness status  stale: the state file no longer counts\n")
	if r := m.rep; r != nil && len(r.Findings) > 0 {
		root := ""
		if r.Snapshot != nil {
			root = r.Snapshot.Root
		}
		fmt.Fprintf(&b, "\n%d problems in what the board read. To list them:\n  taskmoor --repo %s validate --source %s\n", len(r.Findings), shellQuote(root), r.Source)
		for i, f := range r.Findings {
			if i == 5 {
				fmt.Fprintf(&b, "  …\n")
				break
			}
			fmt.Fprintf(&b, "  %s\n", text.Line(f.String()))
		}
	}
	return b.String()
}

// shellQuote quotes a path for a command line shown to the person.
func shellQuote(p string) string {
	if p != "" && strings.IndexFunc(p, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) < 0 {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
