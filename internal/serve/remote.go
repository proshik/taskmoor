package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// The web from outside (stage 9, v2: C5, C7, A10). The service listens a
// second time, on 127.0.0.1:remote_port, which only the reverse ssh tunnel
// from the person's VPS reaches (Caddy there checks a passkey login first,
// through Pocket ID and oauth2-proxy). That listener knows no sign-in link
// and no API token: a device is paired once with a code shown on the Mac
// and registers a passkey for remote_host; its requests carry the device's
// token. Every dangerous approval from it (A11: Accept, Approve, allowing
// an agent's request) needs a fresh passkey assertion of that device (C7's
// step-up), checked by the service itself. The listener on 127.0.0.1:port
// stays the Mac's own, as before.

// remoteStreamTTL bounds a remote event stream (the board reconnects).
var remoteStreamTTL = 10 * time.Minute

const (
	devicePairTTL = 10 * time.Minute
	ceremonyTTL   = 5 * time.Minute
	stepupTTL     = 2 * time.Minute
	maxPairTries  = 10
)

// Device is a paired remote device.
type Device struct {
	ID        string              `json:"id"`
	Name      string              `json:"name"`
	Created   string              `json:"created"`
	LastSeen  string              `json:"last_seen,omitempty"`
	TokenHash string              `json:"token_hash"`
	Handle    []byte              `json:"handle"` // its WebAuthn user handle
	Cred      webauthn.Credential `json:"credential"`
}

// DeviceView is a device as clients see it.
type DeviceView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Created  string `json:"created"`
	LastSeen string `json:"last_seen,omitempty"`
}

// deviceUser is a device as WebAuthn's user: one passkey per device.
type deviceUser struct {
	handle []byte
	name   string
	creds  []webauthn.Credential
}

