package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Real ceremonies against the real handlers, with a software authenticator
// (ES256) — the kind anyone could build with python-fido2 or similar.

const (
	e2eHost   = "todo.example"
	e2eOrigin = "https://todo.example"
	flagUP    = 0x01
	flagUV    = 0x04
)

var b64u = base64.RawURLEncoding

// e2eHandler: the app's real routing (main.go), without rate limits — the
// tests fire thousands of ceremonies from one address.
func e2eHandler(s *server) http.Handler {
	return s.routes(newRateLimiter(1_000_000, 1_000_000), newRateLimiter(1_000_000, 1_000_000))
}

type softKey struct {
	priv *ecdsa.PrivateKey
	id   []byte
}

func newSoftKey(t *testing.T) *softKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	rand.Read(id)
	return &softKey{priv: priv, id: id}
}

// store saves the key as a passkey in the DB. verified = UV was shown during
// registration (mandatory today; older keys without a PIN: false).
func (k *softKey) store(t *testing.T, s *server, verified bool) {
	t.Helper()
	k.storeFor(t, s, testUID, verified)
}

// storeFor: like store, for account uid.
func (k *softKey) storeFor(t *testing.T, s *server, uid int64, verified bool) {
	t.Helper()
	data, _ := json.Marshal(webauthn.Credential{
		ID: k.id, PublicKey: k.cose(t), AttestationType: "none",
		Flags: webauthn.CredentialFlags{UserPresent: true, UserVerified: verified},
	})
	if _, err := s.db.Exec("INSERT INTO credentials (name, created_at, data, user_id) VALUES ('soft', 'z', ?, ?)", string(data), uid); err != nil {
		t.Fatal(err)
	}
}

// cose: the public key in COSE format, the way passkeys report it.
func (k *softKey) cose(t *testing.T) []byte {
	t.Helper()
	raw, err := k.priv.PublicKey.Bytes() // uncompressed: 0x04 || X || Y
	if err != nil {
		t.Fatal(err)
	}
	pk, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         int64(webauthncose.P256),
		XCoord:        raw[1:33],
		YCoord:        raw[33:65],
	})
	if err != nil {
		t.Fatal(err)
	}
	return pk
}

// registration answers a registration challenge (attestation "none",
// with UP and UV).
func (k *softKey) registration(t *testing.T, challenge string) []byte {
	t.Helper()
	return k.registrationWith(t, challenge, nil, nil)
}

// registrationWith: like registration, plus what a client may add of its
// own accord — extra fields in clientDataJSON, a list of transports.
func (k *softKey) registrationWith(t *testing.T, challenge string, extra map[string]any, transports []string) []byte {
	t.Helper()
	cd := map[string]any{"type": "webauthn.create", "challenge": challenge, "origin": e2eOrigin, "crossOrigin": false}
	maps.Copy(cd, extra)
	cdj, _ := json.Marshal(cd)
	rpHash := sha256.Sum256([]byte(e2eHost))
	var ad bytes.Buffer
	ad.Write(rpHash[:])
	ad.WriteByte(flagUP | flagUV | 0x40) // 0x40: with credential data
	binary.Write(&ad, binary.BigEndian, uint32(0))
	ad.Write(make([]byte, 16)) // AAGUID
	binary.Write(&ad, binary.BigEndian, uint16(len(k.id)))
	ad.Write(k.id)
	ad.Write(k.cose(t))
	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": ad.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	response := map[string]any{
		"clientDataJSON":    b64u.EncodeToString(cdj),
		"attestationObject": b64u.EncodeToString(att),
	}
	if transports != nil {
		response["transports"] = transports
	}
	body, _ := json.Marshal(map[string]any{
		"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": response,
	})
	return body
}

// addPasskeyChallenge starts adding a passkey from the logged-in session
// (control panel) and returns the challenge.
func (b *browser) addPasskeyChallenge(t *testing.T) string {
	t.Helper()
	begin := b.do(http.MethodPost, "/api/passkeys/begin", nil)
	var opt struct {
		PublicKey struct{ Challenge string } `json:"publicKey"`
	}
	if err := json.Unmarshal(begin.Body.Bytes(), &opt); err != nil || opt.PublicKey.Challenge == "" {
		t.Fatalf("passkeys/begin: %d %s", begin.Code, begin.Body)
	}
	return opt.PublicKey.Challenge
}

