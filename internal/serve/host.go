package serve

import (
	"fmt"
	"strings"
	"sync"
)

// sessionHost connects one agent runner to the service. Its callbacks act
// for that runner only: a later runner of the same session has its own.
type sessionHost struct {
	s *Service
	l *live
	r agentRunner
}

func (h *sessionHost) bind(r agentRunner) {
	h.r = r
	h.s.mu.Lock()
	h.l.runner = r
	h.l.stopping = false
	h.s.mu.Unlock()
}

func (h *sessionHost) runner() agentRunner { return h.r }

func (h *sessionHost) initialized(apiKeySource, model, permissionMode string) {
	if permissionMode == "bypassPermissions" {
		// nothing would reach the host: taskmoor's rules would not apply
		h.r.kill()
		h.s.addActivity(h.l, Activity{Type: ActError, Text: "Claude runs in bypassPermissions mode (a managed or forced setting), so taskmoor's rules would not apply; stopped"})
		h.s.setState(h.l, StateError, "Claude runs in bypassPermissions mode")
		return
	}
	billing := "subscription"
	if apiKeySource != "none" {
		billing = "api"
	}
	h.s.mu.Lock()
	h.l.s.Billing = billing
	if model != "" && h.l.s.Model == "" {
		h.l.s.Model = model
	}
	allowed := h.l.profile.APIBilling
	h.s.mu.Unlock()
	if billing == "api" && !allowed {
		// A30: the badge must be subscription unless the profile allows API
		// billing. init comes after the first message (Claude sends nothing
		// before it), so the process is killed at once, before its turn
		// gets far; the preflight checks keep this case rare.
		h.r.kill()
		h.s.addActivity(h.l, Activity{Type: ActError, Text: fmt.Sprintf("Claude reports apiKeySource %q: this session would bill an API key, which profile %s does not allow; stopped", apiKeySource, h.l.profile.Name)})
		h.s.setState(h.l, StateError, "API billing not allowed by the profile")
		return
	}
	h.s.setState(h.l, StateActive, "")
}

func (h *sessionHost) activity(a Activity) { h.s.addActivity(h.l, a) }

func (h *sessionHost) permission(requestID, tool string, input map[string]any) {
	r := h.runner()
	if r == nil {
		return
	}
	if tool == "AskUserQuestion" {
		qs := normalizeQuestions(input)
		if len(qs) == 0 {
			_ = r.deny(requestID, "The question could not be read; ask again with questions[] and options[].")
			return
		}
		a := &Ask{Kind: "question", Questions: qs, Tool: tool, requestID: requestID, input: input}
		h.s.openAsk(h.l, a)
		go h.s.dispatch(h.l, a) // C3: (a) and (b) without the person
		return
	}
	if strings.HasPrefix(tool, acpAsk) {
		// an ACP request the rules cannot judge (more sandbox, a network
		// destination, another kind): always the person's
		h.s.openAsk(h.l, &Ask{Kind: "approval", Tool: tool, Detail: detail(tool, input),
			Reason: "the agent asks for something taskmoor's rules do not judge", requestID: requestID, input: input})
		return
	}
	v := h.l.rules.Decide(tool, input)
	if paths, ok := input["paths"].([]string); ok && len(paths) > 1 {
		v = h.l.rules.decideAll(tool, paths) // one decision covers every changed file
	}
	switch v.Decision {
	case Allow:
		_ = r.allow(requestID, input)
	case Deny:
		_ = r.deny(requestID, v.Reason+".")
		h.s.addActivity(h.l, Activity{Type: ActResponse, Tool: tool, Text: "refused by the rules: " + v.Reason, By: "rules"})
	default:
		h.s.openAsk(h.l, &Ask{Kind: "approval", Tool: tool, Detail: detail(tool, input), Reason: v.Reason, requestID: requestID, input: input})
	}
}

func (h *sessionHost) cancelled(requestID string) {
	h.s.mu.Lock()
	aid := h.l.pending[requestID]
	a := h.s.asks[aid]
	h.s.mu.Unlock()
	if a != nil && h.s.take(a) {
		h.s.resolve(h.l, a, "withdrawn by the agent", "agent")
	}
}

func (h *sessionHost) result(res claudeResult) {
	h.s.mu.Lock()
	h.l.s.CostUSD += res.CostUSD
	stopping := h.l.stopping
	h.s.mu.Unlock()
	if stopping {
		return // exited() records the stop
	}
	if res.IsError {
		text := res.Text
		if text == "" {
			text = res.Subtype
		}
		h.r.closeInput()
		h.s.addActivity(h.l, Activity{Type: ActError, Text: firstLine(text, 400)})
		h.s.setState(h.l, StateError, firstLine(text, 200))
		return
	}
	// one conversation per run: the input closes before the session says it
	// is complete, so an instruction from then on resumes instead of being
	// written to a process that is ending
	h.r.closeInput()
	h.s.addActivity(h.l, Activity{Type: ActResult, Text: firstLine(res.Text, 2000)})
	h.s.complete(h.l) // complete and handed over (review) in one write
}

