package serve

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	logf("open the board: %s (one-time link; `taskmoor web` makes a new one)", s.LoginURL(port))
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	wctx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go s.watch(wctx)
	go s.pollUsage(wctx)        // the plans' usage (stage 6)
	s.startTelegram(wctx, port) // the bot (stage 7)
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	logf("stopping: ending the running sessions (their claims stay)")
	s.Shutdown()     // refuses new launches first, then stops and waits
	s.bus.closeAll() // event streams end, so open browsers do not hold the server
	stopWatch()
	s.stopTelegram()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close() // a connection that would not finish: cut it
	}
	return nil
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
	return writeAtomic(p, append(data, '\n'))
}

// writeAtomic writes p through a temporary file of its own: two writers
// never share one (a shared name could leave one's tail in the other's).
func writeAtomic(p string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Handler is the API: every request needs the token and a loopback Host
// (no DNS rebinding from a browser page).
func (s *Service) Handler(token string, port int) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", s.hSnapshot)
	mux.HandleFunc("GET /api/events", s.hEvents)
	mux.HandleFunc("GET /api/position", func(w http.ResponseWriter, r *http.Request) {
		epoch, rev := s.bus.position()
		writeJSON(w, http.StatusOK, map[string]any{"epoch": epoch, "rev": rev})
	})
	mux.HandleFunc("GET /api/sessions/{id}/feed", s.hFeed)
	mux.HandleFunc("POST /api/run", s.hRun)
	mux.HandleFunc("POST /api/sessions/{id}/stop", s.hStop)
	mux.HandleFunc("POST /api/sessions/{id}/say", s.hSay)
	mux.HandleFunc("POST /api/sessions/{id}/release", s.hRelease)
	mux.HandleFunc("POST /api/asks/{id}/answer", s.hAnswer)
	mux.HandleFunc("POST /api/sessions/{id}/open", s.hOpen)
	mux.HandleFunc("POST /api/sessions/{id}/handover", s.hHandover)
	mux.HandleFunc("POST /api/sessions/{id}/restart", s.hRestart)
	mux.HandleFunc("POST /api/projects/{project}/keep", s.hKeep)
	mux.HandleFunc("GET /api/sessions/{id}/acceptance", func(w http.ResponseWriter, r *http.Request) {
		id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
		var a *Acceptance
		if err == nil {
			a, err = s.Acceptance(r.Context(), id)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a)
	})
	mux.HandleFunc("POST /api/sessions/{id}/accept", func(w http.ResponseWriter, r *http.Request) {
		id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
		var res *AcceptResult
		if err == nil {
			var body struct {
				Tip string `json:"tip"`
			}
			if err = decode(r, &body); err == nil {
				res, err = s.Accept(context.WithoutCancel(r.Context()), id, body.Tip)
			}
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/ideas", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Project, Title, Text string }
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		i, err := s.AddIdea(body.Project, body.Title, body.Text)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, i)
	})
	mux.HandleFunc("GET /api/ideas", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Ideas())
	})
	mux.HandleFunc("GET /api/ideas/{id}", func(w http.ResponseWriter, r *http.Request) {
		i, err := s.IdeaByID(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, i)
	})
	mux.HandleFunc("POST /api/ideas/{id}/grill", func(w http.ResponseWriter, r *http.Request) {
		var body GrillRequest
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		sess, err := s.Grill(context.WithoutCancel(r.Context()), r.PathValue("id"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sess)
	})
	mux.HandleFunc("GET /api/ideas/{id}/review", func(w http.ResponseWriter, r *http.Request) {
		rv, err := s.ReviewIdea(r.Context(), r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rv)
	})
	mux.HandleFunc("POST /api/ideas/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tip  string `json:"tip"`
			Main string `json:"main"`
		}
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		res, err := s.Approve(context.WithoutCancel(r.Context()), r.PathValue("id"), body.Tip, body.Main)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/ideas/{id}/drop", func(w http.ResponseWriter, r *http.Request) {
		i, err := s.DropIdea(r.PathValue("id"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, i)
	})
	mux.HandleFunc("GET /api/telegram", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.TelegramStatus())
	})
	mux.HandleFunc("POST /api/telegram/setup", func(w http.ResponseWriter, r *http.Request) {
		if kind, _ := s.authorize(r, token); kind != credAPI { // the CLI only: the token comes from a terminal
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "set the bot up with taskmoor telegram setup"})
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		st, err := s.TelegramSetup(r.Context(), body.Token)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("POST /api/telegram/pair", func(w http.ResponseWriter, r *http.Request) {
		code, bot, err := s.TelegramPair()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"code": code, "bot": bot})
	})
	mux.HandleFunc("POST /api/telegram/off", func(w http.ResponseWriter, r *http.Request) {
		if err := s.TelegramOff(); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.TelegramStatus())
	})
	mux.HandleFunc("GET /api/decisions", func(w http.ResponseWriter, r *http.Request) {
		ds, err := s.Decisions(0) // all: every override id stays findable
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ds)
	})
	mux.HandleFunc("POST /api/decisions/{id}/override", func(w http.ResponseWriter, r *http.Request) {
		var ans Answer
		if err := decode(r, &ans); err != nil {
			writeErr(w, err)
			return
		}
		d, err := s.Override(context.WithoutCancel(r.Context()), r.PathValue("id"), ans)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	})
	mux.HandleFunc("POST /api/login-code", func(w http.ResponseWriter, r *http.Request) {
		// a sign-in link is for whoever holds the API token (the CLI), not
		// for a board that is already signed in
		if kind, _ := s.authorize(r, token); kind != credAPI {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "a sign-in link needs the API token"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": s.LoginURL(port)})
	})
	mux.HandleFunc("GET /api/projects/{project}/tasks/{id}", s.hTask)
	hosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	static := staticFiles()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		switch {
		case r.URL.Path == "/api/sign-in" && r.Method == http.MethodPost:
			s.hSignIn(w, r)
			return
		case r.URL.Path == "/api/proof" && r.Method == http.MethodGet:
			// the service shows it holds the API token without anyone
			// sending it: a client checks this before handing over a secret
			writeJSON(w, http.StatusOK, map[string]string{"proof": proof(token, r.URL.Query().Get("nonce"))})
			return
		case !strings.HasPrefix(r.URL.Path, "/api/"):
			// the page itself holds nothing secret; its API calls need a token
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
			w.Header().Set("Cache-Control", "no-store")
			static.ServeHTTP(w, r)
			return
		}
		kind, exp := s.authorize(r, token)
		if kind == credNone {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: run `taskmoor web` to sign this browser in"})
			return
		}
		if !exp.IsZero() { // a long-lived stream ends when the token does
			r = r.WithContext(context.WithValue(r.Context(), expiryKey{}, exp))
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

func (s *Service) hRestart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Profile string `json:"profile"`
		Anyway  bool   `json:"anyway"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
	var out *Session
	if err == nil {
		out, err = s.Restart(context.WithoutCancel(r.Context()), id, body.Profile, body.Anyway)
	}
	if err != nil && out == nil {
		writeErr(w, err)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"session": out, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": out})
}

func (s *Service) hKeep(w http.ResponseWriter, r *http.Request) {
	var body struct {
		N int `json:"n"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.SetKeepBusy(r.PathValue("project"), body.N); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": r.PathValue("project"), "keep_busy": body.N})
}

func (s *Service) hSay(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string `json:"text"`
		Anyway bool   `json:"anyway"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.SessionID(r.PathValue("id"), r.URL.Query().Get("project"))
	if err == nil {
		err = s.sayGuarded(context.WithoutCancel(r.Context()), id, body.Text, body.Anyway)
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
	fl.Flush() // the client sees the stream open now, not at the first event
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
	var expired <-chan time.Time
	if exp, ok := r.Context().Value(expiryKey{}).(time.Time); ok {
		t := time.NewTimer(time.Until(exp))
		defer t.Stop()
		expired = t.C
	}
	for {
		select {
		case <-expired:
			return
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

// proof is HMAC-SHA256(token, "taskmoor-proof:"+nonce), hex.
func proof(token, nonce string) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("taskmoor-proof:" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}