// addPasskey registers k as a further passkey of the logged-in account;
// returns finish's response.
func (b *browser) addPasskey(t *testing.T, k *softKey) *httptest.ResponseRecorder {
	t.Helper()
	return b.do(http.MethodPost, "/api/passkeys/finish", k.registration(t, b.addPasskeyChallenge(t)))
}

// setup registers k via /setup with code code; returns finish's response.
func (b *browser) setup(t *testing.T, k *softKey, code string) *httptest.ResponseRecorder {
	t.Helper()
	b.setupCode = code
	defer func() { b.setupCode = "" }()
	begin := b.do(http.MethodPost, "/api/setup/begin", nil)
	if begin.Code != http.StatusOK {
		return begin
	}
	var opt struct {
		PublicKey struct{ Challenge string } `json:"publicKey"`
	}
	if err := json.Unmarshal(begin.Body.Bytes(), &opt); err != nil || opt.PublicKey.Challenge == "" {
		t.Fatalf("setup/begin: %d %s", begin.Code, begin.Body)
	}
	return b.do(http.MethodPost, "/api/setup/finish", k.registration(t, opt.PublicKey.Challenge))
}

// assertion answers a login challenge with the given flags.
func (k *softKey) assertion(t *testing.T, challenge string, flags byte) []byte {
	t.Helper()
	return k.assertionWith(t, challenge, flags, nil)
}

// assertionWith: like assertion, with user handle handle in the response.
func (k *softKey) assertionWith(t *testing.T, challenge string, flags byte, handle []byte) []byte {
	t.Helper()
	var uh any
	if handle != nil {
		uh = b64u.EncodeToString(handle)
	}
	cdj, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": e2eOrigin, "crossOrigin": false})
	rpHash := sha256.Sum256([]byte(e2eHost))
	var ad bytes.Buffer
	ad.Write(rpHash[:])
	ad.WriteByte(flags)
	binary.Write(&ad, binary.BigEndian, uint32(0)) // counter: synced passkeys report 0
	cdh := sha256.Sum256(cdj)
	digest := sha256.Sum256(append(append([]byte{}, ad.Bytes()...), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k.priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": b64u.EncodeToString(k.id), "rawId": b64u.EncodeToString(k.id), "type": "public-key",
		"response": map[string]any{
			"authenticatorData": b64u.EncodeToString(ad.Bytes()),
			"clientDataJSON":    b64u.EncodeToString(cdj),
			"signature":         b64u.EncodeToString(sig),
			"userHandle":        uh,
		},
	})
	return body
}

// browser holds cookies like a browser across multiple requests.
type browser struct {
	h         http.Handler
	cookies   map[string]*http.Cookie
	setupCode string // X-Setup-Code, while set
}

func (b *browser) do(method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, e2eOrigin+path, bytes.NewReader(body))
	req.Header.Set("Origin", e2eOrigin)
	if b.setupCode != "" {
		req.Header.Set("X-Setup-Code", b.setupCode)
	}
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return rec
}

func (b *browser) login(t *testing.T, k *softKey, flags byte) *httptest.ResponseRecorder {
	t.Helper()
	begin := b.do(http.MethodPost, "/api/auth/login/begin", nil)
	var opt struct {
		PublicKey struct{ Challenge string } `json:"publicKey"`
	}
	if err := json.Unmarshal(begin.Body.Bytes(), &opt); err != nil || opt.PublicKey.Challenge == "" {
		t.Fatalf("login/begin: %d %s", begin.Code, begin.Body)
	}
	return b.do(http.MethodPost, "/api/auth/login/finish", k.assertion(t, opt.PublicKey.Challenge, flags))
}

func e2eSetup(t *testing.T) (*server, *browser) {
	t.Helper()
	s := testServer(t)
	s.cfg.insecureCookie = true
	return s, &browser{h: e2eHandler(s), cookies: map[string]*http.Cookie{}}
}

// A stolen security key without its PIN: the attacker submits a login with
// only "present" (UP), without verification (UV). For a passkey capable
// of UV, that must not be enough.
func TestLoginRequiresUVForPasskeysThatHaveIt(t *testing.T) {
	s, b := e2eSetup(t)
	key := newSoftKey(t)
	key.store(t, s, true)

	if rec := b.login(t, key, flagUP); rec.Code != http.StatusUnauthorized {
		t.Fatalf("Anmeldung ohne UV: %d, erwartet 401", rec.Code)
	}
	if rec := b.login(t, key, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("Anmeldung mit UV: %d %s", rec.Code, rec.Body)
	}
}

