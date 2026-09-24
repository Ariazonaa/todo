package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestPriorityCreateUpdateValidate(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Steuer","priority":3}`)
	if task.Priority != 3 {
		t.Fatalf("angelegt mit %d", task.Priority)
	}
	// Without the field: 0 — old clients, subtasks via "Add"
	if plain := mustCreate(t, s, `{"title":"ohne"}`); plain.Priority != 0 {
		t.Fatalf("ohne Feld %d", plain.Priority)
	}
	rec := call(t, s.handleUpdateTask, http.MethodPut, "/", strings.NewReader(`{"title":"Steuer","priority":1}`), "id", fmt.Sprint(task.ID))
	var upd Task
	json.Unmarshal(rec.Body.Bytes(), &upd)
	if rec.Code != http.StatusOK || upd.Priority != 1 {
		t.Fatalf("ändern: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []int{-1, 4} {
		code, msg := errorOf(t, s.handleCreateTask, "en", "POST", "/", fmt.Sprintf(`{"title":"x","priority":%d}`, bad))
		if code != http.StatusBadRequest || msg != "invalid priority" {
			t.Errorf("priority %d: %d %q", bad, code, msg)
		}
	}
}

// Checking off a recurring task reschedules it — the priority stays.
func TestPrioritySurvivesRecurrence(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Müll","due_date":"2026-03-01","recurrence":"weekly","priority":2}`)
	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(task.ID))
	got, err := getTask(s.db, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DueDate == nil || *got.DueDate == "2026-03-01" || got.Priority != 2 {
		t.Fatalf("nach Abhaken: %+v", got)
	}
}

func TestPriorityMigration(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Recreate the state before the column existed
	if _, err := db.Exec("ALTER TABLE tasks DROP COLUMN priority"); err != nil {
		t.Fatal(err)
	}
	db.Exec("INSERT INTO tasks (title, created_at) VALUES ('alt', 'z')")
	for range 2 { // second run changes nothing
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var p int
	if err := db.QueryRow("SELECT priority FROM tasks").Scan(&p); err != nil || p != 0 {
		t.Fatalf("Bestand: %d %v", p, err)
	}
	if _, err := db.Exec("UPDATE tasks SET priority = 9"); err == nil {
		t.Fatal("CHECK fehlt: 9 angenommen")
	}
}

func TestPriorityExportImport(t *testing.T) {
	s := testServer(t)
	mustCreate(t, s, `{"title":"hoch","priority":3}`)
	var data exportData
	json.Unmarshal(call(t, s.handleExport, http.MethodGet, "/", nil).Body.Bytes(), &data)
	if len(data.Tasks) != 1 || data.Tasks[0].Priority != 3 {
		t.Fatalf("Export: %+v", data.Tasks)
	}
	// Old backups don't know the field (JSON without "priority")
	raw := `{"tasks":[{"id":1,"title":"alt","created_at":"z"}],"settings":{"tz":"Europe/Berlin","digest_time":"09:00"}}`
	rec := call(t, s.handleImport, http.MethodPost, "/", strings.NewReader(raw))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Import alt: %d %s", rec.Code, rec.Body)
	}
	if got, _ := getTask(s.db, 1); got.Priority != 0 {
		t.Fatalf("alt importiert mit %d", got.Priority)
	}
	bad := strings.Replace(raw, `"created_at":"z"`, `"created_at":"z","priority":7`, 1)
	code, msg := errorOf(t, s.handleImport, "en", "POST", "/", bad)
	if code != http.StatusBadRequest || msg != "invalid backup file (priority)" {
		t.Fatalf("Import 7: %d %q", code, msg)
	}
}

// High-priority tasks carry a "‼️ " before the title in Discord and push,
// all others don't.
func TestNotificationsMarkHighPriority(t *testing.T) {
	sc, st := pushFixture(t)
	stub, hook := newDiscordStub(t)
	hi := insertDue(t, sc.db, "Zahnarzt", "2026-03-01", "08:00")
	mid := insertDue(t, sc.db, "Friseur", "2026-03-01", "08:30")
	sc.db.Exec("UPDATE tasks SET priority = 3 WHERE id = ?", hi)
	sc.db.Exec("UPDATE tasks SET priority = 2 WHERE id = ?", mid)
	sc.db.Exec("INSERT INTO tasks (title, due_date, created_at, priority) VALUES ('Steuer', '2026-03-01', 'z', 3)")

	sc.exactReminders(testUID, hook, "2026-03-01", "09:00")
	sc.morningDigest(testUID, hook, "00:00", "2026-03-01", "09:00")
	msgs := stub.contents()
	if len(msgs) != 2 {
		t.Fatalf("Discord: %q", msgs)
	}
	for _, want := range []string{"⏰ **‼️ Zahnarzt** — fällig 08:00", "⏰ **Friseur** — fällig 08:30"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("Ping ohne %q:\n%s", want, msgs[0])
		}
	}
	if !strings.Contains(msgs[1], "• ‼️ Steuer") {
		t.Errorf("Digest:\n%s", msgs[1])
	}

	sc.pushReminders(testUID, "2026-03-01", "09:00")
	sc.pushDigest(testUID, "09:00", "2026-03-01", "09:00")
	got := st.received()
	if len(got) != 3 || got[0].Title != "‼️ Zahnarzt" || got[1].Title != "Friseur" || !strings.Contains(got[2].Body, "‼️ Steuer") {
		t.Fatalf("Push: %+v", got)
	}
}

// A client without a priority field (a tab still running the old app.js
// from before the deploy) must not silently reset the level to 0 on save.
func TestUpdateWithoutPriorityKeepsIt(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Steuer","priority":3}`)
	rec := call(t, s.handleUpdateTask, http.MethodPut, "/", strings.NewReader(`{"title":"Steuer","note":"nur Notiz"}`), "id", fmt.Sprint(task.ID))
	var upd Task
	json.Unmarshal(rec.Body.Bytes(), &upd)
	if rec.Code != http.StatusOK || upd.Priority != 3 || upd.Note != "nur Notiz" {
		t.Fatalf("%d %+v", rec.Code, upd)
	}
}
