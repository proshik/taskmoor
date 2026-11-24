package serve_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/proshik/taskmoor/internal/serve"
)

var b64u = base64.RawURLEncoding

// softKey is a passkey in software: a P-256 key and its counter.
type softKey struct {
	key    *ecdsa.PrivateKey
	id     []byte
	count  uint32
	rpID   string
	origin string
}

func newSoftKey(t *testing.T, rpID, origin string) *softKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &softKey{key: k, id: id, rpID: rpID, origin: origin}
}

func (k *softKey) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return b
}

func (k *softKey) authData(flags byte, attested []byte) []byte {
	rp := sha256.Sum256([]byte(k.rpID))
	k.count++
	var b bytes.Buffer
	b.Write(rp[:])
	b.WriteByte(flags)
	_ = binary.Write(&b, binary.BigEndian, k.count)
	b.Write(attested)
	return b.Bytes()
}

// create answers navigator.credentials.create.
func (k *softKey) create(t *testing.T, options map[string]any) map[string]any {
	t.Helper()
	pk := options["publicKey"].(map[string]any)
	x, y := k.key.PublicKey.X.FillBytes(make([]byte, 32)), k.key.PublicKey.Y.FillBytes(make([]byte, 32))
	cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		t.Fatal(err)
	}
	var att bytes.Buffer
	att.Write(make([]byte, 16)) // aaguid
	_ = binary.Write(&att, binary.BigEndian, uint16(len(k.id)))
	att.Write(k.id)
	att.Write(cose)
	ad := k.authData(0x45, att.Bytes()) // UP, UV, AT
	ao, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64u.EncodeToString(k.clientData("webauthn.create", pk["challenge"].(string))),
			"attestationObject": b64u.EncodeToString(ao)}}
}

// get answers navigator.credentials.get.
func (k *softKey) get(t *testing.T, options map[string]any) map[string]any {
	t.Helper()
	pk := options["publicKey"].(map[string]any)
	cd := k.clientData("webauthn.get", pk["challenge"].(string))
	ad := k.authData(0x05, nil) // UP, UV
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), h[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64u.EncodeToString(cd), "authenticatorData": b64u.EncodeToString(ad),
			"signature": b64u.EncodeToString(sig)}}
}

// remote is a client of the listener the tunnel reaches.
type remote struct {
	t          *testing.T
	base, host string
	token      string
}

func (c *remote) do(method, path string, body any, header map[string]string) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Host = c.host
	if c.token != "" {
		req.Header.Set("Authorization", "Device "+c.token)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func startRemote(t *testing.T, scenario string) (*rig, *remote, *softKey) {
	t.Helper()
	port := freePort(t)
	rg := startWith(t, newRepo(t), scenario, func(c *serve.Config) {
		c.RemoteHost, c.RemotePort = "localhost", port
	}, nil)
	host := fmt.Sprintf("localhost:%d", port)
	rc := &remote{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), host: host}
	eventually(t, "the remote listener", func() bool {
		resp, err := http.Get(rc.base + "/api/whoami")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil
	})
	return rg, rc, newSoftKey(t, "localhost", "http://"+host)
}

// pair pairs the soft key as a device.
func pair(t *testing.T, rg *rig, rc *remote, k *softKey) {
	t.Helper()
	code, _, err := rg.client.DevicePair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, b := rc.do("POST", "/api/device/pair/begin", map[string]string{"code": strings.ToUpper(code), "name": "phone"}, nil)
	if st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	st, f := rc.do("POST", "/api/device/pair/finish", map[string]any{"ceremony": b["ceremony"], "credential": k.create(t, b["options"].(map[string]any))}, nil)
	if st != 200 || f["token"] == nil {
		t.Fatalf("%d %v", st, f)
	}
	rc.token = f["token"].(string)
}

