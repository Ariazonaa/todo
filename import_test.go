package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testUID: the admin account of every test instance (testServer creates it).
const testUID int64 = 1

func testServer(t *testing.T) *server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if id, err := createUser(db, "Admin", true, []byte("todo-single-user")); err != nil || id != testUID {
		t.Fatalf("Test-Admin: %d %v", id, err)
	}
	store, err := newSettingsStore(db)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	vapid, err := loadOrCreateVAPID(db)
	if err != nil {
		t.Fatalf("vapid: %v", err)
	}
	return &server{db: db, store: store, cer: newCeremonies(), hub: newHub(), vapid: vapid}
}

func postImport(t *testing.T, s *server, data exportData) *httptest.ResponseRecorder {
	t.Helper()
	return postImportAs(t, s, testUID, data)
}

// postImportAs replays data as user uid.
func postImportAs(t *testing.T, s *server, uid int64, data exportData) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	s.handleImport(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(string(body))), uid))
	return rec
}

// mustUser creates another account (without a passkey) and returns its ID.
func mustUser(t *testing.T, s *server, name string) int64 {
	t.Helper()
	id, err := createUser(s.db, name, false, []byte("handle-"+name))
	if err != nil {
		t.Fatalf("Konto %q: %v", name, err)
	}
	return id
}

func ptr(s string) *string { return &s }

// A backup is a foreign file. Its MIME type ends up as the Content-Type in
// the browser on retrieval — "text/html" would be stored XSS on our own origin.
func TestImportDropsNonImageAttachments(t *testing.T) {
	s := testServer(t)
	rec := postImport(t, s, exportData{
		Tasks: []exportTask{{ID: 1, Title: "task", CreatedAt: "2024-01-01T00:00:00Z"}},
		Attachments: []exportAttachment{
			{ID: 1, TaskID: 1, Mime: "text/html", CreatedAt: "2024-01-01T00:00:00Z", Data: []byte("<script>alert(1)</script>")},
			{ID: 2, TaskID: 1, Mime: "image/svg+xml", CreatedAt: "2024-01-01T00:00:00Z", Data: []byte("<svg onload=alert(1)>")},
			{ID: 3, TaskID: 1, Mime: "image/png", CreatedAt: "2024-01-01T00:00:00Z", Data: []byte("echtes bild")},
		},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Import fehlgeschlagen: %d %s", rec.Code, rec.Body)
	}
	var mimes []string
	rows, err := s.db.Query("SELECT mime FROM attachments ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		mimes = append(mimes, m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(mimes) != 1 || mimes[0] != "image/png" {
		t.Fatalf("erwartet nur image/png, bekam %v", mimes)
	}
}

// Unknown recurrence values used to make a task permanently uncompletable:
// handleCompleteTask -> nextOccurrence -> error -> 500, on every attempt.
func TestImportRejectsInvalidTaskFields(t *testing.T) {
	cases := map[string]exportTask{
		"unbekannte Wiederholung": {ID: 1, Title: "x", CreatedAt: "z", Recurrence: "hourly"},
		"kaputtes Datum":          {ID: 1, Title: "x", CreatedAt: "z", DueDate: ptr("01.02.2026")},
		"kaputte Uhrzeit":         {ID: 1, Title: "x", CreatedAt: "z", DueDate: ptr("2026-02-01"), DueTime: ptr("25:99")},
		"Uhrzeit ohne Datum":      {ID: 1, Title: "x", CreatedAt: "z", DueTime: ptr("09:00")},
		"leerer Titel":            {ID: 1, Title: "   ", CreatedAt: "z"},
		"Titel zu lang":           {ID: 1, Title: strings.Repeat("a", maxTitleLen+1), CreatedAt: "z"},
	}
	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			s := testServer(t)
			rec := postImport(t, s, exportData{Tasks: []exportTask{task}})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("erwartet 400, bekam %d (%s)", rec.Code, rec.Body)
			}
		})
	}
}