// A key without a PIN, registered before UV became mandatory, must not be
// locked out — otherwise its owner loses access on update.
func TestLoginKeepsLegacyPasskeyWithoutUVWorking(t *testing.T) {
	s, b := e2eSetup(t)
	key := newSoftKey(t)
	key.store(t, s, false)

	if rec := b.login(t, key, flagUP); rec.Code != http.StatusNoContent {
		t.Fatalf("Bestandsschlüssel ohne PIN ausgesperrt: %d %s", rec.Code, rec.Body)
	}
}

// The whole path: an old session must not manage passkeys, but after
// confirming via passkey it can. The replaced session stays valid briefly
// (requests in flight) but is no longer renewed.
func TestReauthReplacesSessionWithGrace(t *testing.T) {
	s, b := e2eSetup(t)
	key := newSoftKey(t)
	key.store(t, s, true)
	if rec := b.login(t, key, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	old := b.cookies[sessionCookie].Value
	stale := time.Now().Add(-reauthWindow - time.Minute).UTC().Format(time.RFC3339)
	s.db.Exec("UPDATE sessions SET auth_at = ? WHERE token = ?", stale, hashToken(old))

	if rec := b.do(http.MethodPost, "/api/passkeys/begin", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("Passkey-begin mit alter Bestätigung: %d, erwartet 403", rec.Code)
	}
	if rec := b.login(t, key, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("Bestätigung: %d %s", rec.Code, rec.Body)
	}
	if b.cookies[sessionCookie].Value == old {
		t.Fatal("Bestätigung hat keine neue Sitzung ausgestellt")
	}
	if rec := b.do(http.MethodPost, "/api/passkeys/begin", nil); rec.Code != http.StatusOK {
		t.Fatalf("Passkey-begin nach Bestätigung: %d %s", rec.Code, rec.Body)
	}

	// Old token: still valid (grace period), but no longer renewable.
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: old})
	rec := httptest.NewRecorder()
	if !authOK(s.renewedAuth(rec, req)) {
		t.Fatal("ersetzte Sitzung ist sofort ungültig — Requests unterwegs bekämen 401")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("ersetzte Sitzung wurde wieder verlängert")
	}
	if exp, _ := sessionExpiry(s.db, old); time.Until(exp) > sessionRetireGrace {
		t.Fatalf("ersetzte Sitzung läuft noch %v", time.Until(exp))
	}
}

