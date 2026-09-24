package main

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAttachmentBelongsToTask(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	if _, err := createUser(db, "Admin", true, []byte("todo-single-user")); err != nil {
		t.Fatal(err)
	}

	res, err := db.Exec("INSERT INTO tasks (id, title, created_at) VALUES (?, ?, ?)", 1, "task", "2024-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	if _, err := res.LastInsertId(); err != nil {
		t.Fatalf("last insert id: %v", err)
	}

	attRes, err := db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, ?, ?, ?)", 1, "image/jpeg", "2024-01-01T00:00:00Z", []byte("img"))
	if err != nil {
		t.Fatalf("insert attachment: %v", err)
	}
	attID, err := attRes.LastInsertId()
	if err != nil {
		t.Fatalf("attachment id: %v", err)
	}

	belongs, err := attachmentBelongsToTask(db, testUID, attID, 1)
	if err != nil {
		t.Fatalf("attachmentBelongsToTask: %v", err)
	}
	if !belongs {
		t.Fatal("expected attachment to belong to task")
	}

	belongs, err = attachmentBelongsToTask(db, testUID, attID, 2)
	if err != nil {
		t.Fatalf("attachmentBelongsToTask mismatch: %v", err)
	}
	if belongs {
		t.Fatal("expected attachment not to belong to different task")
	}
}

func TestAttachmentBelongsToTaskMissingAttachment(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	belongs, err := attachmentBelongsToTask(db, testUID, 999, 1)
	if err != nil {
		t.Fatalf("attachmentBelongsToTask missing: %v", err)
	}
	if belongs {
		t.Fatal("expected missing attachment to not belong to task")
	}
}

// The limit depends on the attachments_limit trigger — the number in the
// schema and maxAttachmentsPerTask must match, or it silently stops
// applying.
func TestAttachmentLimitTrigger(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec("INSERT INTO tasks (id, title, created_at) VALUES (?, ?, ?)", 1, "task", "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	insert := func(taskID int) error {
		_, err := db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, ?, ?, ?)",
			taskID, "image/jpeg", "2024-01-01T00:00:00Z", []byte("img"))
		return err
	}

	for i := range maxAttachmentsPerTask {
		if err := insert(1); err != nil {
			t.Fatalf("insert %d von %d: %v", i+1, maxAttachmentsPerTask, err)
		}
	}
	err = insert(1)
	if err == nil {
		t.Fatalf("erwartete Ablehnung ab Bild %d", maxAttachmentsPerTask+1)
	}
	if !strings.Contains(err.Error(), "attachment limit reached") {
		t.Fatalf("unerwarteter Fehler (Handler prüft auf diesen Text): %v", err)
	}

	// The limit applies per task, not globally
	if _, err := db.Exec("INSERT INTO tasks (id, title, created_at) VALUES (?, ?, ?)", 2, "andere", "2024-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert task 2: %v", err)
	}
	if err := insert(2); err != nil {
		t.Fatalf("anderer Task muss eigene Bilder erlauben: %v", err)
	}
}

func TestAttachmentBelongsToTaskBrokenDB(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	_, err = attachmentBelongsToTask(db, testUID, 1, 1)
	if err == nil {
		t.Fatal("expected error for unopened schema")
	}
}
