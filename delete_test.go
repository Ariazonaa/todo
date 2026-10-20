package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
)

// Deletion used to run as separate statements without error checking. If
// one failed after the DELETE of the tasks, links and images were left
// behind as orphans, and the client still got a 204 — and because
// tasks.id gets reused, the next new task would inherit them.
func TestDeleteTaskFailureKeepsEverything(t *testing.T) {
	s := testServer(t)
	parent := mustCreate(t, s, `{"title":"Eltern"}`)
	other := mustCreate(t, s, `{"title":"andere"}`)
	s.db.Exec("INSERT INTO links (a, b) VALUES (?, ?)", parent.ID, other.ID)
	s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/png', 'z', x'00')", parent.ID)
	// The image cannot be deleted: simulates a failure in the middle of
	// the cascade.
	if _, err := s.db.Exec("CREATE TRIGGER boom BEFORE DELETE ON attachments BEGIN SELECT RAISE(ABORT, 'boom'); END"); err != nil {
		t.Fatal(err)
	}

	rec := call(t, s.handleDeleteTask, http.MethodDelete, "/", nil, "id", fmt.Sprint(parent.ID))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Löschen mit Fehler: %d %s, erwartet 500", rec.Code, rec.Body)
	}
	var tasks, links, atts int
	s.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE id = ?", parent.ID).Scan(&tasks)
	s.db.QueryRow("SELECT COUNT(*) FROM links").Scan(&links)
	s.db.QueryRow("SELECT COUNT(*) FROM attachments").Scan(&atts)
	if tasks != 1 || links != 1 || atts != 1 {
		t.Fatalf("nach gescheitertem Löschen: task=%d links=%d bilder=%d, erwartet alles 1", tasks, links, atts)
	}
}

func TestDeleteTaskRemovesSubsLinksAndAttachments(t *testing.T) {
	s := testServer(t)
	parent := mustCreate(t, s, `{"title":"Eltern"}`)
	sub := mustCreate(t, s, fmt.Sprintf(`{"title":"Sub","parent_id":%d}`, parent.ID))
	other := mustCreate(t, s, `{"title":"bleibt"}`)
	s.db.Exec("INSERT INTO links (a, b) VALUES (?, ?)", parent.ID, other.ID)
	s.db.Exec("INSERT INTO links (a, b) VALUES (?, ?)", sub.ID, other.ID)
	s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/png', 'z', x'00')", sub.ID)
	s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/png', 'z', x'00')", other.ID)

	rec := call(t, s.handleDeleteTask, http.MethodDelete, "/", nil, "id", fmt.Sprint(parent.ID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Löschen: %d %s", rec.Code, rec.Body)
	}
	var tasks, links, atts int
	s.db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&tasks)
	s.db.QueryRow("SELECT COUNT(*) FROM links").Scan(&links)
	s.db.QueryRow("SELECT COUNT(*) FROM attachments").Scan(&atts)
	if tasks != 1 || links != 0 || atts != 1 {
		t.Fatalf("nach dem Löschen: tasks=%d links=%d bilder=%d, erwartet 1/0/1", tasks, links, atts)
	}
}

// Orphans from earlier failed deletions would otherwise keep being
// inherited by the next task with the same ID. Startup cleans them up.
func TestOpenDBRemovesOrphans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todo.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("INSERT INTO tasks (id, title, created_at) VALUES (1, 'lebt', 'z')")
	db.Exec("INSERT INTO links (a, b) VALUES (1, 2)")
	db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (2, 'image/png', 'z', x'00')")
	db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (1, 'image/png', 'z', x'00')")
	db.Close()

	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var links, atts int
	db.QueryRow("SELECT COUNT(*) FROM links").Scan(&links)
	db.QueryRow("SELECT COUNT(*) FROM attachments").Scan(&atts)
	if links != 0 || atts != 1 {
		t.Fatalf("nach dem Start: links=%d bilder=%d, erwartet 0/1", links, atts)
	}
}

