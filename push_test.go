package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// An existing DB from before web push: push adopts Discord's state.
// Otherwise the first device that enables push would get notified again for
// every task that was ever due.
func TestMigrationCopiesDiscordStateToPush(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{schema, attachmentsTable, importAttachmentsTable, attachmentsTrigger} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	// Existing instance with a passkey (→ admin, who owns the tasks)
	if _, err := db.Exec("INSERT INTO credentials (name, created_at, data) VALUES ('Handy', 'z', '{}')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO tasks (title, created_at, due_date, due_time, reminded_at) VALUES ('alt', 'z', '2026-01-01', '08:00', '2026-01-01T08:00:00Z'), ('neu', 'z', '2026-01-01', '08:00', NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := setMeta(db, "last_digest_date", "2026-09-22"); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	// Discord state → push state → owner's stamp (task_reminders)
	var altID, neuID int64
	db.QueryRow("SELECT id FROM tasks WHERE title = 'alt'").Scan(&altID)
	db.QueryRow("SELECT id FROM tasks WHERE title = 'neu'").Scan(&neuID)
	alt, neu := column(t, db, "push_reminded_at", altID), column(t, db, "push_reminded_at", neuID)
	if alt.String != "2026-01-01T08:00:00Z" || neu.Valid {
		t.Fatalf("push_reminded_at: alt=%v neu=%v", alt, neu)
	}
	if got := getMeta(db, "last_push_digest_date"); got != "2026-09-22" {
		t.Fatalf("last_push_digest_date = %q", got)
	}
	// Idempotent: running it a second time changes nothing
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
}

// Reload and self-healing re-subscribe the same device: one row, the keys
// and the passkey come from last time.
func TestSavePushSubUpserts(t *testing.T) {
	db := testServer(t).db
	ep := "https://fcm.googleapis.com/fcm/send/abc"
	if err := savePushSub(db, ep, []byte{1}, []byte{2}, "https://a.example", 1, testUID); err != nil {
		t.Fatal(err)
	}
	if err := savePushSub(db, ep, []byte{3}, []byte{4}, "https://b.example", 2, testUID); err != nil {
		t.Fatal(err)
	}
	if n, _ := countPushSubs(db, testUID); n != 1 {
		t.Fatalf("%d Zeilen, erwartet 1", n)
	}
	sub, err := getPushSub(db, testUID, ep)
	if err != nil {
		t.Fatal(err)
	}
	if sub.P256dh[0] != 3 || sub.Auth[0] != 4 || sub.Origin != "https://b.example" {
		t.Fatalf("nicht aktualisiert: %+v", sub)
	}
	var cred int64
	db.QueryRow("SELECT credential_id FROM push_subscriptions").Scan(&cred)
	if cred != 2 {
		t.Fatalf("credential_id = %d", cred)
	}
}

func TestDeletePushSubsOfCredential(t *testing.T) {
	db := testServer(t).db
	savePushSub(db, "https://fcm.googleapis.com/a", []byte{1}, []byte{1}, "o", 1, testUID)
	savePushSub(db, "https://fcm.googleapis.com/b", []byte{1}, []byte{1}, "o", 2, testUID)
	savePushSub(db, "https://fcm.googleapis.com/c", []byte{1}, []byte{1}, "o", 0, testUID) // without a passkey (old session)
	if err := deletePushSubsOfCredential(db, testUID, 1); err != nil {
		t.Fatal(err)
	}
	subs, _ := listPushSubs(db, testUID)
	if len(subs) != 1 || subs[0].Endpoint != "https://fcm.googleapis.com/b" {
		t.Fatalf("übrig: %+v", subs)
	}
}

func TestSessionCredential(t *testing.T) {
	db := testServer(t).db
	mustCreateSession(t, db, "mit", time.Now().Add(time.Hour), 7, testUID)
	mustCreateSession(t, db, "ohne", time.Now().Add(time.Hour), 0, testUID)
	if got := sessionCredential(db, "mit"); got != 7 {
		t.Fatalf("mit: %d", got)
	}
	if got := sessionCredential(db, "ohne"); got != 0 {
		t.Fatalf("ohne: %d", got)
	}
	if got := sessionCredential(db, "gibtsnicht"); got != 0 {
		t.Fatalf("unbekannt: %d", got)
	}
}

// stampBoth: marks every task with a time as notified for testUID, across
// both channels.
func stampBoth(t *testing.T, s *server) {
	t.Helper()
	for _, ch := range []string{chDiscord, chPush} {
		if _, err := s.db.Exec("INSERT OR REPLACE INTO task_reminders (task_id, user_id, channel, due, sent_at) "+
			"SELECT id, ?, ?, due_date || ' ' || due_time, 'x' FROM tasks WHERE due_date IS NOT NULL AND due_time IS NOT NULL", testUID, ch); err != nil {
			t.Fatal(err)
		}
	}
}

func pushReminded(t *testing.T, s *server, id int64) bool {
	t.Helper()
	return column(t, s.db, "push_reminded_at", id).Valid
}

// Everywhere the Discord reminder gets re-armed, so does the push one —
// otherwise the new date would never get a push again.
func TestPushReminderResetsLikeDiscord(t *testing.T) {
	t.Run("Fälligkeit geändert", func(t *testing.T) {
		s := testServer(t)
		task := mustCreate(t, s, `{"title":"A","due_date":"2026-05-01","due_time":"08:00"}`)
		stampBoth(t, s)
		rec := call(t, s.handleUpdateTask, http.MethodPut, "/", strings.NewReader(`{"title":"A","due_date":"2026-05-02","due_time":"08:00"}`), "id", fmt.Sprint(task.ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("update: %d %s", rec.Code, rec.Body)
		}
		if pushReminded(t, s, task.ID) {
			t.Fatal("push_reminded_at nicht zurückgesetzt")
		}
	})
	t.Run("Snooze", func(t *testing.T) {
		s := testServer(t)
		task := mustCreate(t, s, `{"title":"A","due_date":"2026-05-01","due_time":"08:00"}`)
		stampBoth(t, s)
		rec := call(t, s.handleSnoozeTask, http.MethodPost, "/", strings.NewReader(`{"days":1}`), "id", fmt.Sprint(task.ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("snooze: %d %s", rec.Code, rec.Body)
		}
		if pushReminded(t, s, task.ID) {
			t.Fatal("push_reminded_at nicht zurückgesetzt")
		}
	})
	t.Run("Wiederholung samt Unteraufgaben", func(t *testing.T) {
		s := testServer(t)
		p := mustCreate(t, s, `{"title":"R","due_date":"2026-05-01","due_time":"08:00","recurrence":"daily"}`)
		sub := mustCreate(t, s, fmt.Sprintf(`{"title":"S","parent_id":%d,"due_date":"2026-05-01","due_time":"08:00"}`, p.ID))
		stampBoth(t, s)
		rec := call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(p.ID))
		if rec.Code != http.StatusOK {
			t.Fatalf("complete: %d %s", rec.Code, rec.Body)
		}
		if pushReminded(t, s, p.ID) || pushReminded(t, s, sub.ID) {
			t.Fatal("push_reminded_at nicht zurückgesetzt (Eltern oder Unteraufgabe)")
		}
	})
}

func TestExportImportKeepsPushState(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"A","due_date":"2026-05-01","due_time":"08:00"}`)
	s.db.Exec("INSERT INTO task_reminders (task_id, user_id, channel, due, sent_at) VALUES (?, ?, 'discord', '2026-05-01 08:00', 'd'), (?, ?, 'push', '2026-05-01 08:00', 'p')",
		task.ID, testUID, task.ID, testUID)
	rec := call(t, s.handleExport, http.MethodGet, "/", nil)
	var data exportData
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if got := data.Tasks[0].PushRemindedAt; got == nil || *got != "p" {
		t.Fatalf("Export push_reminded_at = %v", got)
	}
	// Backup from before web push: field missing → adopt Discord's state
	rec = postImport(t, s, exportData{Tasks: []exportTask{
		{ID: 1, Title: "alt", CreatedAt: "z", DueDate: ptr("2026-05-01"), DueTime: ptr("08:00"), RemindedAt: ptr("d")},
	}})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Import: %d %s", rec.Code, rec.Body)
	}
	var id int64
	s.db.QueryRow("SELECT id FROM tasks WHERE title = 'alt'").Scan(&id)
	if v := column(t, s.db, "push_reminded_at", id); v.String != "d" {
		t.Fatalf("Import ohne Feld: push_reminded_at = %v", v)
	}
}

func TestPushAllRemovesGoneDevices(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	st.register(t, s.db, "/weg", 0)
	st.register(t, s.db, "/da", 0)
	st.setStatus("/weg", http.StatusGone)
	ok, err := pushAll(s.db, s.vapid, testUID, pushMsg{Title: "x"}, time.Minute, "normal")
	if !ok || err != nil {
		t.Fatalf("pushAll: %v %v", ok, err)
	}
	subs, _ := listPushSubs(s.db, testUID)
	if len(subs) != 1 || subs[0].Endpoint != st.endpoint("/da") {
		t.Fatalf("übrig: %+v", subs)
	}
}

// 404 is, like 410, a "the service no longer knows this device" (webpush.go).
func TestPushAllRemoves404Device(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	st.register(t, s.db, "/weg", 0)
	st.register(t, s.db, "/da", 0)
	st.setStatus("/weg", http.StatusNotFound)
	ok, err := pushAll(s.db, s.vapid, testUID, pushMsg{Title: "x"}, time.Minute, "normal")
	if !ok || err != nil {
		t.Fatalf("pushAll: %v %v", ok, err)
	}
	subs, _ := listPushSubs(s.db, testUID)
	if len(subs) != 1 || subs[0].Endpoint != st.endpoint("/da") {
		t.Fatalf("übrig: %+v", subs)
	}
}

func TestPushAllNotDeliveredOn5xx(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	st.register(t, s.db, "/dev", 0)
	st.setStatus("/dev", http.StatusServiceUnavailable)
	ok, err := pushAll(s.db, s.vapid, testUID, pushMsg{Title: "x"}, time.Minute, "normal")
	if ok || err == nil {
		t.Fatalf("pushAll: %v %v", ok, err)
	}
	if n, _ := countPushSubs(s.db, testUID); n != 1 {
		t.Fatal("Gerät bei 5xx gelöscht — das ist nur vorübergehend")
	}
}

// pushReq is a request from the "tok" session with the app's origin.
func pushReq(s *server, method, body string) *http.Request {
	req := withUser(httptest.NewRequest(method, "/api/push/subscription", strings.NewReader(body)), testUID)
	req.Header.Set("Origin", "https://todo.example")
	req.AddCookie(&http.Cookie{Name: s.cookieName(sessionCookie), Value: "tok"})
	return req
}

func subBody(st *pushStub, path string) string {
	return fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`,
		st.endpoint(path), b64.EncodeToString(st.ua.PublicKey().Bytes()), b64.EncodeToString(st.secret))
}

