package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	ceremonyTTL    = 5 * time.Minute
	ceremonyCookie = "ceremony"
	// Redeemed ceremonies, remembered until they'd expire anyway (single use).
	// Only recorded after a passed passkey check (consume): the
	// list only grows with real logins/registrations, not with
	// junk requests. If it overflows anyway, the oldest entry gets evicted.
	maxUsedCeremonies = 4096
	// reauthWindow: how long after a passkey login a session may
	// manage passkeys. After that it needs a fresh confirmation — otherwise
	// a stolen session cookie alone would be enough to register a new passkey
	// and delete the owner's: a permanent takeover
	// that not even "sign out everywhere" would fix.
	reauthWindow = 10 * time.Minute
)

// wanUser: an account for go-webauthn. id is the user handle carried by
// the account's passkeys (the migrated legacy user: "todo-single-user").
type wanUser struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

func (u *wanUser) WebAuthnID() []byte                         { return u.id }
func (u *wanUser) WebAuthnName() string                       { return u.name }
func (u *wanUser) WebAuthnDisplayName() string                { return u.name }
func (u *wanUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// wanData: an account (or, at the start of login, all passkeys of the
// instance) plus a mapping credential ID -> DB row (SignCount) and -> account.
type wanData struct {
	user    *wanUser
	rowByID map[string]int64
	userOf  map[string]int64
}

// loadCreds attaches the passkeys of uid (0 = all) to a wanUser.
func loadCreds(db *sql.DB, uid int64, u *wanUser) (*wanData, error) {
	stored, err := listCredentials(db, uid)
	if err != nil {
		return nil, err
	}
	d := &wanData{user: u, rowByID: map[string]int64{}, userOf: map[string]int64{}}
	for _, sc := range stored {
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(sc.Data), &c); err != nil {
			return nil, fmt.Errorf("credential %d: %w", sc.ID, err)
		}
		d.user.creds = append(d.user.creds, c)
		d.rowByID[string(c.ID)] = sc.ID
		d.userOf[string(c.ID)] = sc.UserID
	}
	return d, nil
}

// loadWanUser: an account with its passkeys.
func loadWanUser(db *sql.DB, uid int64) (*wanData, error) {
	u := &wanUser{}
	if err := db.QueryRow("SELECT webauthn_id, name FROM users WHERE id = ?", uid).Scan(&u.id, &u.name); err != nil {
		return nil, err
	}
	return loadCreds(db, uid, u)
}

// loadWanAll: all passkeys of the instance under a placeholder account — for
// the start of login, where the user is only identified once the response arrives.
func loadWanAll(db *sql.DB) (*wanData, error) {
	return loadCreds(db, 0, &wanUser{id: []byte("todo-login"), name: "Todo"})
}

// ceremonies: in-flight WebAuthn ceremonies (between begin and finish) live
// not on the server, but encrypted and signed (AES-GCM, key per
// process) in the browser's short-lived ceremony cookie. A table on
// the server could be flooded: ~128 IPv6 /64s out of a single free /48
// would fill it and evict the user's in-flight login. In the cookie
// there's nothing to evict, and each browser only carries its own.
type ceremonies struct {
	aead cipher.AEAD
	now  func() time.Time

	mu   sync.Mutex
	used map[string]time.Time // redeemed -> expiry; single use
	fifo []string             // redemption order = expiry order (fixed TTL)
}

type ceremonyToken struct {
	Kind    string                `json:"k"`
	Nonce   string                `json:"n"` // unique per ceremony, key into used
	Expires int64                 `json:"e"`
	SD      *webauthn.SessionData `json:"s"`
}

func newCeremonies() *ceremonies {
	key := make([]byte, 32)
	rand.Read(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // only with a wrong key length
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &ceremonies{aead: aead, now: time.Now, used: map[string]time.Time{}}
}

func (c *ceremonies) seal(kind string, sd *webauthn.SessionData) (string, error) {
	id, err := randomToken()
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(ceremonyToken{Kind: kind, Nonce: id, Expires: c.now().Add(ceremonyTTL).Unix(), SD: sd})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.aead.NonceSize())
	rand.Read(nonce)
	// The kind is also embedded in the AAD: a registration cookie
	// won't even decrypt during login.
	return base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, plain, []byte(kind))), nil
}

