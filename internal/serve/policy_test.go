package serve

import "testing"

func TestRules(t *testing.T) {
	r := Rules{Worktree: "/w/repo-tm-001", MainBranch: "main", MainCheckout: "/w/repo"}
	cases := []struct {
		tool  string
		input map[string]any
		want  Decision
	}{
		{"Bash", map[string]any{"command": "go test ./..."}, Allow},
		{"Bash", map[string]any{"command": "git commit -am 'x'"}, Allow},
		{"Bash", map[string]any{"command": "git merge main"}, Allow}, // main into the branch
		{"Bash", map[string]any{"command": "git push origin HEAD"}, Deny},
		{"Bash", map[string]any{"command": "git -C . push"}, Deny},
		{"Bash", map[string]any{"command": "make && git push --force"}, Deny},
		{"Bash", map[string]any{"command": "npm publish"}, Deny},
		{"Bash", map[string]any{"command": "cargo publish --dry-run"}, Deny},
		{"Bash", map[string]any{"command": "gh pr merge 3"}, Deny},
		{"Bash", map[string]any{"command": "docker push img"}, Deny},
		{"Bash", map[string]any{"command": "kubectl apply -f x.yaml"}, Deny},
		{"Bash", map[string]any{"command": "git checkout main"}, Deny},
		{"Bash", map[string]any{"command": "git switch main"}, Deny},
		{"Bash", map[string]any{"command": "git checkout mainline"}, Allow},
		{"Bash", map[string]any{"command": "rm -rf build"}, Allow},
		{"Bash", map[string]any{"command": "rm -rf /w/repo-tm-001/build"}, Allow},
		{"Bash", map[string]any{"command": "rm -rf /tmp/x"}, AskPerson},
		{"Bash", map[string]any{"command": "rm ../other/file"}, AskPerson},
		{"Bash", map[string]any{"command": "rm ~/notes"}, AskPerson},
		{"Bash", map[string]any{"command": "ls /w/repo"}, AskPerson},
		{"Bash", map[string]any{"command": "cat .env"}, AskPerson},
		{"Bash", map[string]any{"command": "echo x > /w/repo/notes.txt"}, AskPerson},
		{"Bash", map[string]any{"command": "echo x >> ../other/log"}, AskPerson},
		{"Bash", map[string]any{"command": "go test ./... 2>&1 > /tmp/out.txt"}, Allow},
		{"Bash", map[string]any{"command": "echo x > out.txt"}, Allow},
		{"Bash", map[string]any{"command": "make 2>/dev/null"}, Allow},
		{"Bash", map[string]any{"command": "cp build/a /usr/local/bin/a"}, AskPerson},
		{"Bash", map[string]any{"command": "cp build/a dist/a"}, Allow},
		{"Bash", map[string]any{"command": "mkdir -p /w/repo-tm-001/dist"}, Allow},
		{"Bash", map[string]any{"command": "tee ~/x.log"}, AskPerson},
		{"Bash", map[string]any{"command": "ls -la /usr/local/bin"}, Allow},
		{"Bash", map[string]any{"command": "kubectl set image deployment/web web=img:2"}, Deny},
		{"Bash", map[string]any{"command": "kubectl --context prod -n web apply -f x.yaml"}, Deny},
		{"Bash", map[string]any{"command": "kubectl get pods -n web"}, Allow},
		{"Bash", map[string]any{"command": "helm --kube-context prod upgrade web ./chart"}, Deny},
		{"Bash", map[string]any{"command": "git -C /w/repo merge task/TM-001-x"}, Deny},
		{"Bash", map[string]any{"command": "cd /w/repo && git commit -am x"}, Deny},
		{"Bash", map[string]any{"command": "echo x 2> /w/repo/x"}, AskPerson},
		{"Bash", map[string]any{"command": "make 2>&1 | tail"}, Allow},
		{"Bash", map[string]any{"command": "echo 'require x v1' >> go.mod"}, AskPerson},
		{"Bash", map[string]any{"command": "cp tmp.json package.json"}, AskPerson},
		{"Bash", map[string]any{"command": "echo X=1 > .env"}, AskPerson},
		{"Write", map[string]any{"file_path": "/w/repo-tm-001/src/a.go"}, Allow},
		{"Write", map[string]any{"file_path": "src/a.go"}, Allow},
		{"Edit", map[string]any{"file_path": "/w/repo/src/a.go"}, AskPerson},
		{"Edit", map[string]any{"file_path": "/w/repo-tm-001/../repo/a.go"}, AskPerson},
		{"Write", map[string]any{"file_path": "/w/repo-tm-001/go.mod"}, AskPerson},
		{"Edit", map[string]any{"file_path": "/w/repo-tm-001/web/package.json"}, AskPerson},
		{"Write", map[string]any{"file_path": "/w/repo-tm-001/.env"}, AskPerson},
		{"Write", map[string]any{}, AskPerson},
		{"Read", map[string]any{"file_path": "/w/repo-tm-001/src/a.go"}, Allow},
		{"Read", map[string]any{"file_path": "/Users/x/.ssh/id_ed25519"}, AskPerson},
		{"WebFetch", map[string]any{"url": "https://example.org"}, Allow},
	}
	for _, c := range cases {
		if got := r.Decide(c.tool, c.input); got.Decision != c.want {
			t.Errorf("%s %v: %v (%s), want %v", c.tool, c.input, got.Decision, got.Reason, c.want)
		}
	}
}

