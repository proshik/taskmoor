// Command fakerevust plays revust for tests: `new` makes the round's
// directories and prints their paths; a run records its arguments to
// $FAKE_LOG and ends as $FAKE_REVUST says (clean: 0, findings: 1, error:
// 2, slow: waits to be killed); `view` prints a findings report.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func arg(name string) string {
	for i, a := range os.Args {
		if a == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "new" {
		round := filepath.Join(arg("--tasks-dir"), arg("--task"), arg("--run"))
		input := filepath.Join(round, "input")
		_ = os.MkdirAll(input, 0o755)
		data, _ := json.MarshalIndent(map[string]any{"round_dir": round, "input_dir": input}, "", "  ")
		fmt.Println(string(data))
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "view" {
		fmt.Println("# Review results\n\n## Active findings\n\n### Major — The greeting is wrong\n\nLocation: `greeting.txt:1`\n\nIt says hi.\n\n## Source observations\n\nnone")
		return
	}
	if f, err := os.OpenFile(os.Getenv("FAKE_LOG"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err == nil {
		scope, _ := os.ReadFile(filepath.Join(arg("--tasks-dir"), arg("--task"), arg("--run"), "input", "scope.md"))
		data, _ := json.Marshal(map[string]any{"kind": "revust", "v": map[string]any{"args": os.Args[1:], "scope": string(scope)}})
		f.Write(append(data, '\n'))
		f.Close()
	}
	switch os.Getenv("FAKE_REVUST") {
	case "findings":
		os.Exit(1)
	case "error":
		fmt.Fprintln(os.Stderr, "revust: provider failed")
		os.Exit(2)
	case "slow":
		time.Sleep(time.Minute)
	}
}