func TestPushSubscribeStoresDevice(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	mustCreateSession(t, s.db, "tok", time.Now().Add(time.Hour), 5, testUID)
	for range 2 { // twice: reload/self-healing, still just one row
		rec := httptest.NewRecorder()
		s.handlePushSubscribe(rec, pushReq(s, http.MethodPut, subBody(st, "/dev")))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("subscribe: %d %s", rec.Code, rec.Body)
		}
	}
	if n, _ := countPushSubs(s.db, testUID); n != 1 {
		t.Fatalf("%d Geräte", n)
	}
	var origin string
	var cred int64
	s.db.QueryRow("SELECT origin, credential_id FROM push_subscriptions").Scan(&origin, &cred)
	if origin != "https://todo.example" || cred != 5 {
		t.Fatalf("origin=%q credential=%d", origin, cred)
	}
}

func TestPushSubscribeRejectsBadInput(t *testing.T) {
	s := testServer(t) // without pushStub: the real service allowlist applies
	mustCreateSession(t, s.db, "tok", time.Now().Add(time.Hour), 0, testUID)
	good, _ := ecdh.P256().GenerateKey(rand.Reader)
	p := b64.EncodeToString(good.PublicKey().Bytes())
	a := b64.EncodeToString(make([]byte, 16))
	fcm := "https://fcm.googleapis.com/fcm/send/x"
	for name, body := range map[string]string{
		"fremder Dienst":   fmt.Sprintf(`{"endpoint":"https://evil.example/x","keys":{"p256dh":%q,"auth":%q}}`, p, a),
		"http":             fmt.Sprintf(`{"endpoint":"http://fcm.googleapis.com/x","keys":{"p256dh":%q,"auth":%q}}`, p, a),
		"p256dh zu kurz":   fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`, fcm, b64.EncodeToString(make([]byte, 64)), a),
		"kein Kurvenpunkt": fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`, fcm, b64.EncodeToString(append([]byte{4}, make([]byte, 64)...)), a),
		"auth 15 Byte":     fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`, fcm, p, b64.EncodeToString(make([]byte, 15))),
		"kein base64url":   fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":"%%%%","auth":%q}}`, fcm, a),
		"kein JSON":        `nope`,
	} {
		rec := httptest.NewRecorder()
		s.handlePushSubscribe(rec, pushReq(s, http.MethodPut, body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, erwartet 400", name, rec.Code)
		}
	}
	if n, _ := countPushSubs(s.db, testUID); n != 0 {
		t.Fatalf("%d Geräte gespeichert", n)
	}
}

