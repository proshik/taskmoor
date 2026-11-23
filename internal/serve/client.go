package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client talks to a running service.
type Client struct {
	base    string
	token   string
	http    *http.Client
	project string
}

// ErrNotRunning means no service is running for the store.
var ErrNotRunning = errors.New("taskmoor serve is not running (start it with `taskmoor serve`)")

// Connect reads <store>/serve.json and the token.
func Connect(store string) (*Client, error) {
	if !Running(store) {
		return nil, ErrNotRunning
	}
	data, err := os.ReadFile(filepath.Join(store, infoName))
	if err != nil {
		return nil, ErrNotRunning
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	tok, err := os.ReadFile(filepath.Join(store, tokenName))
	if err != nil {
		return nil, err
	}
	return &Client{base: fmt.Sprintf("http://127.0.0.1:%d", info.Port), token: strings.TrimSpace(string(tok)), http: &http.Client{}}, nil
}

// APIError is an error the service returned.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

// Refused reports whether the service declined the request (HTTP 409/404).
func (e *APIError) Refused() bool {
	return e.Status == http.StatusConflict || e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &APIError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Snapshot fetches everything.
func (c *Client) Snapshot(ctx context.Context) (*Snapshot, error) {
	var s Snapshot
	return &s, c.do(ctx, http.MethodGet, "/api/snapshot", nil, &s)
}

// Position is the service's (epoch, rev): it changes with every event.
func (c *Client) Position(ctx context.Context) (string, int64, error) {
	var out struct {
		Epoch string `json:"epoch"`
		Rev   int64  `json:"rev"`
	}
	err := c.do(ctx, http.MethodGet, "/api/position", nil, &out)
	return out.Epoch, out.Rev, err
}

// Run starts a task.
func (c *Client) Run(ctx context.Context, req RunRequest) (*Session, error) {
	var s Session
	return &s, c.do(ctx, http.MethodPost, "/api/run", req, &s)
}

// Project scopes task-id lookups (Feed, Stop, Say, Release) to a project;
// "" searches all and fails on ambiguity.
func (c *Client) Project(name string) *Client {
	cc := *c
	cc.project = name
	return &cc
}

func (c *Client) sessionPath(id, action string) string {
	p := "/api/sessions/" + url.PathEscape(id) + "/" + action
	if c.project != "" {
		p += "?project=" + url.QueryEscape(c.project)
	}
	return p
}

// Feed returns a session (or a task's latest session) and its activities.
func (c *Client) Feed(ctx context.Context, id string) (*Session, []Activity, error) {
	var out struct {
		Session    *Session   `json:"session"`
		Activities []Activity `json:"activities"`
	}
	err := c.do(ctx, http.MethodGet, c.sessionPath(id, "feed"), nil, &out)
	return out.Session, out.Activities, err
}

// Stop stops a session.
func (c *Client) Stop(ctx context.Context, id string) (*Session, error) {
	var s Session
	return &s, c.do(ctx, http.MethodPost, c.sessionPath(id, "stop"), map[string]any{}, &s)
}

// Say sends an instruction.
func (c *Client) Say(ctx context.Context, id, text string) (*Session, error) {
	var s Session
	return &s, c.do(ctx, http.MethodPost, c.sessionPath(id, "say"), map[string]string{"text": text}, &s)
}

// Release hands a task back: the session stops and the state file goes.
func (c *Client) Release(ctx context.Context, id string) (*Session, string, error) {
	var out struct {
		Session   *Session `json:"session"`
		Lifecycle string   `json:"lifecycle"`
	}
	err := c.do(ctx, http.MethodPost, c.sessionPath(id, "release"), map[string]any{}, &out)
	return out.Session, out.Lifecycle, err
}

// Open shows a session's worktree on the Mac ("finder" or "editor").
func (c *Client) Open(ctx context.Context, id, with string) error {
	return c.do(ctx, http.MethodPost, c.sessionPath(id, "open"), map[string]string{"with": with}, nil)
}

// Handover stops the session and hands it to the person's terminal; it
// returns the command that continues it.
func (c *Client) Handover(ctx context.Context, id string) (*Session, string, error) {
	var out struct {
		Session *Session `json:"session"`
		Resume  string   `json:"resume"`
	}
	err := c.do(ctx, http.MethodPost, c.sessionPath(id, "handover"), map[string]any{}, &out)
	return out.Session, out.Resume, err
}

// Restart restarts a session with another profile (A24); the new session
// is returned, also when its start failed (err says why).
func (c *Client) Restart(ctx context.Context, id, profile string) (*Session, error) {
	var out struct {
		Session *Session `json:"session"`
		Error   string   `json:"error"`
	}
	if err := c.do(ctx, http.MethodPost, c.sessionPath(id, "restart"), map[string]any{"profile": profile}, &out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return out.Session, errors.New(out.Error)
	}
	return out.Session, nil
}

// Acceptance is a session's acceptance view.
func (c *Client) Acceptance(ctx context.Context, id string) (*Acceptance, error) {
	var out Acceptance
	err := c.do(ctx, http.MethodGet, c.sessionPath(id, "acceptance"), nil, &out)
	return &out, err
}

// Accept merges a session's branch into main (A8): tip is the commit the
// person looked at ("" takes the branch as it is).
func (c *Client) Accept(ctx context.Context, id, tip string) (*AcceptResult, error) {
	var out AcceptResult
	err := c.do(ctx, http.MethodPost, c.sessionPath(id, "accept"), map[string]any{"tip": tip}, &out)
	return &out, err
}

// Decisions lists the dispatcher's decisions, newest first.
func (c *Client) Decisions(ctx context.Context) ([]Ruling, error) {
	var out []Ruling
	err := c.do(ctx, http.MethodGet, "/api/decisions", nil, &out)
	return out, err
}

// Override replaces a dispatcher answer with the person's (C3).
func (c *Client) Override(ctx context.Context, id string, ans Answer) (*Ruling, error) {
	var out Ruling
	err := c.do(ctx, http.MethodPost, "/api/decisions/"+url.PathEscape(id)+"/override", ans, &out)
	return &out, err
}

// Keep sets a project's "Keep N busy" (A4).
func (c *Client) Keep(ctx context.Context, project string, n int) error {
	return c.do(ctx, http.MethodPost, "/api/projects/"+url.PathEscape(project)+"/keep", map[string]any{"n": n}, nil)
}

// LoginURL asks for a one-time sign-in link for the browser.
func (c *Client) LoginURL(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	err := c.do(ctx, http.MethodPost, "/api/login-code", map[string]any{}, &out)
	return out.URL, err
}

// Answer answers an ask.
func (c *Client) Answer(ctx context.Context, id string, ans Answer) error {
	return c.do(ctx, http.MethodPost, "/api/asks/"+url.PathEscape(id)+"/answer", ans, nil)
}

// Events streams events until ctx ends or the stream breaks; it calls f
// for each one.
func (c *Client) Events(ctx context.Context, epoch string, rev int64, f func(Event) bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/events?epoch=%s&rev=%d", c.base, url.QueryEscape(epoch), rev), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	cl := &http.Client{Timeout: 0}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, Msg: resp.Status}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev Event
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if !f(ev) {
			return nil
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

// WaitReady waits until a service answers (for tests and scripts).
func WaitReady(ctx context.Context, store string) (*Client, error) {
	for {
		c, err := Connect(store)
		if err == nil {
			if _, err = c.Snapshot(ctx); err == nil {
				return c, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(50 * time.Millisecond):
		}
	}
}
