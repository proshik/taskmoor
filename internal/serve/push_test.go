package serve_test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/proshik/taskmoor/internal/serve"
)

// fakePush is a browser's push service and the browser's keys.
type fakePush struct {
	mu     sync.Mutex
	key    *ecdh.PrivateKey
	auth   []byte
	got    []map[string]string
	status int
	once   int           // answer the next ones 429 (Retry-After: 1)
	hold   chan struct{} // non-nil: a POST waits for it
	srv    *httptest.Server
}

func newFakePush(t *testing.T) *fakePush {
	k, _ := ecdh.P256().GenerateKey(rand.Reader)
	f := &fakePush{key: k, auth: make([]byte, 16), status: 201}
	_, _ = rand.Read(f.auth)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		msg := f.decrypt(t, body)
		f.mu.Lock()
		if h := f.hold; h != nil {
			f.mu.Unlock()
			<-h
			f.mu.Lock()
		}
		st := f.status
		if f.once > 0 {
			f.once--
			st = 429
			w.Header().Set("Retry-After", "1")
		} else {
			f.got = append(f.got, map[string]string{"auth": r.Header.Get("Authorization"), "enc": r.Header.Get("Content-Encoding"), "msg": msg, "path": r.URL.Path})
		}
		f.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePush) decrypt(t *testing.T, body []byte) string {
	if len(body) < 21 {
		return ""
	}
	salt, idlen := body[:16], int(body[20])
	asPub := body[21 : 21+idlen]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		return ""
	}
	shared, _ := f.key.ECDH(as)
	prkKey, _ := hkdf.Extract(sha256.New, shared, f.auth)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, string(append(append([]byte("WebPush: info\x00"), f.key.PublicKey().Bytes()...), asPub...)), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil || len(plain) == 0 {
		return ""
	}
	return string(plain[:len(plain)-1])
}

func (f *fakePush) sub() map[string]any { return f.subAt("/push/abc") }

func (f *fakePush) subAt(path string) map[string]any {
	return map[string]any{"endpoint": f.srv.URL + path, "keys": map[string]string{
		"p256dh": b64u.EncodeToString(f.key.PublicKey().Bytes()), "auth": b64u.EncodeToString(f.auth)}}
}

func (f *fakePush) messages() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string{}, f.got...)
}