// The UI only knows one level — deeper chains it could neither display
// nor resolve again.
func TestImportRejectsNestedSubtasks(t *testing.T) {
	s := testServer(t)
	one, two := int64(1), int64(2)
	rec := postImport(t, s, exportData{Tasks: []exportTask{
		{ID: 1, Title: "opa", CreatedAt: "z"},
		{ID: 2, Title: "vater", CreatedAt: "z", ParentID: &one},
		{ID: 3, Title: "kind", CreatedAt: "z", ParentID: &two},
	}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("erwartet 400, bekam %d (%s)", rec.Code, rec.Body)
	}
}

// More images than allowed used to make the attachments_limit trigger abort
// the entire import with a 500 — instead of just dropping the excess.
func TestImportTrimsAttachmentsToLimit(t *testing.T) {
	s := testServer(t)
	data := exportData{Tasks: []exportTask{{ID: 1, Title: "task", CreatedAt: "z"}}}
	for i := range maxAttachmentsPerTask + 4 {
		data.Attachments = append(data.Attachments, exportAttachment{
			ID: int64(i + 1), TaskID: 1, Mime: "image/png", CreatedAt: "z", Data: []byte("x"),
		})
	}
	rec := postImport(t, s, data)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("erwartet 204, bekam %d (%s)", rec.Code, rec.Body)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM attachments WHERE task_id = 1").Scan(&n)
	if n != maxAttachmentsPerTask {
		t.Fatalf("erwartet %d Bilder, bekam %d", maxAttachmentsPerTask, n)
	}
}

// Whatever the API normalizes or rejects outright, the import must handle
// the same way — otherwise imported tasks would behave differently from any other.
func TestImportNormalizesLikeTheAPI(t *testing.T) {
	s := testServer(t)
	one := int64(1)
	rec := postImport(t, s, exportData{
		Tasks: []exportTask{
			// "9:30" would break lexical comparisons: never overdue today, no reminder
			{ID: 1, Title: "Arzt", CreatedAt: "z", DueDate: ptr("2026-03-01"), DueTime: ptr("9:30")},
			{ID: 2, Title: "Sub", CreatedAt: "z", ParentID: &one, DueDate: ptr("2026-03-01"), Recurrence: "daily", Pinned: true, InProgress: true},
			{ID: 3, Title: "ohne Datum", CreatedAt: "z", Recurrence: "weekly"},
		},
		Links: [][2]int64{{3, 3}, {1, 3}},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("erwartet 204, bekam %d (%s)", rec.Code, rec.Body)
	}
	arzt, _ := getTask(s.db, 1)
	if arzt.DueTime == nil || *arzt.DueTime != "09:30" {
		t.Fatalf("Uhrzeit nicht normalisiert: %v", arzt.DueTime)
	}
	sub, _ := getTask(s.db, 2)
	if sub.Recurrence != "" || sub.Pinned || sub.InProgress {
		t.Fatalf("Unteraufgabe mit Wiederholung/Pin/In-Arbeit übernommen: %+v", sub)
	}
	if ohne, _ := getTask(s.db, 3); ohne.Recurrence != "" {
		t.Fatalf("Wiederholung ohne Datum übernommen: %q", ohne.Recurrence)
	}
	var selfLinks, links int
	s.db.QueryRow("SELECT COUNT(*) FROM links WHERE a = b").Scan(&selfLinks)
	s.db.QueryRow("SELECT COUNT(*) FROM links").Scan(&links)
	if selfLinks != 0 || links != 1 {
		t.Fatalf("Verknüpfungen: %d mit sich selbst, %d insgesamt (erwartet 0 und 1)", selfLinks, links)
	}
}

// Oversized numbers from a backup would cripple the app later: MAX(sort_order)+10
// overflows, and IDs beyond 2^53 are no longer exact in JavaScript.
func TestImportRejectsOutOfRangeNumbers(t *testing.T) {
	cases := map[string]exportTask{
		"sort_order": {ID: 1, Title: "x", CreatedAt: "z", SortOrder: 1 << 62},
		"ID":         {ID: 1 << 60, Title: "x", CreatedAt: "z"},
	}
	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			s := testServer(t)
			if rec := postImport(t, s, exportData{Tasks: []exportTask{task}}); rec.Code != http.StatusBadRequest {
				t.Fatalf("erwartet 400, bekam %d (%s)", rec.Code, rec.Body)
			}
		})
	}
	s := testServer(t)
	rec := postImport(t, s, exportData{
		Tasks: []exportTask{{ID: 1, Title: "x", CreatedAt: "z"}},
		Stats: map[string]int{"2026-03-01": 1 << 40, "2026-03-02": 4},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("erwartet 204, bekam %d (%s)", rec.Code, rec.Body)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM stats").Scan(&n)
	if n != 1 {
		t.Fatalf("absurder Tageszähler hätte wegfallen müssen (%d Einträge)", n)
	}
}

// The ID from the backup could belong to a URL the browser still associates
// with a different image — imported images get fresh IDs.
func TestImportAssignsFreshAttachmentIDs(t *testing.T) {
	s := testServer(t)
	s.db.Exec("INSERT INTO tasks (id, title, created_at) VALUES (1, 'x', 'z')")
	for range 5 {
		s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (1, 'image/png', 'z', x'00')")
	}
	rec := postImport(t, s, exportData{
		Tasks:       []exportTask{{ID: 1, Title: "x", CreatedAt: "z"}},
		Attachments: []exportAttachment{{ID: 2, TaskID: 1, Mime: "image/png", CreatedAt: "z", Data: []byte("x")}},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("erwartet 204, bekam %d (%s)", rec.Code, rec.Body)
	}
	var id int64
	s.db.QueryRow("SELECT id FROM attachments").Scan(&id)
	if id <= 5 {
		t.Fatalf("importiertes Bild hat ID %d — schon einmal vergeben", id)
	}
}

// The webhook URL determines where task titles go. A backup must not
// silently redirect one that's already set; a fresh instance adopts it.
func TestImportKeepsConfiguredWebhook(t *testing.T) {
	backup := func(webhook string) exportData {
		return exportData{
			Tasks:    []exportTask{{ID: 1, Title: "x", CreatedAt: "z"}},
			Settings: appSettings{WebhookURL: webhook, TZ: "Europe/Vienna", DigestTime: "08:00", ArchiveDays: 7},
		}
	}
	s := testServer(t)
	mine := "https://discord.com/api/webhooks/1/meiner"
	if err := s.store.save(testUID, appSettings{WebhookURL: mine, TZ: "Europe/Berlin", DigestTime: "09:00", ArchiveDays: 30}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if rec := postImport(t, s, backup("https://discord.com/api/webhooks/666/fremd")); rec.Code != http.StatusNoContent {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	got := s.store.get(testUID)
	if got.WebhookURL != mine {
		t.Fatalf("Webhook wurde durch das Backup ersetzt: %q", got.WebhookURL)
	}
	// The archive retention period is instance-wide — a backup does not set it.
	if got.TZ != "Europe/Vienna" || got.DigestTime != "08:00" || got.ArchiveDays != 30 {
		t.Fatalf("übrige Einstellungen nicht übernommen: %+v", got)
	}

	fresh := testServer(t)
	restore := "https://discord.com/api/webhooks/1/backup"
	postImport(t, fresh, backup(restore))
	if got := fresh.store.get(testUID).WebhookURL; got != restore {
		t.Fatalf("frische Instanz hat den Webhook aus dem Backup nicht übernommen: %q", got)
	}
}

func TestImportAcceptsValidBackup(t *testing.T) {
	s := testServer(t)
	one := int64(1)
	rec := postImport(t, s, exportData{
		Tasks: []exportTask{
			{ID: 1, Title: "einkaufen", CreatedAt: "2024-01-01T00:00:00Z", DueDate: ptr("2026-02-01"), DueTime: ptr("09:00"), Recurrence: "weekly"},
			{ID: 2, Title: "milch", CreatedAt: "2024-01-01T00:00:00Z", ParentID: &one},
		},
		Links: [][2]int64{{1, 2}},
		Stats: map[string]int{"2026-01-31": 3, "kaputt": 5, "2026-01-30": -7},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("erwartet 204, bekam %d (%s)", rec.Code, rec.Body)
	}
	var tasks, stats int
	s.db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&tasks)
	s.db.QueryRow("SELECT COUNT(*) FROM stats").Scan(&stats)
	if tasks != 2 {
		t.Fatalf("erwartet 2 Tasks, bekam %d", tasks)
	}
	if stats != 1 {
		t.Fatalf("kaputte/negative Tageszähler hätten wegfallen müssen, bekam %d", stats)
	}
}
