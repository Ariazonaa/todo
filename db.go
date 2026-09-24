package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id           INTEGER PRIMARY KEY,
	title        TEXT NOT NULL,
	due_date     TEXT,
	due_time     TEXT,
	recurrence   TEXT NOT NULL DEFAULT '',
	done         INTEGER NOT NULL DEFAULT 0,
	created_at   TEXT NOT NULL,
	completed_at TEXT,
	reminded_at  TEXT,
	parent_id    INTEGER,
	note         TEXT NOT NULL DEFAULT '',
	pinned       INTEGER NOT NULL DEFAULT 0,
	sort_order   INTEGER NOT NULL DEFAULT 0,
	in_progress  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT NOT NULL,
	name_key      TEXT NOT NULL UNIQUE,
	is_admin      INTEGER NOT NULL DEFAULT 0,
	webauthn_id   BLOB NOT NULL UNIQUE,
	setup_hash    TEXT,
	setup_expires TEXT,
	created_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS user_settings (
	user_id INTEGER NOT NULL,
	key     TEXT NOT NULL,
	value   TEXT NOT NULL,
	PRIMARY KEY (user_id, key)
);
CREATE TABLE IF NOT EXISTS lists (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	owner_id   INTEGER,
	name       TEXT,
	name_key   TEXT, -- listKey(name): unique, case-insensitive beyond ASCII too
	is_default INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS links (
	a INTEGER NOT NULL,
	b INTEGER NOT NULL,
	PRIMARY KEY (a, b)
);
-- members of shared lists (the owner is lists.owner_id)
CREATE TABLE IF NOT EXISTS list_members (
	list_id  INTEGER NOT NULL,
	user_id  INTEGER NOT NULL,
	added_at TEXT NOT NULL,
	PRIMARY KEY (list_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_list_members_user ON list_members(user_id);
-- reminder stamps per person and channel (reminders.go); due is the due
-- date/time (due_date || ' ' || due_time) the reminder was sent for.
CREATE TABLE IF NOT EXISTS task_reminders (
	task_id INTEGER NOT NULL,
	user_id INTEGER NOT NULL,
	channel TEXT NOT NULL CHECK (channel IN ('discord', 'push')),
	due     TEXT NOT NULL,
	sent_at TEXT NOT NULL,
	PRIMARY KEY (task_id, user_id, channel)
);
CREATE INDEX IF NOT EXISTS idx_task_reminders_user ON task_reminders(user_id);
-- tasks.id is reused: stamps go away with the task.
CREATE TRIGGER IF NOT EXISTS tasks_reminders_cleanup AFTER DELETE ON tasks
	BEGIN DELETE FROM task_reminders WHERE task_id = OLD.id; END;
CREATE TABLE IF NOT EXISTS stats (
	user_id INTEGER NOT NULL,
	date    TEXT NOT NULL,
	done    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (user_id, date)
);
CREATE TABLE IF NOT EXISTS sessions (
	token         TEXT PRIMARY KEY, -- SHA-256 of the cookie value (hashToken)
	expires_at    TEXT NOT NULL,
	credential_id INTEGER,
	auth_at       TEXT,
	hashed        INTEGER NOT NULL DEFAULT 0,
	user_id       INTEGER
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS credentials (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	data       TEXT NOT NULL,
	user_id    INTEGER
);
CREATE TABLE IF NOT EXISTS push_subscriptions (
	id            INTEGER PRIMARY KEY,
	endpoint      TEXT NOT NULL UNIQUE,
	p256dh        TEXT NOT NULL, -- base64url, 65 bytes uncompressed
	auth          TEXT NOT NULL, -- base64url, 16 bytes
	origin        TEXT NOT NULL, -- the app's origin at subscription time (VAPID sub)
	credential_id INTEGER,       -- passkey of the subscribing session
	created_at    TEXT NOT NULL,
	user_id       INTEGER
);
`

// attachmentsTable: AUTOINCREMENT is mandatory, not cosmetic. Without it,
// SQLite reassigns the highest deleted rowid — "delete the wrong image,
// upload the right one" would then produce the same ID and thus the same
// URL, which we let the browser cache as immutable for a year. It would then
// keep showing the old image permanently.
const attachmentsTable = `
CREATE TABLE IF NOT EXISTS attachments (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id    INTEGER NOT NULL,
	mime       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	data       BLOB NOT NULL,
	thumb      BLOB
);
`

// Image limit per task. Must be a trigger: SQLite forbids subqueries in
// CHECK constraints (and NEW. doesn't exist there). Runs in the same write
// transaction as the INSERT -> no TOCTOU between counting and inserting.
// The 6 must match maxAttachmentsPerTask (a test pins this). When changing
// it, drop the trigger in migrate() — CREATE TRIGGER IF NOT EXISTS does not
// replace it.
// Import staging area: images are stored here one at a time as they're read
// in and only committed at the end in a short transaction. This way the
// whole backup is never held in memory, and the single DB connection isn't
// locked for the duration of an upload.
const importAttachmentsTable = `
CREATE TABLE IF NOT EXISTS import_attachments (
	seq        INTEGER PRIMARY KEY,
	task_id    INTEGER NOT NULL,
	mime       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	data       BLOB NOT NULL
);
`

const attachmentsTrigger = `
CREATE TRIGGER IF NOT EXISTS attachments_limit
BEFORE INSERT ON attachments
WHEN (SELECT COUNT(*) FROM attachments WHERE task_id = NEW.task_id) >= 6
BEGIN
	SELECT RAISE(ABORT, 'attachment limit reached');
END;
`

type Task struct {
	ID          int64   `json:"id"`
	Title       string  `json:"title"`
	DueDate     *string `json:"due_date"`
	DueTime     *string `json:"due_time"`
	Recurrence  string  `json:"recurrence"`
	Done        bool    `json:"done"`
	CreatedAt   string  `json:"created_at"`
	CompletedAt *string `json:"completed_at"`
	ParentID    *int64  `json:"parent_id"`
	Note        string  `json:"note"`
	Pinned      bool    `json:"pinned"`
	InProgress  bool    `json:"in_progress"`
	Priority    int     `json:"priority"` // 0 none, 1 low, 2 medium, 3 high
	ListID      int64   `json:"list_id"`
	SortOrder   int64   `json:"sort_order"`
	Overdue     bool    `json:"overdue"`
	Links       []int64 `json:"links"`
	Attachments []int64 `json:"attachments"`
}

func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", filepath.ToSlash(path))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single writer: HTTP handlers and the scheduler can never produce SQLITE_BUSY for each other.
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{schema, attachmentsTable, importAttachmentsTable, attachmentsTrigger} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate upgrades existing databases. Column check instead of
// user_version: idempotent and robust for fresh databases.
func migrate(db *sql.DB) error {
	colExists := func(table, name string) (bool, error) {
		var n int
		err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", table, name).Scan(&n)
		return n > 0, err
	}
	if ok, err := colExists("tasks", "parent_id"); err != nil {
		return err
	} else if !ok {
		for _, stmt := range []string{
			"ALTER TABLE tasks ADD COLUMN parent_id INTEGER",
			"ALTER TABLE tasks ADD COLUMN note TEXT NOT NULL DEFAULT ''",
			"ALTER TABLE tasks ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0",
		} {
			if _, err := db.Exec(stmt); err != nil {
				return err
			}
		}
	}
	if ok, err := colExists("tasks", "sort_order"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec("ALTER TABLE tasks ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		// starting order = creation order
		if _, err := db.Exec("UPDATE tasks SET sort_order = id"); err != nil {
			return err
		}
	}
	if ok, err := colExists("tasks", "in_progress"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec("ALTER TABLE tasks ADD COLUMN in_progress INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	// Priority: marker only, no sorting (0 none … 3 high)
	if ok, err := colExists("tasks", "priority"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec("ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 3)"); err != nil {
			return err
		}
	}
	// --- Accounts ---
	for _, c := range []struct{ table, col string }{
		{"credentials", "user_id"}, {"sessions", "user_id"}, {"push_subscriptions", "user_id"}, {"lists", "owner_id"},
	} {
		if ok, err := colExists(c.table, c.col); err != nil {
			return err
		} else if !ok {
			if _, err := db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.col + " INTEGER"); err != nil {
				return err
			}
		}
	}
	if ok, err := colExists("tasks", "list_id"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec("ALTER TABLE tasks ADD COLUMN list_id INTEGER"); err != nil {
			return err
		}
	}
	if err := migrateSingleUser(db); err != nil {
		return err
	}
	if err := migrateStatsPerUser(db); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		// Every user has a default list.
		{"INSERT INTO lists (owner_id, name, is_default, created_at) SELECT u.id, NULL, 1, ? FROM users u " +
			"WHERE NOT EXISTS (SELECT 1 FROM lists WHERE owner_id = u.id AND is_default = 1)", []any{now}},
		// Safety net: tasks without a list (or with a dead one) belong in the
		// admin's default list (the oldest one) — all write paths set list_id
		// themselves.
		{"UPDATE tasks SET list_id = (SELECT id FROM lists WHERE is_default = 1 ORDER BY id LIMIT 1) " +
			"WHERE (list_id IS NULL OR list_id NOT IN (SELECT id FROM lists)) AND EXISTS (SELECT 1 FROM lists WHERE is_default = 1)", nil},
		{"DROP TRIGGER IF EXISTS tasks_default_list", nil},
		{"CREATE TRIGGER tasks_default_list AFTER INSERT ON tasks WHEN NEW.list_id IS NULL " +
			"BEGIN UPDATE tasks SET list_id = (SELECT id FROM lists WHERE is_default = 1 ORDER BY id LIMIT 1) WHERE id = NEW.id; END", nil},
		// Who may see which list — the single place for visibility: owner and
		// members.
		{"DROP VIEW IF EXISTS list_access", nil},
		{"CREATE VIEW list_access AS SELECT id AS list_id, owner_id AS user_id FROM lists " +
			"UNION SELECT list_id, user_id FROM list_members", nil},
		{"DROP INDEX IF EXISTS idx_lists_name_key", nil},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			return err
		}
	}
	if ok, err := colExists("attachments", "thumb"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec("ALTER TABLE attachments ADD COLUMN thumb BLOB"); err != nil {
			return err
		}
	}
	if ok, err := colExists("sessions", "credential_id"); err != nil {
		return err
	} else if !ok {
		// Existing sessions stay valid but can't be attributed to any passkey
		// — deleteSessionsOfCredential removes them along with it.
		if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN credential_id INTEGER"); err != nil {
			return err
		}
	}
	if ok, err := colExists("sessions", "auth_at"); err != nil {
		return err
	} else if !ok {
		// Existing sessions stay valid, but count as not freshly confirmed:
		// managing passkeys there requires a new login first.
		if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN auth_at TEXT"); err != nil {
			return err
		}
	}
	if ok, err := colExists("sessions", "hashed"); err != nil {
		return err
	} else if !ok {
		if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN hashed INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	if ok, err := colExists("tasks", "push_reminded_at"); err != nil {
		return err
	} else if !ok {
		// Push takes over Discord's state: otherwise the first device to turn
		// on push would get every ever-due task reported once more. One
		// transaction, so a crash in the middle doesn't leave the column
		// without the carried-over state (colExists would see it as "done" on
		// the next start).
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, stmt := range []string{
			"ALTER TABLE tasks ADD COLUMN push_reminded_at TEXT",
			"UPDATE tasks SET push_reminded_at = reminded_at",
			"INSERT OR IGNORE INTO meta (key, value) SELECT 'last_push_digest_date', value FROM meta WHERE key = 'last_digest_date'",
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if err := migrateReminderStamps(db); err != nil {
		return err
	}
	if err := migrateSessionHashes(db); err != nil {
		return err
	}
	// Must run after the thumb column: the rebuild copies it along.
	if err := migrateAttachmentsAutoincrement(db); err != nil {
		return err
	}
	// After the attachments rebuild: rebuilding a table discards its indexes.
	// Cascading deletes, hasSubs and the COUNT in the attachments_limit
	// trigger all look these columns up.
	for _, q := range []string{
		"CREATE INDEX IF NOT EXISTS idx_tasks_parent ON tasks(parent_id)",
		"CREATE INDEX IF NOT EXISTS idx_tasks_list ON tasks(list_id)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_lists_owner_name ON lists(owner_id, name_key)",
		"CREATE INDEX IF NOT EXISTS idx_lists_owner ON lists(owner_id)",
		"CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)",
		"CREATE INDEX IF NOT EXISTS idx_credentials_user ON credentials(user_id)",
		"CREATE INDEX IF NOT EXISTS idx_links_b ON links(b)",
		"CREATE INDEX IF NOT EXISTS idx_attachments_task ON attachments(task_id)",
	} {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	// Orphans from delete operations that, before the transaction in
	// deleteTaskCascade existed, could fail midway: otherwise the next task
	// with the same ID would inherit them. Cheap as long as there are none.
	for _, q := range []string{
		"DELETE FROM links WHERE a NOT IN (SELECT id FROM tasks) OR b NOT IN (SELECT id FROM tasks)",
		"DELETE FROM attachments WHERE task_id NOT IN (SELECT id FROM tasks)",
	} {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// migrateSessionHashes replaces plaintext tokens from before hashToken with
// their hash. The sessions stay valid — the browser keeps sending the same
// cookie value. A hash and a token look the same (64 hex chars), hence the
// hashed column instead of a format check.
func migrateSessionHashes(db *sql.DB) error {
	rows, err := db.Query("SELECT token FROM sessions WHERE hashed = 0")
	if err != nil {
		return err
	}
	var plain []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return err
		}
		plain = append(plain, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(plain) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range plain {
		if _, err := tx.Exec("UPDATE sessions SET token = ?, hashed = 1 WHERE token = ? AND hashed = 0", hashToken(t), t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateAttachmentsAutoincrement rebuilds the attachments table once, so
// that deleted IDs never get reassigned (see attachmentsTable). AUTOINCREMENT
// cannot be retrofitted via ALTER TABLE.
func migrateAttachmentsAutoincrement(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'attachments'").Scan(&ddl); err != nil {
		return err
	}
	if strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT") {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// DROP TABLE takes the attachments_limit trigger down with it — recreate it afterwards.
	for _, stmt := range []string{
		`CREATE TABLE attachments_new (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id    INTEGER NOT NULL,
			mime       TEXT NOT NULL,
			created_at TEXT NOT NULL,
			data       BLOB NOT NULL,
			thumb      BLOB
		)`,
		// Explicit IDs when copying pull sqlite_sequence up to max(id), so
		// existing URLs stay valid and are never reassigned.
		"INSERT INTO attachments_new (id, task_id, mime, created_at, data, thumb) " +
			"SELECT id, task_id, mime, created_at, data, thumb FROM attachments",
		"DROP TABLE attachments",
		"ALTER TABLE attachments_new RENAME TO attachments",
		attachmentsTrigger,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("attachments migration (%.40s…): %w", stmt, err)
		}
	}
	return tx.Commit()
}

const taskCols = "id, title, due_date, due_time, recurrence, done, created_at, completed_at, parent_id, note, pinned, sort_order, in_progress, priority, list_id"

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var due, dueTime, completed sql.NullString
	var parent, list sql.NullInt64
	var done, pinned, inProgress int
	err := row.Scan(&t.ID, &t.Title, &due, &dueTime, &t.Recurrence, &done, &t.CreatedAt, &completed, &parent, &t.Note, &pinned, &t.SortOrder, &inProgress, &t.Priority, &list)
	if err != nil {
		return t, err
	}
	t.ListID = list.Int64
	t.Done = done != 0
	t.Pinned = pinned != 0
	t.InProgress = inProgress != 0
	if due.Valid {
		t.DueDate = &due.String
	}
	if dueTime.Valid {
		t.DueTime = &dueTime.String
	}
	if completed.Valid {
		t.CompletedAt = &completed.String
	}
	if parent.Valid {
		t.ParentID = &parent.Int64
	}
	return t, nil
}

// getTask reads a task without a user — only where visibility has already
// been established (after taskFor, or a statement using visibleLists).
func getTask(db *sql.DB, id int64) (Task, error) {
	row := db.QueryRow("SELECT "+taskCols+" FROM tasks WHERE id = ?", id)
	return scanTask(row)
}

// taskFor reads a task uid is allowed to see; otherwise sql.ErrNoRows.
func taskFor(db *sql.DB, uid, id int64) (Task, error) {
	row := db.QueryRow("SELECT "+taskCols+" FROM tasks WHERE id = ? AND list_id "+visibleLists, id, uid)
	return scanTask(row)
}

// category: -2=in progress, -1=pinned, 0=overdue, 1=today, 2=future, 3=no date, 4=done
func category(t Task, today, hhmm string) int {
	if t.Done {
		return 4
	}
	if t.InProgress && t.ParentID == nil {
		return -2
	}
	if t.Pinned && t.ParentID == nil {
		return -1
	}
	if t.DueDate == nil {
		return 3
	}
	switch {
	case *t.DueDate < today:
		return 0
	case *t.DueDate == today:
		if t.DueTime != nil && *t.DueTime < hhmm {
			return 0
		}
		return 1
	default:
		return 2
	}
}

func listTasks(db *sql.DB, uid int64, now time.Time) ([]Task, error) {
	today := now.Format("2006-01-02")
	hhmm := now.Format("15:04")
	rows, err := db.Query("SELECT "+taskCols+" FROM tasks WHERE list_id "+visibleLists, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		t.Overdue = isOverdue(t, today, hhmm)
		t.Links = []int64{}
		t.Attachments = []int64{}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := attachLinks(db, tasks); err != nil {
		return nil, err
	}
	if err := attachAttachments(db, tasks); err != nil {
		return nil, err
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		a, b := tasks[i], tasks[j]
		ca, cb := category(a, today, hhmm), category(b, today, hhmm)
		if ca != cb {
			return ca < cb
		}
		if ca == 4 { // done: most recently completed first
			as, bs := "", ""
			if a.CompletedAt != nil {
				as = *a.CompletedAt
			}
			if b.CompletedAt != nil {
				bs = *b.CompletedAt
			}
			if as != bs {
				return as > bs
			}
			return a.ID > b.ID
		}
		// Within a section, manual ordering applies (drag & drop)
		if a.SortOrder != b.SortOrder {
			return a.SortOrder < b.SortOrder
		}
		return a.ID < b.ID
	})
	return tasks, nil
}

// errParentInvalid: the parent task was gone by the time of the write, or
// has itself become a subtask in the meantime — the check before the write
// still saw the old state.
var errParentInvalid = errors.New("parent missing or not top-level")

func createTask(db *sql.DB, uid int64, title string, dueDate, dueTime *string, recurrence, note string, pinned bool, priority int, listID, parentID *int64) (Task, error) {
	created := time.Now().UTC().Format(time.RFC3339)
	// One statement instead of check-then-write: the one-level rule and the
	// list are enforced within the INSERT itself, otherwise a second device
	// could reparent or delete the parent task or the list in between.
	// Subtasks live in their parent task's list (a given one then doesn't
	// count), otherwise the given one, or the default list if none is given.
	// New tasks land at the end of their list — subtasks at the end of their
	// siblings.
	res, err := db.Exec(
		"INSERT INTO tasks (title, due_date, due_time, recurrence, created_at, note, pinned, priority, list_id, parent_id, sort_order) "+
			"SELECT ?, ?, ?, ?, ?, ?, ?, ?, "+
			"CASE WHEN ? IS NOT NULL THEN (SELECT list_id FROM tasks WHERE id = ?) WHEN ? IS NOT NULL THEN ? "+
			"ELSE (SELECT id FROM lists WHERE owner_id = ? AND is_default = 1) END, "+
			"?, (SELECT COALESCE(MAX(sort_order), 0) + 10 FROM tasks WHERE parent_id IS ?) "+
			// Parent task and list must be visible to uid — within the INSERT itself.
			"WHERE (? IS NULL OR EXISTS (SELECT 1 FROM tasks WHERE id = ? AND parent_id IS NULL AND list_id "+visibleLists+")) "+
			"AND (? IS NOT NULL OR ? IS NULL OR ? "+visibleLists+")",
		title, dueDate, dueTime, recurrence, created, note, boolInt(pinned), priority,
		parentID, parentID, listID, listID, uid,
		parentID, parentID,
		parentID, parentID, uid,
		parentID, listID, listID, uid)
	if err != nil {
		return Task{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return Task{}, err
	} else if n == 0 {
		if parentID == nil {
			return Task{}, errListInvalid
		}
		return Task{}, errParentInvalid
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Task{}, err
	}
	return getTask(db, id)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func hasSubs(db *sql.DB, id int64) (bool, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM tasks WHERE parent_id = ?", id).Scan(&n)
	return n > 0, err
}

// isOverdue is independent of category (pinned tasks can also be overdue).
func isOverdue(t Task, today, hhmm string) bool {
	if t.Done || t.DueDate == nil {
		return false
	}
	if *t.DueDate < today {
		return true
	}
	return *t.DueDate == today && t.DueTime != nil && *t.DueTime < hhmm
}

// Links (peer relationships, normalized a < b)

func attachLinks(db *sql.DB, tasks []Task) error {
	rows, err := db.Query("SELECT a, b FROM links")
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[int64]*Task{}
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return err
		}
		// Only links where both ends are visible — otherwise the ID of someone
		// else's task would show up in the task JSON.
		ta, okA := byID[a]
		tb, okB := byID[b]
		if okA && okB {
			ta.Links = append(ta.Links, b)
			tb.Links = append(tb.Links, a)
		}
	}
	return rows.Err()
}

func attachAttachments(db *sql.DB, tasks []Task) error {
	rows, err := db.Query("SELECT id, task_id FROM attachments ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := map[int64]*Task{}
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}
	for rows.Next() {
		var id, taskID int64
		if err := rows.Scan(&id, &taskID); err != nil {
			return err
		}
		if t, ok := byID[taskID]; ok {
			t.Attachments = append(t.Attachments, id)
		}
	}
	return rows.Err()
}

func normalizePair(a, b int64) (int64, int64) {
	if a > b {
		return b, a
	}
	return a, b
}

func linkTasks(db *sql.DB, uid, a, b int64) error {
	a, b = normalizePair(a, b)
	// Only if both tasks still exist and are visible to uid at write time: a
	// link to a task deleted concurrently would otherwise be inherited by the
	// next new task — tasks.id gets reused.
	_, err := db.Exec("INSERT OR IGNORE INTO links (a, b) SELECT ?, ? WHERE (SELECT COUNT(*) FROM tasks WHERE id IN (?, ?) AND list_id "+visibleLists+") = 2",
		a, b, a, b, uid)
	return err
}

func unlinkTasks(db *sql.DB, uid, a, b int64) error {
	a, b = normalizePair(a, b)
	// Both ends visible, same as linking: a link to a task uid can't see
	// belongs to someone else (shared lists).
	_, err := db.Exec("DELETE FROM links WHERE a = ? AND b = ? AND (SELECT COUNT(*) FROM tasks WHERE id IN (?, ?) AND list_id "+visibleLists+") = 2",
		a, b, a, b, uid)
	return err
}

// Stats (daily counter of completed tasks — survives the auto-archive)

// bumpStat counts completions on day date for uid (the acting user).
func bumpStat(db execer, uid int64, date string, delta int) error {
	_, err := db.Exec(
		"INSERT INTO stats (user_id, date, done) VALUES (?, ?, max(0, ?)) ON CONFLICT(user_id, date) DO UPDATE SET done = max(0, done + ?)",
		uid, date, delta, delta)
	return err
}

// Sessions

// createSession records which passkey opened the session — deleting that
// passkey removes exactly its sessions. credentialID 0 means "unknown"
// (should only occur with legacy data).
// auth_at is the moment of the passkey login — unlike expires_at it does not
// move forward on use (see requireFreshAuth).
func createSession(db *sql.DB, token string, expires time.Time, credentialID, userID int64) error {
	var cred any
	if credentialID > 0 {
		cred = credentialID
	}
	_, err := db.Exec("INSERT INTO sessions (token, expires_at, credential_id, auth_at, hashed, user_id) VALUES (?, ?, ?, ?, 1, ?)",
		hashToken(token), expires.UTC().Format(time.RFC3339), cred, time.Now().UTC().Format(time.RFC3339), userID)
	return err
}

// hashToken: only the SHA-256 of the session token is stored in the DB.
// Anyone reading the DB or a backup of it cannot use it to log in. No salt is
// needed — the token itself already has 256 bits of randomness.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// sessionAuthAt returns when the session last authenticated with a passkey.
func sessionAuthAt(db *sql.DB, token string) (time.Time, bool) {
	if token == "" {
		return time.Time{}, false
	}
	var at sql.NullString
	if err := db.QueryRow("SELECT auth_at FROM sessions WHERE token = ?", hashToken(token)).Scan(&at); err != nil || !at.Valid {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, at.String)
	return t, err == nil
}

// sessionExpiry returns the expiration time of a valid session.
// sessionUser: the session's user and expiration. A session without a user
// (or with a deleted one) is not a login — never fall back to a default
// user.
func sessionUser(db *sql.DB, token string) (uid int64, expires time.Time, ok bool) {
	if token == "" {
		return 0, time.Time{}, false
	}
	var exp string
	if err := db.QueryRow("SELECT expires_at, user_id FROM sessions WHERE token = ? "+
		"AND user_id IN (SELECT id FROM users)", hashToken(token)).Scan(&exp, &uid); err != nil {
		return 0, time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil || !t.After(time.Now().UTC()) {
		return 0, time.Time{}, false
	}
	return uid, t, true
}

func sessionValid(db *sql.DB, token string) bool {
	_, _, ok := sessionUser(db, token)
	return ok
}

func renewSession(db *sql.DB, token string, expires time.Time) error {
	_, err := db.Exec("UPDATE sessions SET expires_at = ? WHERE token = ?",
		expires.UTC().Format(time.RFC3339), hashToken(token))
	return err
}

func deleteSession(db *sql.DB, token string) {
	db.Exec("DELETE FROM sessions WHERE token = ?", hashToken(token))
}

// retireSession lets a replaced session expire after grace (only shortens
// it, never extends it). renewedAuth no longer renews it that close to
// expiry.
func retireSession(db *sql.DB, token string, grace time.Duration) {
	until := time.Now().Add(grace).UTC().Format(time.RFC3339)
	db.Exec("UPDATE sessions SET expires_at = ? WHERE token = ? AND expires_at > ?", until, hashToken(token), until)
}

// deleteSessionsOfCredential ends the sessions of a deleted passkey.
// Unattributable legacy sessions (credential_id NULL) go with them — when in
// doubt, they could be from this exact device. The session currently active
// stays: nobody should get locked out of their own account.
func deleteSessionsOfCredential(db *sql.DB, uid, credentialID int64, keepToken string) {
	db.Exec("DELETE FROM sessions WHERE user_id = ? AND (credential_id = ? OR credential_id IS NULL) AND token != ?",
		uid, credentialID, hashToken(keepToken))
}

// sessionCredential: which passkey session token was logged in with
// (0 = unknown, e.g. for sessions predating the attribution).
func sessionCredential(db *sql.DB, token string) int64 {
	var id sql.NullInt64
	db.QueryRow("SELECT credential_id FROM sessions WHERE token = ?", hashToken(token)).Scan(&id)
	return id.Int64
}

func purgeSessions(db *sql.DB) {
	now := time.Now().UTC()
	db.Exec("DELETE FROM sessions WHERE expires_at < ?", now.Format(time.RFC3339))
	// Cap sessions left over from the fixed 180-day lifetime to the idle
	// timeout. A no-op after the first run.
	limit := now.Add(sessionIdleTimeout).Format(time.RFC3339)
	db.Exec("UPDATE sessions SET expires_at = ? WHERE expires_at > ?", limit, limit)
}

// Credentials (Passkeys)

type storedCredential struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Data      string `json:"-"`
	UserID    int64  `json:"-"`
}

// listCredentials: a user's passkeys (uid 0: all of the instance — only for
// the start of login, where the user is only known once the response comes
// back).
func listCredentials(db *sql.DB, uid int64) ([]storedCredential, error) {
	rows, err := db.Query("SELECT id, name, created_at, data, COALESCE(user_id, 0) FROM credentials "+
		"WHERE ? = 0 OR user_id = ? ORDER BY id", uid, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var creds []storedCredential
	for rows.Next() {
		var c storedCredential
		if err := rows.Scan(&c.ID, &c.Name, &c.CreatedAt, &c.Data, &c.UserID); err != nil {
			return nil, err
		}
		creds = append(creds, c)
	}
	return creds, rows.Err()
}

func countCredentials(db *sql.DB, uid int64) (int, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM credentials WHERE user_id = ?", uid).Scan(&n)
	return n, err
}

func insertCredential(db execer, uid int64, name, data string) (int64, error) {
	res, err := db.Exec("INSERT INTO credentials (name, created_at, data, user_id) VALUES (?, ?, ?, ?)",
		name, time.Now().UTC().Format(time.RFC3339), data, uid)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Meta

func getMeta(db *sql.DB, key string) string {
	var v string
	db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	return v
}

// execer: *sql.DB or *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func setMeta(db execer, key, value string) error {
	_, err := db.Exec(
		"INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value)
	return err
}

// migrateSingleUser: an instance from before accounts existed (passkeys but
// no accounts) gets an admin with the old fixed user handle — its passkeys
// carry it over — and everything belongs to it. One transaction: a crash
// midway leaves no half-attributed account behind.
func migrateSingleUser(db *sql.DB) error {
	var users, creds int
	if err := db.QueryRow("SELECT (SELECT COUNT(*) FROM users), (SELECT COUNT(*) FROM credentials)").Scan(&users, &creds); err != nil {
		return err
	}
	if users > 0 || creds == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	uid, err := createUser(tx, "Admin", true, []byte("todo-single-user"))
	if err != nil {
		return err
	}
	for _, q := range []string{
		"UPDATE credentials SET user_id = ? WHERE user_id IS NULL",
		"UPDATE sessions SET user_id = ? WHERE user_id IS NULL",
		"UPDATE push_subscriptions SET user_id = ? WHERE user_id IS NULL",
		"UPDATE lists SET owner_id = ? WHERE owner_id IS NULL",
	} {
		if _, err := tx.Exec(q, uid); err != nil {
			return err
		}
	}
	// Settings from meta → to the admin (archive_days stays instance-wide)
	for metaKey, key := range map[string]string{
		"set_webhook_url": "webhook_url", "set_tz": "tz", "set_digest_time": "digest_time",
		"set_language": "language", "last_lang": "last_lang",
		"last_digest_date": "last_digest_date", "last_push_digest_date": "last_push_digest_date",
	} {
		if _, err := tx.Exec("INSERT OR IGNORE INTO user_settings (user_id, key, value) SELECT ?, ?, value FROM meta WHERE key = ?",
			uid, key, metaKey); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateStatsPerUser converts stats(date, done) to stats(user_id, date,
// done); the existing counters belong to the admin (without accounts there
// are no counters).
func migrateStatsPerUser(db *sql.DB) error {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('stats') WHERE name = 'user_id'").Scan(&n); err != nil || n > 0 {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"CREATE TABLE stats_new (user_id INTEGER NOT NULL, date TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (user_id, date))",
		"INSERT INTO stats_new (user_id, date, done) SELECT (SELECT id FROM users WHERE is_admin = 1 ORDER BY id LIMIT 1), date, done " +
			"FROM stats WHERE EXISTS (SELECT 1 FROM users WHERE is_admin = 1)",
		"DROP TABLE stats",
		"ALTER TABLE stats_new RENAME TO stats",
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateReminderStamps: stamps from the task columns reminded_at /
// push_reminded_at become rows for the owner in task_reminders (with the
// current due date); afterwards the columns are empty and unused. One
// transaction — a second run finds nothing left to do.
func migrateReminderStamps(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"INSERT OR IGNORE INTO task_reminders (task_id, user_id, channel, due, sent_at) " +
			"SELECT t.id, l.owner_id, 'discord', t.due_date || ' ' || t.due_time, t.reminded_at FROM tasks t JOIN lists l ON l.id = t.list_id " +
			"WHERE t.reminded_at IS NOT NULL AND t.due_date IS NOT NULL AND t.due_time IS NOT NULL AND l.owner_id IS NOT NULL",
		"INSERT OR IGNORE INTO task_reminders (task_id, user_id, channel, due, sent_at) " +
			"SELECT t.id, l.owner_id, 'push', t.due_date || ' ' || t.due_time, t.push_reminded_at FROM tasks t JOIN lists l ON l.id = t.list_id " +
			"WHERE t.push_reminded_at IS NOT NULL AND t.due_date IS NOT NULL AND t.due_time IS NOT NULL AND l.owner_id IS NOT NULL",
		"UPDATE tasks SET reminded_at = NULL, push_reminded_at = NULL WHERE reminded_at IS NOT NULL OR push_reminded_at IS NOT NULL",
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}
