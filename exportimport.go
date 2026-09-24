package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// exportTask contains all columns including internal state (sort_order; the
// exporting user's reminder timestamps as reminded_at/
// push_reminded_at), so a restore is truly lossless.
type exportTask struct {
	ID             int64   `json:"id"`
	Title          string  `json:"title"`
	DueDate        *string `json:"due_date"`
	DueTime        *string `json:"due_time"`
	Recurrence     string  `json:"recurrence"`
	Done           bool    `json:"done"`
	CreatedAt      string  `json:"created_at"`
	CompletedAt    *string `json:"completed_at"`
	RemindedAt     *string `json:"reminded_at"`
	PushRemindedAt *string `json:"push_reminded_at"`
	ParentID       *int64  `json:"parent_id"`
	Note           string  `json:"note"`
	Pinned         bool    `json:"pinned"`
	InProgress     bool    `json:"in_progress"`
	Priority       int     `json:"priority"`
	ListID         int64   `json:"list_id"`
	SortOrder      int64   `json:"sort_order"`
}

type exportList struct {
	ID        int64   `json:"id"`
	Name      *string `json:"name"`
	IsDefault bool    `json:"is_default"`
}

// exportAttachment: []byte is automatically base64-encoded by encoding/json
type exportAttachment struct {
	ID        int64  `json:"id"`
	TaskID    int64  `json:"task_id"`
	Mime      string `json:"mime"`
	CreatedAt string `json:"created_at"`
	Data      []byte `json:"data"`
}

type exportData struct {
	ExportedAt  string             `json:"exported_at"`
	Lists       []exportList       `json:"lists"`
	Tasks       []exportTask       `json:"tasks"`
	Links       [][2]int64         `json:"links"`
	Stats       map[string]int     `json:"stats"`
	Attachments []exportAttachment `json:"attachments"`
	Settings    appSettings        `json:"settings"`
}