// ceremonyTicket is an opened, not-yet-redeemed ceremony.
type ceremonyTicket struct {
	sd    *webauthn.SessionData
	nonce string
	exp   time.Time
}

// open checks authenticity, kind, expiry, and whether the ceremony has
// already been redeemed. It's only redeemed via consume — after the passkey check.
// open used to mark it immediately; then begin + junk finish calls filled the list
// and evicted real logins, which could then be replayed.
func (c *ceremonies) open(kind, value string) *ceremonyTicket {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	ns := c.aead.NonceSize()
	if err != nil || len(raw) < ns {
		return nil
	}
	plain, err := c.aead.Open(nil, raw[:ns], raw[ns:], []byte(kind))
	if err != nil {
		return nil
	}
	var tok ceremonyToken
	if err := json.Unmarshal(plain, &tok); err != nil || tok.Kind != kind || tok.SD == nil {
		return nil
	}
	exp := time.Unix(tok.Expires, 0)
	if !c.now().Before(exp) {
		return nil
	}
	c.mu.Lock()
	_, seen := c.used[tok.Nonce]
	c.mu.Unlock()
	if seen {
		return nil
	}
	return &ceremonyTicket{sd: tok.SD, nonce: tok.Nonce, exp: exp}
}

// consume redeems the ceremony; false if that already happened (two
// simultaneous finish calls with the same cookie — only one wins).
func (c *ceremonies) consume(t *ceremonyTicket) bool {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.fifo) > 0 && (len(c.fifo) >= maxUsedCeremonies || !now.Before(c.used[c.fifo[0]])) {
		delete(c.used, c.fifo[0])
		c.fifo = c.fifo[1:]
	}
	if _, seen := c.used[t.nonce]; seen {
		return false
	}
	c.used[t.nonce] = t.exp
	c.fifo = append(c.fifo, t.nonce)
	return true
}

// startCeremony hands the ceremony to the browser as a cookie.
func (s *server) startCeremony(w http.ResponseWriter, r *http.Request, kind string, sd *webauthn.SessionData) error {
	value, err := s.cer.seal(kind, sd)
	if err != nil {
		return err
	}
	s.setCookie(w, ceremonyCookie, value, int(ceremonyTTL.Seconds()))
	return nil
}

// takeCeremony retrieves the ceremony from the cookie and deletes it. After a
// successful passkey check, the caller must call s.cer.consume.
func (s *server) takeCeremony(w http.ResponseWriter, r *http.Request, kind string) *ceremonyTicket {
	value := s.cookie(r, ceremonyCookie)
	s.setCookie(w, ceremonyCookie, "", -1)
	if value == "" {
		return nil
	}
	return s.cer.open(kind, value)
}

// wan builds the WebAuthn instance from the request: RP ID = host (without port),
// allowed origin = Origin header. That way no domain configuration is needed.
func (s *server) wan(r *http.Request) (*webauthn.WebAuthn, error) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	origins := []string{"https://" + r.Host, "http://" + r.Host}
	if o := r.Header.Get("Origin"); o != "" {
		origins = []string{o}
	}
	return webauthn.New(&webauthn.Config{
		RPDisplayName: "Todo",
		RPID:          host,
		RPOrigins:     origins,
		// Passkey-only: the key is the entire login. The library
		// only checks user verification (PIN/biometrics) when set to "required". That applies
		// here to every new registration; for login see uvRequiredFor.
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			UserVerification: protocol.VerificationRequired,
		},
	})
}

// uvRequiredFor: at login, UV is required for every passkey that has
// already shown it before — during registration (where it's required) or
// a previous login. The library can only require UV for all passkeys at
// once, which would lock out security keys without a PIN that were registered before
// this rule existed. This way a stolen key can't get in
// without its PIN, and existing keys stay usable.
func uvRequiredFor(creds []webauthn.Credential, id []byte) bool {
	for _, c := range creds {
		if bytes.Equal(c.ID, id) {
			return c.Flags.UserVerified
		}
	}
	return true
}

// requireFreshAuth only lets through sessions that authenticated via passkey
// less than reauthWindow ago. Otherwise 403 with "reauth": the
// client logs in again via passkey (which yields a fresh session)
// and retries the request.
func (s *server) requireFreshAuth(w http.ResponseWriter, r *http.Request) bool {
	if at, ok := sessionAuthAt(s.db, s.sessionToken(r)); ok && time.Since(at) < reauthWindow {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":  T(requestLang(r), "passkeys.reauth"),
		"reauth": true,
	})
	return false
}

