// Command fakeclaude speaks `claude -p` stream-json for tests: it records
// its arguments and every input line to $FAKE_LOG and plays $FAKE_SCENARIO.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

var (
	in  = bufio.NewScanner(os.Stdin)
	log *os.File
)

func emit(v any) {
	data, _ := json.Marshal(v)
	fmt.Println(string(data))
}

func record(kind string, v any) {
	data, _ := json.Marshal(map[string]any{"kind": kind, "v": v})
	log.Write(append(data, '\n'))
}

// next reads the next input line as JSON.
func next() map[string]any {
	for in.Scan() {
		var m map[string]any
		if json.Unmarshal(in.Bytes(), &m) == nil {
			record("in", m)
			return m
		}
	}
	return nil
}

// ask sends a can_use_tool request and returns the response payload.
func ask(id, tool string, input map[string]any) map[string]any {
	emit(map[string]any{"type": "control_request", "request_id": id,
		"request": map[string]any{"subtype": "can_use_tool", "tool_name": tool, "input": input}})
	for {
		m := next()
		if m == nil {
			return nil
		}
		if m["type"] != "control_response" {
			continue
		}
		resp, _ := m["response"].(map[string]any)
		if resp["request_id"] != id {
			continue
		}
		payload, _ := resp["response"].(map[string]any)
		return payload
	}
}

func say(text string) {
	emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}})
}

func result(text string) {
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text, "total_cost_usd": 0.01, "stop_reason": "end_turn"})
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		v := os.Getenv("FAKE_VERSION")
		if v == "" {
			v = "2.1.330"
		}
		fmt.Println(v + " (Claude Code)")
		return
	}
	var err error
	log, err = os.OpenFile(os.Getenv("FAKE_LOG"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		os.Exit(9)
	}
	if strings.Contains(strings.Join(os.Args, " "), "--no-session-persistence") {
		dispatcher() // the dispatcher's one short call
		return
	}
	argv := append([]string(nil), os.Args[1:]...)
	for i := range argv {
		if argv[i] == "--settings" && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "{") {
			if data, err := os.ReadFile(argv[i+1]); err == nil {
				argv[i+1] = string(data) // the tests read what Claude would
			}
		}
	}
	record("args", argv)
	record("start", os.Getpid())
	defer record("exit", os.Getpid())
	record("env", map[string]string{"ANTHROPIC_API_KEY": os.Getenv("ANTHROPIC_API_KEY"), "pwd": func() string { d, _ := os.Getwd(); return d }()})
	first := next()
	if first == nil {
		os.Exit(2)
	}
	src := os.Getenv("FAKE_APIKEYSOURCE")
	if src == "" {
		src = "none"
	}
	resumedHold := strings.Contains(strings.Join(os.Args, " "), "--resume") && os.Getenv("FAKE_RESUME_HOLD") != ""
	if resumedHold { // a resumed run that never gets to init
		for next() != nil {
		}
		return
	}
	mode := os.Getenv("FAKE_MODE")
	if mode == "" {
		mode = "default"
	}
	emit(map[string]any{"type": "system", "subtype": "init", "apiKeySource": src, "model": "claude-test", "session_id": "x", "permissionMode": mode})
	resumed := strings.Contains(strings.Join(os.Args, " "), "--resume")
	switch scenario := os.Getenv("FAKE_SCENARIO"); {
	case resumed:
		msg, _ := first["message"].(map[string]any)
		say(fmt.Sprintf("resumed with: %v", msg["content"]))
		time.Sleep(150 * time.Millisecond) // a window for a second resume to overlap
		result("Resumed and done.")
	case scenario == "work":
		say("Looking at the task.")
		if r := ask("r1", "Bash", map[string]any{"command": "go test ./...", "description": "Run the tests"}); r["behavior"] != "allow" {
			say("tests were refused")
		}
		r := ask("r2", "AskUserQuestion", map[string]any{"questions": []any{map[string]any{
			"question": "Which approach?", "header": "Approach", "multiSelect": false,
			"options": []any{map[string]any{"label": "Small fix (Recommended)", "description": "keep it small"}, map[string]any{"label": "Rewrite"}}}}})
		up, _ := r["updatedInput"].(map[string]any)
		answers, _ := up["answers"].(map[string]any)
		say(fmt.Sprintf("Going with %v.", answers["Which approach?"]))
		if r := ask("r3", "Bash", map[string]any{"command": "git push origin HEAD"}); r["behavior"] == "deny" {
			say("push refused: " + fmt.Sprint(r["message"]))
		}
		result("Done: small fix committed.")
	case scenario == "approval":
		r := ask("r1", "Write", map[string]any{"file_path": "/etc/hosts", "content": "x"})
		say("write: " + fmt.Sprint(r["behavior"]))
		result("Done.")
	case scenario == "wait":
		say("Working for a long time.")
	case scenario == "crash":
		record("exit", os.Getpid())
		os.Exit(3)
	}
	// like claude: read until the input closes
	for next() != nil {
	}
}

// dispatcher plays the dispatcher's call: it records the prompt and its
// arguments and answers $FAKE_DISPATCH.
func dispatcher() {
	data, _ := io.ReadAll(os.Stdin)
	record("dispatch", map[string]any{"args": os.Args[1:], "prompt": string(data)})
	src := os.Getenv("FAKE_DISPATCH_SOURCE")
	if src == "" {
		src = "none"
	}
	emit(map[string]any{"type": "system", "subtype": "init", "apiKeySource": src})
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": os.Getenv("FAKE_DISPATCH")})
}