// reauth confirms the session with key; allowed returns the passkey IDs
// begin offered.
func (b *browser) reauth(t *testing.T, k *softKey, flags byte) (rec *httptest.ResponseRecorder, allowed []string) {
	t.Helper()
	begin := b.do(http.MethodPost, "/api/auth/reauth/begin", nil)
	var opt struct {
		PublicKey struct {
			Challenge        string
			AllowCredentials []struct{ ID string } `json:"allowCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(begin.Body.Bytes(), &opt); err != nil || opt.PublicKey.Challenge == "" {
		t.Fatalf("reauth/begin: %d %s", begin.Code, begin.Body)
	}
	for _, c := range opt.PublicKey.AllowCredentials {
		allowed = append(allowed, c.ID)
	}
	return b.do(http.MethodPost, "/api/auth/reauth/finish", k.assertion(t, opt.PublicKey.Challenge, flags)), allowed
}

// The confirmation before managing passkeys used to be a full login: on a
// shared device, picking someone else's passkey silently switched accounts.
// Now only the session's own account counts — and the session is still
// replaced, so a copied cookie doesn't turn fresh along with it.
func TestReauthOnlyAcceptsOwnPasskeyAndReplacesSession(t *testing.T) {
	s, b := e2eSetup(t)
	mine := newSoftKey(t)
	mine.store(t, s, true)
	bea := mustUser(t, s, "Bea")
	theirs := newSoftKey(t)
	theirs.storeFor(t, s, bea, true)
	if rec := b.login(t, mine, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	token := b.cookies[sessionCookie].Value
	stale := time.Now().Add(-reauthWindow - time.Minute).UTC().Format(time.RFC3339)
	s.db.Exec("UPDATE sessions SET auth_at = ? WHERE token = ?", stale, hashToken(token))
	// An attacker holding a copy of the same cookie
	thief := &browser{h: b.h, cookies: map[string]*http.Cookie{sessionCookie: b.cookies[sessionCookie]}}

	rec, allowed := b.reauth(t, theirs, flagUP|flagUV)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Bestätigung mit fremdem Passkey: %d, erwartet 401", rec.Code)
	}
	if len(allowed) != 1 || allowed[0] != b64u.EncodeToString(mine.id) {
		t.Fatalf("angebotene Passkeys %v — erwartet nur den eigenen", allowed)
	}
	if rec := b.do(http.MethodPost, "/api/passkeys/begin", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("nach fehlgeschlagener Bestätigung: %d, erwartet 403", rec.Code)
	}
	if rec, _ := b.reauth(t, mine, flagUP); rec.Code != http.StatusUnauthorized {
		t.Fatalf("Bestätigung ohne UV: %d, erwartet 401", rec.Code)
	}
	if rec, _ := b.reauth(t, mine, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("Bestätigung: %d %s", rec.Code, rec.Body)
	}
	if b.cookies[sessionCookie].Value == token {
		t.Fatal("Bestätigung hat keine neue Sitzung ausgestellt")
	}
	if rec := b.do(http.MethodPost, "/api/passkeys/begin", nil); rec.Code != http.StatusOK {
		t.Fatalf("Passkey-begin nach Bestätigung: %d %s", rec.Code, rec.Body)
	}
	// The copy stays stale and expires after the grace period
	if rec := thief.do(http.MethodGet, "/api/auth/fresh", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("kopiertes Cookie nach der Bestätigung des Opfers: %d, erwartet 403", rec.Code)
	}
	if exp, _ := sessionExpiry(s.db, token); time.Until(exp) > sessionRetireGrace {
		t.Fatalf("ersetzte Sitzung läuft noch %v", time.Until(exp))
	}
}

// A re-auth ceremony belongs to the session that started it: finished from
// another session (even of the same account) it doesn't count.
func TestReauthCeremonyIsBoundToItsSession(t *testing.T) {
	s, b := e2eSetup(t)
	key := newSoftKey(t)
	key.store(t, s, true)
	b.login(t, key, flagUP|flagUV)
	other := &browser{h: b.h, cookies: map[string]*http.Cookie{}}
	other.login(t, key, flagUP|flagUV)
	begin := b.do(http.MethodPost, "/api/auth/reauth/begin", nil)
	var opt struct {
		PublicKey struct{ Challenge string } `json:"publicKey"`
	}
	json.Unmarshal(begin.Body.Bytes(), &opt)
	other.cookies[ceremonyCookie] = b.cookies[ceremonyCookie]
	if rec := other.do(http.MethodPost, "/api/auth/reauth/finish", key.assertion(t, opt.PublicKey.Challenge, flagUP|flagUV)); rec.Code != http.StatusBadRequest {
		t.Fatalf("fremde Sitzung beendet die Zeremonie: %d, erwartet 400", rec.Code)
	}
}

// A ceremony used to be marked as consumed already during decryption, before
// the passkey check. 4096 unauthenticated begin + garbage-finish calls would
// push any consumed login out of the list this way — a captured pair of
// cookie and response could then be replayed.
func TestLoginCannotBeReplayedAfterGarbageFinishes(t *testing.T) {
	s, b := e2eSetup(t)
	key := newSoftKey(t)
	key.store(t, s, true)

	begin := b.do(http.MethodPost, "/api/auth/login/begin", nil)
	var opt struct {
		PublicKey struct{ Challenge string } `json:"publicKey"`
	}
	if err := json.Unmarshal(begin.Body.Bytes(), &opt); err != nil {
		t.Fatal(err)
	}
	cer := b.cookies[ceremonyCookie]
	body := key.assertion(t, opt.PublicKey.Challenge, flagUP|flagUV)
	if rec := b.do(http.MethodPost, "/api/auth/login/finish", body); rec.Code != http.StatusNoContent {
		t.Fatalf("Anmeldung: %d %s", rec.Code, rec.Body)
	}
	replay := func() int {
		atk := &browser{h: e2eHandler(s), cookies: map[string]*http.Cookie{ceremonyCookie: cer}}
		return atk.do(http.MethodPost, "/api/auth/login/finish", body).Code
	}
	if c := replay(); c == http.StatusNoContent {
		t.Fatal("sofortige Wiederholung angenommen")
	}
	for range maxUsedCeremonies {
		atk := &browser{h: e2eHandler(s), cookies: map[string]*http.Cookie{}}
		atk.do(http.MethodPost, "/api/auth/login/begin", nil)
		atk.do(http.MethodPost, "/api/auth/login/finish", []byte("{}"))
	}
	if c := replay(); c == http.StatusNoContent {
		t.Fatalf("Wiederholung nach %d Müll-finish angenommen", maxUsedCeremonies)
	}
}

// Every login parses the passkeys of the whole instance: an account must
// not be able to add any number of them. Begin refuses at the limit
// (TestPasskeyLimitIsCheckedBeforeTheCeremony); two devices that started
// with one place left are decided by the INSERT.
func TestPasskeysPerAccountAreLimited(t *testing.T) {
	s, b := e2eSetup(t)
	key := newSoftKey(t)
	key.store(t, s, true)
	for range maxPasskeysPerUser - 2 {
		newSoftKey(t).store(t, s, true)
	}
	other := newBrowser(s)
	for _, br := range []*browser{b, other} {
		if rec := br.login(t, key, flagUP|flagUV); rec.Code != http.StatusNoContent {
			t.Fatalf("login: %d %s", rec.Code, rec.Body)
		}
	}
	first, second := b.addPasskeyChallenge(t), other.addPasskeyChallenge(t)
	if rec := b.do(http.MethodPost, "/api/passkeys/finish", newSoftKey(t).registration(t, first)); rec.Code != http.StatusNoContent {
		t.Fatalf("letzter erlaubter Passkey: %d %s", rec.Code, rec.Body)
	}
	if rec := other.do(http.MethodPost, "/api/passkeys/finish", newSoftKey(t).registration(t, second)); rec.Code != http.StatusConflict {
		t.Fatalf("Passkey über dem Limit: %d, erwartet 409", rec.Code)
	}
	if n, _ := countCredentials(s.db, testUID); n != maxPasskeysPerUser {
		t.Fatalf("%d Passkeys gespeichert, erwartet %d", n, maxPasskeysPerUser)
	}
}

// What a registration sends beyond the key itself is the client's to
// choose — client data, attestation, made-up transports. It used to be
// stored in full, and every login of anyone parses it again.
func TestRegistrationStoresOnlyThePasskey(t *testing.T) {
	s, b := e2eSetup(t)
	key := newSoftKey(t)
	key.store(t, s, true)
	if rec := b.login(t, key, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	padded := newSoftKey(t)
	body := padded.registrationWith(t, b.addPasskeyChallenge(t),
		map[string]any{"pad": strings.Repeat("x", 30000)}, []string{"usb", strings.Repeat("y", 10000)})
	if rec := b.do(http.MethodPost, "/api/passkeys/finish", body); rec.Code != http.StatusNoContent {
		t.Fatalf("Registrierung: %d %s", rec.Code, rec.Body)
	}
	var data string
	if err := s.db.QueryRow("SELECT data FROM credentials ORDER BY id DESC LIMIT 1").Scan(&data); err != nil {
		t.Fatal(err)
	}
	if len(data) > 2000 {
		t.Fatalf("gespeicherter Passkey %d Byte groß", len(data))
	}
	if !strings.Contains(data, `"usb"`) {
		t.Fatalf("bekannter Transport verloren: %s", data)
	}
	if rec := newBrowser(s).login(t, padded, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("Login mit dem neuen Passkey: %d %s", rec.Code, rec.Body)
	}
}

// The limit is already checked at begin: at finish, the authenticator has
// created a passkey for good — it would stay behind in the password manager
// as a dead entry.
func TestPasskeyLimitIsCheckedBeforeTheCeremony(t *testing.T) {
	s, b := e2eSetup(t)
	key := newSoftKey(t)
	key.store(t, s, true)
	for range maxPasskeysPerUser - 1 {
		newSoftKey(t).store(t, s, true)
	}
	if rec := b.login(t, key, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(http.MethodPost, "/api/passkeys/begin", nil); rec.Code != http.StatusConflict {
		t.Fatalf("begin am Limit: %d, erwartet 409", rec.Code)
	}
}
