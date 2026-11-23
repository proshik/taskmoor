package serve

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Plan usage (stage 6, F11): how much of each subscription's windows is
// left, read by taskmoor itself the way Guild reads it. Claude: the OAuth
// token Claude Code keeps in the login Keychain and the undocumented
// oauth/usage endpoint; the token stays in memory and goes nowhere else.
// Codex: `codex app-server`'s account/rateLimits/read, so Codex keeps its
// own token. OpenCode bills an API key here (A27): no plan to read.

// Window is one usage window of a plan.
type Window struct {
	Name     string `json:"name"` // session (5 h), week, or "N min"
	Left     int    `json:"left"` // percent left, 0–100
	ResetsAt string `json:"resets_at,omitempty"`
}

// Usage is one harness's plan as last read.
type Usage struct {
	Harness string   `json:"harness"`
	Windows []Window `json:"windows,omitempty"`
	// Left is the lowest window's share left; -1 while unknown.
	Left  int    `json:"left"`
	At    string `json:"at,omitempty"`
	Error string `json:"error,omitempty"`
}

// UsageInterval is how often the plans are read; UsageThreshold the share
// left (percent) below which new automatic starts wait and manual ones
// need --anyway, unless the config's usage_threshold says otherwise.
var (
	UsageInterval  = 5 * time.Minute
	UsageThreshold = 10
)

const usageEndpoint = "https://api.anthropic.com/api/oauth/usage"

// usageClient carries the OAuth token: it follows no redirect (the token
// goes to the endpoint and nowhere else) and speaks HTTP/1.1 only (Go's
// HTTP/2 debug logging would print request headers).
var usageClient = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy:             http.ProxyFromEnvironment,
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	},
}

type usageState struct {
	mu    sync.Mutex
	last  map[string]Usage
	pause map[string]time.Time // a 429's Retry-After
}

// UsageReader reads one harness's plan (tests replace them).
type UsageReader func(ctx context.Context) (Usage, error)

func (s *Service) usageOf(harness string) (Usage, bool) {
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	u, ok := s.usage.last[harness]
	return u, ok
}

// Usages are the last readings, by harness name.
func (s *Service) Usages() []Usage {
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	out := []Usage{}
	for _, u := range s.usage.last {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Harness < out[j].Harness })
	return out
}

