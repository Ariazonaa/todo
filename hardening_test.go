package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Responses with settings (webhook token), tasks, and backups must not
// linger in the browser cache.
func TestAPIResponsesAreNotStored(t *testing.T) {
	s := testServer(t)
	for name, h := range map[string]http.HandlerFunc{
		"settings": s.handleGetSettings,
		"tasks":    s.handleListTasks,
		"export":   s.handleExport,
	} {
		rec := call(t, h, http.MethodGet, "/", nil)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q, erwartet no-store", name, got)
		}
	}
}

// The webhook client used to follow redirects: a redirect to discord.com
// (open redirect) could have sent the POST to an arbitrary target.
func TestPostDiscordDoesNotFollowRedirects(t *testing.T) {
	var hit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	if err := postDiscord(redirect.URL+"/api/webhooks/1/x", "hallo"); err == nil {
		t.Fatal("Weiterleitung als Erfolg gewertet")
	}
	if hit.Load() {
		t.Fatal("Webhook-Client ist der Weiterleitung gefolgt")
	}
}

// Session tokens used to be stored in plaintext in the DB — anyone with a
// DB backup could have logged in with any still-valid session.
func TestSessionTokensAreStoredHashed(t *testing.T) {
	s := sessionServer(t)
	rec := httptest.NewRecorder()
	if err := s.startSession(rec, 1, testUID); err != nil {
		t.Fatal(err)
	}
	token := rec.Result().Cookies()[0].Value
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token = ?", token).Scan(&n)
	if n != 0 {
		t.Fatal("Token steht im Klartext in der DB")
	}
	if !sessionValid(s.db, token) {
		t.Fatal("Sitzung mit gehashtem Token nicht mehr gültig")
	}
}

// Existing sessions survive the switch (no forced logout) and are no
// longer stored in plaintext afterward.
func TestSessionHashMigrationKeepsSessions(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"DROP TABLE sessions",
		"CREATE TABLE sessions (token TEXT PRIMARY KEY, expires_at TEXT NOT NULL, credential_id INTEGER, auth_at TEXT)",
		"INSERT INTO sessions (token, expires_at) VALUES ('klartext', '2099-01-01T00:00:00Z')",
		"INSERT INTO credentials (name, created_at, data) VALUES ('Handy', 'z', '{}')",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil { // twice: must not hash twice
		t.Fatal(err)
	}
	if !sessionValid(db, "klartext") {
		t.Fatal("Bestands-Sitzung nach der Umstellung ungültig")
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token = 'klartext'").Scan(&n)
	if n != 0 {
		t.Fatal("Bestands-Token steht weiter im Klartext in der DB")
	}
}

func TestSecurityHeadersIsolateTheApp(t *testing.T) {
	rec := httptest.NewRecorder()
	secHeaders(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	for h, want := range map[string]string{
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := rec.Header().Get(h); got != want {
			t.Errorf("%s: %q, erwartet %q", h, got, want)
		}
	}
	if pp := rec.Header().Get("Permissions-Policy"); !strings.Contains(pp, "geolocation=()") {
		t.Errorf("Permissions-Policy: %q", pp)
	}
}

func TestIconsDirectoryIsNotListed(t *testing.T) {
	h := iconHandler()
	for path, want := range map[string]int{"/icons/": http.StatusNotFound, "/icons/icon-192.png": http.StatusOK} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, erwartet %d", path, rec.Code, want)
		}
	}
}

// Links and stats used to be read in without an upper bound — with the
// import's 2 GB limit, that was enough to blow up memory.
func TestImportRejectsTooManyLinks(t *testing.T) {
	s := testServer(t)
	var b strings.Builder
	b.WriteString(`{"tasks":[],"links":[`)
	for i := range maxImportLinks + 1 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("[1,2]")
	}
	b.WriteString("]}")
	rec := call(t, s.handleImport, http.MethodPost, "/", strings.NewReader(b.String()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d Links: %d %s, erwartet 400", maxImportLinks+1, rec.Code, rec.Body)
	}
}

func TestImportRejectsTooManyStats(t *testing.T) {
	s := testServer(t)
	var b strings.Builder
	b.WriteString(`{"tasks":[],"stats":{`)
	day := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range maxImportStats + 1 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:1", day.AddDate(0, 0, i).Format(dateFmt))
	}
	b.WriteString("}}")
	rec := call(t, s.handleImport, http.MethodPost, "/", strings.NewReader(b.String()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d Statistik-Tage: %d %s, erwartet 400", maxImportStats+1, rec.Code, rec.Body)
	}
}

// The upper bound for tasks applies already while reading, not only
// afterward.
func TestImportRejectsTooManyTasks(t *testing.T) {
	s := testServer(t)
	var b strings.Builder
	b.WriteString(`{"tasks":[`)
	for i := range maxImportTasks + 1 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"title":"t","created_at":"z"}`, i+1)
	}
	b.WriteString("]}")
	rec := call(t, s.handleImport, http.MethodPost, "/", strings.NewReader(b.String()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d Tasks: %d %s, erwartet 400", maxImportTasks+1, rec.Code, rec.Body)
	}
}

// Cascading deletes, hasSubs, and the COUNT in the attachments_limit
// trigger scanned the whole table without an index.
func TestLookupIndexesExist(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, col := range map[string]string{"tasks": "parent_id", "links": "b", "attachments": "task_id"} {
		var n int
		err := db.QueryRow(`SELECT COUNT(*) FROM pragma_index_list(?) il
			JOIN pragma_index_info(il.name) ii WHERE ii.seqno = 0 AND ii.name = ?`, table, col).Scan(&n)
		if err != nil || n == 0 {
			t.Errorf("kein Index mit %s.%s vorn (err=%v)", table, col, err)
		}
	}
}

// Without a prefix, any subdomain of the same domain could set a "session"
// cookie for the whole domain (XSS or an uploaded HTML file is enough).
// With the more specific path it would shadow the real one, and the app
// only read the first one: 401, and again after every re-login. The
// browser only accepts __Host- from the host itself — Secure, Path=/, no
// Domain.
func TestCookiesHaveHostPrefix(t *testing.T) {
	s, b := e2eSetup(t)
	s.cfg.insecureCookie = false
	key := newSoftKey(t)
	key.store(t, s, true)
	if rec := b.login(t, key, flagUP|flagUV); rec.Code != http.StatusNoContent {
		t.Fatalf("Anmeldung: %d %s", rec.Code, rec.Body)
	}
	b.do(http.MethodPost, "/api/auth/login/begin", nil)
	for _, name := range []string{"__Host-session", "__Host-ceremony"} {
		c := b.cookies[name]
		if c == nil {
			t.Errorf("kein Cookie %s", name)
			continue
		}
		if !c.Secure || c.Path != "/" || c.Domain != "" {
			t.Errorf("%s: Secure=%v Path=%q Domain=%q — der Browser verwirft es", name, c.Secure, c.Path, c.Domain)
		}
	}
	if b.cookies[sessionCookie] != nil || b.cookies[ceremonyCookie] != nil {
		t.Error("Cookie ohne Präfix gesetzt")
	}
}

func TestPlantedDomainCookieDoesNotLockOut(t *testing.T) {
	s := testServer(t)
	s.cfg.insecureCookie = false
	if err := createSession(s.db, "echt", time.Now().Add(time.Hour), 0, testUID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Cookie", "session=muell; __Host-session=echt")
	rec := httptest.NewRecorder()
	s.requireAuth(http.HandlerFunc(s.handleListTasks)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("untergeschobenes Cookie sperrt aus: %d", rec.Code)
	}
}