func (u *deviceUser) WebAuthnID() []byte                         { return u.handle }
func (u *deviceUser) WebAuthnName() string                       { return u.name }
func (u *deviceUser) WebAuthnDisplayName() string                { return u.name }
func (u *deviceUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

type pairCode struct {
	exp  time.Time
	used bool
}

type ceremony struct {
	kind    string // "pair" or "stepup"
	exp     time.Time
	session webauthn.SessionData
	device  string // stepup: the device
	action  string // stepup: what it allows
	code    string // pair: the code it spends
	name    string // pair: the device's name
	handle  []byte // pair: the new user handle
}

type stepup struct {
	device, action string
	exp            time.Time
}

type remoteState struct {
	mu         sync.Mutex
	loaded     bool
	devices    []*Device
	codes      map[string]pairCode
	tries      int
	ceremonies map[string]*ceremony
	stepups    map[string]stepup // token hash → grant
}

func newRemoteState() remoteState {
	return remoteState{codes: map[string]pairCode{}, ceremonies: map[string]*ceremony{}, stepups: map[string]stepup{}}
}

func (s *Service) devicesPath() string { return filepath.Join(s.Paths.Store, "devices.json") }

// rmLoad reads the devices once; rm.mu is held.
func (s *Service) rmLoad() error {
	if s.rm.loaded {
		return nil
	}
	data, err := os.ReadFile(s.devicesPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(data, &s.rm.devices); err != nil {
			return fmt.Errorf("%s: %w", s.devicesPath(), err)
		}
	}
	s.rm.loaded = true
	return nil
}

// rmSave writes the devices; rm.mu is held.
func (s *Service) rmSave() error {
	if s.rm.devices == nil {
		s.rm.devices = []*Device{}
	}
	return writeJSONFile(s.devicesPath(), s.rm.devices)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// webAuthn is the relying party of remote_host.
func (s *Service) webAuthn() (*webauthn.WebAuthn, error) {
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	if cfg.RemoteHost == "" {
		return nil, refusedf("the web from outside is off (remote_host and remote_port in the config)")
	}
	origins := []string{"https://" + cfg.RemoteHost}
	if cfg.RemoteHost == "localhost" { // a check on the Mac itself: browsers allow passkeys there
		origins = append(origins, fmt.Sprintf("http://localhost:%d", cfg.RemotePort))
	}
	return webauthn.New(&webauthn.Config{RPID: cfg.RemoteHost, RPDisplayName: "taskmoor", RPOrigins: origins})
}

// DevicePairCode makes a one-time code for pairing a device (the Mac's
// CLI only).
func (s *Service) DevicePairCode() (string, error) {
	cfg, err := s.Config()
	if err != nil {
		return "", err
	}
	if cfg.RemoteHost == "" || cfg.RemotePort == 0 {
		return "", refusedf("the web from outside is off: set remote_host and remote_port in the config first")
	}
	// 10 characters a phone can type: ~50 bits, at most 10 tries in all
	const alphabet = "23456789abcdefghjkmnpqrstuvwxyz"
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	code := string(b[:5]) + "-" + string(b[5:])
	s.rm.mu.Lock()
	s.rm.codes = map[string]pairCode{code: {exp: s.Now().Add(devicePairTTL)}} // one code at a time
	s.rm.tries = 0
	s.rm.mu.Unlock()
	return code, nil
}

// Devices lists the paired devices.
func (s *Service) Devices() ([]DeviceView, error) {
	s.rm.mu.Lock()
	defer s.rm.mu.Unlock()
	if err := s.rmLoad(); err != nil {
		return nil, err
	}
	out := []DeviceView{}
	for _, d := range s.rm.devices {
		out = append(out, DeviceView{ID: d.ID, Name: d.Name, Created: d.Created, LastSeen: d.LastSeen})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out, nil
}

// RevokeDevice forgets a device: its token and passkey stop at once.
func (s *Service) RevokeDevice(id string) error {
	// no push is under way to it, and none starts, while it goes
	s.push.gate.Lock()
	defer s.push.gate.Unlock()
	s.rm.mu.Lock()
	defer s.rm.mu.Unlock()
	if err := s.rmLoad(); err != nil {
		return err
	}
	for k, d := range s.rm.devices {
		if d.ID == id {
			// the store first: a revocation that could not be written
			// changes nothing and can be tried again
			left := append(append([]*Device{}, s.rm.devices[:k]...), s.rm.devices[k+1:]...)
			if err := writeJSONFile(s.devicesPath(), left); err != nil {
				return err
			}
			s.rm.devices = left
			// its notifications end with it (sending checks the device too,
			// so a leftover after a failed write reaches no one)
			_ = s.PushUnsubscribe(id, "")
			for t, g := range s.rm.stepups {
				if g.device == id {
					delete(s.rm.stepups, t)
				}
			}
			for c, cer := range s.rm.ceremonies {
				if cer.device == id {
					delete(s.rm.ceremonies, c)
				}
			}
			return nil
		}
	}
	return &NotFound{Msg: fmt.Sprintf("no device %q (taskmoor device list)", id)}
}

// prune drops what expired; rm.mu is held.
func (s *Service) rmPrune() {
	now := s.Now()
	for c, p := range s.rm.codes {
		if now.After(p.exp) {
			delete(s.rm.codes, c)
		}
	}
	for id, c := range s.rm.ceremonies {
		if now.After(c.exp) {
			delete(s.rm.ceremonies, id)
		}
	}
	for t, g := range s.rm.stepups {
		if now.After(g.exp) {
			delete(s.rm.stepups, t)
		}
	}
}

// pairBegin starts registering a device's passkey, given a pairing code.
func (s *Service) pairBegin(code, name string) (string, *protocol.CredentialCreation, error) {
	wa, err := s.webAuthn()
	if err != nil {
		return "", nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "device"
	}
	name = clip(strings.Join(strings.Fields(name), " "), 60)
	code = strings.ToLower(strings.TrimSpace(code))
	s.rm.mu.Lock()
	defer s.rm.mu.Unlock()
	s.rmPrune()
	p, ok := s.rm.codes[code]
	if len(s.rm.ceremonies) >= maxCeremonies {
		return "", nil, refusedf("too many confirmations under way: try again in a few minutes")
	}
	if !ok || p.used {
		s.rm.tries++
		if s.rm.tries >= maxPairTries {
			s.rm.codes = map[string]pairCode{} // too many wrong codes: none is good any more
		}
		return "", nil, refusedf("that pairing code is wrong or spent: make a new one on the Mac (taskmoor device pair)")
	}
	handle := []byte(randomHex(32))
	user := &deviceUser{handle: handle, name: name}
	creation, session, err := wa.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementPreferred, UserVerification: protocol.VerificationRequired}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		return "", nil, err
	}
	p.used = true // one ceremony per code
	s.rm.codes[code] = p
	id := randomHex(16)
	exp := s.Now().Add(ceremonyTTL)
	if p.exp.Before(exp) {
		exp = p.exp // no longer than its code
	}
	s.rm.ceremonies[id] = &ceremony{kind: "pair", exp: exp, session: *session, code: code, name: name, handle: handle}
	return id, creation, nil
}

// pairFinish checks the new passkey and pairs the device: its token is
// returned once and kept only as a hash.
func (s *Service) pairFinish(id string, response []byte) (string, *DeviceView, error) {
	wa, err := s.webAuthn()
	if err != nil {
		return "", nil, err
	}
	s.rm.mu.Lock()
	defer s.rm.mu.Unlock()
	s.rmPrune()
	c := s.rm.ceremonies[id]
	delete(s.rm.ceremonies, id) // one try
	if c == nil || c.kind != "pair" {
		return "", nil, refusedf("this pairing has expired: start again with a new code")
	}
	// its code must still be the one issued: a new code, or too many wrong
	// tries, voids the pairings under way
	if p, ok := s.rm.codes[c.code]; !ok || !p.used || s.Now().After(p.exp) {
		return "", nil, refusedf("this pairing's code is no longer valid: start again with a new code")
	}
	delete(s.rm.codes, c.code)
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		return "", nil, refusedf("the passkey could not be read: %v", err)
	}
	user := &deviceUser{handle: c.handle, name: c.name}
	cred, err := wa.CreateCredential(user, c.session, parsed)
	if err != nil {
		return "", nil, refusedf("the passkey was not accepted: %v", err)
	}
	if err := s.rmLoad(); err != nil {
		return "", nil, err
	}
	tok := randomHex(32)
	d := &Device{ID: "dev-" + randomHex(4), Name: c.name, Created: stamp(s.Now()), TokenHash: hashToken(tok), Handle: c.handle, Cred: *cred}
	s.rm.devices = append(s.rm.devices, d)
	if err := s.rmSave(); err != nil {
		s.rm.devices = s.rm.devices[:len(s.rm.devices)-1]
		return "", nil, err
	}
	return tok, &DeviceView{ID: d.ID, Name: d.Name, Created: d.Created}, nil
}

