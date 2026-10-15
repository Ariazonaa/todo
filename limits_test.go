package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Links are free and every task list request reads them: there's an upper
// bound per task.
func TestLinkLimitPerTask(t *testing.T) {
	s := testServer(t)
	hub := mustCreate(t, s, `{"title":"Knoten"}`)
	tx, _ := s.db.Begin()
	for i := range maxLinksPerTask {
		res, _ := tx.Exec("INSERT INTO tasks (title, created_at, list_id) SELECT ?, 'z', list_id FROM tasks WHERE id = ?", fmt.Sprint(i), hub.ID)
		id, _ := res.LastInsertId()
		a, b := normalizePair(hub.ID, id)
		tx.Exec("INSERT INTO links (a, b) VALUES (?, ?)", a, b)
	}
	tx.Commit()
	extra := mustCreate(t, s, `{"title":"eine zu viel"}`)
	if code, msg := errorOf(t, s.handleAddLink, "en", "POST", "/", fmt.Sprintf(`{"other":%d}`, extra.ID), "id", fmt.Sprint(hub.ID)); code != http.StatusConflict || msg != "at most 100 links per task" {
		t.Fatalf("über dem Limit: %d %q", code, msg)
	}
	// An existing link can be set again (idempotent), others are unaffected
	var first int64
	s.db.QueryRow("SELECT CASE WHEN a = ? THEN b ELSE a END FROM links LIMIT 1", hub.ID).Scan(&first)
	if err := linkTasks(s.db, testUID, hub.ID, first); err != nil {
		t.Fatalf("bestehende Verknüpfung: %v", err)
	}
	other := mustCreate(t, s, `{"title":"andere"}`)
	if err := linkTasks(s.db, testUID, extra.ID, other.ID); err != nil {
		t.Fatalf("andere Aufgaben: %v", err)
	}
}

// The task list only reads the links and images of one's own tasks — and
// never shows someone else's.
func TestTaskListOnlyReadsVisibleLinksAndImages(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	a1, a2 := mustCreate(t, s, `{"title":"a1"}`), mustCreate(t, s, `{"title":"a2"}`)
	linkTasks(s.db, testUID, a1.ID, a2.ID)
	s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/png', 'z', x'00')", a1.ID)
	b1 := mustCreateAs(t, s, bea, `{"title":"b1"}`)
	var got struct{ Tasks []Task }
	json.Unmarshal(callAs(t, bea, s.handleListTasks, "GET", "/", nil).Body.Bytes(), &got)
	if len(got.Tasks) != 1 || got.Tasks[0].ID != b1.ID || len(got.Tasks[0].Links) != 0 || len(got.Tasks[0].Attachments) != 0 {
		t.Fatalf("Bea: %+v", got.Tasks)
	}
	json.Unmarshal(call(t, s.handleListTasks, "GET", "/", nil).Body.Bytes(), &got)
	for _, x := range got.Tasks {
		if x.ID == a1.ID && (len(x.Links) != 1 || len(x.Attachments) != 1) {
			t.Fatalf("Admin: %+v", x)
		}
	}
}

// Uploads are read into memory whole: one person can't have more than two
// running at a time.
func TestUploadsPerPersonAreCapped(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Bild"}`)
	for range 2 {
		if !userUploads.acquire(testUID) {
			t.Fatal("Upload-Slot belegt")
		}
		defer userUploads.release(testUID)
	}
	rec := call(t, s.handleAttachmentUpload, "POST", "/", bytes.NewReader(pngBytes(t)), "id", fmt.Sprint(task.ID))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("dritter Upload: %d, erwartet 429", rec.Code)
	}
	// Someone else isn't affected
	bea := mustUser(t, s, "Bea")
	bt := mustCreateAs(t, s, bea, `{"title":"Beas Bild"}`)
	if rec := callAs(t, bea, s.handleAttachmentUpload, "POST", "/", bytes.NewReader(pngBytes(t)), "id", fmt.Sprint(bt.ID)); rec.Code != http.StatusCreated {
		t.Fatalf("Beas Upload: %d %s", rec.Code, rec.Body)
	}
}

