// Command fakeacp is an ACP agent for tests: it records its arguments, its
// environment and every message it receives to $FAKE_LOG (the same format
// as fakeclaude) and plays $FAKE_SCENARIO for each prompt:
//
//	approval  asks to run `go test ./...`, then ends the turn
//	question  asks a form question (two options, the second recommended)
//	opencode  asks a form question whose recommended option is the default
//	wait      waits until the turn is cancelled
//	work      says something and ends the turn
//
// A resumed session (session/load) replays one old message first.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

var (
	log     *os.File
	outMu   sync.Mutex
	nextID  = 1000
	waiters = map[string]chan map[string]any{} // our request id → reply
	wmu     sync.Mutex
	cancel  = make(chan struct{}, 1)
	session = "fake-acp-session"
)

func record(kind string, v any) {
	data, _ := json.Marshal(map[string]any{"kind": kind, "v": v})
	log.Write(append(data, '\n'))
}

func emit(v any) {
	data, _ := json.Marshal(v)
	outMu.Lock()
	fmt.Println(string(data))
	outMu.Unlock()
}

func reply(id any, result any) { emit(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }

func update(u map[string]any) {
	emit(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": u}})
}

func say(text string) {
	update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
}

// request sends a request to the client and waits for its result.
func request(method string, params any) map[string]any {
	wmu.Lock()
	nextID++
	id := fmt.Sprint(nextID)
	ch := make(chan map[string]any, 1)
	waiters[id] = ch
	wmu.Unlock()
	emit(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return <-ch
}

func main() {
	var err error
	log, err = os.OpenFile(os.Getenv("FAKE_LOG"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		os.Exit(9)
	}
	record("start", nil)
	defer record("exit", nil)
	record("args", os.Args[1:])
	env := map[string]any{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	record("env", env)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<24)
	for in.Scan() {
		var m map[string]any
		if json.Unmarshal(in.Bytes(), &m) != nil {
			continue
		}
		record("in", m)
		method, _ := m["method"].(string)
		if method == "" { // a reply to one of our requests
			id := fmt.Sprint(m["id"])
			wmu.Lock()
			ch := waiters[id]
			delete(waiters, id)
			wmu.Unlock()
			if ch != nil {
				res, _ := m["result"].(map[string]any)
				ch <- res
			}
			continue
		}
		params, _ := m["params"].(map[string]any)
		switch method {
		case "initialize":
			if os.Getenv("FAKE_ACP_HANG") == "initialize" {
				continue // never answers
			}
			reply(m["id"], map[string]any{"protocolVersion": 1,
				"agentCapabilities": map[string]any{"loadSession": true},
				"authMethods":       []any{map[string]any{"id": "chat-gpt", "name": "ChatGPT"}}})
		case "authenticate":
			reply(m["id"], map[string]any{})
		case "session/new":
			if os.Getenv("FAKE_ACP_HANG") == "session/new" {
				continue
			}
			reply(m["id"], map[string]any{"sessionId": session, "configOptions": []any{
				map[string]any{"id": "model", "name": "Model", "category": "model", "type": "select",
					"options": []any{map[string]any{"value": "fast-1", "name": "Fast"}, map[string]any{"value": "big-1", "name": "Big"}}}}})
		case "session/load":
			if s, _ := params["sessionId"].(string); s != "" {
				session = s
			}
			// the history replays before the reply (ACP); the client must
			// not journal it again
			update(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "old prompt"}})
			say("an old answer")
			reply(m["id"], map[string]any{})
		case "session/set_config_option":
			reply(m["id"], map[string]any{"configOptions": []any{}})
		case "session/cancel":
			select {
			case cancel <- struct{}{}:
			default:
			}
		case "session/prompt":
			go turn(m["id"], params)
		default:
			if m["id"] != nil {
				emit(map[string]any{"jsonrpc": "2.0", "id": m["id"], "error": map[string]any{"code": -32601, "message": "no " + method}})
			}
		}
	}
}

func text(params map[string]any) string {
	blocks, _ := params["prompt"].([]any)
	var b strings.Builder
	for _, x := range blocks {
		if m, ok := x.(map[string]any); ok {
			t, _ := m["text"].(string)
			b.WriteString(t)
		}
	}
	return b.String()
}

func turn(id any, params map[string]any) {
	prompt := text(params)
	end := func(reason string) { reply(id, map[string]any{"stopReason": reason}) }
	if strings.HasPrefix(prompt, "more") || strings.HasPrefix(prompt, "again") || strings.HasPrefix(prompt, "The person") {
		say("resumed with: " + prompt)
		end("end_turn")
		return
	}
	switch os.Getenv("FAKE_SCENARIO") {
	case "wait":
		say("Working.")
		<-cancel
		end("cancelled")
	case "approval":
		say("Running the tests.")
		update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "go test ./...", "kind": "execute", "status": "pending",
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "x"}}}})
		res := request("session/request_permission", map[string]any{"sessionId": session,
			"toolCall": map[string]any{"toolCallId": "t1", "title": "go test ./...", "kind": "execute",
				"rawInput": map[string]any{"command": []any{"/bin/zsh", "-lc", command()}, "cwd": os.Getenv("FAKE_ACP_CWD")}},
			"options": []any{
				map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
				map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"}}})
		out, _ := res["outcome"].(map[string]any)
		if out["outcome"] == "cancelled" {
			end("cancelled")
			return
		}
		say(fmt.Sprintf("permission: %v", out["optionId"]))
		update(map[string]any{"sessionUpdate": "usage_update", "used": 10, "size": 100, "cost": map[string]any{"amount": 0.02, "currency": "USD"}})
		end("end_turn")
	case "orphan":
		// a helper outside the agent's process group keeps stdout open
		c := exec.Command("sleep", "30")
		c.Stdout = os.Stdout
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		_ = c.Start()
		say("Working.")
		<-cancel
		end("cancelled")
	case "cmdperm", "move", "fetch":
		tc := map[string]any{"toolCallId": "t3", "title": "go test with network", "kind": "execute",
			"rawInput": map[string]any{"command": "go test ./...", "additionalPermissions": map[string]any{"network": true}}}
		switch os.Getenv("FAKE_SCENARIO") {
		case "move":
			tc = map[string]any{"toolCallId": "t3", "title": "rename", "kind": "move", "locations": []any{map[string]any{"path": "a.go"}}}
		case "fetch":
			tc = map[string]any{"toolCallId": "t3", "title": "fetch https://example.com", "kind": "fetch", "rawInput": map[string]any{"url": "https://example.com"}}
		}
		res := request("session/request_permission", map[string]any{"sessionId": session, "toolCall": tc,
			"options": []any{map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
				map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"}}})
		out, _ := res["outcome"].(map[string]any)
		say(fmt.Sprintf("permission: %v", out["optionId"]))
		end("end_turn")
	case "permissions", "network", "edits":
		tc := map[string]any{"toolCallId": "t2", "title": "more access", "kind": "other",
			"rawInput": map[string]any{"permissions": map[string]any{"network": map[string]any{"enabled": true}}}}
		switch os.Getenv("FAKE_SCENARIO") {
		case "network":
			tc = map[string]any{"toolCallId": "t2", "title": "curl", "kind": "execute",
				"rawInput": map[string]any{"command": "curl https://example.com", "url": "https://example.com"}}
		case "edits":
			var locs []any
			for _, p := range strings.Split(os.Getenv("FAKE_ACP_PATHS"), ",") {
				locs = append(locs, map[string]any{"path": p})
			}
			tc = map[string]any{"toolCallId": "t2", "title": "edit files", "kind": "edit", "locations": locs}
		}
		res := request("session/request_permission", map[string]any{"sessionId": session, "toolCall": tc,
			"options": []any{map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
				map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"}}})
		out, _ := res["outcome"].(map[string]any)
		say(fmt.Sprintf("permission: %v", out["optionId"]))
		end("end_turn")
	case "async":
		go request("elicitation/create", map[string]any{"sessionId": session, "mode": "form", "message": "Q",
			"_meta": map[string]any{"codex": map[string]any{"delivery": "async"}},
			"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string", "title": "Go on?",
				"oneOf": []any{map[string]any{"const": "yes", "title": "Yes (Recommended)"}, map[string]any{"const": "no", "title": "No"}}}}, "required": []any{"q"}}})
		<-cancel
		end("cancelled")
	case "forms":
		res := request("elicitation/create", map[string]any{"sessionId": session, "mode": "form", "message": "Q",
			"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{
				"a":     map[string]any{"type": "string", "title": "Pick", "oneOf": []any{map[string]any{"const": "x", "title": "X"}, map[string]any{"const": "y", "title": "Y"}}},
				"b":     map[string]any{"type": "string", "title": "Pick", "oneOf": []any{map[string]any{"const": "x", "title": "X"}, map[string]any{"const": "y", "title": "Y"}}},
				"count": map[string]any{"type": "integer", "title": "How many?"},
				"note":  map[string]any{"type": "string", "title": "Anything else?"},
				"pin":   map[string]any{"type": "string", "title": "Token", "_meta": map[string]any{"codex": map[string]any{"isSecret": true}}}},
				"required": []any{"a", "b", "count", "pin"}}})
		data, _ := json.Marshal(res["content"])
		say("content: " + string(data))
		end("end_turn")
	case "question", "opencode":
		title := "Which store?"
		if t := os.Getenv("FAKE_QUESTION"); t != "" {
			title = t
		}
		prop := map[string]any{"type": "string", "title": title, "description": "Storage",
			"oneOf": []any{map[string]any{"const": "files", "title": "Plain files"}, map[string]any{"const": "sqlite", "title": "SQLite (Recommended)"}}}
		if os.Getenv("FAKE_SCENARIO") == "opencode" {
			prop = map[string]any{"type": "string", "title": "Which store?", "default": "sqlite",
				"oneOf": []any{map[string]any{"const": "files", "title": "Plain files"}, map[string]any{"const": "sqlite", "title": "SQLite"}}}
		}
		props := map[string]any{"store": prop}
		if os.Getenv("FAKE_NOTE") != "" { // codex-acp's own-answer field beside the choice
			props["store_note"] = map[string]any{"type": "string", "title": "Additional answer or note",
				"_meta": map[string]any{"codex": map[string]any{"role": "user_note", "questionId": "store"}}}
		}
		res := request("elicitation/create", map[string]any{"sessionId": session, "mode": "form", "message": "Codex needs your input to continue.",
			"requestedSchema": map[string]any{"type": "object", "properties": props, "required": []any{"store"}}})
		content, _ := res["content"].(map[string]any)
		say(fmt.Sprintf("answer: %v %v", res["action"], content["store"]))
		end("end_turn")
	default:
		say("Done the work.")
		end("end_turn")
	}
}

func command() string {
	if c := os.Getenv("FAKE_ACP_COMMAND"); c != "" {
		return c
	}
	return "go test ./..."
}