// deviceFor is the device a request's token names (nil: none).
func (s *Service) deviceFor(r *http.Request) *Device {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Device ")
	if !ok || got == "" {
		return nil
	}
	h := hashToken(got)
	s.rm.mu.Lock()
	defer s.rm.mu.Unlock()
	if s.rmLoad() != nil {
		return nil
	}
	for _, d := range s.rm.devices {
		if subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(h)) == 1 {
			now := s.Now()
			if t, err := time.Parse(time.RFC3339, d.LastSeen); err != nil || now.Sub(t) > time.Hour {
				d.LastSeen = stamp(now)
				_ = s.rmSave()
			}
			cp := *d
			return &cp
		}
	}
	return nil
}

func (s *Service) deviceExists(id string) bool {
	exists, _ := s.deviceState(id)
	return exists
}

// deviceState: whether the device is paired, and whether that is known
// (the devices could be read).
func (s *Service) deviceState(id string) (exists, known bool) {
	s.rm.mu.Lock()
	defer s.rm.mu.Unlock()
	if s.rmLoad() != nil {
		return false, false
	}
	for _, d := range s.rm.devices {
		if d.ID == id {
			return true, true
		}
	}
	return false, true
}

var actionRe = regexp.MustCompile(`^(accept|approve|allow):[A-Za-z0-9._-]{1,128}$`)

const (
	maxCeremoniesPerDevice = 8
	maxCeremonies          = 64
)

// stepupBegin asks the device for a passkey assertion for action.
func (s *Service) stepupBegin(d *Device, action string) (string, *protocol.CredentialAssertion, error) {
	if !actionRe.MatchString(action) {
		return "", nil, refusedf("nothing to confirm by that name")
	}
	wa, err := s.webAuthn()
	if err != nil {
		return "", nil, err
	}
	user := &deviceUser{handle: d.Handle, name: d.Name, creds: []webauthn.Credential{d.Cred}}
	assertion, session, err := wa.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return "", nil, err
	}
	id := randomHex(16)
	s.rm.mu.Lock()
	s.rmPrune()
	mine := 0
	for _, c := range s.rm.ceremonies {
		if c.device == d.ID {
			mine++
		}
	}
	if mine >= maxCeremoniesPerDevice || len(s.rm.ceremonies) >= maxCeremonies {
		s.rm.mu.Unlock()
		return "", nil, refusedf("too many confirmations under way: finish or wait for one")
	}
	s.rm.ceremonies[id] = &ceremony{kind: "stepup", exp: s.Now().Add(ceremonyTTL), session: *session, device: d.ID, action: action}
	s.rm.mu.Unlock()
	return id, assertion, nil
}