// Every SSE stream checks the session on each heartbeat: one person can't
// open them by the thousand.
func TestEventStreamsPerPersonAreCapped(t *testing.T) {
	s := testServer(t)
	for range 16 {
		if !sseStreams.acquire(testUID) {
			t.Fatal("Stream-Slot belegt")
		}
		defer sseStreams.release(testUID)
	}
	rec := call(t, s.handleEvents, "GET", "/", nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("17. Stream: %d, erwartet 429", rec.Code)
	}
}

// Timestamps from a backup are compared as strings (auto-archive, order of
// the "done" section): broken ones get repaired, not stored.
func TestImportRepairsBrokenTimestamps(t *testing.T) {
	s := testServer(t)
	if rec := postImport(t, s, exportData{Tasks: []exportTask{
		{ID: 1, Title: "sofort archiviert", CreatedAt: "z", Done: true, CompletedAt: ptr("0")},
		{ID: 2, Title: "nie archiviert", CreatedAt: "2024-01-01T00:00:00Z", Done: true},
		{ID: 3, Title: "offen", CreatedAt: "2024-01-01T00:00:00Z", CompletedAt: ptr("2024-01-02T00:00:00Z")},
	}}); rec.Code != http.StatusNoContent {
		t.Fatalf("Import: %d %s", rec.Code, rec.Body)
	}
	rows, _ := s.db.Query("SELECT title, created_at, completed_at FROM tasks ORDER BY title")
	defer rows.Close()
	for rows.Next() {
		var title, created string
		var completed *string
		rows.Scan(&title, &created, &completed)
		if !strings.HasPrefix(created, "20") {
			t.Errorf("%s: created_at %q", title, created)
		}
		switch title {
		case "offen":
			if completed != nil {
				t.Errorf("offene Aufgabe mit completed_at %q", *completed)
			}
		default:
			if completed == nil || !strings.HasPrefix(*completed, "20") {
				t.Errorf("%s: completed_at %v", title, completed)
			}
		}
	}
}

// Names identify things to other people: invisible characters must not make
// "Admin" out of something else.
func TestNamesLoseInvisibleCharacters(t *testing.T) {
	s := testServer(t)
	rec := call(t, s.handleCreateUser, "POST", "/", strings.NewReader(`{"name":"Ad\u200bmin"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("„Ad\\u200bmin“ neben „Admin“: %d %s, erwartet 400 (gibt es schon)", rec.Code, rec.Body)
	}
	if got := cleanName("\u202enimda\u200f"); got != "nimda" {
		t.Fatalf("cleanName: %q", got)
	}
	l := mustList(t, s, "Ein\u2066kauf\u2069")
	if l.Name == nil || *l.Name != "Einkauf" {
		t.Fatalf("Listenname %v", l.Name)
	}
	task := mustCreate(t, s, `{"title":"Rech\u202enung\u0007","note":"Zeile 1\nZeile\u202e 2\u2028Zeile 3\t."}`)
	if task.Title != "Rechnung" || task.Note != "Zeile 1\nZeile 2\nZeile 3\t." {
		t.Fatalf("Titel %q, Notiz %q", task.Title, task.Note)
	}
	// Emoji sequences keep their joiner
	if got := cleanTitle("👩\u200d💻 Code"); got != "👩\u200d💻 Code" {
		t.Fatalf("ZWJ entfernt: %q", got)
	}
	// Invisible operators and blank fillers look like nothing either
	if got := cleanName("Ad⁡minㅤ͏"); got != "Admin" {
		t.Fatalf("cleanName: %q", got)
	}
	// Flag sequences (tag characters) stay
	if flag := "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F"; cleanTitle(flag) != flag {
		t.Fatalf("Flagge zerstört: %q", cleanTitle(flag))
	}
}
