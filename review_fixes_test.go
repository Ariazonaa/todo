package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A backup's title and note are checked while it's read, not after: decoded
// first and checked later, 10,000 tasks with huge titles sat in memory up to
// the body limit (2 GB) — more than the container has.
func TestImportChecksTextLengthWhileReading(t *testing.T) {
	s := testServer(t)
	long := strings.Repeat("a", 100<<10)
	body := `{"tasks":[{"id":1,"title":"` + long + `"},{"id":2,"title":"` + long + `"}]}`
	data, err := s.readImport(strings.NewReader(body), testUID)
	if _, ok := errors.AsType[fieldTooLongError](err); !ok {
		t.Fatalf("erwartet fieldTooLongError, bekam %v (%d Aufgaben gelesen)", err, len(data.Tasks))
	}
	if rec := postImportRaw(t, s, testUID, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("Import mit überlangem Titel: Status %d, erwartet 400", rec.Code)
	}
}

// Only images may be as large as maxImportElement: a task padded with an
// unknown field must not get megabytes of buffer each.
func TestImportTaskElementsAreSmall(t *testing.T) {
	s := testServer(t)
	body := `{"tasks":[{"id":1,"title":"ok","junk":"` + strings.Repeat("a", 1<<20) + `"}]}`
	if _, err := s.readImport(strings.NewReader(body), testUID); !errors.Is(err, errElementTooLarge) {
		t.Fatalf("erwartet errElementTooLarge, bekam %v", err)
	}
}

// Stats keys are days (YYYY-MM-DD): anything else is dropped while reading
// instead of being kept as a map key until the import validates it.
func TestImportDropsBrokenStatsKeysWhileReading(t *testing.T) {
	s := testServer(t)
	body := `{"stats":{"` + strings.Repeat("9", 100<<10) + `":1,"2026-01-02":3}}`
	data, err := s.readImport(strings.NewReader(body), testUID)
	if err != nil {
		t.Fatalf("readImport: %v", err)
	}
	if len(data.Stats) != 1 || data.Stats["2026-01-02"] != 3 {
		t.Fatalf("erwartet nur den gültigen Tag, bekam %d Einträge", len(data.Stats))
	}
}

// A backup may not bring more images than an owner may hold — otherwise the
// next export no longer fits through the import. Nothing is replaced then.
func TestImportRespectsImageLimit(t *testing.T) {
	s := testServer(t)
	old := maxAttachmentBytesPerOwner
	maxAttachmentBytesPerOwner = 100
	t.Cleanup(func() { maxAttachmentBytesPerOwner = old })
	mustCreateAs(t, s, testUID, `{"title":"bleibt"}`)

	img := make([]byte, 80)
	rec := postImport(t, s, exportData{
		Tasks: []exportTask{{ID: 1, Title: "neu", CreatedAt: "2024-01-01T00:00:00Z"}},
		Attachments: []exportAttachment{
			{ID: 1, TaskID: 1, Mime: "image/png", Data: img},
			{ID: 2, TaskID: 1, Mime: "image/png", Data: img},
		},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("Import über dem Bild-Limit: Status %d, erwartet 409 (%s)", rec.Code, rec.Body)
	}
	var kept int
	s.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE title = 'bleibt'").Scan(&kept)
	if kept != 1 {
		t.Fatalf("abgelehnter Import hat die Daten trotzdem ersetzt")
	}
}

// The image limit is part of the INSERT itself: checked only before it, two
// uploads running side by side both got in.
func TestImageLimitIsPartOfTheInsert(t *testing.T) {
	s := testServer(t)
	old := maxAttachmentBytesPerOwner
	maxAttachmentBytesPerOwner = 100
	t.Cleanup(func() { maxAttachmentBytesPerOwner = old })
	task := mustCreateAs(t, s, testUID, `{"title":"Bild"}`)

	if _, err := insertAttachment(s.db, testUID, task.ID, "image/png", make([]byte, 80), nil); err != nil {
		t.Fatalf("erstes Bild: %v", err)
	}
	if _, err := insertAttachment(s.db, testUID, task.ID, "image/png", make([]byte, 80), nil); !errors.Is(err, errStorageLimit) {
		t.Fatalf("zweites Bild: erwartet errStorageLimit, bekam %v", err)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM attachments").Scan(&n)
	if n != 1 {
		t.Fatalf("%d Bilder gespeichert, erwartet 1", n)
	}
}

// Titles from a shared list land in the other members' Discord channels: a
// masked link ("[Rechnung](https://…)") must arrive as text, not as a link.
func TestDiscordMessagesEscapeMarkdownInTitles(t *testing.T) {
	sc := schedulerFixture(t)
	d, url := newDiscordStub(t)
	if _, err := sc.db.Exec("INSERT INTO tasks (title, due_date, due_time, created_at) VALUES (?, '2026-03-01', '08:00', 'z')",
		"[Rechnung](https://evil.example) *wichtig*"); err != nil {
		t.Fatal(err)
	}
	sc.exactReminders(testUID, url, "2026-03-01", "09:00")
	msgs := d.contents()
	if len(msgs) != 1 {
		t.Fatalf("erwartet eine Nachricht, bekam %d", len(msgs))
	}
	if strings.Contains(msgs[0], "[Rechnung](") || !strings.Contains(msgs[0], `\[Rechnung\](https://evil.example) \*wichtig\*`) {
		t.Fatalf("Markdown im Titel nicht entschärft: %q", msgs[0])
	}
}

// The limit is passed in MB — the text used to call it GB ("1280 GB").
func TestStorageLimitMessageNamesMB(t *testing.T) {
	for _, lang := range languages {
		if msg := T(lang, "error.storage_limit", "n", 1280); !strings.Contains(msg, "1280 MB") {
			t.Errorf("%s: %q", lang, msg)
		}
	}
}
