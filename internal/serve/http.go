package serve

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func jsonRaw(v any) (json.RawMessage, error) { return json.Marshal(v) }

// Info is <store>/serve.json: how clients reach the running service.
type Info struct {
	PID   int    `json:"pid"`
	Port  int    `json:"port"`
	Epoch string `json:"epoch"`
}

const (
	lockName  = "serve.lock"
	infoName  = "serve.json"
	tokenName = "token"
)

// Running reports whether a `taskmoor serve` holds the store's lock.
func Running(store string) bool {
	f, err := os.OpenFile(filepath.Join(store, lockName), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// Serve runs an opened service until ctx is done: the API on
// 127.0.0.1:port, token-guarded. It stops the sessions and closes the
// service on the way out.
func Serve(ctx context.Context, s *Service, logf func(string, ...any)) error {
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	token, err := ensureToken(s.Paths.Store)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	epoch, _ := s.bus.position()
	if err := writeJSONFile(filepath.Join(s.Paths.Store, infoName), Info{PID: os.Getpid(), Port: port, Epoch: epoch}); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(s.Paths.Store, infoName))
	srv := &http.Server{Handler: s.Handler(token, port), ReadHeaderTimeout: 10 * time.Second}
	logf("taskmoor serve %s: http://127.0.0.1:%d, store %s, config %s", s.Version, port, s.Paths.Store, s.Paths.Config)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	logf("stopping: ending the running sessions (their claims stay)")
	s.Shutdown() // refuses new launches first, then stops and waits
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

func ensureToken(store string) (string, error) {
	p := filepath.Join(store, tokenName)
	if data, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(data))) >= 32 {
		return strings.TrimSpace(string(data)), nil
	}
	tok := newID() + newID() + newID() + newID() + newID() + newID()
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func writeJSONFile(p string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Handler is the API: every request needs the token and a loopback Host
// (no DNS rebinding from a browser page).
func (s *Service) Handler(token string, port int) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", s.hSnapshot)
	mux.HandleFunc("GET /api/events", s.hEvents)
	mux.HandleFunc("GET /api/sessions/{id}/feed", s.hFeed)
	mux.HandleFunc("POST /api/run", s.hRun)
	mux.HandleFunc("POST /api/sessions/{id}/stop", s.hStop)
	mux.HandleFunc("POST /api/sessions/{id}/say", s.hSay)
	mux.HandleFunc("POST /api/sessions/{id}/release", s.hRelease)
	mux.HandleFunc("POST /api/asks/{id}/answer", s.hAnswer)
	hosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var ref *Refused
	var nf *NotFound
	switch {
	case errors.As(err, &ref):
		code = http.StatusConflict
	case errors.As(err, &nf):
		code = http.StatusNotFound
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &Refused{Msg: "bad request body: " + err.Error()}
	}
	return nil
}

func (s *Service) hSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.Snapshot(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Service) hFeed(w http.ResponseWriter, r *http.Request) {
	id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// the state first, then the feed: an activity is written before the
	// state change it leads to, so a finished state comes with its result
	sess := s.snapshotSession(id)
	acts, err := s.Feed(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if acts == nil {
		acts = []Activity{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "activities": acts})
}

func (s *Service) hRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	// the session outlives the request
	sess, err := s.Run(context.WithoutCancel(r.Context()), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Service) hStop(w http.ResponseWriter, r *http.Request) {
	id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
	if err == nil {
		err = s.Stop(id)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotSession(id))
}

func (s *Service) hSay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
	if err == nil {
		err = s.Say(context.WithoutCancel(r.Context()), id, body.Text)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.snapshotSession(id))
}

func (s *Service) hRelease(w http.ResponseWriter, r *http.Request) {
	id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
	var after string
	if err == nil {
		after, err = s.Release(context.WithoutCancel(r.Context()), id)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": s.snapshotSession(id), "lifecycle": after})
}

func (s *Service) hAnswer(w http.ResponseWriter, r *http.Request) {
	var ans Answer
	if err := decode(r, &ans); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.AnswerAsk(r.PathValue("id"), ans); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"answered": r.PathValue("id")})
}

// hEvents streams changes as server-sent events. A client passes the
// epoch and rev it has; a different epoch gets a "reset" first, telling it
// to take a new snapshot.
func (s *Service) hEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.bus.subscribe()
	defer cancel()
	epoch, rev := s.bus.position()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	send := func(ev Event) bool {
		data, err := json.Marshal(ev)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "id: %s:%d\ndata: %s\n\n", ev.Epoch, ev.Rev, data); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if q := r.URL.Query(); q.Get("epoch") != epoch {
		if !send(Event{Epoch: epoch, Rev: rev, Type: "reset"}) {
			return
		}
	} else if have, _ := strconv.ParseInt(q.Get("rev"), 10, 64); have != rev {
		// deltas are not kept for replay: a client that missed some takes a
		// new snapshot
		if !send(Event{Epoch: epoch, Rev: rev, Type: "reset"}) {
			return
		}
	}
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				send(Event{Epoch: epoch, Type: "reset"})
				return
			}
			if !send(ev) {
				return
			}
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