func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	// Only own data: own lists (ownedLists — shared lists belong to their
	// owner), their tasks, links between them, images, stats.
	uid := userID(r)
	data := exportData{
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Tasks:      []exportTask{},
		Links:      [][2]int64{},
		Stats:      map[string]int{},
		Settings:   s.store.get(userID(r)),
	}
	lists, err := listLists(s.db, userID(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	for _, l := range lists {
		data.Lists = append(data.Lists, exportList{ID: l.ID, Name: l.Name, IsDefault: l.IsDefault})
	}
	// Reminder timestamps: the exporting user's own, only for the current
	// due date (older ones are meaningless).
	stamp := func(ch string) string {
		return "(SELECT sent_at FROM task_reminders r WHERE r.task_id = tasks.id AND r.user_id = ? AND r.channel = '" + ch +
			"' AND r.due = tasks.due_date || ' ' || tasks.due_time)"
	}
	rows, err := s.db.Query("SELECT id, title, due_date, due_time, recurrence, done, created_at, completed_at, "+stamp(chDiscord)+", "+stamp(chPush)+
		", parent_id, note, pinned, in_progress, sort_order, priority, COALESCE(list_id, 0) FROM tasks WHERE list_id "+ownedLists, uid, uid, uid)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	for rows.Next() {
		var t exportTask
		var done, pinned, inProgress int
		if err := rows.Scan(&t.ID, &t.Title, &t.DueDate, &t.DueTime, &t.Recurrence, &done, &t.CreatedAt, &t.CompletedAt, &t.RemindedAt, &t.PushRemindedAt, &t.ParentID, &t.Note, &pinned, &inProgress, &t.SortOrder, &t.Priority, &t.ListID); err != nil {
			rows.Close()
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		t.Done = done != 0
		t.Pinned = pinned != 0
		t.InProgress = inProgress != 0
		data.Tasks = append(data.Tasks, t)
	}
	// A backup must be complete — abort hard on read errors
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	rows.Close()
	own := "SELECT id FROM tasks WHERE list_id " + ownedLists
	lrows, err := s.db.Query("SELECT a, b FROM links WHERE a IN ("+own+") AND b IN ("+own+")", uid, uid)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	for lrows.Next() {
		var a, b int64
		if err := lrows.Scan(&a, &b); err != nil {
			lrows.Close()
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		data.Links = append(data.Links, [2]int64{a, b})
	}
	if err := lrows.Err(); err != nil {
		lrows.Close()
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	lrows.Close()
	srows, err := s.db.Query("SELECT date, done FROM stats WHERE user_id = ?", userID(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	for srows.Next() {
		var d string
		var n int
		if err := srows.Scan(&d, &n); err != nil {
			srows.Close()
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		data.Stats[d] = n
	}
	if err := srows.Err(); err != nil {
		srows.Close()
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	srows.Close()
	// Images make up nearly the entire size. They're streamed out one at a time, each
	// with its own short query: an open cursor would hold the single
	// DB connection for the entire duration of the download, and a
	// fully assembled backup would sit entirely (plus base64) in memory.
	var attIDs []int64
	arows, err := s.db.Query("SELECT id FROM attachments WHERE task_id IN ("+own+") ORDER BY id", uid)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	for arows.Next() {
		var id int64
		if err := arows.Scan(&id); err != nil {
			arows.Close()
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		attIDs = append(attIDs, id)
	}
	if err := arows.Err(); err != nil {
		arows.Close()
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	arows.Close()

	head, err := json.Marshal(struct {
		ExportedAt string         `json:"exported_at"`
		Lists      []exportList   `json:"lists"`
		Tasks      []exportTask   `json:"tasks"`
		Links      [][2]int64     `json:"links"`
		Stats      map[string]int `json:"stats"`
	}{data.ExportedAt, data.Lists, data.Tasks, data.Links, data.Stats})
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	settings, err := json.Marshal(data.Settings)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"todo-backup-%s.json\"", s.nowFor(userID(r)).Format(dateFmt)))
	bw := bufio.NewWriterSize(w, 64<<10)
	// Same field order as exportData: head without the closing brace.
	// A write error means: client gone — then stop silently.
	if _, err := bw.Write(append(head[:len(head)-1], `,"attachments":[`...)); err != nil {
		return
	}
	first := true
	for _, id := range attIDs {
		var a exportAttachment
		err := s.db.QueryRow("SELECT id, task_id, mime, created_at, data FROM attachments WHERE id = ? AND task_id IN ("+own+")", id, uid).
			Scan(&a.ID, &a.TaskID, &a.Mime, &a.CreatedAt, &a.Data)
		if errors.Is(err, sql.ErrNoRows) {
			continue // deleted during the export
		}
		if err != nil {
			// The response is already underway: abort the connection so the browser
			// shows an error instead of saving an incomplete backup.
			panic(http.ErrAbortHandler)
		}
		b, err := json.Marshal(a)
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		if !first {
			if err := bw.WriteByte(','); err != nil {
				return
			}
		}
		first = false
		if _, err := bw.Write(b); err != nil {
			return
		}
	}
	tail := append(append([]byte(`],"settings":`), settings...), "}\n"...)
	if _, err := bw.Write(tail); err != nil {
		return
	}
	_ = bw.Flush() // last write attempt; client gone is not a server error
}

// handleImport replaces the entire data set with the backup
// (tasks, links, stats; settings only if they're valid).
// Upper bounds for numbers from a backup. Real-world values are orders of magnitude
// below these; anything higher would break the app later: MAX(sort_order)+10
// would overflow int64, and IDs beyond 2^53 can't be represented exactly by
// JavaScript — such a task would no longer be addressable in the browser.
const (
	maxImportID        = 1<<53 - 1
	maxImportSortOrder = 1 << 40
	maxImportStatDay   = 1_000_000
)

// Upper bound for a backup. The import only needs memory for one
// image at a time (readImport) — the limit protects disk space, not RAM.
// Must match client_max_body_size in deploy/nginx.
const maxImportBytes = 2 << 30

func (s *server) handleImport(w http.ResponseWriter, r *http.Request) {
	if !s.importMu.TryLock() {
		writeError(w, r, http.StatusConflict, "error.import_running")
		return
	}
	defer s.importMu.Unlock()
	extendReadDeadline(w, time.Hour) // up to 2 GB, even over a slow connection
	if _, err := s.db.Exec("DELETE FROM import_attachments"); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer s.db.Exec("DELETE FROM import_attachments")
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	data, err := s.readImport(r.Body)
	if err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			writeError(w, r, http.StatusRequestEntityTooLarge, "error.backup_too_big")
			return
		}
		if _, dbErr := errors.AsType[stageError](err); dbErr {
			writeError(w, r, http.StatusInternalServerError, "error.import_failed")
			return
		}
		if tooMany, ok := errors.AsType[tooManyError](err); ok {
			writeError(w, r, http.StatusBadRequest, "error.backup_too_many", "what", tooMany.what)
			return
		}
		writeError(w, r, http.StatusBadRequest, "error.backup_invalid")
		return
	}
	// A backup is a foreign file, not trusted state: everything
	// the API checks when creating a task must be checked here just the same. Otherwise
	// values could end up in the DB that no handler could ever produce — e.g. an
	// unknown recurrence that makes checking off the task permanently fail with 500.
	lists, knownLists, defaultList, ok := importLists(data.Lists)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "error.backup.lists")
		return
	}
	ids := map[int64]bool{}
	byID := map[int64]exportTask{}
	for i := range data.Tasks {
		t := &data.Tasks[i]
		if t.ID <= 0 || t.ID > maxImportID || strings.TrimSpace(t.Title) == "" {
			writeError(w, r, http.StatusBadRequest, "error.backup.no_id")
			return
		}
		if t.SortOrder < -maxImportSortOrder || t.SortOrder > maxImportSortOrder {
			writeError(w, r, http.StatusBadRequest, "error.backup.order")
			return
		}
		if ids[t.ID] {
			writeError(w, r, http.StatusBadRequest, "error.backup.dup_id")
			return
		}
		if utf8.RuneCountInString(t.Title) > maxTitleLen || utf8.RuneCountInString(t.Note) > maxNoteLen {
			writeError(w, r, http.StatusBadRequest, "error.backup.too_long")
			return
		}
		switch t.Recurrence {
		case "", "daily", "weekly", "monthly":
		default:
			writeError(w, r, http.StatusBadRequest, "error.backup.recurrence")
			return
		}
		if t.Priority < 0 || t.Priority > 3 {
			writeError(w, r, http.StatusBadRequest, "error.backup.priority")
			return
		}
		// Due dates are lexically comparable wall-clock time strings —
		// a different format breaks sorting, the digest, and reminders.
		if t.DueDate != nil {
			if _, err := time.Parse(dateFmt, *t.DueDate); err != nil {
				writeError(w, r, http.StatusBadRequest, "error.backup.date")
				return
			}
		}
		if t.DueTime != nil {
			parsed, err := time.Parse("15:04", *t.DueTime)
			if err != nil {
				writeError(w, r, http.StatusBadRequest, "error.backup.time")
				return
			}
			// normalized like the API: Go also accepts "9:30", which would break
			// the lexical wall-clock comparisons (overdue, pings)
			norm := parsed.Format("15:04")
			t.DueTime = &norm
		}
		if t.DueTime != nil && t.DueDate == nil {
			writeError(w, r, http.StatusBadRequest, "error.backup.time_no_date")
			return
		}
		// Straighten out combinations the API would never produce, instead of
		// rejecting them — otherwise e.g. a recurring subtask would behave
		// completely differently from any other when checked off.
		if t.DueDate == nil {
			t.Recurrence = ""
		}
		if t.ParentID != nil {
			t.Recurrence = ""
			t.Pinned = false
			t.InProgress = false
		}
		switch {
		case len(data.Lists) == 0 || t.ListID == 0:
			t.ListID = defaultList
		case !knownLists[t.ListID]:
			writeError(w, r, http.StatusBadRequest, "error.backup.lists")
			return
		}
		ids[t.ID] = true
		byID[t.ID] = *t
	}
	for i := range data.Tasks {
		t := &data.Tasks[i]
		if t.ParentID == nil {
			continue
		}
		parent, ok := byID[*t.ParentID]
		if !ok || *t.ParentID == t.ID {
			writeError(w, r, http.StatusBadRequest, "error.backup.orphan")
			return
		}
		// Subtasks only go one level deep — the UI can neither display nor
		// resolve deeper nesting.
		if parent.ParentID != nil {
			writeError(w, r, http.StatusBadRequest, "error.backup.nested")
			return
		}
		t.ListID = parent.ListID // as in the API: subtasks follow their parent task
	}
	// Members of the lists the import replaces: after the write they're
	// no longer in list_access, but still need to reload.
	formerMembers := userIDs(s.db, "SELECT user_id FROM list_members WHERE list_id IN (SELECT id FROM lists WHERE owner_id = ? AND is_default = 0)", userID(r))
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	// Replaces only the own data and assigns new IDs: IDs are instance-wide,
	// the ones from the backup would collide with other users'.
	uid := userID(r)
	restore := func() error {
		own := "SELECT id FROM tasks WHERE list_id " + ownedLists
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{"DELETE FROM links WHERE a IN (" + own + ") OR b IN (" + own + ")", []any{uid, uid}},
			{"DELETE FROM attachments WHERE task_id IN (" + own + ")", []any{uid}},
			{"DELETE FROM tasks WHERE list_id " + ownedLists, []any{uid}},
			{"DELETE FROM list_members WHERE list_id IN (SELECT id FROM lists WHERE owner_id = ? AND is_default = 0)", []any{uid}},
			{"DELETE FROM lists WHERE owner_id = ? AND is_default = 0", []any{uid}},
			{"DELETE FROM stats WHERE user_id = ?", []any{uid}},
			{"CREATE TEMP TABLE IF NOT EXISTS import_task_map (old INTEGER PRIMARY KEY, new INTEGER NOT NULL)", nil},
			{"DELETE FROM import_task_map", nil},
		} {
			if _, err := tx.Exec(q.sql, q.args...); err != nil {
				return err
			}
		}
		// Lists: the backup's default list becomes the own default list
		// (same ID, name from the backup), the rest get new IDs.
		var ownDefault int64
		if err := tx.QueryRow("SELECT id FROM lists WHERE owner_id = ? AND is_default = 1", uid).Scan(&ownDefault); err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		listMap := map[int64]int64{}
		for _, l := range lists {
			var key *string
			if l.Name != nil {
				k := listKey(*l.Name)
				key = &k
			}
			if l.IsDefault {
				if _, err := tx.Exec("UPDATE lists SET name = ?, name_key = ? WHERE id = ?", l.Name, key, ownDefault); err != nil {
					return err
				}
				listMap[l.ID] = ownDefault
				continue
			}
			res, err := tx.Exec("INSERT INTO lists (owner_id, name, name_key, is_default, created_at) VALUES (?, ?, ?, 0, ?)", uid, l.Name, key, now)
			if err != nil {
				return err
			}
			if listMap[l.ID], err = res.LastInsertId(); err != nil {
				return err
			}
		}
		// Tasks: top-level ones first, then the subtasks (their parent ID
		// is already in the map by then).
		taskMap := map[int64]int64{}
		insert := func(t exportTask) error {
			// Backups from before web push don't know this field: fall back to
			// the Discord state, otherwise push would re-report everything old.
			pushed := t.PushRemindedAt
			if pushed == nil {
				pushed = t.RemindedAt
			}
			var parent any
			if t.ParentID != nil {
				parent = taskMap[*t.ParentID]
			}
			res, err := tx.Exec(
				"INSERT INTO tasks (title, due_date, due_time, recurrence, done, created_at, completed_at, parent_id, note, pinned, in_progress, sort_order, priority, list_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				t.Title, t.DueDate, t.DueTime, t.Recurrence, boolInt(t.Done), t.CreatedAt, t.CompletedAt, parent, t.Note, boolInt(t.Pinned), boolInt(t.InProgress), t.SortOrder, t.Priority, listMap[t.ListID])
			if err != nil {
				return err
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			// Timestamps become rows for the importing user, for the due date
			// from the backup.
			if t.DueDate != nil && t.DueTime != nil {
				for _, st := range []struct {
					ch string
					at *string
				}{{chDiscord, t.RemindedAt}, {chPush, pushed}} {
					if st.at == nil {
						continue
					}
					if _, err := tx.Exec("INSERT INTO task_reminders (task_id, user_id, channel, due, sent_at) VALUES (?, ?, ?, ?, ?)",
						id, uid, st.ch, *t.DueDate+" "+*t.DueTime, *st.at); err != nil {
						return err
					}
				}
			}
			taskMap[t.ID] = id
			_, err = tx.Exec("INSERT INTO import_task_map (old, new) VALUES (?, ?)", t.ID, id)
			return err
		}
		for _, sub := range []bool{false, true} {
			for _, t := range data.Tasks {
				if (t.ParentID != nil) == sub {
					if err := insert(t); err != nil {
						return err
					}
				}
			}
		}
		// As in the API: an open subtask has an open parent task
		// (reopenParentOf) — otherwise it would sit invisibly in the collapsed "Done" section.
		if _, err := tx.Exec("UPDATE tasks SET done = 0, completed_at = NULL WHERE done = 1 AND list_id "+ownedLists+" AND id IN "+
			"(SELECT parent_id FROM tasks WHERE done = 0 AND parent_id IS NOT NULL)", uid); err != nil {
			return err
		}
		for _, l := range data.Links {
			if !ids[l[0]] || !ids[l[1]] || l[0] == l[1] {
				continue
			}
			a, b := normalizePair(taskMap[l[0]], taskMap[l[1]])
			if _, err := tx.Exec("INSERT OR IGNORE INTO links (a, b) VALUES (?, ?)", a, b); err != nil {
				return err
			}
		}
		for d, n := range data.Stats {
			if _, err := time.Parse(dateFmt, d); err != nil || n < 0 || n > maxImportStatDay {
				continue // broken day counter: better to skip it than to distort the stats
			}
			if _, err := tx.Exec("INSERT INTO stats (user_id, date, done) VALUES (?, ?, ?)", uid, d, n); err != nil {
				return err
			}
		}
		// Images from the staging area (validated in stageAttachments), moved
		// to the new task IDs. Only for tasks from the backup and at most
		// maxAttachmentsPerTask per task in backup order — the
		// attachments_limit trigger would otherwise break the entire import with 500
		// instead of just dropping the overflow. Deliberately without the ID from the
		// backup: a new ID never collides with a URL the browser still associates
		// with a different image. The window function only looks at seq/task_id:
		// with the image data included, SQLite would sort all images in memory.
		_, err := tx.Exec(`INSERT INTO attachments (task_id, mime, created_at, data)
			SELECT m.new, i.mime, i.created_at, i.data FROM import_attachments i
			JOIN import_task_map m ON m.old = i.task_id
			WHERE i.seq IN (
				SELECT seq FROM (
					SELECT seq, ROW_NUMBER() OVER (PARTITION BY task_id ORDER BY seq) AS n
					FROM import_attachments WHERE task_id IN (SELECT old FROM import_task_map)
				) WHERE n <= ?
			) ORDER BY i.seq`, maxAttachmentsPerTask)
		return err
	}
	if err := restore(); err != nil {
		tx.Rollback()
		writeError(w, r, http.StatusInternalServerError, "error.import_failed")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.import_failed")
		return
	}
	// Only adopt settings if they pass validation. A
	// webhook URL that's already set stays: it determines where task titles go, and
	// a file shouldn't be able to silently change that. Only a fresh instance
	// without a webhook gets the one from the backup.
	if cur := s.store.get(userID(r)).WebhookURL; cur != "" {
		data.Settings.WebhookURL = cur
	}
	if validateSettings(data.Settings) == nil {
		err := s.store.save(userID(r), data.Settings)
		// Old backups don't have a language: then the configured one stays.
		if err == nil && data.Settings.Language != "" {
			err = s.store.saveLanguage(userID(r), data.Settings.Language)
		}
		if err != nil {
			s.changed(userID(r), formerMembers...)
			writeError(w, r, http.StatusInternalServerError, "error.import_settings")
			return
		}
	}
	s.changed(userID(r), formerMembers...)
	w.WriteHeader(http.StatusNoContent)
}

// stageError: a DB error while staging, not a broken backup.
type stageError struct{ err error }

func (e stageError) Error() string { return e.err.Error() }
func (e stageError) Unwrap() error { return e.err }

// readImport reads a backup without loading it entirely into memory: everything
// except the images is small and gets decoded normally, the images go
// into the staging area one at a time. Field names are matched case-insensitively,
// as encoding/json does; unknown fields are skipped.
func (s *server) readImport(body io.Reader) (exportData, error) {
	var data exportData
	dec := json.NewDecoder(body)
	if tok, err := dec.Token(); err != nil {
		return data, err
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return data, errors.New("not a JSON object")
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return data, err
		}
		key, _ := tok.(string)
		switch strings.ToLower(key) {
		case "attachments":
			err = s.stageAttachments(dec)
		case "exported_at":
			err = dec.Decode(&data.ExportedAt)
		case "lists":
			data.Lists, err = decodeList[exportList](dec, maxLists, "lists")
		case "tasks":
			data.Tasks, err = decodeList[exportTask](dec, maxImportTasks, "tasks")
		case "links":
			data.Links, err = decodeList[[2]int64](dec, maxImportLinks, "links")
		case "stats":
			data.Stats, err = decodeStats(dec)
		case "settings":
			err = dec.Decode(&data.Settings)
		default:
			var skip json.RawMessage
			err = dec.Decode(&skip)
		}
		if err != nil {
			return data, err
		}
	}
	_, err := dec.Token() // '}'
	return data, err
}

func (s *server) stageAttachments(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil // "attachments": null
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errors.New("attachments is not a list")
	}
	for dec.More() {
		var a exportAttachment
		if err := dec.Decode(&a); err != nil {
			return err
		}
		// The mime type ends up as the Content-Type when the browser fetches it —
		// unchecked, a "text/html" attachment would be stored XSS on the app's own
		// origin. Anything outside the image whitelist gets rejected, as does anything
		// larger than an upload is ever allowed to be.
		if len(a.Data) == 0 || len(a.Data) > maxAttachmentSize || !allowedImageMimes[a.Mime] {
			continue
		}
		if _, err := s.db.Exec("INSERT INTO import_attachments (task_id, mime, created_at, data) VALUES (?, ?, ?, ?)",
			a.TaskID, a.Mime, a.CreatedAt, a.Data); err != nil {
			return stageError{err}
		}
	}
	_, err = dec.Token() // ']'
	return err
}

// Upper bounds for the number of entries, checked element by element: a
// huge list aborts this way before it's fully loaded into memory. The length
// of individual fields (title, note) is only checked by the import after decoding —
// so a single huge field can use memory up to the body limit. Only the
// logged-in user themselves can trigger that. Real data sets are orders of
// magnitude below these limits.
const (
	maxImportTasks = 100_000
	maxImportLinks = 1_000_000
	maxImportStats = 100_000 // days, ~270 years
)

type tooManyError struct{ what string } // what: field in the backup ("tasks", "links", "stats")

func (e tooManyError) Error() string { return "too many " + e.what }

// decodeList reads a JSON list element by element (null = empty).
func decodeList[T any](dec *json.Decoder, max int, what string) ([]T, error) {
	tok, err := dec.Token()
	if err != nil || tok == nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("%s is not a list", what)
	}
	var out []T
	for dec.More() {
		if len(out) >= max {
			return nil, tooManyError{what}
		}
		var v T
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	_, err = dec.Token() // ']'
	return out, err
}

// decodeStats reads the stats object {"YYYY-MM-DD": n} entry by entry.
func decodeStats(dec *json.Decoder) (map[string]int, error) {
	tok, err := dec.Token()
	if err != nil || tok == nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("stats is not an object")
	}
	out := map[string]int{}
	for dec.More() {
		if len(out) >= maxImportStats {
			return nil, tooManyError{"stats"}
		}
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := k.(string)
		var n int
		if err := dec.Decode(&n); err != nil {
			return nil, err
		}
		out[key] = n
	}
	_, err = dec.Token() // '}'
	return out, err
}

// importLists validates a backup's lists. Without lists (a backup from
// before lists existed), there's a default list with ID 1. Otherwise: exactly one
// default list, unique IDs and names, only the default list is nameless.
func importLists(in []exportList) (out []exportList, known map[int64]bool, defaultID int64, ok bool) {
	if len(in) == 0 {
		return []exportList{{ID: 1, IsDefault: true}}, map[int64]bool{1: true}, 1, true
	}
	known = map[int64]bool{}
	names := map[string]bool{}
	for _, l := range in {
		if l.ID <= 0 || l.ID > maxImportID || known[l.ID] {
			return nil, nil, 0, false
		}
		known[l.ID] = true
		if l.IsDefault {
			if defaultID != 0 {
				return nil, nil, 0, false
			}
			defaultID = l.ID
		}
		if l.Name == nil {
			if !l.IsDefault {
				return nil, nil, 0, false
			}
		} else {
			name, err := listName(*l.Name)
			if err != nil || names[listKey(name)] {
				return nil, nil, 0, false
			}
			names[listKey(name)] = true
			l.Name = &name
		}
		out = append(out, l)
	}
	if defaultID == 0 {
		return nil, nil, 0, false
	}
	return out, known, defaultID, true
}
