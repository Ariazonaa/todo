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
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func unb64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(s)
	if err != nil {
		t.Fatalf("base64url %q: %v", s, err)
	}
	return b
}

// decryptPush is the device side (tests only): this is how a test verifies
// that what arrives is what the server meant to send. Returns errors instead
// of calling t.Fatal — the test push service calls it from its server
// goroutine.
func decryptPush(ua *ecdh.PrivateKey, authSecret, body []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("body zu kurz")
	}
	salt, idlen := body[:16], int(body[20])
	if len(body) < 21+idlen {
		return nil, errors.New("header zu kurz")
	}
	asPub, ct := body[21:21+idlen], body[21+idlen:]
	as, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		return nil, err
	}
	secret, err := ua.ECDH(as)
	if err != nil {
		return nil, err
	}
	keyInfo := "WebPush: info\x00" + string(ua.PublicKey().Bytes()) + string(asPub)
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
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(plain, 0x02)
	if i < 0 {
		return nil, errors.New("kein Padding-Trenner")
	}
	return plain[:i], nil
}

// Example from RFC 8291 section 5 (intermediate values in appendix A): fixed
// keys and salt produce exactly this body. If a single byte is off, every
// push service rejects the message.
func TestEncryptPushMatchesRFC8291(t *testing.T) {
	as, err := ecdh.P256().NewPrivateKey(unb64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := encryptPushWith(as,
		unb64(t, "DGv6ra1nlYgDCS1FRnbzlw"),
		unb64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		unb64(t, "BTBZMqHH6r4Tts7J_aSIgg"),
		[]byte("When I grow up, I want to be a watermelon"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if b64.EncodeToString(got) != want {
		t.Fatalf("Body weicht von RFC 8291 ab:\n got %s\nwant %s", b64.EncodeToString(got), want)
	}
}

func TestEncryptPushRoundTrip(t *testing.T) {
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	msg := []byte(`{"title":"Zahnarzt"}`)
	a, err := encryptPush(ua.PublicKey().Bytes(), secret, msg)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := encryptPush(ua.PublicKey().Bytes(), secret, msg)
	if bytes.Equal(a, b) {
		t.Fatal("zwei Nachrichten gleich verschlüsselt — Salt/Schlüssel nicht zufällig")
	}
	got, err := decryptPush(ua, secret, a)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("Round-Trip: %q, %v", got, err)
	}
}

func TestEncryptPushRejectsBadKeys(t *testing.T) {
	secret := make([]byte, 16)
	if _, err := encryptPush(make([]byte, 65), secret, []byte("x")); err == nil {
		t.Error("p256dh ohne gültigen Kurvenpunkt angenommen")
	}
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	if _, err := encryptPush(ua.PublicKey().Bytes(), make([]byte, 15), []byte("x")); err == nil {
		t.Error("auth mit 15 Byte angenommen")
	}
}

func TestVAPIDTokenVerifies(t *testing.T) {
	db := testServer(t).db
	k, err := loadOrCreateVAPID(db)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := k.token("https://fcm.googleapis.com", "https://todo.example", time.Unix(1_900_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT hat %d Teile", len(parts))
	}
	var claims map[string]any
	if err := json.Unmarshal(unb64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims["aud"] != "https://fcm.googleapis.com" || claims["sub"] != "https://todo.example" || claims["exp"] != float64(1_900_000_000) {
		t.Fatalf("Claims: %v", claims)
	}
	sig := unb64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("Signatur %d Byte, JWS ES256 verlangt r‖s mit 64", len(sig))
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), unb64(t, k.publicKey()))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("Signatur passt nicht zum öffentlichen Schlüssel")
	}
	// A restart loads the same key — otherwise every device registration would be worthless.
	k2, err := loadOrCreateVAPID(db)
	if err != nil || k2.publicKey() != k.publicKey() {
		t.Fatalf("zweiter Start: anderer Schlüssel (%v)", err)
	}
}

// pushStub is a push service for tests: decrypts what arrives and responds
// per path with the configured status (default 201). For the duration of
// the test it disables the allowed-services list and makes pushClient trust
// its certificate.
type pushStub struct {
	srv    *httptest.Server
	ua     *ecdh.PrivateKey
	secret []byte

	mu      sync.Mutex
	status  map[string]int
	hook    func()
	msgs    []pushMsg
	headers []http.Header
}

func newPushStub(t *testing.T) *pushStub {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	st := &pushStub{ua: ua, secret: make([]byte, 16), status: map[string]int{}}
	rand.Read(st.secret)
	st.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		st.mu.Lock()
		code, ok := st.status[r.URL.Path]
		hook := st.hook
		st.mu.Unlock()
		if !ok {
			code = http.StatusCreated
		}
		if hook != nil {
			hook()
		}
		if code/100 == 2 {
			var m pushMsg
			plain, err := decryptPush(st.ua, st.secret, body)
			if err == nil {
				err = json.Unmarshal(plain, &m)
			}
			if err != nil {
				t.Errorf("Push nicht lesbar: %v", err)
			}
			st.mu.Lock()
			st.msgs = append(st.msgs, m)
			st.headers = append(st.headers, r.Header.Clone())
			st.mu.Unlock()
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(st.srv.Close)
	oldAllowed, oldTransport := pushHostAllowed, pushClient.Transport
	pushHostAllowed = func(*url.URL) bool { return true }
	pushClient.Transport = st.srv.Client().Transport
	t.Cleanup(func() { pushHostAllowed, pushClient.Transport = oldAllowed, oldTransport })
	return st
}

func (st *pushStub) endpoint(path string) string { return st.srv.URL + path }

// register registers a device with this service (all devices share the keys).
func (st *pushStub) register(t *testing.T, db *sql.DB, path string, credentialID int64) {
	t.Helper()
	if err := savePushSub(db, st.endpoint(path), st.ua.PublicKey().Bytes(), st.secret, "https://todo.example", credentialID, testUID); err != nil {
		t.Fatal(err)
	}
}

func (st *pushStub) setStatus(path string, code int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.status[path] = code
}

func (st *pushStub) setHook(f func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.hook = f
}

func (st *pushStub) received() []pushMsg {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]pushMsg(nil), st.msgs...)
}

