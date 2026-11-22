package serve

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/proshik/taskmoor/internal/state"
)

//go:embed web
var webFiles embed.FS

const (
	codeTTL    = 10 * time.Minute
	browserTTL = 30 * 24 * time.Hour
)

// webAuth keeps the one-time sign-in codes (in memory) and the browser
// tokens — as SHA-256 hashes with their expiry in <store>/web-sessions.json
// (mode 0600), so a restart does not sign browsers out.
//
// A browser token is not a cookie: the page keeps it in localStorage, which
// belongs to its origin (scheme, host and port), and sends it in the
// Authorization header. No other site or local port receives it, and a
// page of another origin has no ambient credential to act with.
type webAuth struct {
	mu      sync.Mutex
	codes   map[string]time.Time // code → expiry
	cookies map[string]time.Time // hash of a browser token → expiry
	now     func() time.Time
	path    string
}

func newWebAuth(now func() time.Time, path string) *webAuth {
	a := &webAuth{codes: map[string]time.Time{}, cookies: map[string]time.Time{}, now: now, path: path}
	if data, err := os.ReadFile(path); err == nil {
		var saved map[string]time.Time
		if json.Unmarshal(data, &saved) == nil {
			for h, exp := range saved {
				if now().Before(exp) {
					a.cookies[h] = exp
				}
			}
		}
	}
	return a
}

