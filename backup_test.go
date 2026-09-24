package main

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// fileServer is testServer with the DB on disk: the backup tests move
// hundreds of MB, which should not also sit in RAM.
func fileServer(t *testing.T) *server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "todo.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := createUser(db, "Admin", true, []byte("todo-single-user")); err != nil {
		t.Fatalf("createUser: %v", err)
	}
	store, err := newSettingsStore(db)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	return &server{db: db, store: store, cer: newCeremonies(), hub: newHub(), cfg: config{dbPath: path}}
}

// pipeWriter passes the export straight through to the import, without
// buffering it in memory.
type pipeWriter struct {
	h    http.Header
	w    *io.PipeWriter
	code int
}

func (p *pipeWriter) Header() http.Header         { return p.h }
func (p *pipeWriter) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeWriter) WriteHeader(code int)        { p.code = code }

// The export used to write images base64-encoded (factor 4/3) without an
// upper bound, while the import accepted at most 200 MB: from ~150 MB of
// images onward, the resulting backup could no longer be restored.
func TestExportLargerThanOldLimitCanBeImported(t *testing.T) {
	if testing.Short() {
		t.Skip("bewegt ~250 MB")
	}
	src := fileServer(t)
	img := make([]byte, maxAttachmentSize)
	rand.Read(img)
	for task := int64(1); task <= 2; task++ {
		if _, err := src.db.Exec("INSERT INTO tasks (id, title, created_at) VALUES (?, 'mit Bildern', 'z')", task); err != nil {
			t.Fatal(err)
		}
		for range maxAttachmentsPerTask {
			if _, err := src.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/jpeg', 'z', ?)", task, img); err != nil {
				t.Fatal(err)
			}
		}
	}

	pr, pw := io.Pipe()
	out := &pipeWriter{h: http.Header{}, w: pw}
	var exported int64
	counted := &countingReader{r: pr, n: &exported}
	go func() {
		src.handleExport(out, withUser(httptest.NewRequest(http.MethodGet, "/api/export", nil), testUID))
		pw.Close()
	}()

	dst := fileServer(t)
	rec := httptest.NewRecorder()
	dst.handleImport(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/import", counted), testUID))
	io.Copy(io.Discard, pr) // don't leave the export hanging if the import aborts early

	if exported <= 200<<20 {
		t.Fatalf("Testaufbau: Export hat nur %d MB, soll über dem alten Limit liegen", exported>>20)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Import eines %d-MB-Exports: %d %s, erwartet 204", exported>>20, rec.Code, rec.Body)
	}
	var n int
	var total int64
	dst.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(LENGTH(data)), 0) FROM attachments").Scan(&n, &total)
	if n != 2*maxAttachmentsPerTask || total != int64(2*maxAttachmentsPerTask*maxAttachmentSize) {
		t.Fatalf("nach dem Import: %d Bilder, %d Bytes", n, total)
	}
	var same int
	dst.db.QueryRow("SELECT COUNT(*) FROM attachments WHERE data = ?", img).Scan(&same)
	if same != n {
		t.Fatalf("%d von %d Bildern kamen verändert an", n-same, n)
	}
}

type countingReader struct {
	r io.Reader
	n *int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	k, err := c.r.Read(b)
	*c.n += int64(k)
	return k, err
}

// A second import running while one is already in progress used to share
// the staging table with the first.
func TestImportRejectsConcurrentImport(t *testing.T) {
	s := testServer(t)
	s.importMu.Lock()
	defer s.importMu.Unlock()
	rec := httptest.NewRecorder()
	s.handleImport(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader([]byte(`{"tasks":[]}`))), testUID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("Import während eines Imports: %d %s, erwartet 409", rec.Code, rec.Body)
	}
}

// Images are staged while reading and only attached to tasks at the end —
// the order of fields in the JSON must not matter.
func TestImportAcceptsAttachmentsBeforeTasks(t *testing.T) {
	s := testServer(t)
	body := `{"attachments":[{"task_id":1,"mime":"image/png","created_at":"z","data":"eA=="}],` +
		`"tasks":[{"id":1,"title":"t","created_at":"z"}]}`
	rec := httptest.NewRecorder()
	s.handleImport(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader([]byte(body))), testUID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("erwartet 204, bekam %d (%s)", rec.Code, rec.Body)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM attachments WHERE task_id = 1").Scan(&n)
	if n != 1 {
		t.Fatalf("erwartet 1 Bild, bekam %d", n)
	}
}

// An aborted import (broken JSON in the middle of the images) leaves the
// old data untouched and no leftovers in the staging table.
func TestImportFailureMidStreamKeepsOldData(t *testing.T) {
	s := testServer(t)
	s.db.Exec("INSERT INTO tasks (id, title, created_at) VALUES (7, 'bleibt', 'z')")
	body := `{"tasks":[{"id":1,"title":"neu","created_at":"z"}],"attachments":[{"task_id":1,"mime":"image/png","created_at":"z","data":"eA=="},{"task_id":1,`
	rec := httptest.NewRecorder()
	s.handleImport(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/import", bytes.NewReader([]byte(body))), testUID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("erwartet 400, bekam %d (%s)", rec.Code, rec.Body)
	}
	var title string
	s.db.QueryRow("SELECT title FROM tasks").Scan(&title)
	if title != "bleibt" {
		t.Fatalf("alter Bestand weg, Task-Titel jetzt %q", title)
	}
	var staged int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM import_attachments").Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("Zwischentabelle: %d Bilder liegen geblieben (err=%v)", staged, err)
	}
}
