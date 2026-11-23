package serve_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/serve"
)

// fakeTG is the Bot API for tests: it queues updates and records calls.
type fakeTG struct {
	mu       sync.Mutex
	updates  []map[string]any
	nextID   int64
	calls    []tgCall
	msgID    int64
	failSend int           // the next sendMessage calls to fail
	failEdit int           // the next editMessageText calls to fail (a 502)
	hold     chan struct{} // non-nil: getUpdates waits for it (or the caller to give up)
	polls    int
}

type tgCall struct {
	Method string
	Params map[string]any
}

func (f *fakeTG) push(u map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	u["update_id"] = f.nextID
	f.updates = append(f.updates, u)
}

func (f *fakeTG) called(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c.Params)
		}
	}
	return out
}

func (f *fakeTG) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/") // /bot<token>/<method>
	if len(parts) != 3 || parts[1] != "bot123:secret" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Unauthorized"})
		return
	}
	method := parts[2]
	var params map[string]any
	_ = json.NewDecoder(r.Body).Decode(&params)
	f.mu.Lock()
	f.calls = append(f.calls, tgCall{method, params})
	var result any = true
	switch method {
	case "getMe":
		result = map[string]any{"username": "moor_bot"}
	case "getUpdates":
		f.polls++
		if h := f.hold; h != nil {
			f.mu.Unlock()
			select {
			case <-h:
			case <-r.Context().Done():
				return
			}
			f.mu.Lock()
		}
		off, _ := params["offset"].(float64)
		var out []map[string]any
		for _, u := range f.updates {
			if float64(u["update_id"].(int64)) >= off {
				out = append(out, u)
			}
		}
		f.mu.Unlock()
		if len(out) == 0 {
			time.Sleep(100 * time.Millisecond) // a short long poll
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": out})
		return
	case "editMessageText":
		if f.failEdit > 0 {
			f.failEdit--
			f.mu.Unlock()
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Bad Gateway"})
			return
		}
	case "sendMessage":
		if f.failSend > 0 {
			f.failSend--
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Too Many Requests"})
			return
		}
		f.msgID++
		result = map[string]any{"message_id": f.msgID}
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func startTG(t *testing.T, rg *rig) *fakeTG {
	t.Helper()
	return pairTG(t, rg, &fakeTG{})
}

func pairTG(t *testing.T, rg *rig, f *fakeTG) *fakeTG {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	old := serve.TelegramAPI
	serve.TelegramAPI = srv.URL
	t.Cleanup(func() { rg.stop(); serve.TelegramAPI = old }) // the service ends before the setting goes back
	ctx := context.Background()
	if _, err := rg.client.TelegramSetup(ctx, "bad token"); err == nil {
		t.Fatal("a bad token was taken")
	}
	st, err := rg.client.TelegramSetup(ctx, "123:secret")
	if err != nil || st.Bot != "moor_bot" || st.Paired {
		t.Fatalf("%+v %v", st, err)
	}
	code, _, err := rg.client.TelegramPair(ctx)
	if err != nil {
		t.Fatal(err)
	}
	msg := func(from int64, text string) map[string]any {
		return map[string]any{"message": map[string]any{"message_id": 1, "from": map[string]any{"id": from},
			"chat": map[string]any{"id": from, "type": "private"}, "text": text}}
	}
	f.push(msg(99, "/start wrong"))
	f.push(msg(42, "/start "+code))
	eventually(t, "paired", func() bool { st, _ := rg.client.TelegramStatus(ctx); return st.Paired })
	f.push(msg(77, "/start "+code)) // the code is spent
	return f
}

func callback(f *fakeTG, from int64, data string) {
	f.push(map[string]any{"callback_query": map[string]any{"id": "cb", "from": map[string]any{"id": from}, "data": data,
		"message": map[string]any{"message_id": 1, "chat": map[string]any{"id": from}}}})
}

func TestTelegramQuestionWithButtons(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "question", nil)
	f := startTG(t, rg)
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	var sent map[string]any
	eventually(t, "the question in Telegram", func() bool {
		for _, m := range f.called("sendMessage") {
			if strings.Contains(m["text"].(string), "Which store?") {
				sent = m
				return true
			}
		}
		return false
	})
	if sent["chat_id"].(float64) != 42 {
		t.Fatalf("sent to %v", sent["chat_id"])
	}
	rows := sent["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	first := rows[0].([]any)[0].(map[string]any)
	if len(rows) != 2 || first["text"] != "1. ★ SQLite" || first["callback_data"] != "a|"+a.ID+"|1" {
		t.Fatalf("%v", rows)
	}
	if !strings.Contains(sent["text"].(string), "1. ★ SQLite") || !strings.Contains(sent["text"].(string), "2. Plain files") {
		t.Fatalf("the choices are not in the text: %q", sent["text"])
	}
	callback(f, 77, "a|"+a.ID+"|0") // another account: ignored
	time.Sleep(500 * time.Millisecond)
	if snap, _ := rg.client.Snapshot(ctx); len(snap.Asks) != 1 {
		t.Fatal("another account answered")
	}
	callback(f, 42, "a|"+a.ID+"|0") // Plain files
	rg.waitState(s.ID, serve.StateComplete)
	if !hasLine(feedTexts(rg, s.ID), "answer: accept files") || !hasLine(feedTexts(rg, s.ID), "answered in Telegram") {
		t.Fatalf("%q", feedTexts(rg, s.ID))
	}
	eventually(t, "the message closed", func() bool {
		for _, m := range f.called("editMessageText") {
			if strings.Contains(m["text"].(string), "✓ answered") {
				return true
			}
		}
		return false
	})
	if len(f.called("answerCallbackQuery")) != 1 {
		t.Fatalf("%d callback answers (only the person's gets one)", len(f.called("answerCallbackQuery")))
	}
	// nothing was sent to the strangers
	for _, m := range f.called("sendMessage") {
		if id := m["chat_id"].(float64); id != 42 {
			t.Fatalf("sent to %v", id)
		}
	}
}

// An approval is a link to the Mac, never a button (A9).
func TestTelegramApprovalIsLinkOnly(t *testing.T) {
	rg := start(t, newRepo(t), "approval")
	f := startTG(t, rg)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	eventually(t, "the approval in Telegram", func() bool {
		for _, m := range f.called("sendMessage") {
			if text := m["text"].(string); strings.Contains(text, "An approval waits") {
				// nothing of the request: its input may hold a secret
				if m["reply_markup"] != nil || !strings.Contains(text, "on your Mac") || !strings.Contains(text, "http://127.0.0.1:") ||
					strings.Contains(text, "/etc/hosts") || strings.Contains(text, "Write") {
					t.Fatalf("%v", m)
				}
				return true
			}
		}
		return false
	})
}

// An ask left waiting is sent again (A20).
func TestTelegramReminder(t *testing.T) {
	old := serve.TelegramRemind
	serve.TelegramRemind = 0 // set before the service starts, back after it ended
	rg := startAgent(t, newRepo(t), "codex", "question", nil)
	t.Cleanup(func() { rg.stop(); serve.TelegramRemind = old })
	f := startTG(t, rg)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	eventually(t, "the question delivered", func() bool { return rg.svc.TelegramSent(a.ID) })
	rg.svc.TelegramPass()
	eventually(t, "the reminder", func() bool {
		for _, m := range f.called("sendMessage") {
			if strings.HasPrefix(m["text"].(string), "Still waiting") {
				return true
			}
		}
		return false
	})
}

// A first send that fails is tried again; an ask open before the pairing
// is sent once paired.
func TestTelegramRetryAndLatePairing(t *testing.T) {
	old := serve.TelegramRetry
	serve.TelegramRetry = 0 // set before the service starts, back after it ended
	rg := startAgent(t, newRepo(t), "codex", "question", nil)
	t.Cleanup(func() { rg.stop(); serve.TelegramRetry = old })
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()                        // opened before there is a bot
	f := pairTG(t, rg, &fakeTG{failSend: 2}) // the "paired" note and the first try fail
	eventually(t, "the failure", func() bool { return rg.svc.TelegramFailed(a.ID) })
	if st, _ := rg.client.TelegramStatus(context.Background()); st.Error == "" || rg.svc.TelegramSent(a.ID) {
		t.Fatalf("%+v", st)
	}
	rg.svc.TelegramPass()
	eventually(t, "the retry", func() bool { return rg.svc.TelegramSent(a.ID) })
	if st, _ := rg.client.TelegramStatus(context.Background()); st.Error != "" {
		t.Fatalf("the error stayed: %q", st.Error)
	}
	n := 0
	for _, m := range f.called("sendMessage") {
		if strings.Contains(m["text"].(string), "Which store?") {
			n++
		}
	}
	if n != 2 { // the failed one and the retry, nothing twice
		t.Fatalf("%d sends", n)
	}
}

// A poll in flight when the bot is turned off brings nothing back: its
// callback answers nothing.
func TestTelegramOffDropsThePollInFlight(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "question", nil)
	f := startTG(t, rg)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	eventually(t, "delivered", func() bool { return rg.svc.TelegramSent(a.ID) })
	f.mu.Lock()
	f.hold = make(chan struct{})
	polls := f.polls
	f.mu.Unlock()
	eventually(t, "a poll held", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.polls > polls })
	callback(f, 42, "a|"+a.ID+"|1")
	if err := rg.client.TelegramOff(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	close(f.hold)
	f.hold = nil
	f.mu.Unlock()
	time.Sleep(time.Second)
	if snap, _ := rg.client.Snapshot(context.Background()); len(snap.Asks) != 1 {
		t.Fatal("answered after off")
	}
	if st, _ := rg.client.TelegramStatus(context.Background()); st.Configured {
		t.Fatalf("%+v", st)
	}
}

// With the dispatcher on, a question reaches Telegram only once it is
// left to the person.
func TestTelegramWaitsForTheDispatcher(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "go")
	rg := startDispatch(t, newRepo(t), "not json", true, "FAKE_DISPATCH_WAIT="+gate)
	f := startTG(t, rg)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	eventually(t, "the dispatcher called", func() bool { return len(rg.dispatchCalls()) == 1 })
	rg.svc.TelegramPass()
	time.Sleep(500 * time.Millisecond)
	if n := questionsSent(f); n != 0 {
		t.Fatalf("%d sent while the dispatcher looks", n)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "left to the person, then sent", func() bool { return rg.svc.TelegramSent(a.ID) })
	if n := questionsSent(f); n != 1 {
		t.Fatalf("%d sends", n)
	}
}

