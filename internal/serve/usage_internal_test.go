package serve

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClaudeWindows(t *testing.T) {
	u, err := claudeWindows([]byte(`{"limits":[{"kind":"session","percent":37.4,"resets_at":"2026-11-23T18:00:00Z"},
		{"kind":"weekly_all","percent":91},{"kind":"weekly_scoped","percent":50,"scope":{"model":{"display_name":"Fable"}}},{"kind":"other","percent":99}]}`))
	if err != nil || len(u.Windows) != 3 || u.Left != 9 || u.Windows[0].Left != 63 || u.Windows[2].Name != "week · Fable" {
		t.Fatalf("%+v %v", u, err)
	}
	u, err = claudeWindows([]byte(`{"five_hour":{"utilization":20},"seven_day":{"utilization":40.6}}`))
	if err != nil || u.Left != 59 || len(u.Windows) != 2 {
		t.Fatalf("old keys: %+v %v", u, err)
	}
	for _, bad := range []string{`not json`, `{"error":"x"}`, `{"limits":[]}`} {
		if _, err := claudeWindows([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestCodexWindows(t *testing.T) {
	var rl map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1795593065},"secondary":{"usedPercent":74,"windowDurationMins":10080},"credits":{}}`), &rl)
	u, err := codexWindows(rl)
	if err != nil || u.Left != 26 || u.Windows[0].Name != "session" || u.Windows[1].Name != "week" || u.Windows[0].ResetsAt == "" {
		t.Fatalf("%+v %v", u, err)
	}
	var empty map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"primary":null}`), &empty)
	if _, err := codexWindows(empty); err == nil {
		t.Fatal("no window accepted")
	}
}

func TestExpiredWindowsDoNotCount(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	u := Usage{Left: 3, Windows: []Window{{Name: "session", Left: 3, ResetsAt: "2026-11-23T11:00:00Z"}, {Name: "week", Left: 40, ResetsAt: "2026-11-29T00:00:00Z"}}}
	if l := u.leftNow(now); l != 40 {
		t.Fatalf("%d", l)
	}
	u.Windows = u.Windows[:1]
	if l := u.leftNow(now); l != -1 {
		t.Fatalf("all expired: %d", l)
	}
}

func TestAllowedButKeylessBillingIsGuarded(t *testing.T) {
	s := &Service{Env: []string{"PATH=/bin"}}
	if s.billsAKey(Profile{Harness: "codex", APIBilling: true}) {
		t.Fatal("no key, yet exempt")
	}
	s.Env = append(s.Env, "OPENAI_API_KEY=x")
	if !s.billsAKey(Profile{Harness: "codex", APIBilling: true}) || s.billsAKey(Profile{Harness: "codex"}) {
		t.Fatal("keys")
	}
}

func TestReviewHarnesses(t *testing.T) {
	for p, want := range map[string]string{"": "codex", "codex-only": "codex", "claude-only": "claude", "comprehensive": "claude codex", "focused": "claude codex"} {
		if got := strings.Join(reviewHarnesses(&Config{ReviewProfile: p}), " "); got != want {
			t.Errorf("%q: %q", p, got)
		}
	}
}

// An old assertion's end does not clear the one that replaced it.
func TestOldAssertionEndKeepsTheNewOne(t *testing.T) {
	var dones []chan struct{}
	stops := 0
	old := Awake
	Awake = func(int) (func(), <-chan struct{}, error) {
		ch := make(chan struct{})
		dones = append(dones, ch)
		return func() { stops++ }, ch, nil
	}
	defer func() { Awake = old }()
	s := &Service{bus: newBus()}
	s.agentUp()     // first assertion
	s.agentDown()   // let go
	s.agentUp()     // a second one
	close(dones[0]) // the first caffeinate's exit arrives late
	time.Sleep(50 * time.Millisecond)
	if on, errText := s.AgentsRunning(); !on || errText != "" {
		t.Fatalf("on %v %q: the old end cleared the new assertion", on, errText)
	}
	s.agentDown()
	if stops != 2 {
		t.Fatalf("stops %d", stops)
	}
}
