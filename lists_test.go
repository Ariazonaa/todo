package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func defaultListID(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow("SELECT id FROM lists WHERE is_default = 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertList(t *testing.T, db *sql.DB, name string) int64 {
	t.Helper()
	res, err := db.Exec("INSERT INTO lists (owner_id, name, is_default, created_at) VALUES (?, ?, 0, 'z')", testUID, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func putTask(t *testing.T, s *server, id int64, body string) (Task, int) {
	t.Helper()
	rec := call(t, s.handleUpdateTask, http.MethodPut, "/", strings.NewReader(body), "id", fmt.Sprint(id))
	var task Task
	json.Unmarshal(rec.Body.Bytes(), &task)
	return task, rec.Code
}

func TestListsMigration(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := createUser(db, "Admin", true, []byte("todo-single-user")); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-lists state (the table sets up the schema anyway)
	for _, q := range []string{
		"DROP TRIGGER tasks_default_list",
		"DROP INDEX idx_tasks_list",
		"ALTER TABLE tasks DROP COLUMN list_id",
		"DELETE FROM lists",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Exec("INSERT INTO tasks (title, created_at) VALUES ('alt', 'z')")
	for range 2 { // second run changes nothing
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM lists").Scan(&n)
	def := defaultListID(t, db)
	var got int64
	db.QueryRow("SELECT list_id FROM tasks WHERE title = 'alt'").Scan(&got)
	if n != 1 || got != def {
		t.Fatalf("%d Listen, Bestand in %d, Standard %d", n, got, def)
	}
	// New rows without a list end up in the default list via trigger
	db.Exec("INSERT INTO tasks (title, created_at) VALUES ('neu', 'z')")
	db.QueryRow("SELECT list_id FROM tasks WHERE title = 'neu'").Scan(&got)
	if got != def {
		t.Fatalf("neu in %d", got)
	}
	// A dangling list_id (list deleted concurrently) is healed on next startup
	db.Exec("UPDATE tasks SET list_id = 999 WHERE title = 'neu'")
	migrate(db)
	db.QueryRow("SELECT list_id FROM tasks WHERE title = 'neu'").Scan(&got)
	if got != def {
		t.Fatalf("tote Liste nicht geheilt: %d", got)
	}
}

func TestTaskListAssignment(t *testing.T) {
	s := testServer(t)
	def := defaultListID(t, s.db)
	arbeit := insertList(t, s.db, "Arbeit")

	if task := mustCreate(t, s, `{"title":"ohne"}`); task.ListID != def {
		t.Fatalf("ohne list_id in %d", task.ListID)
	}
	parent := mustCreate(t, s, fmt.Sprintf(`{"title":"Projekt","list_id":%d}`, arbeit))
	if parent.ListID != arbeit {
		t.Fatalf("mit list_id in %d", parent.ListID)
	}
	code, msg := errorOf(t, s.handleCreateTask, "en", "POST", "/", `{"title":"x","list_id":999}`)
	if code != http.StatusBadRequest || msg != "List not found" {
		t.Fatalf("unbekannte Liste: %d %q", code, msg)
	}
	// Subtask: takes the parent task's list, a submitted one does not count
	sub := mustCreate(t, s, fmt.Sprintf(`{"title":"Teil","parent_id":%d,"list_id":%d}`, parent.ID, def))
	if sub.ListID != arbeit {
		t.Fatalf("Unteraufgabe in %d", sub.ListID)
	}
	// Updating without list_id: list stays the same
	if upd, code := putTask(t, s, parent.ID, `{"title":"Projekt neu"}`); code != http.StatusOK || upd.ListID != arbeit {
		t.Fatalf("ohne list_id: %d %d", code, upd.ListID)
	}
	// Parent task changes list: subtasks move along
	if upd, code := putTask(t, s, parent.ID, fmt.Sprintf(`{"title":"Projekt","list_id":%d}`, def)); code != http.StatusOK || upd.ListID != def {
		t.Fatalf("verschieben: %d %d", code, upd.ListID)
	}
	if got, _ := getTask(s.db, sub.ID); got.ListID != def {
		t.Fatalf("Unteraufgabe blieb in %d", got.ListID)
	}
	// Reparenting under a parent task from another list adopts that list
	other := mustCreate(t, s, fmt.Sprintf(`{"title":"Anderes","list_id":%d}`, arbeit))
	if upd, _ := putTask(t, s, sub.ID, fmt.Sprintf(`{"title":"Teil","parent_id":%d}`, other.ID)); upd.ListID != arbeit {
		t.Fatalf("umgehängt in %d", upd.ListID)
	}
	if _, code := putTask(t, s, other.ID, `{"title":"Anderes","list_id":999}`); code != http.StatusBadRequest {
		t.Fatalf("ändern in unbekannte Liste: %d", code)
	}
}

func TestListTasksIncludesLists(t *testing.T) {
	s := testServer(t)
	insertList(t, s.db, "Arbeit")
	var body struct {
		Lists []List `json:"lists"`
	}
	json.Unmarshal(call(t, s.handleListTasks, http.MethodGet, "/", nil).Body.Bytes(), &body)
	if len(body.Lists) != 2 || !body.Lists[0].IsDefault || body.Lists[0].Name != nil || *body.Lists[1].Name != "Arbeit" {
		t.Fatalf("%+v", body.Lists)
	}
}

func mustList(t *testing.T, s *server, name string) List {
	t.Helper()
	rec := call(t, s.handleCreateList, http.MethodPost, "/", strings.NewReader(`{"name":`+strconv.Quote(name)+`}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("Liste %q: %d %s", name, rec.Code, rec.Body)
	}
	var l List
	json.Unmarshal(rec.Body.Bytes(), &l)
	return l
}

func TestListCreateRename(t *testing.T) {
	s := testServer(t)
	l := mustList(t, s, "  Einkauf  ")
	if l.Name == nil || *l.Name != "Einkauf" || l.IsDefault {
		t.Fatalf("%+v", l)
	}
	for name, want := range map[string]string{
		"":                      "List name must be 1–40 characters",
		strings.Repeat("a", 41): "List name must be 1–40 characters",
		"einkauf":               "A list with this name already exists",
	} {
		code, msg := errorOf(t, s.handleCreateList, "en", "POST", "/", `{"name":`+strconv.Quote(name)+`}`)
		if code != http.StatusBadRequest || msg != want {
			t.Errorf("%q: %d %q", name, code, msg)
		}
	}
	// A different casing of one's own name is allowed
	rec := call(t, s.handleRenameList, http.MethodPut, "/", strings.NewReader(`{"name":"EINKAUF"}`), "id", fmt.Sprint(l.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("umbenennen: %d %s", rec.Code, rec.Body)
	}
	mustList(t, s, "Arbeit")
	code, _ := errorOf(t, s.handleRenameList, "en", "PUT", "/", `{"name":"arbeit"}`, "id", fmt.Sprint(l.ID))
	if code != http.StatusBadRequest {
		t.Fatalf("umbenennen auf fremden Namen: %d", code)
	}
	// The default list can be renamed
	def := defaultListID(t, s.db)
	if rec := call(t, s.handleRenameList, http.MethodPut, "/", strings.NewReader(`{"name":"Privat"}`), "id", fmt.Sprint(def)); rec.Code != http.StatusOK {
		t.Fatalf("Standardliste umbenennen: %d", rec.Code)
	}
}

func TestListLimit(t *testing.T) {
	s := testServer(t)
	for i := range maxLists - 1 { // the default list counts too
		mustList(t, s, fmt.Sprintf("L%d", i))
	}
	code, msg := errorOf(t, s.handleCreateList, "en", "POST", "/", `{"name":"zu viel"}`)
	if code != http.StatusBadRequest || msg != "At most 50 lists" {
		t.Fatalf("%d %q", code, msg)
	}
}

func TestListDelete(t *testing.T) {
	s := testServer(t)
	def := defaultListID(t, s.db)
	del := func(id int64, mode string) int {
		req := "/"
		if mode != "" {
			req += "?tasks=" + mode
		}
		return call(t, s.handleDeleteList, http.MethodDelete, req, nil, "id", fmt.Sprint(id)).Code
	}
	if code := del(def, "delete"); code != http.StatusConflict {
		t.Fatalf("Standardliste gelöscht: %d", code)
	}
	leer := mustList(t, s, "Leer")
	if code := del(leer.ID, ""); code != http.StatusNoContent {
		t.Fatalf("leere Liste: %d", code)
	}

	arbeit := mustList(t, s, "Arbeit")
	task := mustCreate(t, s, fmt.Sprintf(`{"title":"Bericht","list_id":%d}`, arbeit.ID))
	if code := del(arbeit.ID, ""); code != http.StatusConflict {
		t.Fatalf("ohne Parameter trotz Aufgaben: %d", code)
	}
	if code := del(arbeit.ID, "move"); code != http.StatusNoContent {
		t.Fatalf("move: %d", code)
	}
	if got, _ := getTask(s.db, task.ID); got.ListID != def || listVisible(s.db, testUID, arbeit.ID) {
		t.Fatalf("nach move: Liste %d, existiert %v", got.ListID, listVisible(s.db, testUID, arbeit.ID))
	}

	weg := mustList(t, s, "Weg")
	parent := mustCreate(t, s, fmt.Sprintf(`{"title":"Umzug","list_id":%d}`, weg.ID))
	sub := mustCreate(t, s, fmt.Sprintf(`{"title":"Kartons","parent_id":%d}`, parent.ID))
	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(sub.ID))
	linkTasks(s.db, testUID, parent.ID, task.ID)
	s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/png', 'z', x'00')", parent.ID)
	if code := del(weg.ID, "delete"); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	var tasks, links, atts int
	s.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE id IN (?, ?)", parent.ID, sub.ID).Scan(&tasks)
	s.db.QueryRow("SELECT COUNT(*) FROM links").Scan(&links)
	s.db.QueryRow("SELECT COUNT(*) FROM attachments").Scan(&atts)
	if tasks != 0 || links != 0 || atts != 0 {
		t.Fatalf("übrig: %d Tasks, %d Links, %d Bilder", tasks, links, atts)
	}
	if statToday(t, s) != 1 {
		t.Fatal("Statistik mitgelöscht")
	}
	if code := del(weg.ID, "move"); code != http.StatusNotFound {
		t.Fatalf("zweites Löschen: %d", code)
	}
}

func TestListsExportImport(t *testing.T) {
	s := testServer(t)
	arbeit := mustList(t, s, "Arbeit")
	mustCreate(t, s, fmt.Sprintf(`{"title":"Bericht","list_id":%d}`, arbeit.ID))
	var data exportData
	json.Unmarshal(call(t, s.handleExport, http.MethodGet, "/", nil).Body.Bytes(), &data)
	if len(data.Lists) != 2 || data.Tasks[0].ListID != arbeit.ID {
		t.Fatalf("Export: %+v / %+v", data.Lists, data.Tasks)
	}

	// Round trip into a fresh instance
	s2 := testServer(t)
	if rec := postImport(t, s2, data); rec.Code != http.StatusNoContent {
		t.Fatalf("Import: %d %s", rec.Code, rec.Body)
	}
	lists, _ := listLists(s2.db, testUID)
	if len(lists) != 2 || lists[1].ID != arbeit.ID || *lists[1].Name != "Arbeit" {
		t.Fatalf("Listen nach Import: %+v", lists)
	}
	if got, _ := getTask(s2.db, data.Tasks[0].ID); got.ListID != arbeit.ID {
		t.Fatalf("Aufgabe in %d", got.ListID)
	}
}

// Backups from before lists existed: one default list, everything in it;
// subtasks always follow their parent task.
func TestImportOldBackupWithoutLists(t *testing.T) {
	s := testServer(t)
	mustList(t, s, "Wird ersetzt")
	raw := `{"tasks":[{"id":1,"title":"alt","created_at":"z"},{"id":2,"title":"sub","created_at":"z","parent_id":1}],` +
		`"settings":{"tz":"Europe/Berlin","digest_time":"09:00"}}`
	if rec := call(t, s.handleImport, http.MethodPost, "/", strings.NewReader(raw)); rec.Code != http.StatusNoContent {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	lists, _ := listLists(s.db, testUID)
	a, _ := getTask(s.db, 1)
	b, _ := getTask(s.db, 2)
	if len(lists) != 1 || !lists[0].IsDefault || a.ListID != lists[0].ID || b.ListID != lists[0].ID {
		t.Fatalf("Listen %+v, Tasks in %d/%d", lists, a.ListID, b.ListID)
	}
}

func TestImportRejectsBrokenLists(t *testing.T) {
	s := testServer(t)
	base := `"settings":{"tz":"Europe/Berlin","digest_time":"09:00"}}`
	for name, raw := range map[string]string{
		"keine Standardliste": `{"lists":[{"id":1,"name":"A"}],"tasks":[],` + base,
		"zwei Standardlisten": `{"lists":[{"id":1,"is_default":true},{"id":2,"is_default":true}],"tasks":[],` + base,
		"unbekannte list_id":  `{"lists":[{"id":1,"is_default":true}],"tasks":[{"id":1,"title":"x","created_at":"z","list_id":7}],` + base,
		"doppelter Name":      `{"lists":[{"id":1,"is_default":true},{"id":2,"name":"A"},{"id":3,"name":"a"}],"tasks":[],` + base,
		"Liste ohne Namen":    `{"lists":[{"id":1,"is_default":true},{"id":2}],"tasks":[],` + base,
	} {
		code, msg := errorOf(t, s.handleImport, "en", "POST", "/", raw)
		if code != http.StatusBadRequest || msg != "invalid backup file (lists)" {
			t.Errorf("%s: %d %q", name, code, msg)
		}
	}
}

// Case doesn't matter for umlauts either: SQLite's NOCASE only folds
// ASCII — otherwise the API would accept both "Ärger" and "ärger", and
// importing the same data would then fail.
func TestListNamesUniqueBeyondASCII(t *testing.T) {
	s := testServer(t)
	a := mustList(t, s, "Ärger")
	code, msg := errorOf(t, s.handleCreateList, "en", "POST", "/", `{"name":"ärger"}`)
	if code != http.StatusBadRequest || msg != "A list with this name already exists" {
		t.Fatalf("anlegen: %d %q", code, msg)
	}
	b := mustList(t, s, "Öl")
	if code, _ := errorOf(t, s.handleRenameList, "en", "PUT", "/", `{"name":"äRGER"}`, "id", fmt.Sprint(b.ID)); code != http.StatusBadRequest {
		t.Fatalf("umbenennen: %d", code)
	}
	// One's own name in a different casing remains allowed
	if rec := call(t, s.handleRenameList, http.MethodPut, "/", strings.NewReader(`{"name":"ÄRGER"}`), "id", fmt.Sprint(a.ID)); rec.Code != http.StatusOK {
		t.Fatalf("eigener Name: %d", rec.Code)
	}
	var data exportData
	json.Unmarshal(call(t, s.handleExport, http.MethodGet, "/", nil).Body.Bytes(), &data)
	if rec := postImport(t, testServer(t), data); rec.Code != http.StatusNoContent {
		t.Fatalf("Rundreise: %d %s", rec.Code, rec.Body)
	}
}

func mustListAs(t *testing.T, s *server, uid int64, name string) List {
	t.Helper()
	rec := callAs(t, uid, s.handleCreateList, "POST", "/", strings.NewReader(`{"name":"`+name+`"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("Liste %q: %d %s", name, rec.Code, rec.Body)
	}
	var l List
	json.Unmarshal(rec.Body.Bytes(), &l)
	return l
}