// setupMode: no admin with a passkey — either a new instance (no account yet) or
// a recovery (admin's passkeys deleted, see README). In that case
// /setup accepts the instance code from the server log.
func (s *server) setupMode() (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM credentials c JOIN users u ON u.id = c.user_id WHERE u.is_admin = 1").Scan(&n)
	return n == 0, err
}

// noPasskeys: nobody can log in — login then redirects to /setup.
// During a recovery the other accounts can still log in normally.
func (s *server) noPasskeys() (bool, error) {
	var n int
	err := s.db.QueryRow("SELECT COUNT(*) FROM credentials").Scan(&n)
	return n == 0, err
}

func credName(r *http.Request) string {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Passkey"
	}
	runes := []rune(name)
	if len(runes) > 60 {
		name = string(runes[:60])
	}
	return name
}

// --- Login ---

func (s *server) handleLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, r, http.StatusForbidden, "error.origin")
		return
	}
	none, err := s.noPasskeys()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if none {
		writeError(w, r, http.StatusConflict, "error.setup_pending")
		return
	}
	wd, err := loadWanAll(s.db)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	wan, err := s.wan(r)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	// "preferred" instead of the "required" default: UV is enforced per passkey
	// in handleLoginFinish (uvRequiredFor). Browsers still prompt for PIN/biometrics
	// with preferred, wherever the authenticator supports it.
	options, sd, err := wan.BeginLogin(wd.user, webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	// The allowed passkeys are always all of them from the DB; finish rereads them from there.
	// In the cookie, with many long key IDs, they'd have pushed past the 4 KB
	// that browsers accept.
	cookieSD := *sd
	cookieSD.AllowedCredentialIDs = nil
	if err := s.startCeremony(w, r, "login", &cookieSD); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	writeJSON(w, http.StatusOK, options)
}

func (s *server) handleLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, r, http.StatusForbidden, "error.origin")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	ticket := s.takeCeremony(w, r, "login")
	if ticket == nil {
		writeError(w, r, http.StatusBadRequest, "error.no_login")
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_authenticator")
		return
	}
	// The user is only identified once the response arrives: via the credential ID. It's then
	// validated against exactly their passkeys and their user handle (ValidateLogin
	// compares it against the one in the response) — a passkey belonging to A can never log in as B.
	all, err := loadWanAll(s.db)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	uid, known := all.userOf[string(parsed.RawID)]
	if !known || uid == 0 {
		writeError(w, r, http.StatusUnauthorized, "error.login_failed")
		return
	}
	wd, err := loadWanUser(s.db, uid)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "error.login_failed")
		return
	}
	wan, err := s.wan(r)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	sd := ticket.sd
	sd.UserID = wd.user.WebAuthnID()
	sd.AllowedCredentialIDs = nil
	for _, c := range wd.user.creds {
		sd.AllowedCredentialIDs = append(sd.AllowedCredentialIDs, c.ID)
	}
	cred, err := wan.ValidateLogin(wd.user, *sd, parsed)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "error.login_failed")
		return
	}
	// cred.Flags come from this login, wd.user.creds from the DB.
	if uvRequiredFor(wd.user.creds, cred.ID) && !cred.Flags.UserVerified {
		writeError(w, r, http.StatusUnauthorized, "error.login_uv")
		return
	}
	if !s.cer.consume(ticket) {
		writeError(w, r, http.StatusBadRequest, "error.no_login")
		return
	}
	if cred.Authenticator.CloneWarning {
		log.Printf("webauthn: CloneWarning for credential %x", cred.ID)
	}
	// Persist SignCount
	credRow := wd.rowByID[string(cred.ID)]
	if credRow > 0 {
		if data, err := json.Marshal(cred); err == nil {
			if _, err := s.db.Exec("UPDATE credentials SET data = ? WHERE id = ?", string(data), credRow); err != nil {
				log.Printf("webauthn: failed to persist signcount: %v", err)
			}
		}
	}
	if err := s.startSession(w, credRow, uid); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	// A login from within an active session (confirmation before
	// managing passkeys): the replaced session expires shortly, instead of staying
	// valid for weeks as a zombie. Not deleted immediately — requests still
	// in flight with the old cookie would otherwise get bounced to login via 401.
	if old := s.sessionToken(r); old != "" {
		retireSession(s.db, old, sessionRetireGrace)
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Setup (first account, admin recovery, account with a code from the admin) ---

func (s *server) handleSetupBegin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, r, http.StatusForbidden, "error.origin")
		return
	}
	// Account with a setup code from the admin
	if _, handle, name, ok := userForCode(s.db, r.Header.Get("X-Setup-Code")); ok {
		s.beginRegistration(w, r, "setup", &wanUser{id: handle, name: name})
		return
	}
	setup, err := s.setupMode()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if !setup || !s.checkSetupCode(r) {
		writeError(w, r, http.StatusForbidden, "error.setup_code")
		return
	}
	// Recovery: the passkey belongs to the existing admin (their user handle).
	// New instance: the first account gets a fresh one; finish reads it
	// from the ceremony and creates the account with it.
	handle, name := newWebAuthnID(), "Admin"
	err = s.db.QueryRow("SELECT webauthn_id, name FROM users WHERE is_admin = 1 ORDER BY id LIMIT 1").Scan(&handle, &name)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.beginRegistration(w, r, "setup", &wanUser{id: handle, name: name})
}