func (st *pushStub) authHeaders() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, h := range st.headers {
		out = append(out, h.Get("Authorization"))
	}
	return out
}

func TestSendPushDeliversDecryptableMessage(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	st.register(t, s.db, "/dev", 0)
	sub, _ := getPushSub(s.db, testUID, st.endpoint("/dev"))
	msg := pushMsg{Title: "Zahnarzt", Body: "fällig 14:30", Tag: "task-1", TaskID: 1, DueDate: "2026-09-23",
		Actions: []pushAction{{"done", "Erledigt"}, {"snooze", "+1 Tag"}}}
	if err := sendPush(s.vapid, sub, msg, time.Hour, "high"); err != nil {
		t.Fatal(err)
	}
	got := st.received()
	if len(got) != 1 || got[0].Title != "Zahnarzt" || got[0].TaskID != 1 || got[0].DueDate != "2026-09-23" || len(got[0].Actions) != 2 || got[0].Actions[1].Title != "+1 Tag" {
		t.Fatalf("empfangen: %+v", got)
	}
	auth := st.authHeaders()[0]
	jwt, rest, ok := strings.Cut(strings.TrimPrefix(auth, "vapid t="), ", k=")
	if !strings.HasPrefix(auth, "vapid t=") || !ok || rest != s.vapid.publicKey() {
		t.Fatalf("Authorization: %q", auth)
	}
	h := st.headers[0]
	if h.Get("Content-Encoding") != "aes128gcm" || h.Get("Content-Type") != "application/octet-stream" ||
		h.Get("TTL") != "3600" || h.Get("Urgency") != "high" {
		t.Fatalf("Header: %+v", h)
	}
	var claims map[string]any
	json.Unmarshal(unb64(t, strings.Split(jwt, ".")[1]), &claims)
	if claims["aud"] != st.srv.URL || claims["sub"] != "https://todo.example" {
		t.Fatalf("Claims: %v", claims)
	}
}

func TestSendPushRefusesUnknownService(t *testing.T) {
	s := testServer(t)
	for _, ep := range []string{"https://evil.example/x", "http://fcm.googleapis.com/fcm/send/x", "https://fcm.googleapis.com.evil.example/x"} {
		sub := pushSub{Endpoint: ep, P256dh: make([]byte, 65), Auth: make([]byte, 16), Origin: "o"}
		if err := sendPush(s.vapid, sub, pushMsg{Title: "x"}, time.Minute, "normal"); err == nil {
			t.Errorf("%s: angenommen", ep)
		}
	}
}

// The device address is the device's secret — it must not end up in an
// error message (and thus in the log).
func TestSendPushErrorHidesEndpoint(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	st.register(t, s.db, "/geheim-123", 0)
	sub, _ := getPushSub(s.db, testUID, st.endpoint("/geheim-123"))
	st.srv.Close()
	err := sendPush(s.vapid, sub, pushMsg{Title: "x"}, time.Minute, "normal")
	if err == nil || strings.Contains(err.Error(), "geheim-123") {
		t.Fatalf("Fehler: %v", err)
	}
}
