package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Existing instance: passkeys, sessions, push devices, lists, and stats
// without accounts → one admin with the old user handle, who owns everything.
func TestMigrationAdoptsExistingInstance(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Rebuild the state from before accounts existed
	for _, q := range []string{
		"DELETE FROM users",
		"INSERT INTO credentials (name, created_at, data) VALUES ('Handy', 'z', '{}')",
		"INSERT INTO sessions (token, expires_at, hashed) VALUES ('h', '2999-01-01T00:00:00Z', 1)",
		"INSERT INTO lists (name, is_default, created_at) VALUES (NULL, 1, 'z')",
		"INSERT INTO tasks (title, created_at) VALUES ('alt', 'z')",
		"INSERT INTO meta (key, value) VALUES ('set_tz', 'Europe/Vienna')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for range 2 {
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	var name string
	var admin int
	var handle []byte
	db.QueryRow("SELECT COUNT(*), MAX(name), MAX(is_admin), MAX(webauthn_id) FROM users").Scan(&n, &name, &admin, &handle)
	if n != 1 || name != "Admin" || admin != 1 || string(handle) != "todo-single-user" {
		t.Fatalf("%d Konten, %q admin=%d handle=%q", n, name, admin, handle)
	}
	var cred, sess, list sql.NullInt64
	db.QueryRow("SELECT user_id FROM credentials").Scan(&cred)
	db.QueryRow("SELECT user_id FROM sessions").Scan(&sess)
	db.QueryRow("SELECT owner_id FROM lists WHERE is_default = 1").Scan(&list)
	if cred.Int64 != 1 || sess.Int64 != 1 || list.Int64 != 1 {
		t.Fatalf("Zuordnung: cred %v sess %v list %v", cred, sess, list)
	}
	var tz string
	db.QueryRow("SELECT value FROM user_settings WHERE user_id = 1 AND key = 'tz'").Scan(&tz)
	if tz != "Europe/Vienna" {
		t.Fatalf("Einstellung nicht übernommen: %q", tz)
	}
}

// Stats from before accounts: one row per day, with no person attached.
// Afterward it belongs to the admin and survives further restarts.
func TestMigrationKeepsOldStats(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"DELETE FROM users",
		"DROP TABLE stats",
		"CREATE TABLE stats (date TEXT PRIMARY KEY, done INTEGER NOT NULL DEFAULT 0)",
		"INSERT INTO stats (date, done) VALUES ('2026-09-01', 4), ('2026-09-02', 7)",
		"INSERT INTO credentials (name, created_at, data) VALUES ('Handy', 'z', '{}')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for range 2 {
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var admin int64
	db.QueryRow("SELECT id FROM users WHERE is_admin = 1").Scan(&admin)
	var n, sum int
	db.QueryRow("SELECT COUNT(*), COALESCE(SUM(done), 0) FROM stats WHERE user_id = ?", admin).Scan(&n, &sum)
	if admin == 0 || n != 2 || sum != 11 {
		t.Fatalf("admin %d: %d Tage, %d erledigt — erwartet 2 Tage, 11", admin, n, sum)
	}
}

func TestFreshInstanceStaysEmpty(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var users, lists int
	db.QueryRow("SELECT COUNT(*) FROM users").Scan(&users)
	db.QueryRow("SELECT COUNT(*) FROM lists").Scan(&lists)
	if users != 0 || lists != 0 {
		t.Fatalf("%d Konten, %d Listen", users, lists)
	}
}

// A session without a person (legacy data) isn't a login.
func TestSessionWithoutUserIsNotAuthenticated(t *testing.T) {
	s := testServer(t)
	s.db.Exec("INSERT INTO sessions (token, expires_at, hashed) VALUES (?, '2999-01-01T00:00:00Z', 1)", hashToken("waise"))
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("durchgelassen")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName(sessionCookie), Value: "waise"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("%d", rec.Code)
	}
}

func TestRequireAuthPutsUserInContext(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	mustCreateSession(t, s.db, "tok", time.Now().Add(time.Hour), 0, bea)
	var got int64
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = userID(r) }))
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName(sessionCookie), Value: "tok"})
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != bea {
		t.Fatalf("Person %d, erwartet %d", got, bea)
	}
	if userID(httptest.NewRequest(http.MethodGet, "/", nil)) != 0 {
		t.Fatal("ohne Kontext eine Person")
	}
}

