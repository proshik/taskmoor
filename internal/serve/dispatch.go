package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/proshik/taskmoor/internal/gitx"
)

// The dispatcher (C3, A13, A14) answers an agent's question without the
// person when the project's documents answer it (level a, with a
// citation) or when it is technical, reversible and has a recommended
// option (level b); everything else (level c) is the person's. It never
// answers an approval: those are the rules' and the person's.

// Ruling is one dispatcher decision, kept in <store>/decisions/.
type Ruling struct {
	ID        string     `json:"id"`
	At        string     `json:"at"`
	Project   string     `json:"project"`
	Task      string     `json:"task"`
	Session   string     `json:"session"`
	Ask       string     `json:"ask"`
	Questions []Question `json:"questions"`
	// Level is a, b or c.
	Level string `json:"level"`
	// Answer is the chosen option labels, one list per question (a, b).
	Answer   [][]string `json:"answer,omitempty"`
	Citation string     `json:"citation,omitempty"` // (a): file and quote
	Reason   string     `json:"reason"`
	// AnsweredBy: dispatcher (a, b) or person (c, once answered).
	AnsweredBy string    `json:"answered_by"`
	Overridden *Override `json:"overridden,omitempty"`
}

// Override is the person's later answer to a dispatcher decision, as sent
// to the session: with an ACP agent mid-turn it is queued for the next
// turn, and a session stopped before then loses it (override again).
type Override struct {
	At     string `json:"at"`
	Answer string `json:"answer"`
}

// DispatchTimeout bounds the dispatcher's call.
var DispatchTimeout = 2 * time.Minute

// guidanceFiles are read from the main branch, never the task's branch:
// an agent cannot write the documents that answer its own questions.
var guidanceFiles = []string{"AGENTS.md", "CLAUDE.md", "README.md"}

const (
	maxGuidanceFile  = 48 << 10
	maxGuidanceTotal = 192 << 10
)

// hardStop is A13's fixed rules for questions: what is always the person's,
// before any call.
var hardStop = regexp.MustCompile(`(?i)\b(push|publish|deploy|release|merg|delet|remov|drop|rm|secret|token|password|passphrase|credential|api[ -]?key|private key|dependenc|install|upgrade|licen[cs]e|migrat|billing|pay)`)

// skippable: an optional free-text field (codex-acp's own-answer note
// beside a choice) the dispatcher leaves empty.
func skippable(q Question) bool { return q.FreeText && q.Optional }

// decidable are the questions the dispatcher answers, by index.
func decidable(a *Ask) []int {
	var out []int
	for i, q := range a.Questions {
		if !skippable(q) {
			out = append(out, i)
		}
	}
	return out
}

