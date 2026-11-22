package serve_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/app"
	"github.com/proshik/taskmoor/internal/project"
	"github.com/proshik/taskmoor/internal/serve"
	"github.com/proshik/taskmoor/internal/testrepo"
)

func (rg *rig) base() string {
	data, _ := os.ReadFile(filepath.Join(rg.paths.Store, "serve.json"))
	var info serve.Info
	_ = json.Unmarshal(data, &info)
	return fmt.Sprintf("http://127.0.0.1:%d", info.Port)
}

// signIn trades a sign-in link's code for a browser token, as the page does.
func (rg *rig) signIn(t *testing.T, link string) (string, int) {
	t.Helper()
	code := link[strings.Index(link, "#code=")+len("#code="):]
	resp, err := http.Post(rg.base()+"/api/sign-in", "application/json", strings.NewReader(`{"code":"`+code+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatal("the sign-in set a cookie") // a cookie would reach other local ports
	}
	var out struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Token, resp.StatusCode
}

func (rg *rig) call(t *testing.T, method, path, token, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, rg.base()+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestBrowserSignIn(t *testing.T) {
	rg := start(t, newRepo(t), "wait")
	ctx := context.Background()
	link, err := rg.client.LoginURL(ctx)
	if err != nil || !strings.HasPrefix(link, rg.base()+"/#code=") {
		t.Fatalf("%q %v", link, err)
	}
	// the page is served to anyone on loopback; the API is not
	resp, err := http.Get(rg.base() + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "taskmoor") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("page: %d %v", resp.StatusCode, resp.Header)
	}
	if code := rg.call(t, "GET", "/api/snapshot", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("snapshot without a token: %d", code)
	}
	tok, code := rg.signIn(t, link)
	if code != 200 || tok == "" {
		t.Fatalf("sign-in: %d", code)
	}
	if _, code := rg.signIn(t, link); code != http.StatusUnauthorized {
		t.Fatalf("a used code signed in again: %d", code)
	}
	if code := rg.call(t, "GET", "/api/snapshot", tok, ""); code != 200 {
		t.Fatalf("snapshot with the browser token: %d", code)
	}
	if code := rg.call(t, "POST", "/api/run", tok, `{"project":"repo","task":"TM-404"}`); code != http.StatusNotFound {
		t.Fatalf("a browser command reaches the handler: %d", code)
	}
	if code := rg.call(t, "POST", "/api/login-code", tok, "{}"); code != http.StatusForbidden {
		t.Fatalf("a browser token made a sign-in link: %d", code)
	}
	if code := rg.call(t, "GET", "/api/snapshot", "nope", ""); code != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %d", code)
	}
	// no cookie is ever set: other local ports get nothing
	resp, _ = http.Post(rg.base()+"/api/sign-in", "application/json", strings.NewReader(`{"code":"x"}`))
	if len(resp.Cookies()) != 0 {
		t.Fatal("a cookie was set")
	}
	resp.Body.Close()
}