func (s *server) handleSetupFinish(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, r, http.StatusForbidden, "error.origin")
		return
	}
	if _, _, _, ok := userForCode(s.db, r.Header.Get("X-Setup-Code")); ok {
		s.finishSetupWithCode(w, r)
		return
	}
	setup, err := s.setupMode()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if !setup || !s.checkSetupCode(r) {
		writeError(w, r, http.StatusForbidden, "error.setup_code")
		return
	}
	cred, handle, ok := s.finishRegistration(w, r, "setup", nil)
	if !ok {
		return
	}
	data, err := json.Marshal(cred)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	// Guards in the same transaction as the write — the transaction holds
	// the single DB connection, so no second setup can write in between.
	// First account: no account yet. Recovery: the admin from begin, still
	// without a passkey.
	uid, credRow, err := func() (int64, int64, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return 0, 0, err
		}
		defer tx.Rollback()
		var uid int64
		var adminHandle []byte
		err = tx.QueryRow("SELECT id, webauthn_id FROM users WHERE is_admin = 1 ORDER BY id LIMIT 1").Scan(&uid, &adminHandle)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); err != nil {
				return 0, 0, err
			}
			if n > 0 {
				return 0, 0, errSetupDone
			}
			if uid, err = createUser(tx, "Admin", true, handle); err != nil {
				return 0, 0, err
			}
		case err != nil:
			return 0, 0, err
		default:
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM credentials WHERE user_id = ?", uid).Scan(&n); err != nil {
				return 0, 0, err
			}
			if n > 0 || !bytes.Equal(adminHandle, handle) {
				return 0, 0, errSetupDone
			}
		}
		credRow, err := insertCredential(tx, uid, credName(r), string(data))
		if err != nil {
			return 0, 0, err
		}
		return uid, credRow, tx.Commit()
	}()
	if errors.Is(err, errSetupDone) {
		writeError(w, r, http.StatusForbidden, "error.setup_done")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if err := s.prepareSetupCode(); err != nil {
		log.Printf("setup: remove setup code: %v", err)
	}
	if err := s.startSession(w, credRow, uid); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// finishSetupWithCode: an account created by the admin gets its first
// passkey. The code is consumed in the same step that saves the
// passkey — a second attempt with the same code fails.
func (s *server) finishSetupWithCode(w http.ResponseWriter, r *http.Request) {
	code := r.Header.Get("X-Setup-Code")
	uid, handle, name, ok := userForCode(s.db, code)
	if !ok {
		writeError(w, r, http.StatusForbidden, "error.setup_code")
		return
	}
	cred, _, ok := s.finishRegistration(w, r, "setup", &wanUser{id: handle, name: name})
	if !ok {
		return
	}
	data, err := json.Marshal(cred)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	credRow, err := func() (int64, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return 0, err
		}
		defer tx.Rollback()
		res, err := tx.Exec("UPDATE users SET setup_hash = NULL, setup_expires = NULL WHERE id = ? AND setup_hash = ? AND setup_expires > ?",
			uid, hashToken(normalizeSetupCode(code)), time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, errSetupDone // code consumed or expired in the meantime
		}
		credRow, err := insertCredential(tx, uid, credName(r), string(data))
		if err != nil {
			return 0, err
		}
		return credRow, tx.Commit()
	}()
	if errors.Is(err, errSetupDone) {
		writeError(w, r, http.StatusForbidden, "error.setup_code")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if err := s.startSession(w, credRow, uid); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Additional passkeys (logged in, in the control panel) ---

// Only begin checks freshness: finish is tied via the ceremony cookie
// firmly to this exact begin, and the registration itself may take longer.
func (s *server) handlePasskeyBegin(w http.ResponseWriter, r *http.Request) {
	if !s.requireFreshAuth(w, r) {
		return
	}
	wd, err := loadWanUser(s.db, userID(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.beginRegistration(w, r, "register", wd.user)
}

func (s *server) handlePasskeyFinish(w http.ResponseWriter, r *http.Request) {
	wd, err := loadWanUser(s.db, userID(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	cred, _, ok := s.finishRegistration(w, r, "register", wd.user)
	if !ok {
		return
	}
	data, err := json.Marshal(cred)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if _, err := insertCredential(s.db, userID(r), credName(r), string(data)); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handlePasskeyList(w http.ResponseWriter, r *http.Request) {
	creds, err := listCredentials(s.db, userID(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if creds == nil {
		creds = []storedCredential{}
	}
	writeJSON(w, http.StatusOK, creds)
}

func (s *server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return
	}
	if !s.requireFreshAuth(w, r) {
		return
	}
	uid := userID(r)
	// Atomic: you can never delete your own last passkey (lockout protection)
	res, err := s.db.Exec("DELETE FROM credentials WHERE id = ? AND user_id = ? AND (SELECT COUNT(*) FROM credentials WHERE user_id = ?) > 1", id, uid, uid)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Two reasons for 0 rows: already gone (e.g. deleted from another
		// device) or really the last one.
		var exists int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM credentials WHERE id = ? AND user_id = ?", id, uid).Scan(&exists); err == nil && exists == 0 {
			writeError(w, r, http.StatusNotFound, "error.passkey_not_found")
			return
		}
		writeError(w, r, http.StatusConflict, "error.last_passkey")
		return
	}
	// A removed passkey should also lock out the device that's logged in
	// with it — otherwise the session would keep running for weeks.
	deleteSessionsOfCredential(s.db, uid, id, s.sessionToken(r))
	// ... and no more notifications to its devices.
	if err := deletePushSubsOfCredential(s.db, uid, id); err != nil {
		log.Printf("push: devices of passkey %d: %v", id, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Shared registration logic ---

// beginRegistration starts a registration for user (exclusions: their
// existing passkeys).
func (s *server) beginRegistration(w http.ResponseWriter, r *http.Request, kind string, user *wanUser) {
	wan, err := s.wan(r)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	var exclusions []protocol.CredentialDescriptor
	for _, c := range user.creds {
		exclusions = append(exclusions, c.Descriptor())
	}
	options, sd, err := wan.BeginRegistration(user,
		webauthn.WithExclusions(exclusions),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
	)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if err := s.startCeremony(w, r, kind, sd); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	writeJSON(w, http.StatusOK, options)
}

// finishRegistration validates the response against user and returns the passkey and
// the user handle. user nil: the first account — its handle comes from the
// ceremony (begin generated it).
func (s *server) finishRegistration(w http.ResponseWriter, r *http.Request, kind string, user *wanUser) (*webauthn.Credential, []byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	ticket := s.takeCeremony(w, r, kind)
	if ticket == nil {
		writeError(w, r, http.StatusBadRequest, "error.no_registration")
		return nil, nil, false
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(r.Body)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_authenticator")
		return nil, nil, false
	}
	if user == nil {
		user = &wanUser{id: ticket.sd.UserID, name: "Admin"}
	}
	wan, err := s.wan(r)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return nil, nil, false
	}
	cred, err := wan.CreateCredential(user, *ticket.sd, parsed)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.registration_failed")
		return nil, nil, false
	}
	if !s.cer.consume(ticket) {
		writeError(w, r, http.StatusBadRequest, "error.no_registration")
		return nil, nil, false
	}
	return cred, user.id, true
}