func hashCookie(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func (a *webAuth) save() error {
	if a.path == "" {
		return nil
	}
	return writeJSONFile(a.path, a.cookies)
}

// newCode makes a one-time login code.
func (a *webAuth) newCode() string {
	c := newID() + newID() + newID()
	a.mu.Lock()
	a.codes[c] = a.now().Add(codeTTL)
	a.mu.Unlock()
	return c
}

// errBadCode: the sign-in code is unknown, used or expired.
var errBadCode = errors.New("this sign-in link is used or expired; run `taskmoor web` for a new one")

// redeem spends a code for a browser token. The code is spent only once
// the token's hash is saved, so a failed save can be retried.
func (a *webAuth) redeem(code string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.codes[code]
	if !ok || a.now().After(exp) {
		delete(a.codes, code)
		return "", errBadCode
	}
	v := newID() + newID() + newID() + newID()
	h := hashCookie(v)
	a.cookies[h] = a.now().Add(browserTTL)
	if err := a.save(); err != nil {
		delete(a.cookies, h)
		return "", fmt.Errorf("the sign-in could not be saved: %w", err)
	}
	delete(a.codes, code)
	return v, nil
}

func (a *webAuth) valid(tok string) bool { _, ok := a.expiry(tok); return ok }

// expiry returns a live browser token's expiry.
func (a *webAuth) expiry(tok string) (time.Time, bool) {
	if tok == "" {
		return time.Time{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.cookies[hashCookie(tok)]
	return exp, ok && a.now().Before(exp)
}

// LoginURL makes a one-time sign-in link for the browser. The code is in
// the fragment, which the browser never sends to a server; the page trades
// it for its token.
func (s *Service) LoginURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/#code=%s", port, s.auth.newCode())
}

// Credential kinds.
const (
	credNone    = ""
	credAPI     = "api"     // the store's API token: CLI clients
	credBrowser = "browser" // a signed-in board
)

// authorize names the credential of a request: the Authorization header
// carries the API token or a browser token; nothing else is accepted. A
// browser token comes with its expiry (zero for the API token).
func (s *Service) authorize(r *http.Request, token string) (string, time.Time) {
	got, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case !found:
		return credNone, time.Time{}
	case subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1:
		return credAPI, time.Time{}
	}
	if exp, ok := s.auth.expiry(got); ok {
		return credBrowser, exp
	}
	return credNone, time.Time{}
}

type expiryKey struct{}

// hSignIn trades a one-time code for a browser token (no credential
// needed: the code is the proof).
func (s *Service) hSignIn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	tok, err := s.auth.redeem(body.Code)
	switch {
	case errors.Is(err, errBadCode):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	case err != nil:
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

func staticFiles() http.Handler {
	sub, _ := fs.Sub(webFiles, "web")
	return http.FileServer(http.FS(sub))
}

// hTask returns one task's file text for the board's card.
func (s *Service) hTask(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Config()
	if err != nil {
		writeErr(w, err)
		return
	}
	p := cfg.ProjectByName(r.PathValue("project"))
	if p == nil {
		writeErr(w, &NotFound{Msg: "no such project"})
		return
	}
	rep, err := s.appFor(*p).Report(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	t, ok := rep.Find(r.PathValue("id"))
	if !ok {
		writeErr(w, &NotFound{Msg: "no such task"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "body": t.Task.Body, "after_release": t.Task.AfterRelease,
		"profile": t.Task.Profile, "model": t.Task.Model})
}

// hOpen opens a session's worktree on the Mac: in Finder, or with the
// configured editor command.
func (s *Service) hOpen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		With string `json:"with"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
	if err == nil {
		err = s.Open(id, body.With)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"opened": body.With})
}

// Open shows a session's worktree: with "finder" (`open DIR`) or "editor"
// (the configured editor command with DIR as its last argument).
func (s *Service) Open(id, with string) error {
	l, err := s.byID(id)
	if err != nil {
		return err
	}
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	var argv []string
	switch with {
	case "finder", "":
		argv = []string{"open", l.s.Worktree}
	case "editor":
		if len(cfg.Editor) == 0 {
			return refusedf("no editor is configured (editor = [\"code\"] in the config)")
		}
		argv = append(append([]string(nil), cfg.Editor...), l.s.Worktree)
	default:
		return refusedf("open with %q: finder or editor", with)
	}
	return s.Opener(argv)
}

func runDetached(argv []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return refusedf("%s: %v", argv[0], err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func (s *Service) hHandover(w http.ResponseWriter, r *http.Request) {
	id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
	var cmd string
	if err == nil {
		cmd, err = s.Handover(context.WithoutCancel(r.Context()), id)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": s.snapshotSession(id), "resume": cmd})
}

// Handover is "Continue in terminal" without Guild: the service stops the
// runner, lets go of the task (owner lock, harness.session) and returns
// the command that continues the conversation; the claim stays, so the
// person finishes the task from the terminal (`taskmoor review`).
//
// The handover holds the session's launch admission from start to end, so
// no Say can start a runner in between; the session is recorded as handed
// over before the claim changes, so a crash in between is finished on the
// next start; a repeated handover returns the same command.
func (s *Service) Handover(ctx context.Context, id string) (string, error) {
	l, err := s.byID(id)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if l.s.State == StateHandedOver && !l.s.HandoverPending {
		cmd := l.s.Resume
		s.mu.Unlock()
		return cmd, nil // done before: the same command
	}
	if l.starting || l.handingOver {
		s.mu.Unlock()
		return "", refusedf("session %s is starting or being handed over; try again in a moment", l.s.ID)
	}
	l.handingOver = true
	r := l.runner
	l.stopping = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		l.handingOver = false
		s.mu.Unlock()
	}()
	if r != nil {
		r.stop(5 * time.Second)
		<-r.done
	}
	if err := s.attach(l); err != nil {
		return "", err
	}
	if !l.s.HandoverPending && !s.ownsClaim(l) {
		return "", refusedf("%s's claim is not this session's", l.s.Task)
	}
	// the intent first, durably: a crash after this is finished by Open
	s.mu.Lock()
	l.s.HandoverPending = true
	s.mu.Unlock()
	if err := s.saveAndPublishErr(l); err != nil {
		return "", fmt.Errorf("the handover could not be recorded: %w", err)
	}
	if err := s.letGo(l); err != nil {
		return "", fmt.Errorf("the handover is recorded but not finished (try again): %w", err)
	}
	if err := s.completeHandover(l); err != nil {
		return "", err
	}
	s.addActivity(l, Activity{Type: ActResponse, Text: "continued in the terminal: " + l.s.Resume, By: "person"})
	return l.s.Resume, nil
}

// finishHandover completes an interrupted handover found on start.
func (s *Service) finishHandover(l *live) {
	if s.ownsClaim(l) {
		if err := s.letGo(l); err != nil {
			return // left pending; a repeated Continue finishes it
		}
	}
	_ = s.completeHandover(l)
}

func (s *Service) completeHandover(l *live) error {
	s.mu.Lock()
	l.s.State = StateHandedOver
	l.s.HandoverPending = false
	s.mu.Unlock()
	return s.saveAndPublishErr(l)
}

// letGo clears the session from its claim (harness unknown, no session)
// and drops the owner lock: the task is the person's now.
func (s *Service) letGo(l *live) error {
	err := s.update(l, func(f *state.File) {
		f.Harness = &state.Harness{Name: l.s.Harness, Status: "unknown", ObservedAt: stamp(s.Now())}
	})
	if err != nil && !errors.Is(err, errNotOurs) {
		return err
	}
	s.dropOwner(l)
	return nil
}