// stepupFinish checks the assertion and grants one use of action.
func (s *Service) stepupFinish(d *Device, id string, response []byte) (string, error) {
	wa, err := s.webAuthn()
	if err != nil {
		return "", err
	}
	s.rm.mu.Lock()
	defer s.rm.mu.Unlock()
	s.rmPrune()
	c := s.rm.ceremonies[id]
	delete(s.rm.ceremonies, id)
	if c == nil || c.kind != "stepup" || c.device != d.ID {
		return "", refusedf("this confirmation has expired: try again")
	}
	var cur *Device
	for _, x := range s.rm.devices {
		if x.ID == d.ID {
			cur = x
		}
	}
	if cur == nil {
		return "", refusedf("this device is no longer paired")
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(response)
	if err != nil {
		return "", refusedf("the passkey's answer could not be read: %v", err)
	}
	user := &deviceUser{handle: cur.Handle, name: cur.Name, creds: []webauthn.Credential{cur.Cred}}
	cred, err := wa.ValidateLogin(user, c.session, parsed)
	if err != nil {
		return "", refusedf("the passkey did not confirm it: %v", err)
	}
	if cred.Authenticator.CloneWarning {
		return "", refusedf("the passkey's counter went back (a cloned authenticator?): the device is not trusted; revoke and pair it again")
	}
	old := cur.Cred.Authenticator.SignCount
	cur.Cred.Authenticator.SignCount = cred.Authenticator.SignCount
	if err := s.rmSave(); err != nil {
		cur.Cred.Authenticator.SignCount = old // a counter that was not kept grants nothing
		return "", fmt.Errorf("the passkey's counter could not be kept: %w", err)
	}
	tok := randomHex(24)
	s.rm.stepups[hashToken(tok)] = stepup{device: d.ID, action: c.action, exp: s.Now().Add(stepupTTL)}
	return tok, nil
}

// useStepup spends a grant of action for the device (false: none).
func (s *Service) useStepup(d *Device, action, tok string) bool {
	if tok == "" {
		return false
	}
	s.rm.mu.Lock()
	defer s.rm.mu.Unlock()
	s.rmPrune()
	h := hashToken(tok)
	g, ok := s.rm.stepups[h]
	if !ok || g.device != d.ID || g.action != action {
		return false
	}
	delete(s.rm.stepups, h)
	return true
}

// dangerous names the step-up a remote request needs ("" for none): A11's
// approvals — Accept, Approve, and allowing an agent's request.
func (s *Service) dangerous(r *http.Request) (string, error) {
	if r.Method != http.MethodPost {
		return "", nil
	}
	p := r.URL.Path
	parts := strings.Split(strings.Trim(p, "/"), "/") // api, kind, id, verb
	if len(parts) != 4 || parts[0] != "api" {
		return "", nil
	}
	switch {
	case parts[1] == "sessions" && parts[3] == "accept":
		// the grant names the session itself: a task id (or a project)
		// resolves once, here, and the request goes on with the session's id
		sid, err := s.SessionID(parts[2], r.URL.Query().Get("project"))
		if err != nil {
			return "", err
		}
		r.URL.Path = "/api/sessions/" + sid + "/accept"
		r.URL.RawPath = ""
		q := r.URL.Query()
		q.Del("project")
		r.URL.RawQuery = q.Encode()
		return "accept:" + sid, nil
	case parts[1] == "ideas" && parts[3] == "approve":
		return "approve:" + parts[2], nil
	case parts[1] == "asks" && parts[3] == "answer":
		// an answer allows something only when it says allow; the body must
		// be one JSON object, read here as the handler reads it, so the two
		// cannot disagree
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return "", err
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		if t := bytes.TrimSpace(data); len(t) == 0 || t[0] != '{' {
			return "", refusedf("the answer must be a JSON object")
		}
		var a Answer
		dec := json.NewDecoder(bytes.NewReader(data))
		if err := dec.Decode(&a); err != nil {
			return "", refusedf("the answer is not JSON: %v", err)
		}
		if _, err := dec.Token(); err != io.EOF {
			return "", refusedf("the answer has something after its JSON object")
		}
		if a.Allow {
			return "allow:" + parts[2], nil
		}
	}
	return "", nil
}

// remoteBlocked are the routes a remote device never reaches: they act on
// the Mac's desktop or set up the service's own secrets.
func remoteBlocked(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/open") && strings.HasPrefix(p, "/api/sessions/"):
		return true
	case strings.HasPrefix(p, "/api/telegram/"), strings.HasPrefix(p, "/api/devices"),
		p == "/api/sign-in", p == "/api/proof", p == "/api/login-url":
		return true
	}
	return false
}