func (h *sessionHost) exited(code int, err error) {
	defer h.s.runners.Done()
	h.s.mu.Lock()
	if h.l.runner != h.r {
		h.s.mu.Unlock()
		return // a newer runner owns the session
	}
	// the slot stays this runner's until its last write below is done, so
	// a resume cannot start a successor that this exit would then overwrite
	defer func() {
		h.s.mu.Lock()
		if h.l.runner == h.r {
			h.l.runner = nil
		}
		h.s.mu.Unlock()
		h.s.publishSession(h.l)
	}()
	c := code // -1: ended by a signal
	h.l.s.ExitCode = &c
	st := h.l.s.State
	stopping := h.l.stopping
	var open []*Ask
	for _, aid := range h.l.pending {
		if a := h.s.asks[aid]; a != nil {
			open = append(open, a)
		}
	}
	h.s.mu.Unlock()
	for _, a := range open {
		if h.s.take(a) {
			h.s.resolve(h.l, a, "withdrawn: the session ended", "agent")
		}
	}
	switch {
	case st == StateComplete || st == StateError:
		// the turn had already ended (a stop or shutdown that arrives while
		// the process exits does not undo that)
		h.s.setState(h.l, st, "")
	case stopping:
		h.s.addActivity(h.l, Activity{Type: ActResponse, Text: "stopped by the person", By: "person"})
		h.s.setState(h.l, StateStopped, "")
	default:
		msg := fmt.Sprintf("the claude process ended (exit %d) before the turn finished", code)
		if err != nil {
			msg += ": " + err.Error()
		}
		h.s.addActivity(h.l, Activity{Type: ActError, Text: firstLine(msg, 400)})
		h.s.setState(h.l, StateError, firstLine(msg, 200))
	}
}

// detail is what an approval shows: the whole command, or the file and the
// size of what is written, so the person approves what will run.
func detail(tool string, input map[string]any) string {
	str := func(k string) string { v, _ := input[k].(string); return v }
	switch tool {
	case "Bash":
		return str("command")
	case "Write":
		return fmt.Sprintf("%s (%d bytes)", str("file_path"), len(str("content")))
	case "Edit", "MultiEdit":
		if paths, ok := input["paths"].([]string); ok && len(paths) > 1 {
			return strings.Join(paths, "\n") // every file the change touches
		}
		return str("file_path")
	case "NotebookEdit":
		return str("notebook_path")
	}
	raw, _ := jsonRaw(input)
	return firstLine(string(raw), 2000)
}

// normalizeQuestions turns AskUserQuestion's input into questions with
// the recommended option marked (F9: the option labelled "(Recommended)").
func normalizeQuestions(input map[string]any) []Question {
	raw, _ := input["questions"].([]any)
	var out []Question
	for _, rq := range raw {
		m, ok := rq.(map[string]any)
		if !ok {
			continue
		}
		q := Question{Recommended: -1, Options: []Option{}}
		q.Question, _ = m["question"].(string)
		q.Header, _ = m["header"].(string)
		q.MultiSelect, _ = m["multiSelect"].(bool)
		opts, _ := m["options"].([]any)
		for _, ro := range opts {
			om, ok := ro.(map[string]any)
			if !ok {
				continue
			}
			label, _ := om["label"].(string)
			desc, _ := om["description"].(string)
			o := Option{Label: label, Description: desc, raw: label}
			if l, ok := cutRecommended(label); ok {
				o.Label = l
				if q.Recommended < 0 {
					q.Recommended = len(q.Options)
				}
			}
			q.Options = append(q.Options, o)
		}
		free, _ := m["freeText"].(bool) // an ACP form field without options
		q.FreeText = free
		q.Optional, _ = m["optional"].(bool)
		q.Secret, _ = m["secret"].(bool)
		if q.Question != "" && (len(q.Options) > 0 || free) {
			out = append(out, q)
		}
	}
	return out
}

func cutRecommended(label string) (string, bool) {
	t := strings.TrimSpace(label)
	for _, suf := range []string{"(Recommended)", "(recommended)"} {
		if strings.HasSuffix(t, suf) {
			return strings.TrimSpace(strings.TrimSuffix(t, suf)), true
		}
	}
	return label, false
}

// bus numbers every change (epoch, rev) and fans it out to subscribers
// (C5's protocol: a new epoch means the service restarted).
type bus struct {
	mu    sync.Mutex
	epoch string
	rev   int64
	subs  map[chan Event]bool
}

// Event is one change.
type Event struct {
	Epoch string `json:"epoch"`
	Rev   int64  `json:"rev"`
	Type  string `json:"type"`
	Data  any    `json:"data"`
}

func newBus() *bus { return &bus{epoch: newID(), subs: map[chan Event]bool{}} }

func (b *bus) position() (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.epoch, b.rev
}

func (b *bus) publish(typ string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rev++
	ev := Event{Epoch: b.epoch, Rev: b.rev, Type: typ, Data: data}
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // a subscriber that cannot keep up takes a new snapshot
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// closeAll ends every subscription (the service is stopping): streams
// send their clients a reset and end.
func (b *bus) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}

func (b *bus) subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	b.subs[ch] = true
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if b.subs[ch] {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}
