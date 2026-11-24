package serve

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

// A small reading of shell commands for the permission rules (stage 11):
// which simple commands run, with which words, and where their output
// goes. What is data — a heredoc's literal body fed to cat or tee, quoted
// text — is not taken for commands or paths; what runs — $(...), `...`,
// sh -c '...', eval, a heredoc or here-string a shell reads, the
// substitutions in an unquoted heredoc — is read as commands too. It is
// best effort and never alone: the rules also judge the raw text with the
// data taken out, and keep the stricter verdict (policy.go).

// shellStmt is one simple command.
type shellStmt struct {
	words    []string // as the shell passes them (quotes removed)
	writes   []string // output redirections' targets
	reads    []string // input redirections' sources
	bodies   []hereBody
	herestr  []string // <<< operands
	pipeNext bool     // its output goes to the next one
	depth    int      // ( ) subshells it is in
}

type hereBody struct {
	text       string
	quoted     bool // a quoted delimiter: no expansion
	stdin      bool // on descriptor 0 (not <<3)
	start, end int  // its runes in the command
}

// cmdIndex is the index of the command word: after assignments and the
// wrappers that run their arguments.
func (st shellStmt) cmdIndex() int {
	i := 0
	for i < len(st.words) {
		w := st.words[i]
		switch {
		case assignRe.MatchString(w):
			i++
		case wrappers[filepath.Base(w)]:
			i++
			for i < len(st.words) && strings.HasPrefix(st.words[i], "-") {
				i++ // the wrapper's own options
			}
		default:
			return i
		}
	}
	return len(st.words)
}

// wrappers run their arguments; the shell's keywords put a command after
// them (if, while, !) — the command word is the one after.
var wrappers = map[string]bool{"env": true, "sudo": true, "nohup": true, "time": true, "command": true, "exec": true, "nice": true, "doas": true, "builtin": true,
	"if": true, "then": true, "else": true, "elif": true, "while": true, "until": true, "do": true, "!": true}

// cmd is the command word's base name ("" for a bare assignment).
func (st shellStmt) cmd() string {
	i := st.cmdIndex()
	if i >= len(st.words) {
		return ""
	}
	return filepath.Base(st.words[i])
}

// args are the command's words after the command word.
func (st shellStmt) args() []string {
	i := st.cmdIndex()
	if i+1 > len(st.words) {
		return nil
	}
	return st.words[i+1:]
}

// text is the command from its command word, named by its base name, for
// the deny patterns.
func (st shellStmt) text() string {
	if st.cmd() == "" {
		return ""
	}
	return strings.Join(append([]string{st.cmd()}, st.args()...), " ")
}

var (
	assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	shells   = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "eval": true, "source": true, ".": true}
)

var errShell = errors.New("the command cannot be read")

// shellScript is what a shell invocation runs: its -c string (ok), or,
// with no -c and no script file, its standard input (stdin). An option
// it cannot place makes the command unreadable.
func shellScript(st shellStmt) (script string, stdin bool, err error) {
	switch st.cmd() {
	case "eval":
		return strings.Join(st.args(), " "), false, nil
	case "source", ".":
		return "", false, nil // a file: not read here
	}
	a := st.args()
	command := false
	for i := 0; i < len(a); i++ {
		w := a[i]
		switch {
		case w == "--":
			if command && i+1 < len(a) {
				return a[i+1], false, nil
			}
			return "", i+1 >= len(a), nil
		case strings.HasPrefix(w, "--"):
			if w == "--rcfile" || w == "--init-file" {
				i++ // its argument
			}
		case strings.HasPrefix(w, "-") || strings.HasPrefix(w, "+"):
			opts := w[1:]
			if strings.ContainsAny(opts, "c") {
				command = true
			}
			if strings.ContainsAny(opts, "s") && !command {
				return "", true, nil // -s: the program is stdin, the rest are its arguments
			}
			if strings.ContainsAny(opts, "oO") { // -o NAME
				i++
			}
		default:
			if command {
				return w, false, nil
			}
			return "", false, nil // a script file
		}
	}
	if command {
		return "", false, errShell // -c with no string
	}
	return "", true, nil
}