func questionsSent(f *fakeTG) int {
	n := 0
	for _, m := range f.called("sendMessage") {
		if strings.Contains(m["text"].(string), "Which store?") {
			n++
		}
	}
	return n
}

// A press whose account is no longer heard when the ask is taken leaves
// it open.
func TestTelegramAnswerNeedsTheGuard(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "question", nil)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	if err := rg.svc.AnswerIf(a.ID, 1, func() bool { return false }); err == nil {
		t.Fatal("answered without the guard")
	}
	if snap, _ := rg.client.Snapshot(context.Background()); len(snap.Asks) != 1 {
		t.Fatal("the ask was taken")
	}
}

// A new pairing gets what is open at once, though it went to the old one.
func TestTelegramRepairingResends(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "question", nil)
	f := startTG(t, rg)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	eventually(t, "delivered", func() bool { return rg.svc.TelegramSent(a.ID) })
	code, _, err := rg.client.TelegramPair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.push(map[string]any{"message": map[string]any{"message_id": 2, "from": map[string]any{"id": 43},
		"chat": map[string]any{"id": 43, "type": "private"}, "text": "/start " + code}})
	eventually(t, "sent to the new chat", func() bool {
		for _, m := range f.called("sendMessage") {
			if m["chat_id"].(float64) == 43 && strings.Contains(m["text"].(string), "Which store?") {
				return true
			}
		}
		return false
	})
	callback(f, 42, "a|"+a.ID+"|1") // the old account is not heard
	time.Sleep(500 * time.Millisecond)
	if snap, _ := rg.client.Snapshot(context.Background()); len(snap.Asks) != 1 {
		t.Fatal("the old account answered")
	}
}

// An edit that fails is tried again.
func TestTelegramEditRetried(t *testing.T) {
	rg := startAgent(t, newRepo(t), "codex", "question", nil)
	f := startTG(t, rg)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	eventually(t, "delivered", func() bool { return rg.svc.TelegramSent(a.ID) })
	f.mu.Lock()
	f.failEdit = 1
	f.mu.Unlock()
	if err := rg.client.Answer(context.Background(), a.ID, serve.Answer{Choices: [][]string{{"1"}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first edit", func() bool { return len(f.called("editMessageText")) == 1 })
	time.Sleep(200 * time.Millisecond)
	rg.svc.TelegramPass()
	eventually(t, "the edit again", func() bool { return len(f.called("editMessageText")) == 2 })
	rg.svc.TelegramPass()
	time.Sleep(200 * time.Millisecond)
	if n := len(f.called("editMessageText")); n != 2 {
		t.Fatalf("%d edits", n)
	}
}