// Design note: a rejected service reports 400 with just the host, so the
// service allowlist can be extended — never the path, which is the device's
// secret.
func TestPushSubscribeRejectionMentionsHostNotPath(t *testing.T) {
	s := testServer(t)
	mustCreateSession(t, s.db, "tok", time.Now().Add(time.Hour), 0, testUID)
	good, _ := ecdh.P256().GenerateKey(rand.Reader)
	p := b64.EncodeToString(good.PublicKey().Bytes())
	a := b64.EncodeToString(make([]byte, 16))
	body := fmt.Sprintf(`{"endpoint":"https://evil.example/geheimer-pfad","keys":{"p256dh":%q,"auth":%q}}`, p, a)
	rec := httptest.NewRecorder()
	s.handlePushSubscribe(rec, pushReq(s, http.MethodPut, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d, erwartet 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "evil.example") {
		t.Fatalf("Host fehlt in der Meldung: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "geheimer-pfad") {
		t.Fatalf("Pfad in der Meldung gelandet: %s", rec.Body)
	}
}

func TestPushUnsubscribe(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	st.register(t, s.db, "/dev", 0)
	rec := httptest.NewRecorder()
	s.handlePushUnsubscribe(rec, pushReq(s, http.MethodDelete, fmt.Sprintf(`{"endpoint":%q}`, st.endpoint("/dev"))))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe: %d", rec.Code)
	}
	if n, _ := countPushSubs(s.db, testUID); n != 0 {
		t.Fatal("Gerät noch da")
	}
}

func TestPushTestGoesToThatDeviceOnly(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	st.register(t, s.db, "/a", 0)
	st.register(t, s.db, "/b", 0)
	rec := httptest.NewRecorder()
	s.handlePushTest(rec, pushReq(s, http.MethodPost, fmt.Sprintf(`{"endpoint":%q}`, st.endpoint("/a"))))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	if got := st.received(); len(got) != 1 || got[0].Tag != "test" {
		t.Fatalf("empfangen: %+v", got)
	}
	rec = httptest.NewRecorder()
	s.handlePushTest(rec, pushReq(s, http.MethodPost, `{"endpoint":"https://fcm.googleapis.com/unbekannt"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unbekanntes Gerät: %d", rec.Code)
	}
	st.setStatus("/b", http.StatusGone)
	rec = httptest.NewRecorder()
	s.handlePushTest(rec, pushReq(s, http.MethodPost, fmt.Sprintf(`{"endpoint":%q}`, st.endpoint("/b"))))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "/b") {
		t.Fatalf("abgemeldetes Gerät: %d %s", rec.Code, rec.Body)
	}
}

func TestLogoutAllRemovesPushDevices(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	st.register(t, s.db, "/dev", 0)
	rec := call(t, s.handleLogoutAll, http.MethodPost, "/", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout-all: %d", rec.Code)
	}
	if n, _ := countPushSubs(s.db, testUID); n != 0 {
		t.Fatal("Push-Geräte nach \"Überall abmelden\" noch da")
	}
}

func TestDeletingPasskeyRemovesItsPushDevices(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	handy := mustInsertCredential(t, s.db, "Handy")
	laptop := mustInsertCredential(t, s.db, "Laptop")
	mustCreateSession(t, s.db, "frisch", time.Now().Add(sessionIdleTimeout), handy, testUID)
	st.register(t, s.db, "/handy", handy)
	st.register(t, s.db, "/laptop", laptop)
	rec := httptest.NewRecorder()
	s.handlePasskeyDelete(rec, passkeyReq(s, http.MethodDelete, "frisch", fmt.Sprint(laptop)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("löschen: %d %s", rec.Code, rec.Body)
	}
	subs, _ := listPushSubs(s.db, testUID)
	if len(subs) != 1 || subs[0].Endpoint != st.endpoint("/handy") {
		t.Fatalf("übrig: %+v", subs)
	}
}

func pushFixture(t *testing.T) (*scheduler, *pushStub) {
	t.Helper()
	sc := schedulerFixture(t)
	st := newPushStub(t)
	st.register(t, sc.db, "/dev", 0)
	return sc, st
}

func insertDue(t *testing.T, db *sql.DB, title, date, tm string) int64 {
	t.Helper()
	res, err := db.Exec("INSERT INTO tasks (title, due_date, due_time, created_at) VALUES (?, ?, ?, 'z')", title, date, tm)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// column reads a task column. "reminded_at"/"push_reminded_at" are no longer
// real columns: they stand for the stamp in task_reminders for the current
// due date (channel discord/push, regardless of which person — the tests
// only have one).
func column(t *testing.T, db *sql.DB, col string, id int64) sql.NullString {
	t.Helper()
	var v sql.NullString
	channel := map[string]string{"reminded_at": chDiscord, "push_reminded_at": chPush}[col]
	if channel != "" {
		err := db.QueryRow("SELECT r.sent_at FROM task_reminders r JOIN tasks t ON t.id = r.task_id "+
			"WHERE r.task_id = ? AND r.channel = ? AND r.due = t.due_date || ' ' || t.due_time", id, channel).Scan(&v)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return v
	}
	if err := db.QueryRow("SELECT "+col+" FROM tasks WHERE id = ?", id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPushRemindersOnePerTaskWithActions(t *testing.T) {
	sc, st := pushFixture(t)
	a := insertDue(t, sc.db, "Zahnarzt", "2026-03-01", "08:00")
	b := insertDue(t, sc.db, "Steuer", "2026-02-27", "10:00")
	insertDue(t, sc.db, "später", "2026-03-01", "18:00")
	sc.pushReminders(testUID, "2026-03-01", "09:00")
	got := st.received()
	if len(got) != 2 {
		t.Fatalf("%d Benachrichtigungen, erwartet 2: %+v", len(got), got)
	}
	for _, m := range got {
		if m.TaskID == 0 || m.DueDate == "" || actionNames(m) != "done,snooze" || m.Actions[0].Title != "Erledigt" || m.Tag != fmt.Sprintf("task-%d", m.TaskID) {
			t.Errorf("Einzel-Ping unvollständig: %+v", m)
		}
	}
	if !column(t, sc.db, "push_reminded_at", a).Valid || !column(t, sc.db, "push_reminded_at", b).Valid {
		t.Fatal("nicht gestempelt")
	}
	if column(t, sc.db, "reminded_at", a).Valid {
		t.Fatal("Push hat den Discord-Stempel gesetzt — die Kanäle sind unabhängig")
	}
	sc.pushReminders(testUID, "2026-03-01", "09:01")
	if len(st.received()) != 2 {
		t.Fatal("zweiter Tick hat erneut geschickt")
	}
}

func TestPushRemindersGroupAboveThree(t *testing.T) {
	sc, st := pushFixture(t)
	var ids []int64
	for i := range 4 {
		ids = append(ids, insertDue(t, sc.db, fmt.Sprintf("Aufgabe %d", i), "2026-03-01", "08:00"))
	}
	sc.pushReminders(testUID, "2026-03-01", "09:00")
	got := st.received()
	if len(got) != 1 || got[0].Tag != "reminders" || got[0].TaskID != 0 || len(got[0].Actions) != 0 {
		t.Fatalf("erwartet eine Sammel-Benachrichtigung: %+v", got)
	}
	if !strings.Contains(got[0].Title, "4") {
		t.Fatalf("Titel nennt die Zahl nicht: %q", got[0].Title)
	}
	for _, id := range ids {
		if !column(t, sc.db, "push_reminded_at", id).Valid {
			t.Fatalf("Aufgabe %d nicht gestempelt", id)
		}
	}
}

// "Erledigt" from the notification also checks off open subtasks — in the
// app that's preceded by a confirmation prompt, which a notification can't do.
func TestPushReminderWithoutDoneForOpenSubs(t *testing.T) {
	sc, st := pushFixture(t)
	p := insertDue(t, sc.db, "Umzug", "2026-03-01", "08:00")
	sc.db.Exec("INSERT INTO tasks (title, parent_id, created_at) VALUES ('Kartons', ?, 'z')", p)
	sc.pushReminders(testUID, "2026-03-01", "09:00")
	got := st.received()
	if len(got) != 1 || actionNames(got[0]) != "snooze" {
		t.Fatalf("Aktionen: %+v", got)
	}
}

func TestPushAndDiscordAreIndependent(t *testing.T) {
	sc, st := pushFixture(t)
	id := insertDue(t, sc.db, "Zahnarzt", "2026-03-01", "08:00")
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	// Discord broken, push works
	sc.exactReminders(testUID, broken.URL+"/api/webhooks/1/x", "2026-03-01", "09:00")
	sc.pushReminders(testUID, "2026-03-01", "09:00")
	if column(t, sc.db, "reminded_at", id).Valid || !column(t, sc.db, "push_reminded_at", id).Valid {
		t.Fatal("Stempel vermischt")
	}
	// Discord recovers: catches up, push doesn't send twice
	d, url := newDiscordStub(t)
	sc.exactReminders(testUID, url, "2026-03-01", "09:01")
	sc.pushReminders(testUID, "2026-03-01", "09:01")
	if len(d.contents()) != 1 || len(st.received()) != 1 {
		t.Fatalf("Discord %d, Push %d — erwartet je 1", len(d.contents()), len(st.received()))
	}
}

// Single device returning 5xx: nothing gets stamped, then exactly one delivery later.
func TestPushRemindersRetryAfter5xx(t *testing.T) {
	sc, st := pushFixture(t)
	id := insertDue(t, sc.db, "Zahnarzt", "2026-03-01", "08:00")
	st.setStatus("/dev", http.StatusServiceUnavailable)
	sc.pushReminders(testUID, "2026-03-01", "09:00")
	if column(t, sc.db, "push_reminded_at", id).Valid {
		t.Fatal("trotz 5xx gestempelt")
	}
	st.setStatus("/dev", http.StatusCreated)
	sc.pushReminders(testUID, "2026-03-01", "09:01")
	sc.pushReminders(testUID, "2026-03-01", "09:02")
	if n := len(st.received()); n != 1 {
		t.Fatalf("%d Zustellungen, erwartet genau 1", n)
	}
}

// While the push request is in flight, another device checks off the
// recurring task (new date, stamp NULL). The stamp written afterward must
// not overwrite that reset.
func TestPushStampKeepsConcurrentReset(t *testing.T) {
	sc, st := pushFixture(t)
	id := insertDue(t, sc.db, "Tabletten", "2026-03-01", "08:00")
	st.setHook(func() {
		if _, err := sc.db.Exec("UPDATE tasks SET due_date = '2026-03-02' WHERE id = ?", id); err != nil {
			t.Errorf("reset: %v", err)
		}
	})
	sc.pushReminders(testUID, "2026-03-01", "09:00")
	if column(t, sc.db, "push_reminded_at", id).Valid {
		t.Fatal("Stempel hat den Reset fürs nächste Vorkommen überschrieben")
	}
}

// 500 characters, each inflating the JSON by up to 6 bytes.
func TestPushLongTitleIsClipped(t *testing.T) {
	sc, st := pushFixture(t)
	id := insertDue(t, sc.db, strings.Repeat("<", 500), "2026-03-01", "08:00")
	sc.pushReminders(testUID, "2026-03-01", "09:00")
	got := st.received()
	if len(got) != 1 || utf8.RuneCountInString(got[0].Title) > pushTitleRunes {
		t.Fatalf("empfangen: %d, Titel %d Zeichen", len(got), utf8.RuneCountInString(got[0].Title))
	}
	if !column(t, sc.db, "push_reminded_at", id).Valid {
		t.Fatal("nicht gestempelt")
	}
}

func TestPushDigestOncePerDay(t *testing.T) {
	sc, st := pushFixture(t)
	sc.db.Exec("INSERT INTO tasks (title, due_date, created_at) VALUES ('Müll', '2026-03-01', 'z'), ('Steuer', '2026-02-20', 'z')")
	sc.pushDigest(testUID, "09:00", "2026-03-01", "08:59")
	if len(st.received()) != 0 {
		t.Fatal("vor der Digest-Uhrzeit geschickt")
	}
	sc.pushDigest(testUID, "09:00", "2026-03-01", "09:00")
	sc.pushDigest(testUID, "09:00", "2026-03-01", "09:01")
	got := st.received()
	if len(got) != 1 || got[0].Tag != "digest" || !strings.Contains(got[0].Body, "1 heute, 1 überfällig") || !strings.Contains(got[0].Body, "Müll") {
		t.Fatalf("Digest: %+v", got)
	}
	if getUserSetting(sc.db, testUID, "last_push_digest_date") != "2026-03-01" || getUserSetting(sc.db, testUID, "last_digest_date") != "" {
		t.Fatal("Digest-Stempel vermischt")
	}
}

// Nothing due: the day's stamp still gets set (otherwise every further tick
// on the same day would reconsider it again), but no message goes out.
func TestPushDigestNothingStampsWithoutSending(t *testing.T) {
	sc, st := pushFixture(t)
	sc.pushDigest(testUID, "09:00", "2026-03-01", "09:00")
	if len(st.received()) != 0 {
		t.Fatal("ohne fällige Aufgaben trotzdem geschickt")
	}
	if getUserSetting(sc.db, testUID, "last_push_digest_date") != "2026-03-01" {
		t.Fatal("Tagesstempel fehlt trotz leerem Digest")
	}
}

// The reverse of TestPushAndDiscordAreIndependent: this time push is stuck,
// Discord goes through — here too the dedup states stay separate.
func TestPushAndDiscordAreIndependentReverse(t *testing.T) {
	sc, st := pushFixture(t)
	id := insertDue(t, sc.db, "Zahnarzt", "2026-03-01", "08:00")
	d, url := newDiscordStub(t)
	// Push broken, Discord works
	st.setStatus("/dev", http.StatusServiceUnavailable)
	sc.exactReminders(testUID, url, "2026-03-01", "09:00")
	sc.pushReminders(testUID, "2026-03-01", "09:00")
	if !column(t, sc.db, "reminded_at", id).Valid || column(t, sc.db, "push_reminded_at", id).Valid {
		t.Fatal("Stempel vermischt")
	}
	// Push recovers: catches up, Discord doesn't send twice
	st.setStatus("/dev", http.StatusCreated)
	sc.exactReminders(testUID, url, "2026-03-01", "09:01")
	sc.pushReminders(testUID, "2026-03-01", "09:01")
	if len(d.contents()) != 1 || len(st.received()) != 1 {
		t.Fatalf("Discord %d, Push %d — erwartet je 1", len(d.contents()), len(st.received()))
	}
}

// tick() itself has to wire up the push branches, not just pushReminders and
// pushDigest on their own.
func TestTickSendsAndStampsPush(t *testing.T) {
	sc, st := pushFixture(t)
	id := insertDue(t, sc.db, "Zahnarzt", "2020-01-01", "08:00")
	// Digest already done for today — otherwise tick() would also send the
	// morning digest from the digest time onward (the test only passed at
	// night).
	setUserSetting(sc.db, testUID, "last_push_digest_date", time.Now().In(sc.store.location(testUID)).Format(dateFmt))
	sc.tick(time.Now())
	if len(st.received()) != 1 {
		t.Fatalf("%d Push-Zustellungen über tick(), erwartet 1", len(st.received()))
	}
	if !column(t, sc.db, "push_reminded_at", id).Valid {
		t.Fatal("tick() hat nicht gestempelt")
	}
}

// Without a device, push doesn't run and stamps nothing (like Discord without a webhook).
func TestTickWithoutPushDevicesStampsNothing(t *testing.T) {
	sc := schedulerFixture(t)
	id := insertDue(t, sc.db, "Zahnarzt", "2020-01-01", "08:00")
	sc.tick(time.Now())
	if column(t, sc.db, "push_reminded_at", id).Valid {
		t.Fatal("ohne Gerät gestempelt")
	}
}

func actionNames(m pushMsg) string {
	var names []string
	for _, a := range m.Actions {
		names = append(names, a.Action)
	}
	return strings.Join(names, ",")
}

// After switching to English, Discord and push messages come out in English
// too — including the date and button labels. 2026-03-01 is a Sunday,
// 2026-02-27 is a Friday.
func TestNotificationsFollowLanguage(t *testing.T) {
	sc, st := pushFixture(t)
	sc.store.noteLang(testUID, "en")
	stub, hook := newDiscordStub(t)
	insertDue(t, sc.db, "Dentist", "2026-03-01", "08:00")
	sc.db.Exec("INSERT INTO tasks (title, due_date, created_at) VALUES ('Taxes', '2026-02-27', 'z')")

	sc.exactReminders(testUID, hook, "2026-03-01", "09:00")
	sc.morningDigest(testUID, hook, "00:00", "2026-03-01", "09:00")
	msgs := stub.contents()
	if len(msgs) != 2 {
		t.Fatalf("Discord: %d Nachrichten %q", len(msgs), msgs)
	}
	if msgs[0] != "⏰ **Dentist** — due 08:00" {
		t.Errorf("Ping: %q", msgs[0])
	}
	for _, want := range []string{"☀️ **Todo — Sun Mar 1**", "**Overdue:**", "• Taxes (since Fri Feb 27)"} {
		if !strings.Contains(msgs[1], want) {
			t.Errorf("Digest ohne %q:\n%s", want, msgs[1])
		}
	}

	sc.pushReminders(testUID, "2026-03-01", "09:00")
	sc.pushDigest(testUID, "09:00", "2026-03-01", "09:00")
	got := st.received()
	if len(got) != 2 {
		t.Fatalf("Push: %+v", got)
	}
	if got[0].Body != "due 08:00" || actionNames(got[0]) != "done,snooze" ||
		got[0].Actions[0].Title != "Done" || got[0].Actions[1].Title != "+1 day" {
		t.Errorf("Ping: %+v", got[0])
	}
	if got[1].Title != "☀️ Todo — Sun Mar 1" || !strings.HasPrefix(got[1].Body, "1 overdue: ") {
		t.Errorf("Digest: %+v", got[1])
	}
}

func TestPushTestFollowsRequestLanguage(t *testing.T) {
	s := testServer(t)
	st := newPushStub(t)
	st.register(t, s.db, "/dev", 0)
	req := withUser(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"endpoint":"`+st.endpoint("/dev")+`"}`)), testUID)
	req.Header.Set("X-Lang", "en")
	rec := httptest.NewRecorder()
	s.handlePushTest(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := st.received(); len(got) != 1 || got[0].Body != "🔔 Test notification" {
		t.Fatalf("%+v", got)
	}
}
