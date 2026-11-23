package serve

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The Telegram bot (stage 7, C6, A9, A12, A20). The service polls
// Telegram from the Mac (outbound only); only the person's account, bound
// by a one-time pairing code, is heard. A simple question (one, single
// choice, not secret) gets option buttons, the recommended one first;
// anything else, and every approval, gets the question and the link to the
// board on this Mac, where it is answered (A9: dangerous approvals are
// answered on the Mac only). An ask still open after the reminder interval
// is sent again (A20); nothing is answered on the person's behalf.

// TelegramAPI is the Bot API's base (tests point it at a fake).
var TelegramAPI = "https://api.telegram.org"

// TelegramRemind is how long an ask waits before a reminder.
var TelegramRemind = 30 * time.Minute

// TelegramRetry is how long a failed send waits for another try.
var TelegramRetry = time.Minute

const pairTTL = 10 * time.Minute

// telegramFile is the bot's settings in the store (mode 0600).
type telegramFile struct {
	Token  string `json:"token"`
	Bot    string `json:"bot,omitempty"`     // its @username
	UserID int64  `json:"user_id,omitempty"` // the person's account
	ChatID int64  `json:"chat_id,omitempty"`
	Offset int64  `json:"offset,omitempty"` // the next update to read
	// OffsetAt is when Offset was saved: Telegram may restart its update
	// ids after a week without updates, so an older offset is dropped.
	OffsetAt string `json:"offset_at,omitempty"`
}

// TelegramStatus is what clients show.
type TelegramStatus struct {
	Configured bool   `json:"configured"`
	Bot        string `json:"bot,omitempty"`
	Paired     bool   `json:"paired"`
	Error      string `json:"error,omitempty"`
}

// telegram is the bot's state, behind mu. The settings live in memory
// with the file as their copy. Every change of who is heard — setup, off,
// a pairing — starts a new generation: it cancels the requests of the
// last one (a poll, a send, a callback's answer) and forgets what was sent
// to it, and nothing a request of an older generation brings back
// answers, pairs, records or saves.
type telegram struct {
	mu        sync.Mutex
	wg        sync.WaitGroup // the bot's goroutines, joined when serve ends
	root      context.Context
	loaded    bool
	cfg       *telegramFile // nil: no bot
	gen       int
	genCtx    context.Context
	genCancel context.CancelFunc
	port      int // the board's, as listened on
	pairCode  string
	pairExp   time.Time
	sent      map[string]tgMessage // ask id → its latest message
	reminded  map[string]time.Time // ask id → when it was last sent
	sending   map[string]int       // ask id → the generation of a send in flight
	tried     map[string]time.Time // ask id → a failed send, retried after TelegramRetry
	closing   map[string]tgMessage // asks resolved whose message is not edited yet
	evtAt     map[string]time.Time // ask id → its last ask_reminder event
	pollErr   string
	sendErr   string
	callbacks chan struct{} // bounds the answers in flight
}

type tgMessage struct {
	chat, id int64
	text     string
}

func newTelegram() telegram {
	return telegram{sent: map[string]tgMessage{}, reminded: map[string]time.Time{}, sending: map[string]int{},
		tried: map[string]time.Time{}, closing: map[string]tgMessage{}, evtAt: map[string]time.Time{},
		callbacks: make(chan struct{}, 4)}
}

func (s *Service) telegramPath() string { return filepath.Join(s.Paths.Store, "telegram.json") }

// tgLoad reads the settings once; tg.mu is held.
func (s *Service) tgLoad() error {
	if s.tg.loaded {
		return nil
	}
	data, err := os.ReadFile(s.telegramPath())
	if errors.Is(err, os.ErrNotExist) {
		s.tg.loaded = true
		return nil
	}
	if err != nil {
		return err
	}
	var f telegramFile
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	s.tg.cfg, s.tg.loaded = &f, true
	return nil
}

