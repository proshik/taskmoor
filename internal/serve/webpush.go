package serve

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

// Web Push on the standard library (stage 10, the person's choice): the
// message encryption of RFC 8291 (aes128gcm, RFC 8188) and the VAPID
// signature of RFC 8292 (ES256). The payload is encrypted on the Mac for
// the browser's own key; the push service (Apple's, Google's, Mozilla's)
// carries only ciphertext.

var b64url = base64.RawURLEncoding

// pushRecordSize is the record size written into the header (one record).
const pushRecordSize = 4096

// encryptPush encrypts plaintext for a subscription's p256dh key and auth
// secret, with the sender's ephemeral key and salt (random when nil: the
// tests pass RFC 8291's).
func encryptPush(plaintext []byte, uaPublic, authSecret []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	curve := ecdh.P256()
	ua, err := curve.NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("the subscription's key: %w", err)
	}
	if len(authSecret) != 16 {
		return nil, errors.New("the subscription's auth secret is not 16 bytes")
	}
	if as == nil {
		if as, err = curve.GenerateKey(rand.Reader); err != nil {
			return nil, err
		}
	}
	if salt == nil {
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
	}
	if len(plaintext)+1+16 > pushRecordSize {
		return nil, errors.New("the message is too long for one record")
	}
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	// IKM from the ECDH secret, keyed by the auth secret (RFC 8291 §3.4)
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// one record, the last: its delimiter is 0x02, no padding
	sealed := gcm.Seal(nil, nonce, append(append([]byte{}, plaintext...), 0x02), nil)
	var out bytes.Buffer
	out.Write(salt)
	_ = binary.Write(&out, binary.BigEndian, uint32(pushRecordSize))
	out.WriteByte(byte(len(asPublic)))
	out.Write(asPublic)
	out.Write(sealed)
	return out.Bytes(), nil
}

// vapidKey is the service's application server key (P-256), kept in the
// store.
type vapidKey struct {
	Private string `json:"private"` // the scalar, base64url
}

func (k vapidKey) ecdsa() (*ecdsa.PrivateKey, error) {
	d, err := b64url.DecodeString(k.Private)
	if err != nil || len(d) != 32 {
		return nil, errors.New("the VAPID key is not readable")
	}
	priv, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		return nil, err
	}
	pub := priv.PublicKey().Bytes() // 0x04 || X || Y
	key := &ecdsa.PrivateKey{D: new(big.Int).SetBytes(d)}
	key.Curve = elliptic.P256()
	key.X, key.Y = new(big.Int).SetBytes(pub[1:33]), new(big.Int).SetBytes(pub[33:])
	return key, nil
}

func newVapidKey() (vapidKey, error) {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return vapidKey{}, err
	}
	return vapidKey{Private: b64url.EncodeToString(k.Bytes())}, nil
}

// publicKey is the key browsers subscribe with (uncompressed, base64url).
func (k vapidKey) publicKey() (string, error) {
	d, err := b64url.DecodeString(k.Private)
	if err != nil {
		return "", err
	}
	priv, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		return "", err
	}
	return b64url.EncodeToString(priv.PublicKey().Bytes()), nil
}

// vapidAuth is the Authorization header for a push endpoint: a JWT for
// its origin, signed ES256 (RFC 8292).
func vapidAuth(k vapidKey, endpoint, subject string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", errors.New("the push endpoint is not an https URL")
	}
	key, err := k.ecdsa()
	if err != nil {
		return "", err
	}
	pub, err := k.publicKey()
	if err != nil {
		return "", err
	}
	head := b64url.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now.Add(12 * time.Hour).Unix(), "sub": subject})
	signing := head + "." + b64url.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JOSE: r || s, 32 bytes each
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64url.EncodeToString(sig) + ", k=" + pub, nil
}

// pushHostOK says whether an endpoint is a known push service's: the
// service posts to it, so a device cannot point it elsewhere.
var pushHostOK = func(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case h == "fcm.googleapis.com", h == "updates.push.services.mozilla.com",
		h == "web.push.apple.com", strings.HasSuffix(h, ".push.apple.com"),
		strings.HasSuffix(h, ".notify.windows.com"):
		return true
	}
	return false
}
