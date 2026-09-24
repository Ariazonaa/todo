package main

import (
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
