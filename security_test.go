package main

import (
	"bytes"
	"database/sql"
	"image"
	"image/png"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// A plain strings.HasSuffix(host, "discord.com") would let "evildiscord.com"
// through — the digest would then go to a foreign host.
func TestValidateSettingsRejectsLookalikeDiscordHosts(t *testing.T) {
	for _, raw := range []string{
		"https://evildiscord.com/api/webhooks/1/x",
		"https://mydiscordapp.com/api/webhooks/1/x",
		"https://discord.com.attacker.io/api/webhooks/1/x",
	} {
		err := validateSettings(appSettings{
			WebhookURL: raw, TZ: "Europe/Berlin", DigestTime: "09:00", ArchiveDays: 30,
		})
		if err == nil {
			t.Errorf("%s hätte abgelehnt werden müssen", raw)
		}
	}
}

func TestValidateSettingsAllowsDiscordSubdomains(t *testing.T) {
	for _, raw := range []string{
		"https://discord.com/api/webhooks/1/x",
		"https://ptb.discord.com/api/webhooks/1/x",
		"https://discordapp.com/api/webhooks/1/x",
	} {
		err := validateSettings(appSettings{
			WebhookURL: raw, TZ: "Europe/Berlin", DigestTime: "09:00", ArchiveDays: 30,
		})
		if err != nil {
			t.Errorf("%s hätte akzeptiert werden müssen: %v", raw, err)
		}
	}
}

// bombPNG: tiny file, huge pixel area.
func bombPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).
		Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// Decompression bomb: 12000x12000 is ~170 KB as a PNG but decodes to
// hundreds of MB. Without a header check, a single request is enough to
// trigger an OOM kill.
func TestCheckImageDimensionsRejectsBomb(t *testing.T) {
	data := bombPNG(t, 12000, 12000)
	if len(data) > maxAttachmentSize {
		t.Fatalf("Testbild %d Bytes — läge schon am Größenlimit", len(data))
	}
	if err := checkImageDimensions(data); err == nil {
		t.Fatalf("%d Bytes mit 144 MP hätten abgelehnt werden müssen", len(data))
	}
	// The thumbnail path (lazy backfill for existing images) must also
	// bail out before decoding.
	if _, err := makeThumb(data); err == nil {
		t.Fatal("makeThumb hätte die Bombe ablehnen müssen")
	}
}

func TestCheckImageDimensionsAcceptsNormalPhoto(t *testing.T) {
	if err := checkImageDimensions(bombPNG(t, 4000, 3000)); err != nil {
		t.Fatalf("12 MP sind ein normales Kamerabild: %v", err)
	}
}

// The stored MIME type is served as Content-Type. A "text/html" coming
// from a backup would otherwise be stored XSS on our own origin.
func TestServeMimeForcesDownloadForNonImages(t *testing.T) {
	for _, m := range []string{"text/html", "image/svg+xml", "application/javascript", ""} {
		rec := httptest.NewRecorder()
		serveMime(rec, m)
		if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("%q wurde als %q ausgeliefert", m, got)
		}
		if rec.Header().Get("Content-Disposition") == "" {
			t.Errorf("%q braucht Content-Disposition: attachment", m)
		}
	}
}

func TestServeMimePassesImagesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	serveMime(rec, "image/png")
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q", got)
	}
	if rec.Header().Get("Content-Disposition") != "" {
		t.Fatal("Bilder sollen inline angezeigt werden")
	}
}