// tgBump starts a new generation; tg.mu is held.
func (s *Service) tgBump() {
	if s.tg.genCancel != nil {
		s.tg.genCancel()
	}
	root := s.tg.root
	if root == nil {
		root = context.Background()
	}
	s.tg.gen++
	s.tg.genCtx, s.tg.genCancel = context.WithCancel(root)
	s.tg.pollErr, s.tg.sendErr = "", ""
	// the messages went to another chat (or bot): this one starts afresh
	s.tg.sent, s.tg.reminded = map[string]tgMessage{}, map[string]time.Time{}
	s.tg.tried, s.tg.closing = map[string]time.Time{}, map[string]tgMessage{}
}

// tgConf is a copy of the settings (nil: no bot), their generation and its
// context.
func (s *Service) tgConf() (*telegramFile, int, context.Context, error) {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	if err := s.tgLoad(); err != nil {
		return nil, 0, nil, err
	}
	if s.tg.genCtx == nil {
		s.tgBump()
	}
	if s.tg.cfg == nil {
		return nil, s.tg.gen, s.tg.genCtx, nil
	}
	cp := *s.tg.cfg
	return &cp, s.tg.gen, s.tg.genCtx, nil
}

// tgReplace puts in new settings (nil: none); tg.mu is held.
func (s *Service) tgReplace(f *telegramFile) error {
	var err error
	if f == nil {
		err = os.Remove(s.telegramPath())
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		err = writeJSONFile(s.telegramPath(), f)
	}
	if err != nil {
		return err
	}
	s.tg.cfg, s.tg.loaded = f, true
	s.tg.pairCode = ""
	s.tgBump()
	return nil
}

// tgHeard says whether gen is current and user its paired person.
func (s *Service) tgHeard(gen int, user int64) bool {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	return s.tg.gen == gen && s.tg.cfg != nil && s.tg.cfg.UserID != 0 && s.tg.cfg.UserID == user
}

// tgClient carries the bot token in its URLs: no redirects, HTTP/1.1 only
// (Go's HTTP/2 debug log prints the path).
var tgClient = &http.Client{
	Timeout:       40 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy:        http.ProxyFromEnvironment,
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	},
}

