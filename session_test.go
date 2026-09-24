package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func sessionServer(t *testing.T) *server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := createUser(db, "Admin", true, []byte("todo-single-user")); err != nil {
		t.Fatal(err)
	}
	return &server{db: db, cer: newCeremonies(), hub: newHub(), cfg: config{insecureCookie: true}}
}

// sessionExpiry: a session's expiry (test helper on top of sessionUser).
func sessionExpiry(db *sql.DB, token string) (time.Time, bool) {
	_, exp, ok := sessionUser(db, token)
	return exp, ok
}

// authOK: just the "logged in?" part of renewedAuth.
func authOK(_ int64, ok bool) bool { return ok }

func mustCreateSession(t *testing.T, db *sql.DB, token string, expires time.Time, cred, uid int64) {
	t.Helper()
	if err := createSession(db, token, expires, cred, uid); err != nil {
		t.Fatalf("createSession: %v", err)
	}
}

func sessionCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// Core of the change: a removed passkey should lock out the device that is
// signed in with it.
func TestDeletingPasskeyEndsItsSessions(t *testing.T) {
	s := sessionServer(t)
	future := time.Now().Add(sessionIdleTimeout)
	mustCreateSession(t, s.db, "handy", future, 1, testUID)
	mustCreateSession(t, s.db, "laptop", future, 2, testUID)
	mustCreateSession(t, s.db, "alt", future, 0, testUID) // legacy, unassociated

	deleteSessionsOfCredential(s.db, testUID, 1, "laptop")

	if sessionValid(s.db, "handy") {
		t.Error("Sitzung des gelöschten Passkeys lebt noch")
	}
	if sessionValid(s.db, "alt") {
		t.Error("nicht zuordenbare Alt-Sitzung hätte mitgehen müssen")
	}
	if !sessionValid(s.db, "laptop") {
		t.Error("die Sitzung eines anderen Passkeys darf nicht wegfallen")
	}
}

// Deleting the passkey currently in use should not lock yourself out.
func TestDeletingPasskeyKeepsOwnSession(t *testing.T) {
	s := sessionServer(t)
	mustCreateSession(t, s.db, "meine", time.Now().Add(sessionIdleTimeout), 1, testUID)

	deleteSessionsOfCredential(s.db, testUID, 1, "meine")

	if !sessionValid(s.db, "meine") {
		t.Fatal("die eigene aktive Sitzung wurde beendet")
	}
}

func TestSessionIsBoundToCredential(t *testing.T) {
	s := sessionServer(t)
	rec := httptest.NewRecorder()
	if err := s.startSession(rec, 7, testUID); err != nil {
		t.Fatalf("startSession: %v", err)
	}
	var cred sql.NullInt64
	if err := s.db.QueryRow("SELECT credential_id FROM sessions").Scan(&cred); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !cred.Valid || cred.Int64 != 7 {
		t.Fatalf("credential_id = %v", cred)
	}
	// credentialID 0 means "unknown" and must become NULL, not 0
	mustCreateSession(t, s.db, "ohne", time.Now().Add(time.Hour), 0, testUID)
	if err := s.db.QueryRow("SELECT credential_id FROM sessions WHERE token = ?", hashToken("ohne")).Scan(&cred); err != nil {
		t.Fatalf("select: %v", err)
	}
	if cred.Valid {
		t.Fatalf("erwartet NULL, bekam %v", cred.Int64)
	}
}