func hardStopReason(a *Ask) string {
	if len(decidable(a)) == 0 {
		return "nothing to choose from"
	}
	for _, q := range a.Questions {
		if skippable(q) && !q.Secret {
			continue
		}
		switch {
		case q.Secret:
			return "a secret is asked for"
		case q.FreeText:
			return "the answer is free text"
		case q.MultiSelect:
			return "several options can be chosen"
		}
		texts := []string{q.Header, q.Question}
		for _, o := range q.Options {
			texts = append(texts, o.Label, o.Description)
		}
		joined := strings.Join(texts, " \n ")
		if m := hardStop.FindString(joined); m != "" {
			return fmt.Sprintf("it touches %q, which is always the person's (C3)", strings.ToLower(m))
		}
		// a secret's path or a dependency manifest named anywhere (A13)
		// split on everything a path name does not use: file=go.mod,
		// --config=.env, go.mod#L12, go.mod—x all leave the name alone
		for _, w := range strings.FieldsFunc(joined, func(r rune) bool {
			return !(r == '.' || r == '_' || r == '-' || r == '/' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
		}) {
			w = strings.Trim(w, ".-") // ".env" is checked again with its dot below
			if secretPath.MatchString("." + w) {
				w = "." + w
			}
			if secretPath.MatchString(w) || manifest.MatchString(w) {
				return fmt.Sprintf("it names %s, a secret's path or a dependency manifest (A13)", w)
			}
		}
	}
	return ""
}

// dispatch decides an ask that just opened. It runs on its own goroutine:
// the ask is in the inbox meanwhile and the person may answer first.
func (s *Service) dispatch(l *live, a *Ask) {
	if a.Kind != "question" {
		return // C3: never an approval
	}
	cfg, err := s.Config()
	if err != nil || cfg.DispatcherProfile == "" {
		return // the dispatcher is off: the person answers everything
	}
	d := &Ruling{ID: newID(), At: stamp(s.Now()), Project: a.Project, Task: a.Task, Session: a.Session, Ask: a.ID,
		Questions: a.Questions, Level: "c"}
	toPerson := func(reason string) {
		d.Reason = reason
		d.AnsweredBy = "person"
		s.mu.Lock()
		a.decision = d.ID // linked first: the person's answer completes the record
		s.mu.Unlock()
		_ = s.saveDecision(d)
		s.setDispatch(a, "for you: "+reason)
	}
	if r := hardStopReason(a); r != "" {
		toPerson(r)
		return
	}
	prof := cfg.Profile(cfg.DispatcherProfile)
	if prof == nil || prof.Harness != "claude" {
		toPerson(fmt.Sprintf("dispatcher_profile %q is not a configured claude profile", cfg.DispatcherProfile))
		return
	}
	if err := s.guard(*prof, true, false); err != nil { // the plan's usage
		toPerson("the dispatcher waits: " + err.Error())
		return
	}
	// managed Claude settings outrank the call's own: their hooks would see
	// the question and the documents, their key would bill
	if f, what := s.managedClaude(); f != "" && !(what == "apiKeyHelper" && prof.APIBilling) {
		toPerson(fmt.Sprintf("the managed Claude settings %s set %s, which the dispatcher's call would run under", f, what))
		return
	}
	s.setDispatch(a, "the dispatcher is looking")
	// a slot, unless the ask ends first
	tick := time.NewTicker(250 * time.Millisecond)
	for admitted := false; !admitted; {
		select {
		case s.dispatchSem <- struct{}{}:
			admitted = true
		case <-tick.C:
			if !s.askOpen(a) {
				tick.Stop()
				return
			}
		}
	}
	tick.Stop()
	defer func() { <-s.dispatchSem }()
	if err := s.guard(*prof, true, false); err != nil { // the plan, again, after the wait
		toPerson("the dispatcher waits: " + err.Error())
		return
	}
	docs := s.guidance(l)
	if !s.askOpen(a) {
		return // answered or withdrawn meanwhile: no call
	}
	ctx, cancel := context.WithTimeout(context.Background(), DispatchTimeout)
	defer cancel()
	go func() { // the call ends when the ask does
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !s.askOpen(a) {
					cancel()
					return
				}
			}
		}
	}()
	if err := s.guard(*prof, true, false); err != nil { // the plan, once more, right before the call
		toPerson("the dispatcher waits: " + err.Error())
		return
	}
	s.agentUp() // the dispatcher's call is an agent too (A19)
	reply, err := s.consult(ctx, *prof, dispatchPrompt(a, docs))
	s.agentDown()
	if !s.askOpen(a) {
		return // the person answered meanwhile
	}
	if err != nil {
		toPerson("the dispatcher's call failed: " + firstLine(err.Error(), 200))
		return
	}
	answers, citation, reason, err := checkReply(a, docs, reply)
	if err != nil {
		toPerson(err.Error())
		return
	}
	// the record first: no dispatcher answer reaches an agent unrecorded;
	// "pending" cannot be overridden until the answer is in
	d.Level, d.Answer, d.Citation, d.Reason, d.AnsweredBy = answers.level, answers.labels, citation, reason, "pending"
	if err := s.saveDecision(d); err != nil {
		s.setDispatch(a, "for you: the dispatcher's decision could not be recorded: "+err.Error())
		return
	}
	note := "answered by the dispatcher (" + d.Level + "): "
	if citation != "" {
		note += citation + "; "
	}
	note += reason
	err = s.answerAs(a.ID, Answer{Choices: answers.choices}, "dispatcher", note)
	var nf *NotFound
	var dl *deliveryError
	switch {
	case err == nil:
		d.AnsweredBy = "dispatcher"
		_ = s.saveDecision(d)
	case errors.As(err, &dl):
		d.AnsweredBy = "dispatcher"
		d.Reason += "; the answer did not reach the agent: " + dl.Error()
		_ = s.saveDecision(d)
	case errors.As(err, &nf):
		_ = s.removeDecision(d.ID) // the person answered first: theirs, not ours
	default:
		// refused before the ask was taken: it stays the person's
		d.Level, d.Answer, d.Citation, d.AnsweredBy = "c", nil, "", "person"
		d.Reason = "the dispatcher's answer was not accepted: " + err.Error()
		_ = s.saveDecision(d)
		s.setDispatch(a, "for you: "+d.Reason)
	}
}