// tgCall calls a Bot API method; the token never appears in an error.
func tgCall(ctx context.Context, token, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TelegramAPI+"/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return errors.New("telegram: a bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := tgClient.Do(req)
	if err != nil {
		return errors.New("telegram " + method + ": " + strings.ReplaceAll(err.Error(), token, "•••"))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if json.Unmarshal(data, &r) != nil || !r.OK {
		desc := r.Description
		if desc == "" {
			desc = resp.Status
		}
		return &tgError{code: resp.StatusCode, msg: fmt.Sprintf("telegram %s: %s", method, strings.ReplaceAll(desc, token, "•••"))}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// tgError is Telegram's own refusal (not a network failure).
type tgError struct {
	code int
	msg  string
}

func (e *tgError) Error() string { return e.msg }

// permanent: asking again gets the same answer (a 4xx but 429).
func permanent(err error) bool {
	var te *tgError
	return errors.As(err, &te) && te.code >= 400 && te.code < 500 && te.code != 429
}

// TelegramSetup checks a bot token with getMe and keeps it (the pairing
// starts over).
func (s *Service) TelegramSetup(ctx context.Context, token string) (*TelegramStatus, error) {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " /\n\t") {
		return nil, refusedf("that is not a bot token (BotFather gives one like 123456:ABC…)")
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := tgCall(ctx, token, "getMe", map[string]any{}, &me); err != nil {
		return nil, refusedf("Telegram does not take this token: %v", err)
	}
	s.tg.mu.Lock()
	err := s.tgReplace(&telegramFile{Token: token, Bot: me.Username})
	s.tg.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.bus.publish("telegram", map[string]bool{"configured": true})
	return s.TelegramStatus(), nil
}

// TelegramPair makes a one-time code for the person to send the bot.
func (s *Service) TelegramPair() (string, string, error) {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	if err := s.tgLoad(); err != nil {
		return "", "", err
	}
	if s.tg.cfg == nil {
		return "", "", refusedf("no bot yet: taskmoor telegram setup first")
	}
	code := newID()
	s.tg.pairCode, s.tg.pairExp = code, s.Now().Add(pairTTL)
	return code, s.tg.cfg.Bot, nil
}

// TelegramOff forgets the bot.
func (s *Service) TelegramOff() error {
	s.tg.mu.Lock()
	err := s.tgReplace(nil)
	s.tg.mu.Unlock()
	if err == nil {
		s.bus.publish("telegram", map[string]bool{"configured": false})
	}
	return err
}

// TelegramStatus says whether the bot is set up and paired.
func (s *Service) TelegramStatus() *TelegramStatus {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	st := &TelegramStatus{}
	if err := s.tgLoad(); err != nil {
		st.Error = err.Error()
		return st
	}
	if f := s.tg.cfg; f != nil {
		st.Configured, st.Bot, st.Paired = true, f.Bot, f.UserID != 0
	}
	st.Error = s.tg.pollErr
	if st.Error == "" {
		st.Error = s.tg.sendErr
	}
	return st
}

// startTelegram runs the bot until ctx ends; stopTelegram waits for it.
func (s *Service) startTelegram(ctx context.Context, port int) {
	s.tg.mu.Lock()
	s.tg.root, s.tg.port = ctx, port
	s.tgBump()
	s.tg.mu.Unlock()
	s.tgGo(func() { s.followAsks(ctx) })
	s.tgGo(func() { s.remindLoop(ctx) })
	s.tgGo(func() { s.pollTelegram(ctx) })
}

func (s *Service) stopTelegram() { s.tg.wg.Wait() }

func (s *Service) tgGo(f func()) {
	s.tg.wg.Add(1)
	go func() {
		defer s.tg.wg.Done()
		f()
	}()
}

// followAsks follows the event bus: subscribed first, then what is already
// open is reconciled, so nothing falls between; a subscriber that falls
// behind is dropped by the bus and subscribes again.
func (s *Service) followAsks(ctx context.Context) {
	for ctx.Err() == nil {
		events, unsub := s.bus.subscribe()
		s.sendOpenAsks(ctx, false)
	recv:
		for {
			select {
			case <-ctx.Done():
				break recv
			case ev, ok := <-events:
				if !ok {
					break recv
				}
				s.telegramEvent(ev)
			}
		}
		unsub()
	}
}

// pollTelegram reads the bot's updates (long polling, outbound); it idles
// while no bot is set up.
func (s *Service) pollTelegram(ctx context.Context) {
	for ctx.Err() == nil {
		f, gen, gctx, err := s.tgConf()
		if err != nil || f == nil {
			sleepCtx(ctx, 2*time.Second)
			continue
		}
		offset := f.Offset
		if at, err := time.Parse(time.RFC3339, f.OffsetAt); err != nil || s.Now().Sub(at) > 6*24*time.Hour {
			offset = 0 // the ids may have started over
		}
		pctx, cancel := context.WithTimeout(gctx, 35*time.Second) // a new generation cancels it
		var ups []tgUpdate
		err = tgCall(pctx, f.Token, "getUpdates", map[string]any{"offset": offset, "timeout": 25,
			"allowed_updates": []string{"message", "callback_query"}}, &ups)
		cancel()
		s.tg.mu.Lock()
		stale := s.tg.gen != gen
		if !stale {
			s.tg.pollErr = ""
			if err != nil {
				s.tg.pollErr = err.Error()
			}
		}
		s.tg.mu.Unlock()
		if stale {
			continue // setup, off or a pairing came meanwhile: this batch is not theirs
		}
		if err != nil {
			sleepCtx(ctx, 10*time.Second)
			continue
		}
		for _, u := range ups {
			gen = s.telegramUpdate(gen, u) // a pairing starts a new one
		}
		if len(ups) == 0 {
			continue
		}
		next := ups[len(ups)-1].UpdateID + 1 // from the ids Telegram returned
		if err := s.tgSaveOffset(gen, next); err != nil {
			s.tg.mu.Lock()
			s.tg.pollErr = "saving the update offset: " + err.Error()
			s.tg.mu.Unlock()
			sleepCtx(ctx, 10*time.Second)
		}
	}
}

// tgSaveOffset keeps the next offset: in memory at once (the poll goes on
// from it), on disk if it can.
func (s *Service) tgSaveOffset(gen int, next int64) error {
	s.tg.mu.Lock()
	defer s.tg.mu.Unlock()
	if s.tg.gen != gen || s.tg.cfg == nil {
		return nil
	}
	s.tg.cfg.Offset, s.tg.cfg.OffsetAt = next, stamp(s.Now())
	return writeJSONFile(s.telegramPath(), s.tg.cfg)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

type tgUser struct {
	ID int64 `json:"id"`
}

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		From      tgUser `json:"from"`
		Chat      struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
	CallbackQuery *struct {
		ID   string `json:"id"`
		From tgUser `json:"from"`
		Data string `json:"data"`
	} `json:"callback_query"`
}

// telegramUpdate handles one update of generation gen's poll and says the
// generation after it; anyone but the paired person is ignored, without an
// answer.
func (s *Service) telegramUpdate(gen int, u tgUpdate) int {
	if m := u.Message; m != nil {
		if m.Chat.Type != "private" {
			return gen
		}
		code, ok := strings.CutPrefix(strings.TrimSpace(m.Text), "/start ")
		if !ok {
			return gen // nothing else is a command
		}
		s.tg.mu.Lock()
		want, exp := s.tg.pairCode, s.tg.pairExp
		valid := s.tg.gen == gen && s.tg.cfg != nil && want != "" && s.Now().Before(exp) &&
			subtle.ConstantTimeCompare([]byte(strings.TrimSpace(code)), []byte(want)) == 1
		if !valid {
			s.tg.mu.Unlock()
			return gen
		}
		s.tg.pairCode = "" // one time
		cp := *s.tg.cfg
		cp.UserID, cp.ChatID = m.From.ID, m.Chat.ID
		if writeJSONFile(s.telegramPath(), &cp) != nil {
			s.tg.mu.Unlock()
			return gen
		}
		s.tg.cfg = &cp
		s.tgBump() // a new person: the old one's requests end
		gen, gctx, token := s.tg.gen, s.tg.genCtx, cp.Token
		s.tg.mu.Unlock()
		s.bus.publish("telegram", map[string]bool{"paired": true})
		// off the poll: the catch-up can be long
		s.tgGo(func() {
			_ = tgCall(gctx, token, "sendMessage", map[string]any{"chat_id": m.Chat.ID,
				"text": "taskmoor: paired. Agents' questions come here; simple ones have buttons, the rest a link to the board on your Mac."}, nil)
			s.sendOpenAsks(gctx, false) // what waited for the pairing
		})
		return gen
	}
	cq := u.CallbackQuery
	if cq == nil || !s.tgHeard(gen, cq.From.ID) {
		return gen // not the person's account (or not this pairing's): ignored
	}
	f, _, gctx, err := s.tgConf()
	if err != nil || f == nil {
		return gen
	}
	answer := func(text string) {
		_ = tgCall(gctx, f.Token, "answerCallbackQuery", map[string]any{"callback_query_id": cq.ID, "text": clip(text, 190)}, nil)
	}
	parts := strings.Split(cq.Data, "|")
	n, err := 0, error(nil)
	if len(parts) == 3 && parts[0] == "a" {
		n, err = strconv.Atoi(parts[2])
	}
	if len(parts) != 3 || parts[0] != "a" || err != nil {
		answer("taskmoor does not know this button")
		return gen
	}
	s.mu.Lock()
	a := s.asks[parts[1]]
	var q Question
	ok := a != nil && a.opened && simpleAsk(a) && n >= 0 && n < len(a.Questions[0].Options)
	if ok {
		q = a.Questions[0]
	}
	s.mu.Unlock()
	if !ok {
		answer("this question is answered or withdrawn")
		return gen
	}
	// the answer goes to the runner off the poll: a runner slow to read
	// holds this one answer, not every update
	select {
	case s.tg.callbacks <- struct{}{}:
	default:
		answer("taskmoor is busy delivering answers; press again in a moment")
		return gen
	}
	from := cq.From.ID
	s.tgGo(func() {
		defer func() { <-s.tg.callbacks }()
		// still heard as the ask is taken: an off or a new pairing since
		// the press leaves it open
		err := s.answerIf(a.ID, Answer{Choices: [][]string{{strconv.Itoa(n + 1)}}}, "person", "answered in Telegram",
			func() bool { return s.tgHeard(gen, from) })
		if err != nil {
			answer("not answered: " + err.Error())
			return
		}
		answer("answered: " + q.Options[n].Label)
	})
	return gen
}

// The budget of a question with buttons: all of it is shown, nothing cut.
const (
	tgQuestionMax = 1000
	tgChoiceMax   = 250 // a label and its description
)

// simpleAsk is A12: one question, single choice, options to press, no
// previews, not secret, a question (never an approval), and short enough
// to show every choice in full.
func simpleAsk(a *Ask) bool {
	if a.Kind != "question" || len(a.Questions) == 0 {
		return false
	}
	// the first question is the one to press; another may only be
	// codex-acp's own-answer field beside it
	for _, x := range a.Questions[1:] {
		if !(x.Note && x.FreeText && x.Optional && !x.Secret) {
			return false
		}
	}
	q := a.Questions[0]
	if len(q.Options) == 0 || len(q.Options) > 8 || q.MultiSelect || q.Secret || q.FreeText ||
		len(q.Question)+len(q.Header) > tgQuestionMax {
		return false
	}
	for _, o := range q.Options {
		if o.Preview != "" || len(o.Label)+len(o.Description) > tgChoiceMax {
			return false
		}
	}
	return true
}

// forPerson says whether an ask waits for the person: approvals always;
// a question once the dispatcher, as decided when it opened, has left it.
func forPerson(a *Ask) bool {
	return a.Kind != "question" || a.Dispatch == "" || strings.HasPrefix(a.Dispatch, "for you")
}

// telegramEvent sends an ask that waits for the person, and closes the
// message of one that no longer does.
func (s *Service) telegramEvent(ev Event) {
	switch ev.Type {
	case "ask":
		if a, ok := ev.Data.(Ask); ok && forPerson(&a) {
			s.sendAsk(&a, false)
		}
	case "ask_resolved":
		if m, ok := ev.Data.(map[string]string); ok {
			s.closeMessage(m["id"])
		}
	}
}

// closeMessage marks an ask's message answered; a failed edit is tried
// again by the next pass.
func (s *Service) closeMessage(id string) {
	s.tg.mu.Lock()
	if msg, ok := s.tg.sent[id]; ok {
		s.tg.closing[id] = msg
	}
	delete(s.tg.sent, id)
	delete(s.tg.reminded, id)
	delete(s.tg.tried, id)
	delete(s.tg.evtAt, id)
	msg, ok := s.tg.closing[id]
	gen, gctx := s.tg.gen, s.tg.genCtx
	var token string
	if s.tg.cfg != nil {
		token = s.tg.cfg.Token
	}
	s.tg.mu.Unlock()
	if !ok || token == "" {
		return
	}
	err := tgCall(gctx, token, "editMessageText", map[string]any{"chat_id": msg.chat, "message_id": msg.id,
		"text": clip(msg.text, 3500) + "\n\n✓ answered"}, nil)
	if err == nil || permanent(err) { // done, or never will be (deleted, unchanged)
		s.tg.mu.Lock()
		if s.tg.gen == gen {
			delete(s.tg.closing, id)
		}
		s.tg.mu.Unlock()
	}
}

// sendAsk sends an ask to the paired person: the first time (once), or
// again as a reminder.
func (s *Service) sendAsk(a *Ask, reminder bool) {
	f, gen, gctx, err := s.tgConf()
	if err != nil || f == nil || f.ChatID == 0 {
		return
	}
	s.tg.mu.Lock()
	_, already := s.tg.sent[a.ID]
	inflight, busy := s.tg.sending[a.ID]
	if (busy && inflight == gen) || (already && !reminder) || s.tg.gen != gen {
		s.tg.mu.Unlock()
		return
	}
	s.tg.sending[a.ID] = gen
	port := s.tg.port
	s.tg.mu.Unlock()
	defer func() {
		s.tg.mu.Lock()
		if s.tg.sending[a.ID] == gen {
			delete(s.tg.sending, a.ID)
		}
		s.tg.mu.Unlock()
	}()
	text, markup := askMessage(a, fmt.Sprintf("http://127.0.0.1:%d/", port), reminder)
	params := map[string]any{"chat_id": f.ChatID, "text": text}
	if markup != nil {
		params["reply_markup"] = markup
	}
	var sent struct {
		MessageID int64 `json:"message_id"`
	}
	// gctx ends with this pairing: an off or a new pairing cancels the send
	if err := tgCall(gctx, f.Token, "sendMessage", params, &sent); err != nil {
		s.tg.mu.Lock()
		if s.tg.gen == gen {
			s.tg.sendErr = err.Error()
			s.tg.tried[a.ID] = s.Now()
		}
		s.tg.mu.Unlock()
		return
	}
	s.tg.mu.Lock()
	if s.tg.gen != gen {
		s.tg.mu.Unlock()
		return
	}
	s.tg.sendErr = ""
	delete(s.tg.tried, a.ID)
	old, hadOld := s.tg.sent[a.ID]
	s.tg.sent[a.ID] = tgMessage{chat: f.ChatID, id: sent.MessageID, text: text}
	s.tg.reminded[a.ID] = s.Now()
	s.tg.mu.Unlock()
	if hadOld {
		// the reminder carries the buttons now: the old message says so
		_ = tgCall(gctx, f.Token, "editMessageText", map[string]any{"chat_id": old.chat, "message_id": old.id,
			"text": clip(old.text, 3500) + "\n\n(see the reminder below)"}, nil)
	}
	// answered while this was on its way: its resolution found nothing
	// to close, so this closes it
	s.mu.Lock()
	_, open := s.asks[a.ID]
	s.mu.Unlock()
	if !open {
		s.closeMessage(a.ID)
	}
}

// askMessage is an ask's text and buttons (nil: none).
func askMessage(a *Ask, board string, reminder bool) (string, any) {
	var b strings.Builder
	if reminder {
		b.WriteString("Still waiting for you.\n")
	}
	fmt.Fprintf(&b, "%s · %s\n", a.Project, a.Task)
	if a.Kind != "question" {
		// nothing of the request itself: a command or a tool's input can
		// hold a secret, and it is answered on the Mac only (A9)
		return b.String() + "An approval waits for you. Answer it on the board on your Mac: " + board, nil
	}
	tail := "\n\nAnswer on the board on your Mac: " + board
	if !simpleAsk(a) {
		for i, q := range a.Questions {
			if q.Secret {
				fmt.Fprintf(&b, "%d. (a secret: only on the board)\n", i+1)
				continue
			}
			fmt.Fprintf(&b, "%d. %s\n", i+1, q.Question)
		}
		return clip(b.String(), 3500) + tail, nil
	}
	q := a.Questions[0] // simpleAsk: it all fits, nothing is cut
	if q.Header != "" {
		fmt.Fprintf(&b, "[%s] ", q.Header)
	}
	b.WriteString(q.Question + "\n")
	order := make([]int, 0, len(q.Options))
	if q.Recommended >= 0 && q.Recommended < len(q.Options) {
		order = append(order, q.Recommended)
	}
	for i := range q.Options {
		if i != q.Recommended {
			order = append(order, i)
		}
	}
	// the full choices in the text, numbered as their buttons: two that
	// begin alike stay apart
	var rows [][]map[string]string
	for k, i := range order {
		o := q.Options[i]
		star := ""
		if i == q.Recommended {
			star = "★ "
		}
		fmt.Fprintf(&b, "\n%d. %s%s", k+1, star, o.Label)
		if o.Description != "" {
			fmt.Fprintf(&b, " — %s", o.Description)
		}
		rows = append(rows, []map[string]string{{"text": fmt.Sprintf("%d. %s%s", k+1, star, clip(o.Label, 50)),
			"callback_data": fmt.Sprintf("a|%s|%d", a.ID, i)}})
	}
	return b.String() + "\n\n(★ recommended) Or answer on the board: " + board,
		map[string]any{"inline_keyboard": rows}
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// sendOpenAsks reconciles: it sends what waits for the person and was not
// sent yet (a failed send after TelegramRetry), closes the messages of
// asks no longer open (an edit that failed, an event missed), and, on the
// minute's pass, reminds (A20).
func (s *Service) sendOpenAsks(ctx context.Context, reminders bool) {
	s.mu.Lock()
	var open []Ask
	isOpen := map[string]bool{}
	for _, id := range s.order {
		if a := s.asks[id]; a != nil && a.opened {
			isOpen[id] = true
			if forPerson(a) {
				open = append(open, *a)
			}
		}
	}
	s.mu.Unlock()
	now := s.Now()
	s.tg.mu.Lock()
	var stale []string
	for id := range s.tg.sent {
		if _, busy := s.tg.sending[id]; !isOpen[id] && !busy {
			stale = append(stale, id)
		}
	}
	for id := range s.tg.closing {
		stale = append(stale, id)
	}
	for id := range s.tg.evtAt {
		if !isOpen[id] {
			delete(s.tg.evtAt, id)
		}
	}
	s.tg.mu.Unlock()
	for _, id := range stale {
		if ctx.Err() != nil {
			return
		}
		s.closeMessage(id)
	}
	for i := range open {
		if ctx.Err() != nil {
			return
		}
		a := &open[i]
		s.tg.mu.Lock()
		last, sent := s.tg.reminded[a.ID]
		tried, failed := s.tg.tried[a.ID]
		// the clients' reminder runs on the ask's own clock, sent or not
		evt, had := s.tg.evtAt[a.ID]
		if !had {
			evt, _ = time.Parse(time.RFC3339, a.CreatedAt)
			s.tg.evtAt[a.ID] = evt
		}
		dueEvt := reminders && now.Sub(evt) >= TelegramRemind
		if dueEvt {
			s.tg.evtAt[a.ID] = now
		}
		s.tg.mu.Unlock()
		if dueEvt {
			s.bus.publish("ask_reminder", map[string]string{"id": a.ID, "task": a.Task})
		}
		switch {
		case !sent && (!failed || !reminders || now.Sub(tried) >= TelegramRetry):
			s.sendAsk(a, false)
		case sent && reminders && now.Sub(last) >= TelegramRemind:
			s.sendAsk(a, true)
		}
	}
}

// remindLoop retries and reminds (A20) once a minute.
func (s *Service) remindLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.sendOpenAsks(ctx, true)
	}
}