func TestBrowserSignInSurvivesARestart(t *testing.T) {
	rg := start(t, newRepo(t), "wait")
	link, _ := rg.client.LoginURL(context.Background())
	tok, _ := rg.signIn(t, link)
	rg.stop()
	rg.open([]string{"FAKE_SCENARIO=wait", "FAKE_LOG=" + rg.log})
	if code := rg.call(t, "GET", "/api/snapshot", tok, ""); code != 200 {
		t.Fatalf("after a restart: %d", code)
	}
	data, _ := os.ReadFile(filepath.Join(rg.paths.Store, "web-sessions.json"))
	if strings.Contains(string(data), tok) {
		t.Fatal("the token is stored in clear")
	}
	if info, _ := os.Stat(filepath.Join(rg.paths.Store, "web-sessions.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestAFailedSignInSaveKeepsTheCode(t *testing.T) {
	rg := start(t, newRepo(t), "wait")
	link, _ := rg.client.LoginURL(context.Background())
	p := filepath.Join(rg.paths.Store, "web-sessions.json")
	_ = os.Remove(p)
	if err := os.Mkdir(p, 0o700); err != nil { // the rename onto a directory fails
		t.Fatal(err)
	}
	if _, code := rg.signIn(t, link); code == 200 {
		t.Fatal("signed in although the token could not be saved")
	}
	_ = os.Remove(p)
	if tok, code := rg.signIn(t, link); code != 200 || tok == "" {
		t.Fatalf("the code was spent by the failed attempt: %d", code)
	}
}

func TestSnapshotRoutesAndProfiles(t *testing.T) {
	repo := newRepo(t, testrepo.Task{ID: "TM-001"}, testrepo.Task{ID: "TM-002", Extra: "profile: nope\n"})
	rg := start(t, repo, "wait")
	snap, err := rg.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Profiles) != 2 || !snap.Profiles[0].Runnable || snap.Profiles[1].Runnable {
		t.Fatalf("profiles %+v", snap.Profiles)
	}
	r := snap.Projects[0].Routes
	if r["TM-001"].Profile != "fake" || !strings.Contains(r["TM-002"].Error, `profile "nope"`) {
		t.Fatalf("routes %+v", r)
	}
}

func TestOpenTheWorktree(t *testing.T) {
	repo := newRepo(t)
	rg := &rig{}
	var mu sync.Mutex
	var opened [][]string
	rg = start(t, repo, "wait")
	rg.stop()
	rg.configure = func(s *serve.Service) {
		s.Opener = func(argv []string) error { mu.Lock(); opened = append(opened, argv); mu.Unlock(); return nil }
	}
	rg.open([]string{"FAKE_SCENARIO=wait", "FAKE_LOG=" + rg.log})
	ctx := context.Background()
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rg.client.Open(ctx, s.ID, "finder"); err != nil {
		t.Fatal(err)
	}
	if err := rg.client.Open(ctx, s.ID, "editor"); err == nil || !strings.Contains(err.Error(), "no editor is configured") {
		t.Fatalf("editor: %v", err)
	}
	cfg, _ := serve.LoadConfig(rg.paths.Config)
	cfg.Editor = []string{"code", "-n"}
	_ = cfg.Save(rg.paths.Config)
	if err := rg.client.Open(ctx, s.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(opened) != 2 || strings.Join(opened[0], " ") != "open "+s.Worktree || strings.Join(opened[1], " ") != "code -n "+s.Worktree {
		t.Fatalf("%v", opened)
	}
}

func TestHandoverToTheTerminal(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	ctx := context.Background()
	s, err := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	if err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateActive)
	got, cmd, err := rg.client.Handover(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != serve.StateHandedOver || !strings.Contains(cmd, "claude --resume "+s.ID) {
		t.Fatalf("%+v %q", got, cmd)
	}
	m := rg.stateFile("TM-001")
	h := m["harness"].(map[string]any)
	if m["lifecycle"] != "in_progress" || h["status"] != "unknown" || h["session"] != nil {
		t.Fatalf("state %v", m)
	}
	// the person finishes from the terminal: the CLI's review works now
	a := &app.App{Dir: repo.Dir, Source: project.SourceMain, CheckOwner: true}
	if _, _, err := a.Review(ctx, "TM-001"); err != nil {
		t.Fatalf("review after the handover: %v", err)
	}
	// and the service does not take the task back
	if _, err := rg.client.Say(ctx, s.ID, "more"); err == nil {
		t.Fatal("say resumed a handed-over session")
	}
}

func TestShutdownWithAnOpenEventStream(t *testing.T) {
	rg := start(t, newRepo(t), "wait")
	epoch, rev, err := rg.client.Position(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := os.ReadFile(filepath.Join(rg.paths.Store, "token"))
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/api/events?epoch=%s&rev=%d", rg.base(), epoch, rev), nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	// the headers arrive at once (no event is pending): the stream is open
	resp, err := (&http.Client{Transport: &http.Transport{ResponseHeaderTimeout: time.Second}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	streaming := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body) // ends when the service closes it
		close(streaming)
	}()
	start := time.Now()
	rg.cancel()
	select {
	case err := <-rg.done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("an open event stream held the service")
	}
	rg.cancel = nil
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	select {
	case <-streaming:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end")
	}
}

func TestChangesOutsideTheServiceReachClients(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "wait")
	epoch, rev, err := rg.client.Position(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got := make(chan time.Time, 16)
	go func() {
		_ = rg.client.Events(ctx, epoch, rev, func(ev serve.Event) bool {
			if ev.Type == "tasks" {
				got <- time.Now()
			}
			return true
		})
	}()
	select { // the watcher's first look: the project is registered
	case <-got:
	case <-ctx.Done():
		t.Fatal("no first event")
	}
	time.Sleep(serve.WatchInterval + 500*time.Millisecond) // its baseline is taken
	for len(got) > 0 {
		<-got
	}
	wrote := time.Now()
	repo.WriteState(testrepo.State{ID: "TM-001"}) // a CLI write, not the service's
	select {
	case at := <-got:
		if at.Before(wrote) {
			t.Fatal("an event from before the write")
		}
	case <-ctx.Done():
		t.Fatal("no tasks event after a change outside the service")
	}
	snap, _ := rg.client.Snapshot(context.Background())
	if snap.Projects[0].Tasks[0].Lifecycle != "in_progress" {
		t.Fatalf("snapshot %s", snap.Projects[0].Tasks[0].Lifecycle)
	}
}

func TestHandoverAndSayDoNotBothWin(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	_ = rg.client.Answer(ctx, a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	var wg sync.WaitGroup
	var handErr error
	wg.Add(2)
	go func() { defer wg.Done(); _, _, handErr = rg.client.Handover(ctx, s.ID) }()
	go func() { defer wg.Done(); _, _ = rg.client.Say(ctx, s.ID, "more") }()
	wg.Wait()
	if handErr != nil {
		// Say came first and holds the launch: a retryable conflict
		var ae *serve.APIError
		if !asAPI(handErr, &ae) || ae.Status != http.StatusConflict {
			t.Fatalf("handover: %v", handErr)
		}
		eventually(t, "the resumed run", func() bool { return rg.session(s.ID).State == serve.StateComplete })
		if _, _, err := rg.client.Handover(ctx, s.ID); err != nil {
			t.Fatalf("handover after say: %v", err)
		}
	}
	// whatever the order, once handed over no runner of the service lives
	eventually(t, "every runner ended", func() bool { starts, exits := rg.lifetimes(); return starts == exits })
	if got := rg.session(s.ID); got.State != serve.StateHandedOver {
		t.Fatalf("state %s", got.State)
	}
	// a repeated handover gives the same command
	if _, cmd, err := rg.client.Handover(ctx, s.ID); err != nil || !strings.Contains(cmd, s.ID) {
		t.Fatalf("repeat: %q %v", cmd, err)
	}
}

func TestInterruptedHandoverIsFinishedOnStart(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	_ = rg.client.Answer(ctx, a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	rg.stop()
	// the journal says handed over; the claim still names the session
	rec := filepath.Join(rg.paths.Store, "sessions", s.ID+".json")
	data, _ := os.ReadFile(rec)
	_ = os.WriteFile(rec, []byte(strings.Replace(string(data), `"state": "complete"`, `"state": "handedOver"`, 1)), 0o600)
	rg.open([]string{"FAKE_SCENARIO=approval", "FAKE_LOG=" + rg.log})
	h := rg.stateFile("TM-001")["harness"].(map[string]any)
	if h["session"] != nil || h["status"] != "unknown" {
		t.Fatalf("harness %v", h)
	}
}

func TestWatcherAnnouncesANewProject(t *testing.T) {
	rg := start(t, newRepo(t), "wait")
	epoch, rev, _ := rg.client.Position(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got := make(chan string, 4)
	go func() {
		_ = rg.client.Events(ctx, epoch, rev, func(ev serve.Event) bool {
			if ev.Type == "tasks" {
				got <- fmt.Sprint(ev.Data)
			}
			return true
		})
	}()
	other := newRepo(t)
	cfg, _ := serve.LoadConfig(rg.paths.Config)
	cfg.Projects = append(cfg.Projects, serve.Project{Name: "other", Path: other.Dir})
	_ = cfg.Save(rg.paths.Config)
	for {
		select {
		case d := <-got:
			if strings.Contains(d, "other") {
				return
			}
		case <-ctx.Done():
			t.Fatal("no tasks event for a project registered while the board was open")
		}
	}
}

func TestFeedActivitiesAreNumbered(t *testing.T) {
	rg := start(t, newRepo(t), "work")
	s, _ := rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	_, acts, err := rg.client.Feed(context.Background(), s.ID)
	if err != nil || len(acts) < 3 {
		t.Fatalf("%v %d", err, len(acts))
	}
	for i, a := range acts {
		if a.Seq != i+1 {
			t.Fatalf("activity %d has seq %d", i, a.Seq)
		}
	}
}

func TestStoppingAResumeBeforeInitIsAStop(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval", "FAKE_RESUME_HOLD=1")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	_ = rg.client.Answer(ctx, a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	if _, err := rg.client.Say(ctx, s.ID, "more"); err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StatePending)
	if _, err := rg.client.Stop(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	rg.waitState(s.ID, serve.StateStopped)
	rg.stop()
	rg.open([]string{"FAKE_SCENARIO=approval", "FAKE_LOG=" + rg.log})
	if st := rg.session(s.ID).State; st != serve.StateStopped {
		t.Fatalf("after a restart: %s", st)
	}
	if lc := rg.stateFile("TM-001")["lifecycle"]; lc != "in_progress" {
		t.Fatalf("lifecycle %v: the stopped resume was read as a completion", lc)
	}
}

func TestInterruptedHandoverWithPendingIntent(t *testing.T) {
	repo := newRepo(t)
	rg := start(t, repo, "approval")
	ctx := context.Background()
	s, _ := rg.client.Run(ctx, serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	_ = rg.client.Answer(ctx, a.ID, serve.Answer{Allow: true})
	rg.waitState(s.ID, serve.StateComplete)
	rg.stop()
	rec := filepath.Join(rg.paths.Store, "sessions", s.ID+".json")
	data, _ := os.ReadFile(rec)
	_ = os.WriteFile(rec, []byte(strings.Replace(string(data), `"resume":`, `"handover_pending": true, "resume":`, 1)), 0o600)
	rg.open([]string{"FAKE_SCENARIO=approval", "FAKE_LOG=" + rg.log})
	if got := rg.session(s.ID); got.State != serve.StateHandedOver || got.HandoverPending {
		t.Fatalf("%+v", got)
	}
	if h := rg.stateFile("TM-001")["harness"].(map[string]any); h["session"] != nil {
		t.Fatalf("harness %v", h)
	}
}