// askOpen: the ask still waits for an answer.
func (s *Service) askOpen(a *Ask) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asks[a.ID] == a
}

// managedClaude names a managed Claude setting the dispatcher's call
// would run under and which: hooks anywhere first, then credentials (an
// apiKeyHelper, or an env block with a key or a provider switch); macOS
// managed preferences cannot be read here and count as unknown.
func (s *Service) managedClaude() (string, string) {
	var helper, helperFile string
	type file struct {
		name string
		m    map[string]json.RawMessage
	}
	var files []file
	for _, f := range managedSettingsFiles() {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(data, &m) != nil {
			return f, "settings taskmoor cannot read"
		}
		files = append(files, file{f, m})
	}
	for _, x := range files { // hooks in any file, first
		if h, ok := x.m["hooks"]; ok && len(h) > 2 {
			return x.name, "hooks"
		}
	}
	for _, x := range files {
		f, m := x.name, x.m
		var env map[string]string
		_ = json.Unmarshal(m["env"], &env)
		for k := range env {
			if apiKeyVars[k] {
				return f, "env." + k
			}
		}
		if _, ok := m["apiKeyHelper"]; ok && helper == "" {
			helper, helperFile = "apiKeyHelper", f
		}
	}
	for _, f := range managedPreferences {
		if _, err := os.Stat(f); err == nil {
			return f, "managed preferences"
		}
	}
	return helperFile, helper
}

// managedPreferences are macOS's managed Claude Code preferences.
var managedPreferences = []string{"/Library/Managed Preferences/com.anthropic.claudecode.plist"}

func (s *Service) setDispatch(a *Ask, status string) {
	s.mu.Lock()
	live := s.asks[a.ID] == a
	if live {
		a.Dispatch = status
	}
	cp := *a
	s.mu.Unlock()
	if live {
		s.bus.publish("ask", cp)
	}
}

type guidanceDoc struct {
	File string
	Text string
}

// guidance reads the project's documents from its main branch.
func (s *Service) guidance(l *live) []guidanceDoc {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	repo := gitx.Repo{Dir: l.rules.MainCheckout}
	branch := "refs/heads/" + l.rules.MainBranch // the branch, not a tag of its name
	files := append([]string(nil), guidanceFiles...)
	for _, dir := range []string{"docs", "spec"} {
		if entries, err := repo.LsTree(ctx, branch, dir); err == nil {
			for _, e := range entries {
				if e.Regular() && strings.HasSuffix(e.Path, ".md") {
					files = append(files, e.Path)
				}
			}
		}
	}
	// the task file, as agreed on the main branch
	if rep, err := l.app.Report(ctx); err == nil {
		if t, ok := rep.Find(l.s.Task); ok {
			files = append([]string{t.File}, files...)
		}
	}
	var out []guidanceDoc
	total := 0
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f] || total >= maxGuidanceTotal {
			continue
		}
		seen[f] = true
		data, ok, err := repo.ShowFile(ctx, branch, f)
		if err != nil || !ok {
			continue
		}
		if len(data) > maxGuidanceFile {
			data = data[:maxGuidanceFile]
		}
		if total+len(data) > maxGuidanceTotal {
			data = data[:maxGuidanceTotal-total]
		}
		total += len(data)
		out = append(out, guidanceDoc{File: f, Text: string(data)})
	}
	return out
}

