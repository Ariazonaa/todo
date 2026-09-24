package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An instance without a passkey used to belong to whoever called /setup
// first — after a recovery, along with all existing data. The setup code
// only lives in the server log and in a file next to the DB; knowing just
// the URL isn't enough to get in.

func setupRequest(t *testing.T, s *server, path, code string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, e2eOrigin+path, nil)
	req.Header.Set("Origin", e2eOrigin)
	if code != "" {
		req.Header.Set("X-Setup-Code", code)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	switch path {
	case "/api/setup/begin":
		s.handleSetupBegin(rec, req)
	case "/api/setup/finish":
		s.handleSetupFinish(rec, req)
	}
	return rec
}

func setupServer(t *testing.T) *server {
	t.Helper()
	s := testServer(t)
	// Setup only exists without accounts
	for _, q := range []string{"DELETE FROM users", "DELETE FROM lists"} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	s.cfg.insecureCookie = true
	s.cfg.dbPath = filepath.Join(t.TempDir(), "todo.db")
	if err := s.prepareSetupCode(); err != nil {
		t.Fatalf("prepareSetupCode: %v", err)
	}
	return s
}

func readSetupCode(t *testing.T, s *server) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(s.cfg.dbPath), setupCodeFile))
	if err != nil {
		t.Fatalf("Setup-Code-Datei: %v", err)
	}
	return strings.TrimSpace(string(b))
}

func TestSetupBeginRejectsMissingCode(t *testing.T) {
	s := setupServer(t)
	if rec := setupRequest(t, s, "/api/setup/begin", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("ohne Code: %d %s, erwartet 403", rec.Code, rec.Body)
	}
}

func TestSetupBeginRejectsWrongCode(t *testing.T) {
	s := setupServer(t)
	if rec := setupRequest(t, s, "/api/setup/begin", "AAAA-BBBB-CCCC-DDDD"); rec.Code != http.StatusForbidden {
		t.Fatalf("falscher Code: %d %s, erwartet 403", rec.Code, rec.Body)
	}
}

func TestSetupBeginAcceptsCodeFromFile(t *testing.T) {
	s := setupServer(t)
	code := readSetupCode(t, s)
	if rec := setupRequest(t, s, "/api/setup/begin", code); rec.Code != http.StatusOK {
		t.Fatalf("richtiger Code: %d %s, erwartet 200", rec.Code, rec.Body)
	}
}

// Typed on a phone: lowercase and without hyphens also count.
func TestSetupCodeIgnoresCaseAndSeparators(t *testing.T) {
	s := setupServer(t)
	code := strings.ToLower(strings.ReplaceAll(readSetupCode(t, s), "-", " "))
	if rec := setupRequest(t, s, "/api/setup/begin", code); rec.Code != http.StatusOK {
		t.Fatalf("Code %q: %d %s, erwartet 200", code, rec.Code, rec.Body)
	}
}

// finish checks the code itself, not only via the ceremony from begin.
func TestSetupFinishRejectsMissingCode(t *testing.T) {
	s := setupServer(t)
	begin := setupRequest(t, s, "/api/setup/begin", readSetupCode(t, s))
	if begin.Code != http.StatusOK {
		t.Fatalf("begin: %d %s", begin.Code, begin.Body)
	}
	rec := setupRequest(t, s, "/api/setup/finish", "", begin.Result().Cookies()...)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("finish ohne Code: %d %s, erwartet 403", rec.Code, rec.Body)
	}
}

// If the admin has a passkey, there is no instance code — and a leftover
// file from an earlier setup is removed.
func TestNoSetupCodeOnceAPasskeyExists(t *testing.T) {
	s := setupServer(t)
	admin, err := createUser(s.db, "Admin", true, []byte("todo-single-user"))
	if err != nil {
		t.Fatal(err)
	}
	newSoftKey(t).storeFor(t, s, admin, true)
	if err := s.prepareSetupCode(); err != nil {
		t.Fatalf("prepareSetupCode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.cfg.dbPath), setupCodeFile)); !os.IsNotExist(err) {
		t.Fatalf("Setup-Code-Datei existiert noch (err=%v)", err)
	}
	if s.checkSetupCode(httptest.NewRequest(http.MethodPost, "/", nil)) {
		t.Fatal("leerer Code wird akzeptiert")
	}
}

// Every start of an empty instance gets a new code.
func TestSetupCodeDiffersPerServer(t *testing.T) {
	a, b := readSetupCode(t, setupServer(t)), readSetupCode(t, setupServer(t))
	if a == b {
		t.Fatalf("zwei Instanzen, gleicher Code %q", a)
	}
}