// parseShell reads a command into its simple commands, those it runs
// inside included (to depth 4).
func parseShell(c string) ([]shellStmt, error) { return parseShellDepth(c, 0) }

func parseShellDepth(c string, depth int) ([]shellStmt, error) {
	if depth > 4 {
		return nil, errShell
	}
	p := &shellLexer{src: []rune(c)}
	if err := p.run(); err != nil {
		return nil, err
	}
	// each statement in order, with what runs inside it next to it (in a
	// child scope: a cd there stays there, and it starts where the parent
	// is)
	child := func(code string, parent shellStmt) ([]shellStmt, error) {
		sub, err := parseShellDepth(code, depth+1)
		if err != nil {
			return nil, err
		}
		for k := range sub {
			sub[k].depth += parent.depth + 1
		}
		return sub, nil
	}
	var out []shellStmt
	for i, st := range p.stmts {
		var inside []string // substitutions: they run before the command
		for k, owner := range p.nestOf {
			if owner == i {
				inside = append(inside, p.nested[k])
			}
		}
		var after []string // what the command itself runs
		if len(st.bodies) > 0 || len(st.herestr) > 0 {
			readsAsScript, err := p.readsScript(i)
			if err != nil {
				return nil, err
			}
			for _, b := range st.bodies {
				if !b.quoted {
					// expanded by the shell that feeds it: its substitutions
					// run, whoever reads the text
					subs, err := substitutions(b.text)
					if err != nil {
						return nil, err
					}
					inside = append(inside, subs...)
				}
				if readsAsScript && b.stdin {
					after = append(after, b.text)
				}
			}
			if readsAsScript {
				after = append(after, st.herestr...)
			}
		}
		if shells[st.cmd()] {
			script, _, err := shellScript(st)
			if err != nil {
				return nil, err
			}
			if script != "" {
				after = append(after, script)
			}
		}
		for _, code := range inside {
			sub, err := child(code, st)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		out = append(out, st)
		for _, code := range after {
			sub, err := child(code, st)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
	}
	for k, owner := range p.nestOf { // a substitution outside any statement
		if owner >= len(p.stmts) {
			sub, err := parseShellDepth(p.nested[k], depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
	}
	return out, nil
}

// readsScript says whether statement i's standard input is a program: it
// is a shell reading its program from stdin, or its output is piped into
// one that does.
func (p *shellLexer) readsScript(i int) (bool, error) {
	for j := i; j < len(p.stmts); j++ {
		if shells[p.stmts[j].cmd()] && p.stmts[j].cmd() != "eval" {
			_, stdin, err := shellScript(p.stmts[j])
			if err != nil {
				return false, err
			}
			if stdin {
				return true, nil
			}
		}
		if !p.stmts[j].pipeNext {
			break
		}
	}
	return false, nil
}

// substitutions are the $(...) and `...` of an unquoted heredoc's body.
func substitutions(body string) ([]string, error) {
	src := []rune(body)
	var out []string
	for i := 0; i < len(src); i++ {
		switch {
		case src[i] == '\\':
			i++
		case src[i] == '$' && i+1 < len(src) && src[i+1] == '(' && (i+2 >= len(src) || src[i+2] != '('):
			p := &shellLexer{src: src}
			s, end, err := p.balanced(i + 2)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
			i = end - 1
		case src[i] == '`':
			end := i + 1
			for end < len(src) && src[end] != '`' {
				end++
			}
			if end >= len(src) {
				return nil, errShell
			}
			out = append(out, string(src[i+1:end]))
			i = end
		}
	}
	return out, nil
}

type shellLexer struct {
	src     []rune
	i       int
	depth   int
	stmts   []shellStmt
	cur     shellStmt
	word    strings.Builder
	inWord  bool
	nested  []string // $(...), `...`, <(...)
	nestOf  []int    // the statement each was in (its index once it ends)
	pending []heredoc
}

type heredoc struct {
	delim  string
	dash   bool
	quoted bool
	stdin  bool
	owner  int // the statement it belongs to (-1 until it ends)
}

// addNested keeps a command that runs inside the current statement.
func (p *shellLexer) addNested(c string) {
	p.nested = append(p.nested, c)
	p.nestOf = append(p.nestOf, len(p.stmts)) // the current one's index once it ends
}

func (p *shellLexer) peek(k int) rune {
	if p.i+k < len(p.src) {
		return p.src[p.i+k]
	}
	return 0
}

func (p *shellLexer) endWord() {
	if p.inWord {
		p.cur.words = append(p.cur.words, p.word.String())
		p.word.Reset()
		p.inWord = false
	}
}

func (p *shellLexer) endStmt() {
	p.endWord()
	owned := false
	for _, h := range p.pending {
		if h.owner < 0 {
			owned = true
		}
	}
	if len(p.cur.words) > 0 || len(p.cur.writes) > 0 || len(p.cur.reads) > 0 || len(p.cur.herestr) > 0 || owned {
		p.cur.depth = p.depth
		p.stmts = append(p.stmts, p.cur)
		for k := range p.pending {
			if p.pending[k].owner < 0 {
				p.pending[k].owner = len(p.stmts) - 1
			}
		}
	}
	p.cur = shellStmt{}
}

func (p *shellLexer) run() error {
	for p.i < len(p.src) {
		r := p.src[p.i]
		switch {
		case r == '\\':
			if p.i+1 < len(p.src) {
				if p.src[p.i+1] != '\n' { // a line continuation joins
					p.word.WriteRune(p.src[p.i+1])
					p.inWord = true
				}
				p.i += 2
				continue
			}
			p.i++
		case r == '\'':
			if err := p.single(); err != nil {
				return err
			}
		case r == '"':
			if err := p.double(); err != nil {
				return err
			}
		case r == '`':
			end := p.i + 1
			for end < len(p.src) && p.src[end] != '`' {
				if p.src[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(p.src) {
				return errShell
			}
			p.addNested(string(p.src[p.i+1 : end]))
			p.word.WriteString("$CMD")
			p.inWord = true
			p.i = end + 1
		case r == '$' && p.peek(1) == '(' && p.peek(2) != '(':
			body, end, err := p.balanced(p.i + 2)
			if err != nil {
				return err
			}
			p.addNested(body)
			p.word.WriteString("$CMD")
			p.inWord = true
			p.i = end
		case r == '#' && !p.inWord:
			for p.i < len(p.src) && p.src[p.i] != '\n' {
				p.i++
			}
		case r == '\n':
			p.endStmt()
			p.i++
			if err := p.bodies(); err != nil {
				return err
			}
		case r == '(' && !p.inWord:
			p.endStmt()
			p.depth++
			p.i++
		case r == ')':
			p.endStmt()
			if p.depth > 0 {
				p.depth--
			}
			p.i++
		case r == ';' || r == '(' || r == '{' && !p.inWord || r == '}' && !p.inWord:
			p.endStmt()
			p.i++
		case r == '|':
			if p.peek(1) == '|' { // ||
				p.endStmt()
				p.i += 2
				continue
			}
			p.cur.pipeNext = true // | and |&
			p.endStmt()
			p.i++
			if p.peek(0) == '&' {
				p.i++
			}
		case r == '&':
			if p.peek(1) == '>' { // &> and &>>: both outputs to a file
				p.endWord()
				p.i += 2
				if p.peek(0) == '>' {
					p.i++
				}
				t, _, err := p.target()
				if err != nil {
					return err
				}
				p.cur.writes = append(p.cur.writes, t)
				continue
			}
			p.endStmt()
			p.i++
			if p.peek(0) == '&' {
				p.i++
			}
		case r == '>' || r == '<':
			if err := p.redirect(); err != nil {
				return err
			}
		case r == ' ' || r == '\t' || r == '\r':
			p.endWord()
			p.i++
		default:
			p.word.WriteRune(r)
			p.inWord = true
			p.i++
		}
	}
	p.endStmt()
	if len(p.pending) > 0 { // a heredoc whose body never came
		return errShell
	}
	return nil
}

// single reads a single-quoted string: all of it is literal.
func (p *shellLexer) single() error {
	end := p.i + 1
	for end < len(p.src) && p.src[end] != '\'' {
		end++
	}
	if end >= len(p.src) {
		return errShell
	}
	p.word.WriteString(string(p.src[p.i+1 : end]))
	p.inWord = true
	p.i = end + 1
	return nil
}

// double reads a double-quoted string: $(...) and `...` inside run.
func (p *shellLexer) double() error {
	p.i++
	for p.i < len(p.src) {
		r := p.src[p.i]
		switch {
		case r == '"':
			p.i++
			p.inWord = true
			return nil
		case r == '\\' && p.i+1 < len(p.src):
			p.word.WriteRune(p.src[p.i+1])
			p.i += 2
		case r == '$' && p.peek(1) == '(' && p.peek(2) != '(':
			body, end, err := p.balanced(p.i + 2)
			if err != nil {
				return err
			}
			p.addNested(body)
			p.word.WriteString("$CMD")
			p.i = end
		case r == '`':
			end := p.i + 1
			for end < len(p.src) && p.src[end] != '`' {
				end++
			}
			if end >= len(p.src) {
				return errShell
			}
			p.addNested(string(p.src[p.i+1 : end]))
			p.word.WriteString("$CMD")
			p.i = end + 1
		default:
			p.word.WriteRune(r)
			p.i++
		}
	}
	return errShell
}

// balanced reads to the ")" that closes a "$(" (quotes respected).
func (p *shellLexer) balanced(from int) (string, int, error) {
	depth := 1
	for j := from; j < len(p.src); j++ {
		switch p.src[j] {
		case '\\':
			j++
		case '\'':
			k := j + 1
			for k < len(p.src) && p.src[k] != '\'' {
				k++
			}
			j = k
		case '"':
			k := j + 1
			for k < len(p.src) && p.src[k] != '"' {
				if p.src[k] == '\\' {
					k++
				}
				k++
			}
			j = k
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return string(p.src[from:j]), j + 1, nil
			}
		}
	}
	return "", 0, errShell
}

// target reads the word after a redirection operator; quoted says whether
// any of it was quoted or escaped.
func (p *shellLexer) target() (string, bool, error) {
	for p.i < len(p.src) && (p.src[p.i] == ' ' || p.src[p.i] == '\t') {
		p.i++
	}
	save := p.cur.words
	p.cur.words = nil
	start, quoted := p.i, false
	for p.i < len(p.src) {
		r := p.src[p.i]
		if r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '&' || r == '|' || r == '(' || r == ')' || r == '<' || r == '>' {
			break
		}
		switch r {
		case '\'':
			quoted = true
			if err := p.single(); err != nil {
				return "", false, err
			}
			continue
		case '"':
			quoted = true
			if err := p.double(); err != nil {
				return "", false, err
			}
			continue
		case '\\':
			quoted = true
			if p.i+1 < len(p.src) {
				p.word.WriteRune(p.src[p.i+1])
				p.i += 2
				continue
			}
		}
		p.word.WriteRune(r)
		p.i++
	}
	t := p.word.String()
	p.word.Reset()
	p.inWord = false
	p.cur.words = save
	if p.i == start {
		return "", false, errShell
	}
	return t, quoted, nil
}

// redirect reads >, >>, >|, >&, <, <<, <<-, <<<, <& and what follows.
func (p *shellLexer) redirect() error {
	// a word of digits just before is a descriptor number, not a word
	fd := ""
	if p.inWord && isDigits(p.word.String()) {
		fd = p.word.String()
		p.word.Reset()
		p.inWord = false
	}
	p.endWord()
	if p.src[p.i] == '>' {
		p.i++
		if p.peek(0) == '>' || p.peek(0) == '|' {
			p.i++
		}
		if p.peek(0) == '&' { // >&2: a descriptor; >&file is a file
			p.i++
			t, _, err := p.target()
			if err != nil {
				return err
			}
			if !isDigits(strings.TrimSuffix(t, "-")) && t != "-" {
				p.cur.writes = append(p.cur.writes, t)
			}
			return nil
		}
		t, _, err := p.target()
		if err != nil {
			return err
		}
		p.cur.writes = append(p.cur.writes, t)
		return nil
	}
	// <
	p.i++
	switch {
	case p.peek(0) == '<' && p.peek(1) == '<': // <<< here-string
		p.i += 2
		t, _, err := p.target()
		if err != nil {
			return err
		}
		p.cur.herestr = append(p.cur.herestr, t)
		return nil
	case p.peek(0) == '<': // heredoc
		p.i++
		dash := false
		if p.peek(0) == '-' {
			dash = true
			p.i++
		}
		d, quoted, err := p.target()
		if err != nil {
			return err
		}
		p.pending = append(p.pending, heredoc{delim: d, dash: dash, quoted: quoted, stdin: fd == "" || fd == "0", owner: -1})
		return nil
	case p.peek(0) == '&':
		p.i++
		_, _, err := p.target()
		return err
	case p.peek(0) == '(': // <(...): a process substitution runs
		body, end, err := p.balanced(p.i + 1)
		if err != nil {
			return err
		}
		p.addNested(body)
		p.i = end
		return nil
	}
	t, _, err := p.target()
	if err != nil {
		return err
	}
	p.cur.reads = append(p.cur.reads, t)
	return nil
}

// bodies reads the heredocs' bodies that start at this line.
func (p *shellLexer) bodies() error {
	for len(p.pending) > 0 && p.pending[0].owner >= 0 {
		h := p.pending[0]
		p.pending = p.pending[1:]
		var body strings.Builder
		found := false
		start := p.i
		bodyEnd := p.i // after the body's last line
		for p.i < len(p.src) {
			eol := p.i
			for eol < len(p.src) && p.src[eol] != '\n' {
				eol++
			}
			line := string(p.src[p.i:eol])
			p.i = eol
			if p.i < len(p.src) {
				p.i++ // the newline
			}
			check := line
			if h.dash {
				check = strings.TrimLeft(line, "\t")
			}
			if check == h.delim {
				found = true
				break
			}
			body.WriteString(line)
			body.WriteString("\n")
			bodyEnd = p.i
		}
		if !found {
			return errShell
		}
		p.stmts[h.owner].bodies = append(p.stmts[h.owner].bodies, hereBody{text: body.String(), quoted: h.quoted, stdin: h.stdin, start: start, end: bodyEnd})
	}
	return nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// dataStripped is the command with what is surely data taken out, for
// the rules that read the raw text (policy.go): the body of a heredoc
// that runs nothing (a quoted delimiter, or no substitution in it), read
// by cat or tee and piped nowhere — a file written from text. Everything else stays. It is
// the command itself when it cannot be read.
func dataStripped(c string) string {
	p := &shellLexer{src: []rune(c)}
	if p.run() != nil {
		return c
	}
	type span struct{ start, end int }
	var cut []span
	for _, st := range p.stmts {
		if st.pipeNext || (st.cmd() != "cat" && st.cmd() != "tee") {
			continue
		}
		for _, b := range st.bodies {
			// literal, or expanding only variables (no command runs)
			literal := b.quoted || !strings.Contains(b.text, "$(") && !strings.Contains(b.text, "`")
			if literal && b.stdin {
				cut = append(cut, span{b.start, b.end})
			}
		}
	}
	if len(cut) == 0 {
		return c
	}
	var out strings.Builder
	at := 0
	for _, sp := range cut { // in source order: the bodies come as read
		out.WriteString(string(p.src[at:sp.start]))
		at = sp.end
	}
	out.WriteString(string(p.src[at:]))
	return out.String()
}