func TestNormalizeQuestions(t *testing.T) {
	qs := normalizeQuestions(map[string]any{"questions": []any{
		map[string]any{"question": "Q1?", "header": "H", "options": []any{
			map[string]any{"label": "A"}, map[string]any{"label": "B (Recommended)", "description": "d"}}},
		map[string]any{"question": "no options"},
		"junk",
	}})
	if len(qs) != 1 || qs[0].Recommended != 1 || qs[0].Options[1].Label != "B" || qs[0].Options[1].raw != "B (Recommended)" {
		t.Fatalf("%+v", qs)
	}
	a := &Ask{Questions: qs, input: map[string]any{"questions": "orig"}}
	in, said, err := answerInput(a, Answer{Choices: [][]string{{"b"}}})
	if err != nil || said != "B (Recommended)" || in["answers"].(map[string]any)["Q1?"] != "B (Recommended)" || in["questions"] != "orig" {
		t.Fatalf("%v %q %v", in, said, err)
	}
	if _, _, err := answerInput(a, Answer{Choices: [][]string{{"3"}}}); err == nil {
		t.Fatal("option 3 accepted")
	}
	if _, _, err := answerInput(a, Answer{Choices: [][]string{{"1", "2"}}}); err == nil {
		t.Fatal("two answers to a single-select question")
	}
	if in, _, err := answerInput(a, Answer{Text: "something else"}); err != nil || in["answers"].(map[string]any)["Q1?"] != "something else" {
		t.Fatalf("free text: %v %v", in, err)
	}
}

func TestAgentEnvStripsAPIKeys(t *testing.T) {
	env := agentEnv([]string{"PATH=/bin", "ANTHROPIC_API_KEY=x", "ANTHROPIC_AUTH_TOKEN=y", "CLAUDECODE=1", "OPENAI_API_KEY=z",
		"CLAUDE_CODE_MESSAGING_TOKEN=t", "CLAUDE_CODE_SESSION_ID=s", "CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1"}, false)
	got := map[string]bool{}
	for _, kv := range env {
		got[kv] = true
	}
	if !got["PATH=/bin"] || !got["OPENAI_API_KEY=z"] || got["ANTHROPIC_API_KEY=x"] || got["ANTHROPIC_AUTH_TOKEN=y"] || got["CLAUDECODE=1"] ||
		got["CLAUDE_CODE_MESSAGING_TOKEN=t"] || got["CLAUDE_CODE_SESSION_ID=s"] || !got["CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1"] {
		t.Fatalf("%v", env)
	}
	if env := agentEnv([]string{"ANTHROPIC_API_KEY=x"}, true); len(env) != 1 {
		t.Fatalf("api billing allowed: %v", env)
	}
}