func TestSettingsPerUser(t *testing.T) {
	s := testServer(t)
	b := mustUser(t, s, "Bea")
	if err := s.store.save(testUID, appSettings{TZ: "Europe/Vienna", DigestTime: "07:00", WebhookURL: "https://discord.com/api/webhooks/1/a"}); err != nil {
		t.Fatal(err)
	}
	if got := s.store.get(b); got.TZ != "Europe/Berlin" || got.WebhookURL != "" || got.DigestTime != "09:00" {
		t.Fatalf("B erbt A's Einstellungen: %+v", got)
	}
	s.store.saveLanguage(b, "en")
	if s.store.notifyLang(testUID) != "de" || s.store.notifyLang(b) != "en" {
		t.Fatal("Sprache nicht getrennt")
	}
	// Archive retention: only the admin can set it
	callAs(t, b, s.handlePutSettings, "PUT", "/", strings.NewReader(`{"tz":"Europe/Berlin","digest_time":"09:00","archive_days":1}`))
	if s.store.archiveDays() != 30 {
		t.Fatalf("Nicht-Admin hat die Archiv-Frist gesetzt: %d", s.store.archiveDays())
	}
	call(t, s.handlePutSettings, "PUT", "/", strings.NewReader(`{"tz":"Europe/Vienna","digest_time":"07:00","archive_days":5}`))
	if s.store.archiveDays() != 5 {
		t.Fatalf("Admin: %d", s.store.archiveDays())
	}
	var me struct {
		Me struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			IsAdmin bool   `json:"is_admin"`
		} `json:"me"`
	}
	json.Unmarshal(callAs(t, b, s.handleGetSettings, "GET", "/", nil).Body.Bytes(), &me)
	if me.Me.ID != b || me.Me.Name != "Bea" || me.Me.IsAdmin {
		t.Fatalf("me: %+v", me.Me)
	}
}

func TestStatsPerUser(t *testing.T) {
	s := testServer(t)
	b := mustUser(t, s, "Bea")
	task := mustCreate(t, s, `{"title":"A"}`)
	call(t, s.handleCompleteTask, "POST", "/", nil, "id", fmt.Sprint(task.ID))
	var sa, sb map[string]int
	json.Unmarshal(call(t, s.handleStats, "GET", "/", nil).Body.Bytes(), &sa)
	json.Unmarshal(callAs(t, b, s.handleStats, "GET", "/", nil).Body.Bytes(), &sb)
	if sa["today"] != 1 || sb["today"] != 0 {
		t.Fatalf("A %+v, B %+v", sa, sb)
	}
}

// A's tasks only go to A's webhook and devices, in A's time zone.
func TestRemindersPerUser(t *testing.T) {
	s := testServer(t)
	b := mustUser(t, s, "Bea")
	stubA, hookA := newDiscordStub(t)
	stubB, hookB := newDiscordStub(t)
	s.store.save(testUID, appSettings{TZ: "Europe/Berlin", DigestTime: "09:00", WebhookURL: hookA})
	s.store.save(b, appSettings{TZ: "Europe/Berlin", DigestTime: "09:00", WebhookURL: hookB})
	mustCreate(t, s, `{"title":"A-Termin","due_date":"2026-03-01","due_time":"08:00"}`)
	mustCreateAs(t, s, b, `{"title":"B-Termin","due_date":"2026-03-01","due_time":"08:00"}`)
	push := newPushStub(t)
	push.register(t, s.db, "/b-geraet", 0)
	s.db.Exec("UPDATE push_subscriptions SET user_id = ?", b)

	sc := newScheduler(s.db, s.store, s.hub, s.vapid)
	sc.exactReminders(testUID, hookA, "2026-03-01", "09:00")
	sc.exactReminders(b, hookB, "2026-03-01", "09:00")
	sc.pushReminders(testUID, "2026-03-01", "09:00")
	sc.pushReminders(b, "2026-03-01", "09:00")
	if a := strings.Join(stubA.contents(), "|"); !strings.Contains(a, "A-Termin") || strings.Contains(a, "B-Termin") {
		t.Fatalf("A's Webhook: %q", a)
	}
	if bb := strings.Join(stubB.contents(), "|"); !strings.Contains(bb, "B-Termin") || strings.Contains(bb, "A-Termin") {
		t.Fatalf("B's Webhook: %q", bb)
	}
	got := push.received()
	if len(got) != 1 || got[0].Title != "B-Termin" {
		t.Fatalf("B's Gerät: %+v", got)
	}
}