// As long as the app is in use, the session must never expire — but without
// writing to the DB on every request.
func TestSessionRenewsOnlyWhenHalfElapsed(t *testing.T) {
	s := sessionServer(t)

	frisch := time.Now().Add(sessionIdleTimeout)
	mustCreateSession(t, s.db, "frisch", frisch, 1, testUID)
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "frisch"})
	rec := httptest.NewRecorder()
	if !authOK(s.renewedAuth(rec, req)) {
		t.Fatal("frische Sitzung muss gültig sein")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("frische Sitzung wurde unnötig verlängert (DB-Schreibzugriff pro Request)")
	}

	// Shortly before expiry: renew and set a new cookie
	bald := time.Now().Add(sessionIdleTimeout/2 - time.Hour)
	mustCreateSession(t, s.db, "bald", bald, 1, testUID)
	req2 := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: "bald"})
	rec2 := httptest.NewRecorder()
	if !authOK(s.renewedAuth(rec2, req2)) {
		t.Fatal("Sitzung kurz vor Ablauf muss gültig bleiben")
	}
	cookies := rec2.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != "bald" {
		t.Fatalf("erwartet aufgefrischtes Cookie mit gleichem Token, bekam %v", cookies)
	}
	exp, ok := sessionExpiry(s.db, "bald")
	if !ok || !exp.After(bald.Add(time.Hour)) {
		t.Fatalf("Ablaufdatum wurde nicht nach vorn geschoben: %v", exp)
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	s := sessionServer(t)
	mustCreateSession(t, s.db, "abgelaufen", time.Now().Add(-time.Minute), 1, testUID)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "abgelaufen"})
	if authOK(s.renewedAuth(httptest.NewRecorder(), req)) {
		t.Fatal("abgelaufene Sitzung wurde akzeptiert")
	}
	// ... and even more so without a cookie
	if authOK(s.renewedAuth(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/tasks", nil))) {
		t.Fatal("Request ohne Cookie wurde akzeptiert")
	}
}

// Legacy sessions with the old 180-day lifetime must be clamped to the
// idle timeout, or the new rule has no effect for months.
func TestPurgeSessionsClampsLegacyLifetimes(t *testing.T) {
	s := sessionServer(t)
	mustCreateSession(t, s.db, "alt", time.Now().Add(180*24*time.Hour), 1, testUID)
	mustCreateSession(t, s.db, "tot", time.Now().Add(-time.Hour), 1, testUID)

	purgeSessions(s.db)

	if sessionCount(t, s.db) != 1 {
		t.Fatalf("abgelaufene Sitzung wurde nicht entfernt (%d übrig)", sessionCount(t, s.db))
	}
	exp, ok := sessionExpiry(s.db, "alt")
	if !ok {
		t.Fatal("gültige Alt-Sitzung wurde entfernt")
	}
	if exp.After(time.Now().Add(sessionIdleTimeout + time.Minute)) {
		t.Fatalf("Laufzeit nicht gekappt: %v", exp)
	}
	// idempotent: a second run extends nothing
	before := exp
	purgeSessions(s.db)
	after, _ := sessionExpiry(s.db, "alt")
	if after.After(before.Add(time.Minute)) {
		t.Fatalf("purgeSessions hat die Laufzeit verlängert: %v -> %v", before, after)
	}
}

// passkeyReq is a request from session token.
func passkeyReq(s *server, method, token, id string) *http.Request {
	req := withUser(httptest.NewRequest(method, "/api/passkeys/"+id, nil), testUID)
	req.AddCookie(&http.Cookie{Name: s.cookieName(sessionCookie), Value: token})
	if id != "" {
		req.SetPathValue("id", id)
	}
	return req
}