func TestRemotePairingAndAccess(t *testing.T) {
	rg, rc, k := startRemote(t, "work")
	if st, b := rc.do("GET", "/api/whoami", nil, nil); st != 200 || b["remote"] != true {
		t.Fatalf("%d %v", st, b)
	}
	if st, _ := rc.do("GET", "/api/snapshot", nil, nil); st != 401 {
		t.Fatalf("unpaired: %d", st)
	}
	// a wrong code pairs nothing
	if st, _ := rc.do("POST", "/api/device/pair/begin", map[string]string{"code": "nope", "name": "x"}, nil); st == 200 {
		t.Fatal("a wrong code was taken")
	}
	pair(t, rg, rc, k)
	if st, b := rc.do("GET", "/api/snapshot", nil, nil); st != 200 || b["projects"] == nil {
		t.Fatalf("%d %v", st, b)
	}
	// what acts on the Mac or its secrets is not reached from outside
	for _, p := range []string{"/api/telegram/pair", "/api/sessions/x/open", "/api/devices/pair", "/api/proof", "/api/cleanup"} {
		if st, _ := rc.do("POST", p, map[string]any{}, nil); st != 403 {
			t.Fatalf("%s: %d", p, st)
		}
	}
	// another host, the Mac's own credentials: refused
	rc2 := *rc
	rc2.host = "evil.example"
	if st, _ := rc2.do("GET", "/api/snapshot", nil, nil); st != 403 {
		t.Fatalf("another host: %d", st)
	}
	// the code is spent
	code, _, _ := rg.client.DevicePair(context.Background())
	st, b := rc.do("POST", "/api/device/pair/begin", map[string]string{"code": code, "name": "x"}, nil)
	if st != 200 {
		t.Fatal(b)
	}
	if st, _ := rc.do("POST", "/api/device/pair/begin", map[string]string{"code": code, "name": "x"}, nil); st == 200 {
		t.Fatal("a code began two pairings")
	}
	ds, err := rg.client.Devices(context.Background())
	if err != nil || len(ds) != 1 || ds[0].Name != "phone" {
		t.Fatalf("%+v %v", ds, err)
	}
	if err := rg.client.RevokeDevice(context.Background(), ds[0].ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := rc.do("GET", "/api/snapshot", nil, nil); st != 401 {
		t.Fatalf("revoked: %d", st)
	}
}

// Each dangerous approval from outside wants the device's passkey again.
func TestRemoteStepUp(t *testing.T) {
	rg, rc, k := startRemote(t, "approval")
	pair(t, rg, rc, k)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	path := "/api/asks/" + a.ID + "/answer"
	// a body the guard and the handler could read differently is refused
	for _, raw := range []string{`{"allow":false} {"allow":true}`, `{"allow":true} x`, `{"allow":true`, `null`, ` null `} {
		req, _ := http.NewRequest("POST", rc.base+path, strings.NewReader(raw))
		req.Host = rc.host
		req.Header.Set("Authorization", "Device "+rc.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 || len(rg.asks()) != 1 {
			t.Fatalf("%q: %d", raw, resp.StatusCode)
		}
	}
	// Accept's grant names the session, whatever id the request used
	if st, b := rc.do("POST", "/api/sessions/TM-001/accept", map[string]string{"tip": "x"}, nil); st != 428 || b["stepup"] != "accept:"+a.Session {
		t.Fatalf("%d %v", st, b)
	}
	// only known actions, and not without end
	if st, _ := rc.do("POST", "/api/stepup/begin", map[string]string{"action": "anything at all"}, nil); st == 200 {
		t.Fatal("an unknown action")
	}
	n := 0
	for ; n < 20; n++ {
		if st, _ := rc.do("POST", "/api/stepup/begin", map[string]string{"action": "allow:" + a.ID}, nil); st != 200 {
			break
		}
	}
	if n != 8 {
		t.Fatalf("%d confirmations under way", n)
	}
	rg.svc.ClearCeremonies()
	st, b := rc.do("POST", path, map[string]any{"allow": true}, nil)
	if st != 428 || b["stepup"] != "allow:"+a.ID {
		t.Fatalf("%d %v", st, b)
	}
	stepup := func(key *softKey, action string) (int, map[string]any) {
		st, b := rc.do("POST", "/api/stepup/begin", map[string]string{"action": action}, nil)
		if st != 200 {
			t.Fatalf("%d %v", st, b)
		}
		return rc.do("POST", "/api/stepup/finish", map[string]any{"ceremony": b["ceremony"], "assertion": key.get(t, b["options"].(map[string]any))}, nil)
	}
	// another key cannot confirm
	other := newSoftKey(t, k.rpID, k.origin)
	other.id = k.id
	if st, _ := stepup(other, "allow:"+a.ID); st == 200 {
		t.Fatal("another key confirmed")
	}
	// a grant is for its action only
	st, g := stepup(k, "accept:"+a.Session)
	if st != 200 {
		t.Fatalf("%d %v", st, g)
	}
	if st, _ := rc.do("POST", path, map[string]any{"allow": true}, map[string]string{"X-Taskmoor-Stepup": g["stepup"].(string)}); st != 428 {
		t.Fatalf("a grant for another action: %d", st)
	}
	st, g = stepup(k, "allow:"+a.ID)
	if st != 200 {
		t.Fatalf("%d %v", st, g)
	}
	if st, b := rc.do("POST", path, map[string]any{"allow": true}, map[string]string{"X-Taskmoor-Stepup": g["stepup"].(string)}); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	eventually(t, "answered", func() bool { return len(rg.asks()) == 0 })
	// one use
	if st, _ := rc.do("POST", path, map[string]any{"allow": true}, map[string]string{"X-Taskmoor-Stepup": g["stepup"].(string)}); st != 428 {
		t.Fatalf("a grant used twice: %d", st)
	}
}

// A deny needs no passkey; the Mac's own listener knows no device token.
func TestRemoteDenyAndLocalRefusesDevices(t *testing.T) {
	rg, rc, k := startRemote(t, "approval")
	pair(t, rg, rc, k)
	_, _ = rg.client.Run(context.Background(), serve.RunRequest{Project: "repo", Task: "TM-001"})
	a := rg.waitAsk()
	if st, b := rc.do("POST", "/api/asks/"+a.ID+"/answer", map[string]any{"allow": false, "text": "no"}, nil); st != 200 {
		t.Fatalf("%d %v", st, b)
	}
	cfg, _ := serve.LoadConfig(rg.paths.Config)
	local := &remote{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", cfg.Port), host: fmt.Sprintf("127.0.0.1:%d", cfg.Port), token: rc.token}
	if st, _ := local.do("GET", "/api/snapshot", nil, nil); st != 401 {
		t.Fatalf("a device token on the Mac's listener: %d", st)
	}
}

func TestRemoteFiles(t *testing.T) {
	req := serve.RemoteFilesRequest{Domain: "board.example.org", AuthDomain: "id.example.org", VPS: "tunnel@vps.example.org",
		VPSPort: 18080, MacPort: 7311, Key: "/Users/me/.ssh/taskmoor_tunnel", Store: "/Users/me/.local/state/taskmoor"}
	files, err := serve.RemoteFiles(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files["vps/sshd-taskmoor-tunnel.conf"], "AllowTcpForwarding remote") ||
		!strings.Contains(files["vps/sshd-taskmoor-tunnel.conf"], "PermitOpen none") ||
		!strings.Contains(files["vps/sshd-taskmoor-tunnel.conf"], "GatewayPorts no") ||
		!strings.Contains(files["vps/Caddyfile"], "admin off") || !strings.Contains(files["vps/Caddyfile"], "not remote_ip {$AUTH_OPEN_TO}") ||
		!strings.Contains(files["mac/com.taskmoor.tunnel.plist"], "StrictHostKeyChecking=yes") ||
		!strings.Contains(files["vps/sshd-taskmoor-tunnel.conf"], "ClientAliveInterval 30") ||
		!strings.Contains(files["vps/Caddyfile"], "header @api X-Taskmoor-Login required") ||
		!strings.Contains(files["README.md"], "sshd -T -C user=tunnel") || !strings.Contains(files["README.md"], "28.0.0") {
		t.Fatalf("%v", files)
	}
	if !strings.Contains(files["mac/com.taskmoor.tunnel.plist"], "<string>127.0.0.1:18080:127.0.0.1:7311</string>") ||
		!strings.Contains(files["vps/authorized_keys.line"], `permitlisten="127.0.0.1:18080"`) ||
		!strings.Contains(files["vps/Caddyfile"], "forward_auth 127.0.0.1:4180") {
		t.Fatalf("%v", files)
	}
	again, _ := serve.RemoteFiles(req)
	if files["vps/.env"] == again["vps/.env"] || !strings.Contains(files["vps/.env"], "OAUTH2_PROXY_COOKIE_SECRET=") {
		t.Fatal("the secrets are not new each time")
	}
	for _, bad := range []serve.RemoteFilesRequest{
		{Domain: "https://x.org", AuthDomain: "id.x.org", VPS: "t@h", VPSPort: 18080, MacPort: 1},
		{Domain: "x.org", AuthDomain: "x.org", VPS: "t@h", VPSPort: 18080, MacPort: 1},
		{Domain: "x.org", AuthDomain: "id.x.org", VPS: "t@h;rm", VPSPort: 18080, MacPort: 1},
		{Domain: "x.org", AuthDomain: "id.x.org", VPS: "t@h", VPSPort: 80, MacPort: 1},
		{Domain: "x.org", AuthDomain: "id.x.org", VPS: "t@h", VPSPort: 1411, MacPort: 1},
		{Domain: "x.org", AuthDomain: "id.x.org", VPS: "t@h", VPSPort: 4180, MacPort: 1},
	} {
		if _, err := serve.RemoteFiles(bad); err == nil {
			t.Fatalf("%+v taken", bad)
		}
	}
}

func TestRemoteConfigChecked(t *testing.T) {
	base := func(c serve.Config) serve.Config {
		c.DefaultProfile, c.Profiles = "claude", []serve.Profile{{Name: "claude", Harness: "claude"}}
		return c
	}
	for _, c := range []serve.Config{
		{Port: 7310, RemoteHost: "board.example.org"},
		{Port: 7310, RemotePort: 7311},
		{Port: 7310, RemoteHost: "https://board.example.org", RemotePort: 7311},
		{Port: 7310, RemoteHost: "board.example.org", RemotePort: 7310},
	} {
		c = base(c)
		if err := c.Check("config.toml"); err == nil || !strings.Contains(err.Error(), "remote_") {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	ok := base(serve.Config{Port: 7310, RemoteHost: "board.example.org", RemotePort: 7311})
	if err := ok.Check("config.toml"); err != nil {
		t.Fatal(err)
	}
}

// A revocation that cannot be written changes nothing and can be retried.
func TestRemoteRevokeRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory: the failed write cannot be made")
	}
	rg, rc, k := startRemote(t, "work")
	pair(t, rg, rc, k)
	ds, _ := rg.client.Devices(context.Background())
	if err := os.Chmod(rg.paths.Store, 0o500); err != nil {
		t.Fatal(err)
	}
	err := rg.client.RevokeDevice(context.Background(), ds[0].ID)
	_ = os.Chmod(rg.paths.Store, 0o700)
	if err == nil {
		t.Fatal("revoked without writing")
	}
	if st, _ := rc.do("GET", "/api/snapshot", nil, nil); st != 200 {
		t.Fatalf("the device stopped though the revocation failed: %d", st)
	}
	if err := rg.client.RevokeDevice(context.Background(), ds[0].ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := rc.do("GET", "/api/snapshot", nil, nil); st != 401 {
		t.Fatalf("revoked: %d", st)
	}
}

// A new code voids a pairing under way.
func TestRemotePairingVoidedByANewCode(t *testing.T) {
	rg, rc, k := startRemote(t, "work")
	code, _, _ := rg.client.DevicePair(context.Background())
	st, b := rc.do("POST", "/api/device/pair/begin", map[string]string{"code": code, "name": "phone"}, nil)
	if st != 200 {
		t.Fatal(b)
	}
	if _, _, err := rg.client.DevicePair(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, f := rc.do("POST", "/api/device/pair/finish", map[string]any{"ceremony": b["ceremony"], "credential": k.create(t, b["options"].(map[string]any))}, nil); st == 200 {
		t.Fatalf("paired with a voided code: %v", f)
	}
}

// A remote event stream ends after a while (the next passes the VPS's
// sign-in again).
func TestRemoteStreamBounded(t *testing.T) {
	t.Cleanup(serve.SetRemoteStreamTTL(300 * time.Millisecond)) // put back after the service stopped
	rg, rc, k := startRemote(t, "work")
	pair(t, rg, rc, k)
	req, _ := http.NewRequest("GET", rc.base+"/api/events", nil)
	req.Host = rc.host
	req.Header.Set("Authorization", "Device "+rc.token)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the stream lasted %v", d)
	}
}

// Approve from outside wants the passkey too.
func TestRemoteApproveNeedsThePasskey(t *testing.T) {
	port := freePort(t)
	repo := newRepo(t)
	rg := startWith(t, repo, "grill", func(c *serve.Config) {
		c.RemoteHost, c.RemotePort = "localhost", port
		c.Profiles = append(c.Profiles, serve.Profile{Name: "agent", Harness: "codex", Command: fakeACP(t)})
		c.DefaultProfile = "agent"
	}, func(s *serve.Service) {
		s.CodexLogin = func() (string, error) { return "Logged in using ChatGPT", nil }
	}, "FAKE_GRILL_FILE=tasks/TM-050-from-the-idea.md", "FAKE_GRILL_BODY="+grilledTask)
	host := fmt.Sprintf("localhost:%d", port)
	rc := &remote{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), host: host}
	k := newSoftKey(t, "localhost", "http://"+host)
	pair(t, rg, rc, k)
	i, _ := grill(t, rg)
	rv, err := rg.client.ReviewIdea(context.Background(), i.ID)
	if err != nil || !rv.Ready {
		t.Fatalf("%+v %v", rv, err)
	}
	before := repo.Git("rev-parse", "main")
	path := "/api/ideas/" + i.ID + "/approve"
	body := map[string]string{"tip": rv.Tip, "main": rv.Main}
	if st, b := rc.do("POST", path, body, nil); st != 428 || b["stepup"] != "approve:"+i.ID {
		t.Fatalf("%d %v", st, b)
	}
	if repo.Git("rev-parse", "main") != before {
		t.Fatal("main moved without the passkey")
	}
	st, b := rc.do("POST", "/api/stepup/begin", map[string]string{"action": "approve:" + i.ID}, nil)
	if st != 200 {
		t.Fatal(b)
	}
	st, g := rc.do("POST", "/api/stepup/finish", map[string]any{"ceremony": b["ceremony"], "assertion": k.get(t, b["options"].(map[string]any))}, nil)
	if st != 200 {
		t.Fatal(g)
	}
	if st, b := rc.do("POST", path, body, map[string]string{"X-Taskmoor-Stepup": g["stepup"].(string)}); st != 200 || b["merged"] == nil {
		t.Fatalf("%d %v", st, b)
	}
	if repo.Git("rev-parse", "main") == before {
		t.Fatal("main did not move")
	}
}