func dispatchPrompt(a *Ask, docs []guidanceDoc) string {
	var b strings.Builder
	b.WriteString(`You are taskmoor's dispatcher. An agent working on a task asked the person the question(s) below. For each question decide whether it may be answered without the person:

(a) the answer is stated in one of the project documents below: give that option and cite the document: the file name exactly as given, and a short quote copied character for character from it that states the answer;
(b) the question is technical, reversible and inside the task's scope, and it has a recommended option: give the recommended option;
(c) anything else, and always: the task's scope, anything users see (UI, texts, behaviour), new dependencies, push, merge, publish or deploy, deletions, secrets, or when you are not sure.

Reply with JSON only, no other text, one entry per question in order:
{"questions": [{"level": "a" | "b" | "c", "option": "<an option label exactly as written>", "file": "<file, for a>", "quote": "<verbatim quote, for a>", "reason": "<one short sentence>"}]}

`)
	b.WriteString("QUESTIONS\n")
	for n, i := range decidable(a) {
		q := a.Questions[i]
		fmt.Fprintf(&b, "%d. ", n+1)
		if q.Header != "" {
			fmt.Fprintf(&b, "[%s] ", q.Header)
		}
		b.WriteString(q.Question + "\n")
		for j, o := range q.Options {
			fmt.Fprintf(&b, "   - %s", o.Label)
			if o.Description != "" {
				fmt.Fprintf(&b, " — %s", o.Description)
			}
			if j == q.Recommended {
				b.WriteString(" (recommended)")
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nDOCUMENTS (from the project's main branch; the first is the task)\n")
	for _, d := range docs {
		fmt.Fprintf(&b, "\n=== FILE: %s ===\n%s\n", d.File, d.Text)
	}
	return b.String()
}

type checkedAnswers struct {
	level   string     // a, or b if any question is b
	choices [][]string // 1-based option numbers: unambiguous (a label can be a number)
	labels  [][]string // the same options as labels, for the record
}

var spaces = regexp.MustCompile(`\s+`)

// negation: a quote that forbids (never, do not, avoid…) is not a
// citation the dispatcher may act on by itself.
var negation = regexp.MustCompile(`(?i)\b(not|never|no|don'?t|doesn'?t|avoid|forbid|forbidden|prohibit|instead|except|unless|without)\b|n't\b`)

func norm(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// checkReply validates the dispatcher's reply (A13, A14): well-formed, one
// answer per question, an existing option; (a) with a quote found in the
// named document; (b) only the recommended option. Any doubt is level c.
func checkReply(a *Ask, docs []guidanceDoc, reply string) (checkedAnswers, string, string, error) {
	var out checkedAnswers
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end < start {
		return out, "", "", errors.New("the dispatcher's reply was not JSON")
	}
	var r struct {
		Questions []struct {
			Level  string `json:"level"`
			Option string `json:"option"`
			File   string `json:"file"`
			Quote  string `json:"quote"`
			Reason string `json:"reason"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), &r); err != nil {
		return out, "", "", fmt.Errorf("the dispatcher's reply was not well-formed: %v", err)
	}
	idxs := decidable(a)
	if len(r.Questions) != len(idxs) {
		return out, "", "", fmt.Errorf("the dispatcher answered %d of %d questions", len(r.Questions), len(idxs))
	}
	byFile := map[string]string{}
	for _, d := range docs {
		byFile[d.File] = norm(d.Text)
	}
	out.level = "a"
	var cites, reasons []string
	out.choices = make([][]string, len(a.Questions)) // the skipped ones stay empty
	out.labels = make([][]string, len(a.Questions))
	for n, i := range idxs {
		q := a.Questions[i]
		ans := r.Questions[n]
		reason := firstLine(ans.Reason, 300)
		if ans.Level == "c" || (ans.Level != "a" && ans.Level != "b") {
			return out, "", "", fmt.Errorf("the dispatcher left %q to the person: %s", q.Question, reason)
		}
		idx := -1
		for j, o := range q.Options {
			if strings.EqualFold(strings.TrimSpace(ans.Option), o.Label) || strings.EqualFold(strings.TrimSpace(ans.Option), o.raw) {
				idx = j
			}
		}
		if idx < 0 {
			return out, "", "", fmt.Errorf("the dispatcher chose %q, which is not an option of %q", ans.Option, q.Question)
		}
		switch ans.Level {
		case "a":
			text, ok := byFile[ans.File]
			quote := norm(ans.Quote)
			if !ok || len(quote) < 12 || !strings.Contains(text, quote) {
				return out, "", "", fmt.Errorf("the dispatcher's citation for %q was not found in %q", q.Question, ans.File)
			}
			// the quote must state the chosen answer, not just exist, and
			// not as something to avoid
			if !strings.Contains(strings.ToLower(quote), strings.ToLower(norm(q.Options[idx].Label))) {
				return out, "", "", fmt.Errorf("the dispatcher's citation for %q does not name the answer %q", q.Question, q.Options[idx].Label)
			}
			if negation.MatchString(quote) {
				return out, "", "", fmt.Errorf("the dispatcher's citation for %q is worded as a rule against something; the person reads it", q.Question)
			}
			cites = append(cites, fmt.Sprintf("%s: “%s”", ans.File, firstLine(quote, 160)))
		case "b":
			if q.Recommended < 0 {
				return out, "", "", fmt.Errorf("%q has no recommended option, so only a citation answers it (A14)", q.Question)
			}
			if idx != q.Recommended {
				return out, "", "", fmt.Errorf("the dispatcher picked a non-recommended option for %q without a citation", q.Question)
			}
			out.level = "b"
		}
		out.choices[i] = []string{fmt.Sprint(idx + 1)}
		out.labels[i] = []string{q.Options[idx].Label}
		reasons = append(reasons, reason)
	}
	return out, strings.Join(cites, "; "), strings.Join(reasons, "; "), nil
}

// consult makes the dispatcher's one short call: `claude -p` of the
// dispatcher profile, no tools, no settings but its own, in an empty
// directory, the prompt on stdin. It refuses an API-billed reply the
// profile does not allow (A30).
func (s *Service) consult(ctx context.Context, prof Profile, prompt string) (string, error) {
	prog, err := programPath(prof)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "taskmoor-dispatch-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--tools", "", "--setting-sources=",
		"--strict-mcp-config", "--no-session-persistence"}
	if prof.Model != "" {
		args = append(args, "--model", prof.Model)
	}
	cmd := exec.CommandContext(ctx, prog, args...)
	cmd.Dir = dir
	cmd.Env = agentEnv(s.Env, prof.APIBilling)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 8 << 10}
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("no reply within %s", DispatchTimeout)
		}
		return "", fmt.Errorf("%v: %s", err, lastLines(stderr.String(), 2))
	}
	source, result := "", ""
	isErr := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var m streamMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch {
		case m.Type == "system" && m.Subtype == "init":
			source = m.APIKeySource
		case m.Type == "result":
			result, isErr = m.Result, m.IsError
		}
	}
	if source != "none" && !prof.APIBilling {
		return "", fmt.Errorf("the dispatcher's call would bill an API key (apiKeySource %q), which profile %s does not allow", source, prof.Name)
	}
	if isErr || result == "" {
		return "", fmt.Errorf("the dispatcher's call ended without an answer")
	}
	return result, nil
}

func (s *Service) saveDecision(d *Ruling) error {
	defer s.dropDecisionCache()
	dir := filepath.Join(s.Paths.Store, "decisions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, d.ID+".json"), d); err != nil {
		return err
	}
	s.bus.publish("decision", map[string]string{"id": d.ID, "level": d.Level, "task": d.Task})
	return nil
}

func (s *Service) dropDecisionCache() {
	s.decMu.Lock()
	s.decCache = nil
	s.decMu.Unlock()
}

func (s *Service) removeDecision(id string) error {
	defer s.dropDecisionCache()
	err := os.Remove(filepath.Join(s.Paths.Store, "decisions", filepath.Base(id)+".json"))
	s.bus.publish("decision", map[string]string{"id": id, "removed": "true"})
	return err
}

// personAnswered completes a level-c record with the person's answer.
func (s *Service) personAnswered(id, answer string) {
	d, err := s.decision(id)
	if err != nil {
		return
	}
	d.AnsweredBy = "person"
	d.Answer = [][]string{{answer}}
	_ = s.saveDecision(d)
}

// Decisions are the dispatcher's decisions, newest first (read once per
// change: the board asks on every refresh).
func (s *Service) Decisions(limit int) ([]Ruling, error) {
	s.decMu.Lock()
	cached := s.decCache
	s.decMu.Unlock()
	if cached == nil {
		all, err := s.readDecisions()
		if err != nil {
			return nil, err
		}
		s.decMu.Lock()
		s.decCache = all
		s.decMu.Unlock()
		cached = all
	}
	out := append([]Ruling(nil), cached...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Service) readDecisions() ([]Ruling, error) {
	des, err := os.ReadDir(filepath.Join(s.Paths.Store, "decisions"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Ruling{}, nil
		}
		return nil, err
	}
	out := []Ruling{}
	for _, de := range des {
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Paths.Store, "decisions", de.Name()))
		if err != nil {
			continue
		}
		var d Ruling
		if json.Unmarshal(data, &d) == nil {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At || out[i].At == out[j].At && out[i].ID > out[j].ID })
	return out, nil
}

func (s *Service) decision(id string) (*Ruling, error) {
	data, err := os.ReadFile(filepath.Join(s.Paths.Store, "decisions", filepath.Base(id)+".json"))
	if err != nil {
		return nil, &NotFound{Msg: "no decision " + id}
	}
	var d Ruling
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Override replaces a dispatcher answer with the person's: an instruction
// to the same session names the question, the dispatcher's answer and the
// person's, and the decision records it (C3). The agent may already have
// acted on the first answer; it is told to change course.
func (s *Service) Override(ctx context.Context, id string, ans Answer) (*Ruling, error) {
	s.overMu.Lock()
	defer s.overMu.Unlock()
	d, err := s.decision(id)
	if err != nil {
		return nil, err
	}
	if d.AnsweredBy != "dispatcher" {
		return nil, refusedf("decision %s was not the dispatcher's answer", id)
	}
	// a later override replaces an earlier one (an instruction can be lost
	// with a session that stops before its next turn): each is sent
	// options read back from the record: the label is what is matched
	for i := range d.Questions {
		for j := range d.Questions[i].Options {
			if o := &d.Questions[i].Options[j]; o.raw == "" {
				o.raw = o.Label
			}
		}
	}
	tmp := &Ask{Kind: "question", Questions: d.Questions}
	_, said, err := answerInput(tmp, ans)
	if err != nil {
		return nil, err
	}
	var was []string
	for _, c := range d.Answer {
		if len(c) > 0 {
			was = append(was, strings.Join(c, ", "))
		}
	}
	var qs []string
	for _, q := range d.Questions {
		qs = append(qs, q.Question)
	}
	msg := fmt.Sprintf("The person overrides the dispatcher. Your question: %s. The dispatcher answered: %s. The person's answer: %s. If you already acted on the dispatcher's answer, change course to the person's.",
		strings.Join(qs, " / "), strings.Join(was, "; "), said)
	prev := d.Overridden
	d.Overridden = &Override{At: stamp(s.Now()), Answer: said}
	if err := s.saveDecision(d); err != nil {
		return nil, fmt.Errorf("the override could not be recorded, nothing was sent: %w", err)
	}
	if err := s.say(ctx, d.Session, msg, false, true); err != nil { // the person's override is the confirmation
		d.Overridden = prev
		_ = s.saveDecision(d)
		return nil, err
	}
	return d, nil
}