// tasks.id used to be reassigned after the newest task was deleted: an
// undo-delete sent late, a notification's "Done" or the auto-archive then
// hit the next new task — in a shared list, someone else's.
func TestTaskIDsAreNeverReused(t *testing.T) {
	s := testServer(t)
	first := mustCreate(t, s, `{"title":"weg"}`)
	if rec := call(t, s.handleDeleteTask, http.MethodDelete, "/", nil, "id", fmt.Sprint(first.ID)); rec.Code != http.StatusNoContent {
		t.Fatalf("löschen: %d %s", rec.Code, rec.Body)
	}
	if second := mustCreate(t, s, `{"title":"neu"}`); second.ID == first.ID {
		t.Fatalf("ID %d nach dem Löschen neu vergeben", second.ID)
	}
}

// The rebuild for AUTOINCREMENT goes through every task of an existing
// instance: every column keeps its value, the triggers, indexes and the
// priority check come back, and new IDs start above the existing ones.
func TestTasksAutoincrementMigrationKeepsEverything(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todo.db")
	old, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	// tasks as the previous version left it (schema plus every ALTER)
	const row = "(7, 'Titel', '2026-03-01', '08:30', 'weekly', 1, '2026-01-01T00:00:00Z', '2026-02-01T00:00:00Z', NULL, 3, 'Notiz', 1, 40, 1, 2, 5, 9, NULL)"
	for _, stmt := range []string{
		"CREATE TABLE tasks (id INTEGER PRIMARY KEY, title TEXT NOT NULL, due_date TEXT, due_time TEXT, recurrence TEXT NOT NULL DEFAULT '', " +
			"done INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, completed_at TEXT, reminded_at TEXT, parent_id INTEGER, " +
			"note TEXT NOT NULL DEFAULT '', pinned INTEGER NOT NULL DEFAULT 0, sort_order INTEGER NOT NULL DEFAULT 0, " +
			"in_progress INTEGER NOT NULL DEFAULT 0, priority INTEGER NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 3), " +
			"list_id INTEGER, completed_by INTEGER, push_reminded_at TEXT)",
		"INSERT INTO tasks VALUES " + row,
		"INSERT INTO tasks (id, title, created_at) VALUES (3, 'Eltern', 'z')",
		"INSERT INTO tasks (id, title, created_at) VALUES (12, 'zuletzt', 'z')",
		// deleted just before the upgrade — its ID mustn't come back either
		"INSERT INTO tasks (id, title, created_at) VALUES (13, 'gelöscht', 'z')",
		"DELETE FROM tasks WHERE id = 13",
	} {
		if _, err := old.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old.Close()

	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB auf Bestands-DB: %v", err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow("SELECT printf('(%d, %Q, %Q, %Q, %Q, %d, %Q, %Q, %Q, %d, %Q, %d, %d, %d, %d, %d, %d, %Q)', " +
		"id, title, due_date, due_time, recurrence, done, created_at, completed_at, reminded_at, parent_id, note, pinned, " +
		"sort_order, in_progress, priority, list_id, completed_by, push_reminded_at) FROM tasks WHERE id = 7").Scan(&got); err != nil {
		t.Fatalf("Aufgabe 7 verloren: %v", err)
	}
	if got != row {
		t.Fatalf("Aufgabe 7 nach der Migration\n%s\nvorher\n%s", got, row)
	}
	var objects int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE tbl_name = 'tasks' AND name IN " +
		"('tasks_reminders_cleanup', 'tasks_default_list', 'idx_tasks_parent', 'idx_tasks_list')").Scan(&objects)
	if objects != 4 {
		t.Fatalf("%d von 4 Triggern und Indizes auf tasks", objects)
	}
	if _, err := db.Exec("UPDATE tasks SET priority = 9 WHERE id = 3"); err == nil {
		t.Fatal("Prioritäts-Check fehlt nach der Migration")
	}
	add := func() int64 {
		t.Helper()
		res, err := db.Exec("INSERT INTO tasks (title, created_at) VALUES ('neu', 'z')")
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	newest := add()
	if newest <= 13 {
		t.Fatalf("neue ID %d — schon vor dem Upgrade vergeben", newest)
	}
	db.Exec("DELETE FROM tasks WHERE id = ?", newest)
	if again := add(); again == newest {
		t.Fatalf("ID %d nach der Migration neu vergeben", again)
	}
	db.Close()
	db2, err := openDB(path)
	if err != nil {
		t.Fatalf("zweiter Start: %v", err)
	}
	defer db2.Close()
	var n int
	db2.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&n)
	if n != 4 {
		t.Fatalf("nach dem zweiten Start %d Aufgaben, erwartet 4", n)
	}
}
