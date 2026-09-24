package main

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
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Web Push per RFC 8030 (delivery), RFC 8291 (encryption), and RFC 8292
// (VAPID) — using only the standard library. Verified against the example
// from RFC 8291 (webpush_test.go).

var b64 = base64.RawURLEncoding

const (
	// One record, as RFC 8291 mandates for Web Push.
	pushRecordSize = 4096
	// Push services guarantee a 4096-byte body; after 86 bytes of header
	// and 17 bytes of tag/delimiter there's still room. The scheduler's
	// texts stay well below this.
	maxPushPlaintext = 3072
)

// vapidKey is the sender key pair (P-256). Generated on first start, it
// lives in meta.push_vapid_key — no env config, not included in exports.
type vapidKey struct {
	priv *ecdsa.PrivateKey
	pub  []byte // 65 Byte, unkomprimiert
}

func loadOrCreateVAPID(db *sql.DB) (*vapidKey, error) {
	if v := getMeta(db, "push_vapid_key"); v != "" {
		raw, err := b64.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("push_vapid_key: %w", err)
		}
		priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
		if err != nil {
			return nil, fmt.Errorf("push_vapid_key: %w", err)
		}
		return newVAPIDKey(priv)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	raw, err := priv.Bytes()
	if err != nil {
		return nil, err
	}
	if err := setMeta(db, "push_vapid_key", b64.EncodeToString(raw)); err != nil {
		return nil, err
	}
	return newVAPIDKey(priv)
}

func newVAPIDKey(priv *ecdsa.PrivateKey) (*vapidKey, error) {
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return &vapidKey{priv: priv, pub: pub}, nil
}

// publicKey is the applicationServerKey for pushManager.subscribe.
func (k *vapidKey) publicKey() string { return b64.EncodeToString(k.pub) }

// token is the VAPID JWT (ES256). aud = scheme+host of the push service,
// sub = contact; Apple requires an https or mailto address there. The
// signature is encoded as r‖s (32 bytes each), not ASN.1 — that's what JWS
// wants.
func (k *vapidKey) token(aud, sub string, exp time.Time) (string, error) {
	header := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{"aud": aud, "exp": exp.Unix(), "sub": sub})
	if err != nil {
		return "", err
	}
	signing := header + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// encryptPush encrypts a message for a device (aes128gcm) with a fresh
// sender key and salt.
func encryptPush(uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encryptPushWith(as, salt, uaPublic, authSecret, plaintext)
}

// encryptPushWith is encryptPush with a fixed key and salt (for the test
// vector). Layout follows RFC 8291 section 3.4 and RFC 8188.
func encryptPushWith(as *ecdh.PrivateKey, salt, uaPublic, authSecret, plaintext []byte) ([]byte, error) {
	ua, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	if len(authSecret) != 16 {
		return nil, errors.New("auth: expected 16 bytes")
	}
	if len(plaintext)+17 > pushRecordSize {
		return nil, errors.New("message too large for one record")
	}
	secret, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	keyInfo := "WebPush: info\x00" + string(uaPublic) + string(asPublic)
	ikm, err := hkdf.Key(sha256.New, secret, authSecret, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
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
	// Header: salt(16) ‖ rs(4) ‖ idlen(1) ‖ keyid (= sender key, 65)
	header := make([]byte, 0, 21+len(asPublic))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, pushRecordSize)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)
	// Only, and thus last, record: delimiter 0x02, no further padding.
	record := append(append([]byte(nil), plaintext...), 0x02)
	return gcm.Seal(header, nonce, record, nil), nil
}

// Push services the server is allowed to send to. The endpoint comes from
// the browser; without this list, a logged-in user could turn the server
// into a tool for requests against arbitrary targets.
var pushHosts = []string{"fcm.googleapis.com", "push.services.mozilla.com", "push.apple.com", "notify.windows.com"}

// pushHostAllowed is a variable so tests can allow a local service.
var pushHostAllowed = func(u *url.URL) bool {
	if u.Scheme != "https" {
		return false
	}
	for _, d := range pushHosts {
		if isHost(u.Hostname(), d) {
			return true
		}
	}
	return false
}

// pushClient does not follow redirects (like the webhook client): a 3xx is
// treated as an error.
var pushClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// pushMsg is the plaintext of a notification; sw.js displays it.
// pushAction: a button on the notification. The label comes from the
// server (in the notification's language), the service worker just
// displays it.
type pushAction struct {
	Action string `json:"action"`
	Title  string `json:"title"`
}

type pushMsg struct {
	Title   string       `json:"title"`
	Body    string       `json:"body"`
	Tag     string       `json:"tag"`
	TaskID  int64        `json:"task_id,omitempty"`
	DueDate string       `json:"due_date,omitempty"` // for expected_due_date on the "Done" button
	Actions []pushAction `json:"actions,omitempty"`
}

// errPushGone: the service no longer knows the device (404/410) — delete it.
var errPushGone = errors.New("push: device unsubscribed at the push service")

// sendPush sends msg to one device. Errors never name the endpoint.
func sendPush(k *vapidKey, sub pushSub, msg pushMsg, ttl time.Duration, urgency string) error {
	u, err := url.Parse(sub.Endpoint)
	if err != nil || !pushHostAllowed(u) {
		return errors.New("push: service not allowed")
	}
	plain, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(plain) > maxPushPlaintext {
		return fmt.Errorf("push: message too large (%d bytes)", len(plain))
	}
	body, err := encryptPush(sub.P256dh, sub.Auth, plain)
	if err != nil {
		return err
	}
	jwt, err := k.token(u.Scheme+"://"+u.Host, sub.Origin, time.Now().Add(12*time.Hour))
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("push: invalid endpoint")
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(ttl.Seconds())))
	req.Header.Set("Urgency", urgency)
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+k.publicKey())
	resp, err := pushClient.Do(req)
	if err != nil {
		// As with the webhook: the *url.Error carries the URL — here
		// that's the device's secret. Pass on only the underlying cause.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			return fmt.Errorf("push: %w", ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return errPushGone
	default:
		return fmt.Errorf("push: status %d", resp.StatusCode)
	}
}
