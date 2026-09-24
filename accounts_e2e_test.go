package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// Real ceremonies across accounts: who gets in with which passkey.

func newBrowser(s *server) *browser {
	return &browser{h: e2eHandler(s), cookies: map[string]*http.Cookie{}}
}

// The passkey determines the person — not who the instance belongs to.
func TestLoginFindsOwnerOfPasskey(t *testing.T) {
	s, admin := e2eSetup(t)
	newSoftKey(t).store(t, s, true)
	bea := mustUser(t, s, "Bea")
	beaKey := newSoftKey(t)
	beaKey.storeFor(t, s, bea, true)
	mustCreate(t, s, `{"title":"vom Admin"}`)
	mustCreateAs(t, s, bea, `{"title":"von Bea"}`)
	_ = admin

	b := newBrowser(s)
	if rec := b.login(t, beaKey, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("Login mit Beas Passkey: %d %s", rec.Code, rec.Body)
	}
	body := b.do(http.MethodGet, "/api/tasks", nil).Body.String()
	if !strings.Contains(body, "von Bea") || strings.Contains(body, "vom Admin") {
		t.Fatalf("nach Beas Login: %s", body)
	}
}

// A passkey from Bea with the admin's user handle in the response is
// forged — no login, neither as Bea nor as admin.
func TestLoginRejectsForeignUserHandle(t *testing.T) {
	s, _ := e2eSetup(t)
	newSoftKey(t).store(t, s, true)
	bea := mustUser(t, s, "Bea")
	beaKey := newSoftKey(t)
	beaKey.storeFor(t, s, bea, true)

	b := newBrowser(s)
	begin := b.do(http.MethodPost, "/api/auth/login/begin", nil)
	var opt struct {
		PublicKey struct{ Challenge string } `json:"publicKey"`
	}
	json.Unmarshal(begin.Body.Bytes(), &opt)
	rec := b.do(http.MethodPost, "/api/auth/login/finish",
		beaKey.assertionWith(t, opt.PublicKey.Challenge, flagUP|flagUV, []byte("todo-single-user")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("fremder User-Handle: %d, erwartet 401", rec.Code)
	}
	if b.do(http.MethodGet, "/api/tasks", nil).Code != http.StatusUnauthorized {
		t.Fatal("trotzdem angemeldet")
	}
}

// Two devices start with the same code: only the first one gets the
// account.
func TestSetupCodeIsSingleUse(t *testing.T) {
	s, _ := e2eSetup(t)
	newSoftKey(t).store(t, s, true)
	bea := mustUser(t, s, "Bea")
	code, err := setUserCode(s.db, bea)
	if err != nil {
		t.Fatal(err)
	}
	begin := func(b *browser) string {
		b.setupCode = code
		rec := b.do(http.MethodPost, "/api/setup/begin", nil)
		var opt struct {
			PublicKey struct{ Challenge string } `json:"publicKey"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &opt); err != nil || opt.PublicKey.Challenge == "" {
			t.Fatalf("setup/begin: %d %s", rec.Code, rec.Body)
		}
		return opt.PublicKey.Challenge
	}
	b1, b2 := newBrowser(s), newBrowser(s)
	k1, k2 := newSoftKey(t), newSoftKey(t)
	c1, c2 := begin(b1), begin(b2)
	if rec := b1.do(http.MethodPost, "/api/setup/finish", k1.registration(t, c1)); rec.Code != http.StatusNoContent {
		t.Fatalf("erstes Einlösen: %d %s", rec.Code, rec.Body)
	}
	if rec := b2.do(http.MethodPost, "/api/setup/finish", k2.registration(t, c2)); rec.Code != http.StatusForbidden {
		t.Fatalf("zweites Einlösen: %d, erwartet 403", rec.Code)
	}
	if n, _ := countCredentials(s.db, bea); n != 1 {
		t.Fatalf("Bea hat %d Passkeys, erwartet 1", n)
	}
	if uid, _, ok := sessionUserOf(t, s, b1); !ok || uid != bea {
		t.Fatalf("Sitzung nach dem Einlösen gehört %d, erwartet Bea (%d)", uid, bea)
	}
}

// Recovery per the README: only delete the admin's passkeys. The instance
// then accepts the code from the log again — and attaches the new passkey
// to the existing admin account, along with its data. Everyone else can
// keep logging in.
func TestRecoveryGivesAdminBack(t *testing.T) {
	s, _ := e2eSetup(t)
	newSoftKey(t).store(t, s, true)
	bea := mustUser(t, s, "Bea")
	beaKey := newSoftKey(t)
	beaKey.storeFor(t, s, bea, true)
	mustCreate(t, s, `{"title":"Admin-Daten"}`)

	for _, q := range []string{
		"DELETE FROM credentials WHERE user_id IN (SELECT id FROM users WHERE is_admin = 1)",
		"DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE is_admin = 1)",
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.dbPath = filepath.Join(t.TempDir(), "todo.db")
	if err := s.prepareSetupCode(); err != nil {
		t.Fatal(err)
	}
	code := readSetupCode(t, s)

	if rec := newBrowser(s).login(t, beaKey, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("Bea während der Recovery: %d %s", rec.Code, rec.Body)
	}
	admin := newBrowser(s)
	if rec := admin.setup(t, newSoftKey(t), code); rec.Code != http.StatusNoContent {
		t.Fatalf("Recovery-Setup: %d %s", rec.Code, rec.Body)
	}
	if body := admin.do(http.MethodGet, "/api/tasks", nil).Body.String(); !strings.Contains(body, "Admin-Daten") {
		t.Fatalf("Admin sieht seine Daten nicht: %s", body)
	}
	var users int
	s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&users)
	if n, _ := countCredentials(s.db, testUID); n != 1 || users != 2 {
		t.Fatalf("Admin hat %d Passkeys, %d Konten", n, users)
	}
	if setup, _ := s.setupMode(); setup {
		t.Fatal("nach der Recovery noch im Setup")
	}
	// The code is used up
	if rec := newBrowser(s).setup(t, newSoftKey(t), code); rec.Code != http.StatusForbidden {
		t.Fatalf("zweite Recovery mit altem Code: %d", rec.Code)
	}
}

func sessionUserOf(t *testing.T, s *server, b *browser) (int64, string, bool) {
	t.Helper()
	c := b.cookies[s.cookieName(sessionCookie)]
	if c == nil {
		return 0, "", false
	}
	uid, _, ok := sessionUser(s.db, c.Value)
	return uid, c.Value, ok
}