func TestPushToADevice(t *testing.T) {
	fp := newFakePush(t)
	restore := serve.SetPushTarget(fp.srv.Client().Transport)
	oldRemind := serve.TelegramRemind
	serve.TelegramRemind = 0                                          // the reminder is due at once
	t.Cleanup(func() { restore(); serve.TelegramRemind = oldRemind }) // after the service stopped
	rg, rc, k := startRemote(t, "approval")
	pair(t, rg, rc, k)
	st, b := rc.do("GET", "/api/push/key", nil, nil)
	if st != 200 || len(b["key"].(string)) < 80 {
		t.Fatalf("%d %v", st, b)
	}
	// the Mac's own listener has no device: no subscriptions there
	if _, err := rg.client.Snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, b := rc.do("POST", "/api/push/subscribe", fp.sub(), nil); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	_, _ = rg.client.Run(t.Context(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	eventually(t, "the push", func() bool { return len(fp.messages()) == 1 })
	m := fp.messages()[0]
	var msg map[string]string
	if err := json.Unmarshal([]byte(m["msg"]), &msg); err != nil {
		t.Fatalf("%q", m["msg"])
	}
	if m["enc"] != "aes128gcm" || !strings.HasPrefix(m["auth"], "vapid t=") || msg["title"] != "repo · TM-001" ||
		msg["body"] != "An approval waits for you" || msg["url"] != "/#inbox" || msg["tag"] != "ask-"+a.ID {
		t.Fatalf("%v %v", m, msg)
	}
	// a subscription the browser let go (410) is forgotten
	fp.mu.Lock()
	fp.status = 410
	fp.mu.Unlock()
	rg.svc.TelegramPass() // the reminder (A20) is pushed too
	eventually(t, "the reminder", func() bool { return len(fp.messages()) == 2 })
	if !strings.Contains(fp.messages()[1]["msg"], "Still waiting") {
		t.Fatalf("%v", fp.messages()[1])
	}
	eventually(t, "the gone subscription forgotten", func() bool {
		st, b := rc.do("GET", "/api/push/key?endpoint="+fp.sub()["endpoint"].(string), nil, nil)
		return st == 200 && b["subscribed"] == false
	})
	if err := rg.client.Answer(t.Context(), a.ID, serve.Answer{Allow: false}); err != nil {
		t.Fatal(err)
	}
}

func TestPushSubscriptionsChecked(t *testing.T) {
	rg, rc, k := startRemote(t, "work")
	pair(t, rg, rc, k)
	fp := newFakePush(t)
	// not a push service's host (the real check)
	if st, _ := rc.do("POST", "/api/push/subscribe", fp.sub(), nil); st == 200 {
		t.Fatal("any endpoint taken")
	}
	bad := fp.sub()
	bad["endpoint"] = "https://fcm.googleapis.com/fcm/send/x"
	bad["keys"] = map[string]string{"p256dh": "AAAA", "auth": "AAAA"}
	if st, _ := rc.do("POST", "/api/push/subscribe", bad, nil); st == 200 {
		t.Fatal("bad keys taken")
	}
	good := fp.sub()
	good["endpoint"] = "https://fcm.googleapis.com/fcm/send/x"
	if st, b := rc.do("POST", "/api/push/subscribe", good, nil); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	// revoking the device ends its subscriptions
	ds, _ := rg.client.Devices(t.Context())
	if err := rg.client.RevokeDevice(t.Context(), ds[0].ID); err != nil {
		t.Fatal(err)
	}
	pair(t, rg, rc, newSoftKey(t, k.rpID, k.origin))
	if st, b := rc.do("GET", "/api/push/key?endpoint=https://fcm.googleapis.com/fcm/send/x", nil, nil); st != 200 || b["subscribed"] != false {
		t.Fatalf("%d %v", st, b)
	}
}

// The board as an app: its manifest and service worker are served.
func TestPWAFiles(t *testing.T) {
	_, rc, _ := startRemote(t, "work")
	for path, typ := range map[string]string{"/manifest.webmanifest": "application/manifest+json", "/sw.js": "javascript", "/icon-192.png": "image/png", "/apple-touch-icon.png": "image/png"} {
		req, _ := http.NewRequest("GET", rc.base+path, nil)
		req.Host = rc.host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), typ) {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

// A push refused for now (429) is tried again; a revoked device gets
// nothing more, the other one goes on.
func TestPushRetryAndRevocation(t *testing.T) {
	fp := newFakePush(t)
	restore := serve.SetPushTarget(fp.srv.Client().Transport)
	oldRemind := serve.TelegramRemind
	serve.TelegramRemind = 0
	t.Cleanup(func() { restore(); serve.TelegramRemind = oldRemind })
	rg, rc, k := startRemote(t, "approval")
	pair(t, rg, rc, k)
	ds, _ := rg.client.Devices(t.Context())
	devA := ds[0].ID // the first device, subscribed at /push/a
	if st, b := rc.do("POST", "/api/push/subscribe", fp.subAt("/push/a"), nil); st != 200 {
		t.Fatal(b)
	}
	rc2 := *rc
	pair(t, rg, &rc2, newSoftKey(t, k.rpID, k.origin))
	if st, b := rc2.do("POST", "/api/push/subscribe", fp.subAt("/push/b"), nil); st != 200 {
		t.Fatal(b)
	}
	fp.mu.Lock()
	fp.once = 1 // the first POST is refused for now
	fp.mu.Unlock()
	_, _ = rg.client.Run(t.Context(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	eventually(t, "both devices, one after a retry", func() bool { return len(fp.messages()) == 2 })
	if err := rg.client.RevokeDevice(t.Context(), devA); err != nil {
		t.Fatal(err)
	}
	// a subscription of the revoked device cannot come back either
	if st, _ := rc.do("POST", "/api/push/subscribe", fp.subAt("/push/a"), nil); st == 200 {
		t.Fatal("a revoked device subscribed")
	}
	rg.svc.TelegramPass() // a reminder
	eventually(t, "the reminder to the other device", func() bool { return len(fp.messages()) == 3 })
	time.Sleep(300 * time.Millisecond)
	ms := fp.messages()
	if len(ms) != 3 || ms[2]["path"] != "/push/b" {
		t.Fatalf("%v", ms)
	}
}

func pushRig(t *testing.T) (*fakePush, *rig, *remote, *softKey) {
	t.Helper()
	fp := newFakePush(t)
	restore := serve.SetPushTarget(fp.srv.Client().Transport)
	oldRemind := serve.TelegramRemind
	serve.TelegramRemind = 0
	t.Cleanup(func() { restore(); serve.TelegramRemind = oldRemind })
	rg, rc, k := startRemote(t, "approval")
	pair(t, rg, rc, k)
	return fp, rg, rc, k
}

// A first message that could not be queued is sent once the queue has
// room: it was not marked as sent.
func TestPushQueueOverflowRecovers(t *testing.T) {
	fp, rg, rc, _ := pushRig(t)
	if st, b := rc.do("POST", "/api/push/subscribe", fp.sub(), nil); st != 200 {
		t.Fatal(b)
	}
	restoreMax := rg.svc.SetPushQueueMax(0)
	_, _ = rg.client.Run(t.Context(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	time.Sleep(500 * time.Millisecond)
	if n := len(fp.messages()); n != 0 {
		t.Fatalf("%d sent with no room", n)
	}
	restoreMax()
	rg.svc.PushKick()
	eventually(t, "sent after the overflow", func() bool { return len(fp.messages()) == 1 })
}

// A retry of an ask answered meanwhile is not sent.
func TestPushRetryOfAnAnsweredAsk(t *testing.T) {
	fp, rg, rc, _ := pushRig(t)
	if st, b := rc.do("POST", "/api/push/subscribe", fp.sub(), nil); st != 200 {
		t.Fatal(b)
	}
	fp.mu.Lock()
	fp.once = 1
	fp.mu.Unlock()
	_, _ = rg.client.Run(t.Context(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	eventually(t, "the refused first POST", func() bool { fp.mu.Lock(); defer fp.mu.Unlock(); return fp.once == 0 })
	if err := rg.client.Answer(t.Context(), a.ID, serve.Answer{Allow: false}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // the retry was due after 1 s
	if n := len(fp.messages()); n != 0 {
		t.Fatalf("%d sent for an answered ask", n)
	}
}

// Revocation waits for a POST already under way, and none starts after;
// a cleanup that could not be written is caught up later.
func TestPushRevocationGateAndCleanup(t *testing.T) {
	fp, rg, rc, k := pushRig(t)
	if st, b := rc.do("POST", "/api/push/subscribe", fp.subAt("/push/a"), nil); st != 200 {
		t.Fatal(b)
	}
	ds, _ := rg.client.Devices(t.Context())
	devA := ds[0].ID
	rc2 := *rc
	pair(t, rg, &rc2, newSoftKey(t, k.rpID, k.origin))
	if st, b := rc2.do("POST", "/api/push/subscribe", fp.subAt("/push/b"), nil); st != 200 {
		t.Fatal(b)
	}
	hold := make(chan struct{})
	fp.mu.Lock()
	fp.hold = hold
	fp.mu.Unlock()
	_, _ = rg.client.Run(t.Context(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	rg.waitAsk()
	time.Sleep(500 * time.Millisecond) // a POST is held
	rg.svc.SetPushWriteFail(io.ErrShortWrite)
	revoked := make(chan error, 1)
	go func() { revoked <- rg.client.RevokeDevice(t.Context(), devA) }()
	select {
	case <-revoked:
		t.Fatal("the revocation did not wait for the POST under way")
	case <-time.After(700 * time.Millisecond):
	}
	fp.mu.Lock()
	fp.hold = nil
	fp.mu.Unlock()
	close(hold)
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	n := len(fp.messages())
	rg.svc.TelegramPass() // a reminder: only the paired device
	eventually(t, "the reminder", func() bool {
		ms := fp.messages()
		return len(ms) > n && ms[len(ms)-1]["path"] == "/push/b"
	})
	time.Sleep(300 * time.Millisecond)
	for _, m := range fp.messages()[n:] {
		if m["path"] == "/push/a" {
			t.Fatalf("the revoked device got %v", m)
		}
	}
	if rg.svc.PushSubscriptions() != 2 {
		t.Fatalf("%d subscriptions (the failed cleanup)", rg.svc.PushSubscriptions())
	}
	rg.svc.SetPushWriteFail(nil)
	rg.svc.PushGC()
	if rg.svc.PushSubscriptions() != 1 {
		t.Fatalf("%d subscriptions after the catch-up", rg.svc.PushSubscriptions())
	}
}

func TestPushKeyOnTheCurve(t *testing.T) {
	_, rg, rc, _ := pushRig(t)
	_ = rg
	sub := newFakePush(t).sub()
	sub["keys"].(map[string]string)["p256dh"] = b64u.EncodeToString(append([]byte{4}, make([]byte, 64)...))
	if st, _ := rc.do("POST", "/api/push/subscribe", sub, nil); st == 200 {
		t.Fatal("a point off the curve")
	}
}