type deviceKey struct{}

// RemoteHandler is the listener the tunnel reaches.
func (s *Service) RemoteHandler(host string, port int, api http.Handler) http.Handler {
	hosts := map[string]bool{host: true, host + ":443": true, fmt.Sprintf("%s:%d", host, port): true}
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
		case r.URL.Path == "/api/whoami" && r.Method == http.MethodGet:
			writeJSON(w, http.StatusOK, map[string]any{"remote": true})
			return
		case r.URL.Path == "/api/device/pair/begin" && r.Method == http.MethodPost:
			var body struct{ Code, Name string }
			if err := decode(r, &body); err != nil {
				writeErr(w, err)
				return
			}
			id, opts, err := s.pairBegin(body.Code, body.Name)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": opts})
			return
		case r.URL.Path == "/api/device/pair/finish" && r.Method == http.MethodPost:
			var body struct {
				Ceremony   string          `json:"ceremony"`
				Credential json.RawMessage `json:"credential"`
			}
			if err := decode(r, &body); err != nil {
				writeErr(w, err)
				return
			}
			tok, d, err := s.pairFinish(body.Ceremony, body.Credential)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"token": tok, "device": d})
			return
		case !strings.HasPrefix(r.URL.Path, "/api/"):
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
			w.Header().Set("Cache-Control", "no-store")
			static.ServeHTTP(w, r)
			return
		}
		// the whole request first: what is admitted below (the device, a
		// grant) is checked once it has arrived, so a slow upload cannot
		// outlive a revocation or a grant
		if r.Method != http.MethodGet && r.Body != nil {
			data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
			if err != nil {
				writeErr(w, err)
				return
			}
			if len(data) > 1<<20 {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "the request is too large"})
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
		}
		d := s.deviceFor(r)
		if d == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized: pair this device with a code from the Mac (taskmoor device pair)"})
			return
		}
		if remoteBlocked(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "not from outside: do this on the Mac"})
			return
		}
		switch r.URL.Path {
		case "/api/stepup/begin":
			var body struct{ Action string }
			if err := decode(r, &body); err != nil {
				writeErr(w, err)
				return
			}
			id, opts, err := s.stepupBegin(d, body.Action)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": opts})
			return
		case "/api/stepup/finish":
			var body struct {
				Ceremony  string          `json:"ceremony"`
				Assertion json.RawMessage `json:"assertion"`
			}
			if err := decode(r, &body); err != nil {
				writeErr(w, err)
				return
			}
			tok, err := s.stepupFinish(d, body.Ceremony, body.Assertion)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"stepup": tok})
			return
		}
		action, err := s.dangerous(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if action != "" && !s.useStepup(d, action, r.Header.Get("X-Taskmoor-Stepup")) {
			// C7: each one confirmed with the device's passkey
			writeJSON(w, http.StatusPreconditionRequired, map[string]string{"error": "confirm it with this device's passkey", "stepup": action})
			return
		}
		ctx := context.WithValue(r.Context(), deviceKey{}, d.ID)
		if r.URL.Path == "/api/events" {
			// a revoked device's stream ends; any ends after a while, so the
			// next one passes the VPS's sign-in again
			c, cancel := context.WithTimeout(ctx, remoteStreamTTL)
			defer cancel()
			go func() {
				t := time.NewTicker(2 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-c.Done():
						return
					case <-t.C:
						if !s.deviceExists(d.ID) {
							cancel()
							return
						}
					}
				}
			}()
			ctx = c
		}
		api.ServeHTTP(w, r.WithContext(ctx))
	})
}
