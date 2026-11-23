package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAskMessage(t *testing.T) {
	board := "http://127.0.0.1:1234/"
	q := Question{Question: "Pick", Recommended: -1, Options: []Option{
		{Label: strings.Repeat("a", 70) + " one"}, {Label: strings.Repeat("a", 70) + " two"}}}
	a := &Ask{Kind: "question", Project: "p", Task: "T-1", ID: "x", Questions: []Question{q}}
	text, markup := askMessage(a, board, false)
	rows := markup.(map[string]any)["inline_keyboard"].([][]map[string]string)
	if rows[0][0]["text"] == rows[1][0]["text"] || !strings.Contains(text, " one") || !strings.Contains(text, " two") {
		t.Fatalf("choices alike: %q %v", text, rows)
	}
	// a preview is the board's (A12)
	a.Questions[0].Options[0].Preview = "```go\nx\n```"
	if simpleAsk(a) {
		t.Fatal("a question with a preview has buttons")
	}
	// a long question keeps the whole link
	a.Questions = []Question{{Question: strings.Repeat("я", 5000), FreeText: true}}
	text, markup = askMessage(a, board, false)
	if markup != nil || !strings.HasSuffix(text, board) || len(text) > 4000 || !strings.Contains(text, "я…") {
		t.Fatalf("%d %q", len(text), text[len(text)-80:])
	}
	// an approval carries nothing of the request
	ap := &Ask{Kind: "approval", Project: "p", Task: "T-1", Tool: "Bash", Detail: "curl -H 'Authorization: Bearer sk-secret'", Reason: "network"}
	text, markup = askMessage(ap, board, false)
	if markup != nil || strings.Contains(text, "sk-secret") || strings.Contains(text, "Bash") || !strings.HasSuffix(text, board) {
		t.Fatalf("%q", text)
	}
}

// buttons only when every choice shows in full; an optional field other
// than codex-acp's own-answer one goes to the board
func TestSimpleAskLimits(t *testing.T) {
	base := func() *Ask {
		return &Ask{Kind: "question", Questions: []Question{{Question: "Q", Recommended: -1,
			Options: []Option{{Label: "A"}, {Label: "B"}}}}}
	}
	if !simpleAsk(base()) {
		t.Fatal("a plain question")
	}
	a := base()
	a.Questions[0].Options[1].Label = strings.Repeat("b", 300)
	if simpleAsk(a) {
		t.Fatal("a choice too long for the message")
	}
	a = base()
	a.Questions = append(a.Questions, Question{Question: "constraints", FreeText: true, Optional: true})
	if simpleAsk(a) {
		t.Fatal("an optional field hidden")
	}
	a.Questions[1].Note = true
	if !simpleAsk(a) {
		t.Fatal("the own-answer field")
	}
}

// the CLI's client hands nothing to a listener that cannot prove it is
// the service
func TestSetupChecksTheListener(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		_, _ = w.Write([]byte(`{"proof":"00"}`))
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, token: "api-token", http: &http.Client{}}
	if _, err := c.TelegramSetup(t.Context(), "123:secret"); err == nil {
		t.Fatal("set up through an impostor")
	}
	if len(got) != 1 || got[0] != "/api/proof" {
		t.Fatalf("%v", got)
	}
	if proof("api-token", "n") == proof("other", "n") {
		t.Fatal("the proof does not depend on the token")
	}
}

func TestNormalizeKeepsPreviews(t *testing.T) {
	qs := normalizeQuestions(map[string]any{"questions": []any{map[string]any{"question": "Q", "options": []any{
		map[string]any{"label": "A", "preview": "<p>a</p>"}, map[string]any{"label": "B"}}}}})
	if len(qs) != 1 || qs[0].Options[0].Preview != "<p>a</p>" {
		t.Fatalf("%+v", qs)
	}
}
