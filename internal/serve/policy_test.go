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

// Stage 11: data is not commands; what runs inside is.
func TestRulesReadTheShell(t *testing.T) {
	r := Rules{Worktree: "/w/repo-tm-001", MainBranch: "main", MainCheckout: "/w/repo"}
	stage8 := "cd /w/repo-tm-001\nCTX='## Context\n\n- Do not touch README.md; output is `N YYYY-MM-DD TEXT`, a/b and / alone.'\n\n" +
		"cat > tasks/DM-006-number-notes.md <<EOF\n---\nid: DM-006\n---\n\n## Limits\n\nDo not touch README.md (DM-008). Never git push.\n\n$CTX\nEOF\n" +
		"cat >> tasks/tasks.toml <<'EOF'\n\n[components.cli]\npaths = [\"notes\", \"tests/**\"]\nEOF\n" +
		"../taskmoor --repo \"$PWD\" --source worktree validate; echo exit=$?"
	// the same files written without the quoted CTX assignment (its words —
	// "touch", "/" — are judged as before stage 11)
	stage8Plain := "cd /w/repo-tm-001\n" +
		"cat > tasks/DM-006-number-notes.md <<EOF\n---\nid: DM-006\n---\n\n## Limits\n\nDo not touch README.md (DM-008). Never git push. a/b and / alone.\n\n$NOTE\nEOF\n" +
		"cat >> tasks/tasks.toml <<'EOF'\n\n[components.cli]\npaths = [\"notes\", \"tests/**\"]\nEOF\n" +
		"../taskmoor --repo \"$PWD\" --source worktree validate; echo exit=$?"
	cases := []struct {
		cmd  string
		want Decision
	}{
		{stage8Plain, Allow},
		{stage8, AskPerson}, // the assignment's words, as before
		{"cat > notes.md <<'EOF'\nnever git push, and rm -rf / is right out\nEOF", Allow},
		// quoted text is judged as before stage 11 (stricter of the two)
		{"echo \"git push later\" > notes.md", Deny},
		{"git commit -m 'do not touch /etc/hosts'", AskPerson},
		{"# git push later\nmake", Allow},
		{"cp /etc/hosts fixtures/hosts", AskPerson}, // the text's rule, as before stage 11: a cp's words are asked about
		{"echo x >&2", Allow},
		{"go test ./... 2>&1 | tee test.log", Allow},
		{"bash -c 'echo x > /etc/hosts'", AskPerson},
		{"sh -c \"git push\"", Deny},
		{"cat <<EOF | sh\ngit push origin HEAD\nEOF", Deny},
		{"echo $(git push)", Deny},
		{"echo `git push`", Deny},
		{"eval \"git push\"", Deny},
		{"sudo tee /etc/x", AskPerson},
		{"mkdir -p /usr/local/x", AskPerson},
		{"echo x &> /etc/y", AskPerson},
		{"cp a b -t /opt/bin", AskPerson},
		{"dd if=/dev/zero of=/etc/disk bs=1", AskPerson},
		{"FOO=1 git push", Deny},
		{"echo 'x > /etc/y", AskPerson}, // unreadable: judged on its text
		{"cat .env", AskPerson},
		{"grep -r token ~/.ssh/config", AskPerson},
		// round 1 of the review: what the reading must not let past
		{"/usr/bin/git push origin HEAD", Deny},
		{"/usr/bin/git checkout main", Deny},
		{"cat > notes.md <<EOF\n$(git push)\nEOF", Deny},
		{"cat > notes.md <<'EOF'\n$(git push)\nEOF", Allow}, // a quoted delimiter: literal
		{"bash --norc -c 'git push'", Deny},
		{"bash -xc 'git push'", Deny},
		{"bash <<< 'git push'", Deny},
		{"cd /tmp/x && rm -rf build", AskPerson},
		{"cd /etc && echo x > hosts", AskPerson},
		{"cd src && rm old.go", Allow},
		{"mv go.mod go.mod.bak", AskPerson},
		{"node --env-file=.env app.js", AskPerson},
		{"cat > tasks/a.md <<'EOF'\nnever git push\nEOF\nbash -c 'go vet ./...'", Allow}, // the shell does not read the heredoc
		{"git commit -m 'never git push'", Deny},
		{"git commit -m 'add the search'", Allow},
		{"diff <(git push) x", Deny},
		// round 2 of the review
		{"if bash -c 'cat .env'; then :; fi", AskPerson},
		{"bash -s -- arg <<< 'cat .env'", AskPerson},
		{"bash -s x <<'EOF'\ncat .env\nEOF", AskPerson},
		{"bash <<EOF\necho '$(git push)'\nEOF", Deny},
		{"cd /tmp && bash -c 'rm -rf x'; cd /w/repo-tm-001", AskPerson},
		{"cp \"go.mod\" vendor/", AskPerson},
		{"cat > notes.md <<EOF\n$(echo x > go.mod)\nEOF", AskPerson}, // the substitution runs
		{"cat > notes.md <<'EOF'\n$(echo x > go.mod)\nEOF", Allow},   // literal
		{"(cd /tmp && ls) && echo x > out.txt", Allow},               // the subshell's cd stays in it
	}
	for _, c := range cases {
		if got := r.Decide("Bash", map[string]any{"command": c.cmd}); got.Decision != c.want {
			t.Errorf("%q: %v (%s), want %v", c.cmd, got.Decision, got.Reason, c.want)
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