// A device that switches owners (A logs out, B logs in) belongs to B afterward.
func TestPushDeviceFollowsLatestUser(t *testing.T) {
	s := testServer(t)
	b := mustUser(t, s, "Bea")
	if err := savePushSub(s.db, "https://fcm.googleapis.com/x", make([]byte, 65), make([]byte, 16), "https://todo", 0, testUID); err != nil {
		t.Fatal(err)
	}
	savePushSub(s.db, "https://fcm.googleapis.com/x", make([]byte, 65), make([]byte, 16), "https://todo", 0, b)
	if n, _ := countPushSubs(s.db, testUID); n != 0 {
		t.Fatalf("A hat noch %d Geräte", n)
	}
	if n, _ := countPushSubs(s.db, b); n != 1 {
		t.Fatalf("B hat %d Geräte", n)
	}
}

func TestUserAdminAPI(t *testing.T) {
	s := testServer(t)
	rec := call(t, s.handleCreateUser, "POST", "/", strings.NewReader(`{"name":"Bea"}`))
	var created struct {
		User User   `json:"user"`
		Code string `json:"code"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if rec.Code != http.StatusCreated || created.User.Name != "Bea" || len(normalizeSetupCode(created.Code)) != 16 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if code, msg := errorOf(t, s.handleCreateUser, "en", "POST", "/", `{"name":"bea"}`); code != 400 || msg != "A person with this name already exists" {
		t.Fatalf("doppelt: %d %q", code, msg)
	}
	var list []User
	json.Unmarshal(call(t, s.handleListUsers, "GET", "/", nil).Body.Bytes(), &list)
	if len(list) != 2 || !list[1].CodePending || list[1].HasPasskey {
		t.Fatalf("%+v", list)
	}
	// Non-admins don't see the admin panel
	b := created.User.ID
	for _, h := range []http.HandlerFunc{s.handleListUsers, s.handleCreateUser} {
		if rec := callAs(t, b, h, "GET", "/", strings.NewReader(`{"name":"X"}`)); rec.Code != http.StatusNotFound {
			t.Fatalf("Nicht-Admin: %d", rec.Code)
		}
	}
	// Redeeming the code finds the account; a new code invalidates the old one
	if uid, _, _, ok := userForCode(s.db, created.Code); !ok || uid != b {
		t.Fatal("Code findet das Konto nicht")
	}
	json.Unmarshal(call(t, s.handleNewUserCode, "POST", "/", nil, "id", fmt.Sprint(b)).Body.Bytes(), &created)
	if _, _, _, ok := userForCode(s.db, "falsch"); ok {
		t.Fatal("falscher Code")
	}
	// Expired
	s.db.Exec("UPDATE users SET setup_expires = '2000-01-01T00:00:00Z' WHERE id = ?", b)
	if _, _, _, ok := userForCode(s.db, created.Code); ok {
		t.Fatal("abgelaufener Code gilt")
	}
}

func TestDeleteUser(t *testing.T) {
	s := testServer(t)
	b := mustUser(t, s, "Bea")
	bt := mustCreateAs(t, s, b, `{"title":"B"}`)
	mustCreateSession(t, s.db, "b-tok", time.Now().Add(time.Hour), 0, b)
	setUserSetting(s.db, b, "tz", "Europe/Vienna")
	at := mustCreate(t, s, `{"title":"A"}`)
	// Without a fresh login: 403 reauth
	mustCreateSession(t, s.db, "alt", time.Now().Add(time.Hour), 0, testUID)
	stale := time.Now().Add(-reauthWindow - time.Minute).UTC().Format(time.RFC3339)
	s.db.Exec("UPDATE sessions SET auth_at = ? WHERE token = ?", stale, hashToken("alt"))
	req := withUser(httptest.NewRequest("DELETE", "/", nil), testUID)
	req.AddCookie(&http.Cookie{Name: s.cookieName(sessionCookie), Value: "alt"})
	req.SetPathValue("id", fmt.Sprint(b))
	rec := httptest.NewRecorder()
	s.handleDeleteUser(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ohne Frische: %d", rec.Code)
	}
	// Freshly logged in
	mustCreateSession(t, s.db, "frisch", time.Now().Add(time.Hour), 0, testUID)
	del := func(id int64) int {
		req := withUser(httptest.NewRequest("DELETE", "/", nil), testUID)
		req.AddCookie(&http.Cookie{Name: s.cookieName(sessionCookie), Value: "frisch"})
		req.SetPathValue("id", fmt.Sprint(id))
		rec := httptest.NewRecorder()
		s.handleDeleteUser(rec, req)
		return rec.Code
	}
	if code := del(testUID); code != http.StatusConflict {
		t.Fatalf("eigenes Konto: %d", code)
	}
	if code := del(b); code != http.StatusNoContent {
		t.Fatalf("löschen: %d", code)
	}
	var rest int
	s.db.QueryRow("SELECT (SELECT COUNT(*) FROM users WHERE id = ?) + (SELECT COUNT(*) FROM lists WHERE owner_id = ?) + "+
		"(SELECT COUNT(*) FROM sessions WHERE user_id = ?) + (SELECT COUNT(*) FROM user_settings WHERE user_id = ?) + "+
		"(SELECT COUNT(*) FROM tasks WHERE id = ?)", b, b, b, b, bt.ID).Scan(&rest)
	if rest != 0 {
		t.Fatalf("%d Reste von B", rest)
	}
	if _, err := getTask(s.db, at.ID); err != nil {
		t.Fatal("A's Aufgabe weg")
	}
	if s.authed(requestWithSession(s, "b-tok")) {
		t.Fatal("B's Sitzung lebt noch")
	}
}

func requestWithSession(s *server, token string) *http.Request {
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName(sessionCookie), Value: token})
	return req
}

func TestSetupMode(t *testing.T) {
	s := testServer(t)
	newSoftKey(t).store(t, s, true)
	var got struct{ First bool }
	json.Unmarshal(call(t, s.handleSetupMode, "GET", "/", nil).Body.Bytes(), &got)
	if got.First {
		t.Fatal("Instanz mit Konto gilt als erstes Setup")
	}
}

func TestBackupPerUser(t *testing.T) {
	s := testServer(t)
	b := mustUser(t, s, "Bea")
	// B has data with IDs that A's backup also uses
	bTask := mustCreateAs(t, s, b, `{"title":"B bleibt"}`)
	arbeit := mustList(t, s, "Arbeit")
	parent := mustCreate(t, s, fmt.Sprintf(`{"title":"Projekt","list_id":%d}`, arbeit.ID))
	sub := mustCreate(t, s, fmt.Sprintf(`{"title":"Teil","parent_id":%d}`, parent.ID))
	linkTasks(s.db, testUID, parent.ID, sub.ID)
	var data exportData
	json.Unmarshal(call(t, s.handleExport, "GET", "/", nil).Body.Bytes(), &data)
	for _, x := range data.Tasks {
		if x.ID == bTask.ID {
			t.Fatal("B's Aufgabe im Export von A")
		}
	}
	// B imports A's backup: B's old data gone, A's untouched, new IDs
	if rec := postImportAs(t, s, b, data); rec.Code != http.StatusNoContent {
		t.Fatalf("Import: %d %s", rec.Code, rec.Body)
	}
	if _, err := getTask(s.db, bTask.ID); err == nil {
		t.Fatal("B's alte Aufgabe blieb")
	}
	if got, _ := getTask(s.db, parent.ID); got.Title != "Projekt" || !listVisible(s.db, testUID, got.ListID) {
		t.Fatal("A's Aufgabe verändert")
	}
	var bs struct {
		Tasks []Task `json:"tasks"`
	}
	json.Unmarshal(callAs(t, b, s.handleListTasks, "GET", "/", nil).Body.Bytes(), &bs)
	byTitle := map[string]Task{}
	for _, x := range bs.Tasks {
		byTitle[x.Title] = x
		if x.ID == parent.ID || x.ID == sub.ID {
			t.Fatalf("ID %d wiederverwendet", x.ID)
		}
	}
	p, c := byTitle["Projekt"], byTitle["Teil"]
	if p.ID == 0 || c.ParentID == nil || *c.ParentID != p.ID || len(p.Links) != 1 || p.Links[0] != c.ID {
		t.Fatalf("Umzug: %+v / %+v", p, c)
	}
}