// Without AUTOINCREMENT, SQLite reassigns the highest deleted rowid. Since
// delivery is cached immutable for a year, the browser would then keep
// showing the old image under the same URL indefinitely.
func TestAttachmentIDsAreNeverReused(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec("INSERT INTO tasks (id, title, created_at) VALUES (1, 'task', '2024-01-01T00:00:00Z')"); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	add := func() int64 {
		t.Helper()
		res, err := db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (1, 'image/jpeg', '2024-01-01T00:00:00Z', x'00')")
		if err != nil {
			t.Fatalf("insert attachment: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	first := add()
	if _, err := db.Exec("DELETE FROM attachments WHERE id = ?", first); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if second := add(); second == first {
		t.Fatalf("ID %d wurde nach dem Löschen neu vergeben", second)
	}
}

// The rebuild must restore the attachments_limit trigger: DROP TABLE takes
// it down along with the table, and CREATE TRIGGER IF NOT EXISTS would not
// replace an existing one.
func TestAutoincrementMigrationKeepsLimitTrigger(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Simulate an existing DB: table without AUTOINCREMENT, with existing
	// data.
	for _, stmt := range []string{
		"DROP TABLE attachments",
		"CREATE TABLE attachments (id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL, mime TEXT NOT NULL, created_at TEXT NOT NULL, data BLOB NOT NULL, thumb BLOB)",
		"INSERT INTO tasks (id, title, created_at) VALUES (1, 'task', '2024-01-01T00:00:00Z')",
		"INSERT INTO attachments (id, task_id, mime, created_at, data) VALUES (7, 1, 'image/jpeg', '2024-01-01T00:00:00Z', x'00')",
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Did the existing data survive?
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM attachments WHERE id = 7").Scan(&n); err != nil || n != 1 {
		t.Fatalf("Bestandsbild verloren (n=%d, err=%v)", n, err)
	}
	// Sequence is at max(id) -> no collision with existing URLs
	res, err := db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (1, 'image/jpeg', '2024-01-01T00:00:00Z', x'00')")
	if err != nil {
		t.Fatalf("insert nach Migration: %v", err)
	}
	if id, _ := res.LastInsertId(); id <= 7 {
		t.Fatalf("neue ID %d kollidiert mit Bestand", id)
	}
	// Is the trigger back?
	for range maxAttachmentsPerTask {
		db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (1, 'image/jpeg', '2024-01-01T00:00:00Z', x'00')")
	}
	var total int
	db.QueryRow("SELECT COUNT(*) FROM attachments WHERE task_id = 1").Scan(&total)
	if total > maxAttachmentsPerTask {
		t.Fatalf("attachments_limit-Trigger fehlt nach der Migration: %d Bilder", total)
	}
	// Migration is idempotent
	if err := migrate(db); err != nil {
		t.Fatalf("zweiter migrate-Lauf: %v", err)
	}
}

// The rebuild touches real user data — exercise it once fully through
// openDB on a file, including WAL and an existing DB in the state before
// the thumb column (the ALTER migration must run before the rebuild).
func TestOpenDBMigratesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todo.db")

	old, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		"CREATE TABLE tasks (id INTEGER PRIMARY KEY, title TEXT NOT NULL, due_date TEXT, due_time TEXT, recurrence TEXT NOT NULL DEFAULT '', done INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, completed_at TEXT, reminded_at TEXT)",
		"CREATE TABLE attachments (id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL, mime TEXT NOT NULL, created_at TEXT NOT NULL, data BLOB NOT NULL)",
		"INSERT INTO tasks (id, title, created_at) VALUES (1, 'alt', '2024-01-01T00:00:00Z')",
		"INSERT INTO attachments (id, task_id, mime, created_at, data) VALUES (42, 1, 'image/jpeg', '2024-01-01T00:00:00Z', x'ffd8ff')",
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

	var mime string
	var data []byte
	if err := db.QueryRow("SELECT mime, data FROM attachments WHERE id = 42").Scan(&mime, &data); err != nil {
		t.Fatalf("Bestandsbild verloren: %v", err)
	}
	if mime != "image/jpeg" || !bytes.Equal(data, []byte{0xff, 0xd8, 0xff}) {
		t.Fatalf("Bestandsbild verändert: %q %x", mime, data)
	}
	// thumb column retrofitted and preserved through the rebuild
	if _, err := db.Exec("UPDATE attachments SET thumb = x'00' WHERE id = 42"); err != nil {
		t.Fatalf("thumb-Spalte fehlt: %v", err)
	}
	res, err := db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (1, 'image/png', '2024-01-01T00:00:00Z', x'00')")
	if err != nil {
		t.Fatalf("insert nach Migration: %v", err)
	}
	if id, _ := res.LastInsertId(); id <= 42 {
		t.Fatalf("neue ID %d kollidiert mit der Bestands-URL von Bild 42", id)
	}

	// A second startup must not rebuild anything else
	db.Close()
	db2, err := openDB(path)
	if err != nil {
		t.Fatalf("zweiter openDB: %v", err)
	}
	defer db2.Close()
	var n int
	if err := db2.QueryRow("SELECT COUNT(*) FROM attachments").Scan(&n); err != nil || n != 2 {
		t.Fatalf("nach Neustart n=%d err=%v", n, err)
	}
}