func mustInsertCredential(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	res, err := db.Exec("INSERT INTO credentials (name, created_at, data, user_id) VALUES (?, 'z', '{}', ?)", name, testUID)
	if err != nil {
		t.Fatalf("insert credential: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// A stolen session cookie alone must not be able to manage passkeys:
// otherwise the attacker could register their own, delete the owner's — and
// "sign out everywhere" would no longer help. A fresh passkey sign-in is required.
func TestPasskeyManagementNeedsFreshAuth(t *testing.T) {
	s := testServer(t)
	future := time.Now().Add(sessionIdleTimeout)
	mustCreateSession(t, s.db, "frisch", future, 1, testUID)
	mustCreateSession(t, s.db, "alt", future, 1, testUID)
	stale := time.Now().Add(-reauthWindow - time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec("UPDATE sessions SET auth_at = ? WHERE token = ?", stale, hashToken("alt")); err != nil {
		t.Fatalf("update: %v", err)
	}
	mustInsertCredential(t, s.db, "Handy")
	laptop := mustInsertCredential(t, s.db, "Laptop")

	for name, h := range map[string]http.HandlerFunc{"begin": s.handlePasskeyBegin, "delete": s.handlePasskeyDelete} {
		rec := httptest.NewRecorder()
		h(rec, passkeyReq(s, http.MethodPost, "alt", fmt.Sprint(laptop)))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s mit alter Sitzung: %d, erwartet 403", name, rec.Code)
		}
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		if body["reauth"] != true {
			t.Fatalf("%s: 403 ohne reauth-Hinweis für den Client: %v", name, body)
		}
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM credentials").Scan(&n)
	if n != 2 {
		t.Fatalf("Passkey wurde trotz alter Sitzung gelöscht (%d übrig)", n)
	}

	// Freshly signed in: deleting works, the last one stays protected, and one
	// that's already deleted is 404 instead of "the last passkey".
	del := func(id int64) int {
		rec := httptest.NewRecorder()
		s.handlePasskeyDelete(rec, passkeyReq(s, http.MethodDelete, "frisch", fmt.Sprint(id)))
		return rec.Code
	}
	if code := del(laptop); code != http.StatusNoContent {
		t.Fatalf("löschen mit frischer Sitzung: %d", code)
	}
	if code := del(laptop); code != http.StatusNotFound {
		t.Fatalf("schon gelöschter Passkey: %d, erwartet 404", code)
	}
	var last int64
	s.db.QueryRow("SELECT id FROM credentials").Scan(&last)
	if code := del(last); code != http.StatusConflict {
		t.Fatalf("letzter Passkey: %d, erwartet 409", code)
	}
}

// Legacy sessions have no auth_at — they remain valid but do not count as
// freshly confirmed.
func TestSessionMigrationAddsAuthAt(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"DROP TABLE sessions",
		"CREATE TABLE sessions (token TEXT PRIMARY KEY, expires_at TEXT NOT NULL, credential_id INTEGER)",
		"INSERT INTO sessions (token, expires_at) VALUES ('alt', '2099-01-01T00:00:00Z')",
		"INSERT INTO credentials (name, created_at, data) VALUES ('Handy', 'z', '{}')",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !sessionValid(db, "alt") {
		t.Fatal("Bestands-Sitzung hat die Migration nicht überlebt")
	}
	if _, ok := sessionAuthAt(db, "alt"); ok {
		t.Fatal("Bestands-Sitzung ohne auth_at darf nicht als frisch gelten")
	}
	if err := createSession(db, "neu", time.Now().Add(time.Hour), 1, testUID); err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if at, ok := sessionAuthAt(db, "neu"); !ok || time.Since(at) > time.Minute {
		t.Fatalf("neue Sitzung ohne gültiges auth_at: %v %v", at, ok)
	}
}

func TestSessionMigrationAddsCredentialColumn(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Recreate the legacy schema without credential_id
	for _, stmt := range []string{
		"DROP TABLE sessions",
		"CREATE TABLE sessions (token TEXT PRIMARY KEY, expires_at TEXT NOT NULL)",
		"INSERT INTO sessions (token, expires_at) VALUES ('alt', '2099-01-01T00:00:00Z')",
		"INSERT INTO credentials (name, created_at, data) VALUES ('Handy', 'z', '{}')",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !sessionValid(db, "alt") {
		t.Fatal("Bestands-Sitzung hat die Migration nicht überlebt")
	}
	if err := createSession(db, "neu", time.Now().Add(time.Hour), 3, testUID); err != nil {
		t.Fatalf("createSession nach Migration: %v", err)
	}
}