// pollUsage reads the plans of the configured subscription harnesses now
// and every UsageInterval.
func (s *Service) pollUsage(ctx context.Context) {
	t := time.NewTicker(UsageInterval)
	defer t.Stop()
	for {
		s.readUsage(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) readUsage(ctx context.Context) {
	// after every reading: what a low plan held back goes on once the
	// guard lets it (postponed review rounds, undelivered findings)
	defer s.resumePostponed()
	cfg, err := s.Config()
	if err != nil {
		return
	}
	want := map[string]bool{}
	for _, p := range cfg.Profiles {
		if (p.Harness == "claude" || p.Harness == "codex") && !s.billsAKey(p) {
			want[p.Harness] = true
		}
	}
	for _, h := range reviewHarnesses(cfg) { // revust runs on its own plans
		want[h] = true
	}
	for h := range want {
		read := s.UsageClaude
		if h == "codex" {
			read = s.UsageCodex
		}
		if read == nil {
			continue
		}
		s.usage.mu.Lock()
		paused := time.Now().Before(s.usage.pause[h])
		s.usage.mu.Unlock()
		if paused {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		u, err := read(rctx)
		cancel()
		u.Harness, u.At = h, stamp(s.Now())
		var ra *retryAfter
		if errors.As(err, &ra) {
			s.usage.mu.Lock()
			s.usage.pause[h] = time.Now().Add(ra.d)
			s.usage.mu.Unlock()
		}
		if err != nil {
			// a failed read keeps the last good figures, marked
			if old, ok := s.usageOf(h); ok && old.Left >= 0 {
				u = old
			} else {
				u.Left = -1
			}
			u.Error = firstLine(err.Error(), 200)
		}
		s.usage.mu.Lock()
		prev, had := s.usage.last[h]
		s.usage.last[h] = u
		s.usage.mu.Unlock()
		if !had || prev.Left != u.Left || prev.Error != u.Error || !sameWindows(prev.Windows, u.Windows) {
			s.bus.publish("usage", u)
		}

	}
}

func sameWindows(a, b []Window) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// reviewHarnesses are the plans revust's review profile may run on:
// codex-only (the default) Codex's, claude-only Claude's, any other both
// (its runners are revust's to resolve, so both are guarded).
func reviewHarnesses(cfg *Config) []string {
	switch p := cfg.ReviewProfile; p {
	case "", "codex-only":
		return []string{"codex"}
	case "claude-only":
		return []string{"claude"}
	}
	return []string{"claude", "codex"}
}

// leftNow is the plan's share left in the windows that have not reset
// yet: a window past its reset no longer counts (-1 when none is left
// to count).
func (u Usage) leftNow(now time.Time) int {
	left := -1
	for _, w := range u.Windows {
		if w.ResetsAt != "" {
			if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil && !t.After(now) {
				continue
			}
		}
		if left < 0 || w.Left < left {
			left = w.Left
		}
	}
	if len(u.Windows) == 0 {
		return u.Left
	}
	return left
}

// threshold is the configured share left that guards starts.
func (s *Service) threshold() int {
	if cfg, err := s.Config(); err == nil && cfg.UsageThreshold != nil {
		return *cfg.UsageThreshold
	}
	return UsageThreshold
}

// guard says whether a start of prof may run on the plan's usage: an
// automatic start waits below the threshold; a manual one needs anyway.
// An API-billed profile has no plan to guard; unknown usage does not stop.
func (s *Service) guard(prof Profile, auto, anyway bool) error {
	if prof.Harness != "claude" && prof.Harness != "codex" || s.billsAKey(prof) {
		return nil
	}
	return s.guardPlan(prof.Harness, auto, anyway)
}

// billsAKey: the profile allows API billing and a key is there to bill,
// so it does not run on the plan. Allowed without a key, Claude and Codex
// fall back to the subscription: that is guarded.
func (s *Service) billsAKey(prof Profile) bool {
	if !prof.APIBilling {
		return false
	}
	keys := []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"}
	if prof.Harness == "codex" {
		keys = []string{"CODEX_API_KEY", "OPENAI_API_KEY"}
	} else if f, _ := apiKeyInSettings(s.Home, envValue(s.Env, "CLAUDE_CONFIG_DIR"), s.Home); f != "" {
		return true // an apiKeyHelper or a key in Claude's settings bills it
	}
	for _, k := range keys {
		if envValue(s.Env, k) != "" {
			return true
		}
	}
	return false
}

// guardPlan guards a start on one harness's plan.
func (s *Service) guardPlan(harness string, auto, anyway bool) error {
	u, ok := s.usageOf(harness)
	th := s.threshold()
	left := u.leftNow(time.Now())
	if !ok || left < 0 || th <= 0 || left >= th {
		return nil
	}
	prof := Profile{Harness: harness}
	u.Left = left
	switch {
	case auto:
		return refusedf("%s's plan has %d%% left (below %d%%): automatic starts wait", prof.Harness, u.Left, th)
	case !anyway:
		return refusedf("%s's plan has %d%% left (below %d%%): start anyway with --anyway (the board: Run anyway)", prof.Harness, u.Left, th)
	}
	return nil
}

// ---- Claude ----

type retryAfter struct{ d time.Duration }

func (r *retryAfter) Error() string { return fmt.Sprintf("the usage endpoint asks to wait %s", r.d) }

// readClaudeUsage is the Keychain read and the one request.
func readClaudeUsage(ctx context.Context) (Usage, error) {
	item, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return Usage{}, errors.New("Claude Code's sign-in is not in the login Keychain (claude, then /login)")
	}
	var cred struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(item, &cred) != nil || cred.ClaudeAiOauth.AccessToken == "" {
		return Usage{}, errors.New("the Keychain item holds no OAuth token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageEndpoint, nil)
	if err != nil {
		return Usage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cred.ClaudeAiOauth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "taskmoor")
	req.Header.Set("Accept", "application/json")
	resp, err := usageClient.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("oauth/usage: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusTooManyRequests {
		d := 15 * time.Minute
		ra := resp.Header.Get("Retry-After")
		if n, err := strconv.Atoi(ra); err == nil && n > 0 {
			d = time.Duration(n) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil && time.Until(t) > 0 {
			d = time.Until(t)
		}
		return Usage{}, &retryAfter{d}
	}
	if resp.StatusCode != http.StatusOK { // a redirect too: not followed
		return Usage{}, fmt.Errorf("oauth/usage answered %s", resp.Status)
	}
	return claudeWindows(body)
}

// claudeWindows reads oauth/usage's answer forgivingly (it is not
// documented): the `limits` array when there is one, the older
// five_hour/seven_day keys otherwise.
func claudeWindows(body []byte) (Usage, error) {
	var a struct {
		Limits []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt string   `json:"resets_at"`
			Scope    struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
		FiveHour *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		} `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return Usage{}, errors.New("oauth/usage's answer is not JSON")
	}
	var ws []Window
	for _, l := range a.Limits {
		if l.Percent == nil {
			continue
		}
		name := map[string]string{"session": "session", "weekly_all": "week", "weekly_scoped": "week · " + l.Scope.Model.DisplayName}[l.Kind]
		if name == "" {
			continue
		}
		ws = append(ws, Window{Name: name, Left: leftOf(*l.Percent), ResetsAt: l.ResetsAt})
	}
	if len(ws) == 0 {
		if a.FiveHour != nil && a.FiveHour.Utilization != nil {
			ws = append(ws, Window{Name: "session", Left: leftOf(*a.FiveHour.Utilization), ResetsAt: a.FiveHour.ResetsAt})
		}
		if a.SevenDay != nil && a.SevenDay.Utilization != nil {
			ws = append(ws, Window{Name: "week", Left: leftOf(*a.SevenDay.Utilization), ResetsAt: a.SevenDay.ResetsAt})
		}
	}
	if len(ws) == 0 {
		return Usage{}, errors.New("oauth/usage's answer names no window")
	}
	return Usage{Windows: ws, Left: lowest(ws)}, nil
}

// ---- Codex ----

// readCodexUsage asks `codex app-server`: initialize, initialized, then
// account/rateLimits/read.
func readCodexUsage(ctx context.Context) (Usage, error) {
	cmd := exec.Command("codex", "app-server")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its helpers end with it
	in, err := cmd.StdinPipe()
	if err != nil {
		return Usage{}, err
	}
	// stdout through a pipe of our own: a helper that keeps it open cannot
	// keep this reading past the deadline
	pr, pw, err := os.Pipe()
	if err != nil {
		return Usage{}, err
	}
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return Usage{}, fmt.Errorf("codex app-server: %v", err)
	}
	pw.Close()
	defer func() {
		_ = in.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		pr.Close()
		_ = cmd.Wait()
	}()
	for _, line := range []string{
		`{"id":0,"method":"initialize","params":{"clientInfo":{"name":"taskmoor","title":"taskmoor","version":"serve"}}}`,
		`{"method":"initialized"}`,
		`{"id":1,"method":"account/rateLimits/read"}`,
	} {
		if _, err := io.WriteString(in, line+"\n"); err != nil {
			return Usage{}, err
		}
	}
	type result struct {
		u   Usage
		err error
	}
	done := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var m struct {
				ID     *int `json:"id"`
				Result struct {
					RateLimits map[string]json.RawMessage `json:"rateLimits"`
				} `json:"result"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil || *m.ID != 1 {
				continue
			}
			if m.Error != nil {
				done <- result{err: fmt.Errorf("account/rateLimits/read: %s", m.Error.Message)}
				return
			}
			u, err := codexWindows(m.Result.RateLimits)
			done <- result{u, err}
			return
		}
		done <- result{err: errors.New("codex app-server ended without an answer")}
	}()
	select {
	case r := <-done:
		return r.u, r.err
	case <-ctx.Done():
		return Usage{}, errors.New("codex app-server did not answer in time")
	}
}

func codexWindows(rl map[string]json.RawMessage) (Usage, error) {
	var ws []Window
	for _, key := range []string{"primary", "secondary"} {
		var w *struct {
			UsedPercent        *float64 `json:"usedPercent"`
			WindowDurationMins *int     `json:"windowDurationMins"`
			ResetsAt           *int64   `json:"resetsAt"`
		}
		if json.Unmarshal(rl[key], &w) != nil || w == nil || w.UsedPercent == nil {
			continue
		}
		name := "window"
		if w.WindowDurationMins != nil {
			switch *w.WindowDurationMins {
			case 300:
				name = "session"
			case 10080:
				name = "week"
			default:
				name = fmt.Sprintf("%d min", *w.WindowDurationMins)
			}
		}
		win := Window{Name: name, Left: leftOf(*w.UsedPercent)}
		if w.ResetsAt != nil {
			win.ResetsAt = time.Unix(*w.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		ws = append(ws, win)
	}
	if len(ws) == 0 {
		return Usage{}, errors.New("codex reported no rate-limit window")
	}
	return Usage{Windows: ws, Left: lowest(ws)}, nil
}

func leftOf(used float64) int {
	l := 100 - int(used+0.5)
	return max(0, min(100, l))
}

func lowest(ws []Window) int {
	l := 100
	for _, w := range ws {
		l = min(l, w.Left)
	}
	return l
}

// ---- power (A19) ----

// awake holds macOS's idle-sleep assertion while any agent runs: one
// `caffeinate -i -w <service pid>` (it ends with the service, whatever
// happens). The lid closed on battery still sleeps: the board says so.
// Agents are sessions' processes, review rounds and dispatcher calls.
type awake struct {
	mu   sync.Mutex
	n    int
	stop func()
	gen  uint64 // the assertion's generation: an old watcher clears nothing
	err  string // the assertion could not be held: why
}

// Awake starts holding the assertion; tests replace it. stop lets go;
// done closes if the assertion ends by itself.
var Awake = func(pid int) (stop func(), done <-chan struct{}, err error) {
	cmd := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(pid))
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	ch := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ch) }()
	return func() { _ = cmd.Process.Kill() }, ch, nil
}

func (s *Service) agentUp() {
	s.awake.mu.Lock()
	defer s.awake.mu.Unlock()
	s.awake.n++
	if s.awake.stop == nil {
		s.holdAwake()
	}
}

// holdAwake starts the assertion (s.awake.mu held); a failure or an early
// end is shown, and the next agent tries again.
func (s *Service) holdAwake() {
	stop, done, err := Awake(s.pid)
	if err != nil {
		s.awake.err = "the Mac is not held awake: " + err.Error()
		s.bus.publish("awake", map[string]any{"on": false, "error": s.awake.err})
		return
	}
	s.awake.gen++
	gen := s.awake.gen
	s.awake.stop, s.awake.err = stop, ""
	s.bus.publish("awake", map[string]bool{"on": true})
	go func() {
		<-done
		s.awake.mu.Lock()
		defer s.awake.mu.Unlock()
		if s.awake.gen == gen && s.awake.n > 0 && s.awake.stop != nil {
			s.awake.stop, s.awake.err = nil, "the idle-sleep assertion ended by itself"
			s.bus.publish("awake", map[string]any{"on": false, "error": s.awake.err})
		}
	}()
}

func (s *Service) agentDown() {
	s.awake.mu.Lock()
	defer s.awake.mu.Unlock()
	if s.awake.n > 0 {
		s.awake.n--
	}
	if s.awake.n == 0 {
		if s.awake.stop != nil {
			stop := s.awake.stop
			s.awake.stop = nil
			stop()
		}
		s.awake.err = ""
		s.bus.publish("awake", map[string]bool{"on": false})
	}
}

// AgentsRunning: agents run and the Mac is held awake for them;
// AwakeError says why it is not while they run.
func (s *Service) AgentsRunning() (bool, string) {
	s.awake.mu.Lock()
	defer s.awake.mu.Unlock()
	return s.awake.n > 0 && s.awake.stop != nil, s.awake.err
}

// sayGuarded is the person's Say (anyway: below the threshold too).
func (s *Service) sayGuarded(ctx context.Context, id, text string, anyway bool) error {
	return s.say(ctx, id, text, false, anyway)
}

// resumePostponed goes on with what a low plan held back: postponed
// review rounds, and findings that could not resume the session.
func (s *Service) resumePostponed() {
	s.mu.Lock()
	var rounds, findings []*live
	for _, l := range s.sessions {
		rv := l.s.Review
		if rv == nil || l.s.State != StateComplete || l.runner != nil {
			continue
		}
		switch {
		case rv.State == "error" && strings.HasPrefix(rv.Error, "postponed:"):
			rounds = append(rounds, l)
		case rv.State == "findings" && rv.SendFailed && strings.Contains(rv.SendError, "plan has"):
			findings = append(findings, l)
		}
	}
	s.mu.Unlock()
	for _, l := range rounds {
		if s.reviewGuard() == nil {
			s.afterComplete(l)
		}
	}
	for _, l := range findings {
		s.resendFindings(l)
	}
}
