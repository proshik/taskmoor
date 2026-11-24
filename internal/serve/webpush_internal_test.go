package serve

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64url.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291, Appendix A: the example message, byte for byte.
func TestEncryptPushRFC8291(t *testing.T) {
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encryptPush([]byte("When I grow up, I want to be a watermelon"),
		mustB64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg"), asPriv, mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if b64url.EncodeToString(got) != want {
		t.Fatalf("got  %s\nwant %s", b64url.EncodeToString(got), want)
	}
}

// decryptPush is the browser's side, for the round trip.
func decryptPush(t *testing.T, body []byte, ua *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != pushRecordSize {
		t.Fatalf("rs %d", rs)
	}
	asPub := body[21 : 21+idlen]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := ua.ECDH(as)
	prkKey, _ := hkdf.Extract(sha256.New, shared, auth)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, string(append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asPub...)), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain[len(plain)-1] != 0x02 {
		t.Fatal("not the last record")
	}
	return plain[:len(plain)-1]
}

func TestEncryptPushRoundTrip(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(nil)
	auth := []byte("0123456789abcdef")
	body, err := encryptPush([]byte(`{"title":"x"}`), ua.PublicKey().Bytes(), auth, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := decryptPush(t, body, ua, auth); string(got) != `{"title":"x"}` {
		t.Fatalf("%q", got)
	}
	if _, err := encryptPush([]byte("x"), []byte("not a key"), auth, nil, nil); err == nil {
		t.Fatal("a bad key taken")
	}
}

// The VAPID JWT is ES256 over its own two parts, for the endpoint's origin.
func TestVapidAuth(t *testing.T) {
	k, err := newVapidKey()
	if err != nil {
		t.Fatal(err)
	}
	h, err := vapidAuth(k, "https://web.push.apple.com/QGuQyavXut", "https://board.example.org", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	tok, pub, ok := strings.Cut(strings.TrimPrefix(h, "vapid t="), ", k=")
	if !ok {
		t.Fatal(h)
	}
	parts := strings.Split(tok, ".")
	var claims map[string]any
	_ = json.Unmarshal(mustB64(t, parts[1]), &claims)
	if claims["aud"] != "https://web.push.apple.com" || claims["sub"] != "https://board.example.org" {
		t.Fatalf("%v", claims)
	}
	key, _ := k.ecdsa()
	want, _ := k.publicKey()
	if pub != want {
		t.Fatal("k is not the public key")
	}
	sig := mustB64(t, parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&key.PublicKey, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("the signature does not verify")
	}
	if _, err := vapidAuth(k, "http://web.push.apple.com/x", "s", time.Now()); err == nil {
		t.Fatal("http taken")
	}
}

func TestPushHostOK(t *testing.T) {
	for e, ok := range map[string]bool{
		"https://fcm.googleapis.com/fcm/send/abc":              true,
		"https://web.push.apple.com/QG":                        true,
		"https://api.push.apple.com/x":                         true,
		"https://updates.push.services.mozilla.com/wpush/v2/x": true,
		"http://fcm.googleapis.com/x":                          false,
		"https://fcm.googleapis.com:8443/x":                    false,
		"https://evil.example/x":                               false,
		"https://127.0.0.1/x":                                  false,
		"https://user@fcm.googleapis.com/x":                    false, // gitleaks:allow (an endpoint with credentials, refused)
		"https://fcm.googleapis.com.evil.example/x":            false,
	} {
		if pushHostOK(e) != ok {
			t.Fatalf("%s: %v", e, !ok)
		}
	}
}
