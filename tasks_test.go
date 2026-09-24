package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// call invokes a handler directly; pv are path values as pairs
// ("id", "3", …), the way the mux would otherwise set them.
func call(t *testing.T, h http.HandlerFunc, method, target string, body io.Reader, pv ...string) *httptest.ResponseRecorder {
	t.Helper()
	return callAs(t, testUID, h, method, target, body, pv...)
}

// callAs invokes h as user uid (as after requireAuth).
func callAs(t *testing.T, uid int64, h http.HandlerFunc, method, target string, body io.Reader, pv ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := withUser(httptest.NewRequest(method, target, body), uid)
	for i := 0; i+1 < len(pv); i += 2 {
		req.SetPathValue(pv[i], pv[i+1])
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func mustCreate(t *testing.T, s *server, body string) Task {
	t.Helper()
	rec := call(t, s.handleCreateTask, http.MethodPost, "/api/tasks", strings.NewReader(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("anlegen %s: %d %s", body, rec.Code, rec.Body)
	}
	var task Task
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
		t.Fatalf("antwort: %v", err)
	}
	return task
}

func statToday(t *testing.T, s *server) int {
	t.Helper()
	var n int
	s.db.QueryRow("SELECT COALESCE(SUM(done), 0) FROM stats WHERE date = ?", s.nowFor(testUID).Format(dateFmt)).Scan(&n)
	return n
}

// Snoozing reopens a completed task. Without reversing the stat, the next
// completion would count a second time toward "done today", week, and streak.
func TestSnoozingDoneTaskTakesBackStat(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Rechnung"}`)
	id := fmt.Sprint(task.ID)

	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", id)
	if got := statToday(t, s); got != 1 {
		t.Fatalf("nach dem Abhaken: %d", got)
	}
	rec := call(t, s.handleSnoozeTask, http.MethodPost, "/", strings.NewReader(`{"days":1}`), "id", id)
	if rec.Code != http.StatusOK {
		t.Fatalf("snooze: %d %s", rec.Code, rec.Body)
	}
	if got, _ := getTask(s.db, task.ID); got.Done {
		t.Fatal("verschobener Task ist noch erledigt")
	}
	if got := statToday(t, s); got != 0 {
		t.Fatalf("Snooze hat den Tageszähler nicht zurückgebucht: %d", got)
	}
	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", id)
	if got := statToday(t, s); got != 1 {
		t.Fatalf("derselbe Task zählt nach Snooze + Abhaken %d-mal", got)
	}
}

// nestedCount counts subtasks of subtasks — these must never exist.
func nestedCount(t *testing.T, s *server) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks c JOIN tasks p ON c.parent_id = p.id WHERE p.parent_id IS NOT NULL").Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
	return n
}

// Two devices at once: one moves C under B, the other moves B under A.
// Checking and writing used to be separate statements, and the other request
// could run in between — that produced A > B > C, and every later backup
// became unimportable. The rule has to hold inside the write itself.
func TestOneLevelRuleHoldsUnderConcurrentMoves(t *testing.T) {
	s := testServer(t)
	for round := range 100 {
		if _, err := s.db.Exec("DELETE FROM tasks"); err != nil {
			t.Fatalf("reset: %v", err)
		}
		for _, id := range []int{1, 2, 3} {
			if _, err := s.db.Exec("INSERT INTO tasks (id, title, created_at) VALUES (?, ?, 'z')", id, fmt.Sprint("T", id)); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
		var wg sync.WaitGroup
		move := func(id, parent int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"title":"T%d","parent_id":%d}`, id, parent)
			call(t, s.handleUpdateTask, http.MethodPut, "/", strings.NewReader(body), "id", fmt.Sprint(id))
		}
		sub := func(parent int) {
			defer wg.Done()
			call(t, s.handleCreateTask, http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"title":"neu","parent_id":%d}`, parent)))
		}
		wg.Add(3)
		go move(3, 2) // C under B
		go move(2, 1) // B under A
		go sub(3)     // new subtask under C
		wg.Wait()
		if n := nestedCount(t, s); n != 0 {
			t.Fatalf("Runde %d: %d Unteraufgaben zweiter Ebene", round, n)
		}
	}
}

func TestCreateTaskRejectsInvalidParentAtomically(t *testing.T) {
	s := testServer(t)
	parent := mustCreate(t, s, `{"title":"Eltern"}`)
	child := mustCreate(t, s, fmt.Sprintf(`{"title":"Kind","parent_id":%d}`, parent.ID))
	for name, pid := range map[string]int64{"Unteraufgabe": child.ID, "gelöscht": 999} {
		if _, err := createTask(s.db, testUID, "x", nil, nil, "", "", false, 0, nil, &pid); !errors.Is(err, errParentInvalid) {
			t.Errorf("%s als Eltern: err = %v, erwartet errParentInvalid", name, err)
		}
	}
}

// When reparenting, the position in the old sibling order no longer applies —
// the task belongs at the end of the new list, just as when creating it.
func TestReparentedTaskGoesToEndOfNewList(t *testing.T) {
	s := testServer(t)
	mustCreate(t, s, `{"title":"A"}`)
	mustCreate(t, s, `{"title":"B"}`)
	p := mustCreate(t, s, `{"title":"P"}`)
	sub := mustCreate(t, s, fmt.Sprintf(`{"title":"s","parent_id":%d}`, p.ID))

	rec := call(t, s.handleUpdateTask, http.MethodPut, "/", strings.NewReader(`{"title":"s","parent_id":null}`), "id", fmt.Sprint(sub.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("umhängen: %d %s", rec.Code, rec.Body)
	}
	var maxOther int64
	s.db.QueryRow("SELECT MAX(sort_order) FROM tasks WHERE parent_id IS NULL AND id != ?", sub.ID).Scan(&maxOther)
	got, _ := getTask(s.db, sub.ID)
	if got.SortOrder <= maxOther {
		t.Fatalf("hochgestufte Unteraufgabe steht mitten in der Liste (sort_order %d, Maximum der anderen %d)", got.SortOrder, maxOther)
	}
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

// onFirstRead runs an action as soon as the handler starts reading the body —
// i.e. exactly between its task check and the INSERT.
type onFirstRead struct {
	r    io.Reader
	hook func()
}

func (o *onFirstRead) Read(p []byte) (int, error) {
	if o.hook != nil {
		o.hook()
		o.hook = nil
	}
	return o.r.Read(p)
}

// A large upload takes seconds. If the task is deleted during that window,
// the image used to end up attached to whichever task next got the same ID.
func TestUploadToTaskDeletedMidUploadIsRejected(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Fotos"}`)
	body := &onFirstRead{r: bytes.NewReader(pngBytes(t)), hook: func() { deleteTaskCascade(s.db, testUID, task.ID) }}

	rec := call(t, s.handleAttachmentUpload, http.MethodPost, "/", body, "id", fmt.Sprint(task.ID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("Upload auf gelöschten Task: %d %s", rec.Code, rec.Body)
	}
	next := mustCreate(t, s, `{"title":"Neue Aufgabe"}`)
	if next.ID != task.ID {
		t.Logf("Hinweis: neue ID %d statt %d — Wiederverwendung hier nicht nachgestellt", next.ID, task.ID)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM attachments").Scan(&n)
	if n != 0 {
		t.Fatalf("%d verwaiste(s) Bild(er) in der DB", n)
	}
}

// Images are revalidated via ETag instead of being served unchecked from the
// browser cache for a year — otherwise they'd stay accessible after logout.
func TestAttachmentsRevalidateInsteadOfImmutableCache(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Fotos"}`)
	rec := call(t, s.handleAttachmentUpload, http.MethodPost, "/", bytes.NewReader(pngBytes(t)), "id", fmt.Sprint(task.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var up struct{ ID int64 }
	json.Unmarshal(rec.Body.Bytes(), &up)

	for name, h := range map[string]http.HandlerFunc{"original": s.handleAttachmentGet, "thumb": s.handleAttachmentThumb} {
		first := call(t, h, http.MethodGet, "/", nil, "task", fmt.Sprint(task.ID), "id", fmt.Sprint(up.ID))
		if first.Code != http.StatusOK || first.Body.Len() == 0 {
			t.Fatalf("%s: %d, %d Bytes", name, first.Code, first.Body.Len())
		}
		if cc := first.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") || !strings.Contains(cc, "no-cache") {
			t.Fatalf("%s: Cache-Control = %q", name, cc)
		}
		etag := first.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: kein ETag", name)
		}
		req := withUser(httptest.NewRequest(http.MethodGet, "/", nil), testUID)
		req.SetPathValue("task", fmt.Sprint(task.ID))
		req.SetPathValue("id", fmt.Sprint(up.ID))
		req.Header.Set("If-None-Match", etag)
		again := httptest.NewRecorder()
		h(again, req)
		if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
			t.Fatalf("%s: Revalidierung ergab %d mit %d Bytes", name, again.Code, again.Body.Len())
		}
	}
}

// A link to a (concurrently) deleted task used to be inherited by whichever
// new task next got the same ID.
func TestLinkToMissingTaskIsNotStored(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"A"}`)
	if err := linkTasks(s.db, testUID, task.ID, 999); err != nil {
		t.Fatalf("linkTasks: %v", err)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM links").Scan(&n)
	if n != 0 {
		t.Fatalf("Verknüpfung auf nicht existierenden Task gespeichert (%d)", n)
	}
}

// tasks.id gets reused after deletion. A notification carrying a stale
// expected_due_date must not complete or snooze a task whose due date has
// since changed (or a new task) — just a 409, nothing changes.
func TestCompleteWithMismatchingExpectedDueDateIs409(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Zahnarzt","due_date":"2026-03-01"}`)
	id := fmt.Sprint(task.ID)

	rec := call(t, s.handleCompleteTask, http.MethodPost, "/", strings.NewReader(`{"expected_due_date":"2026-02-01"}`), "id", id)
	if rec.Code != http.StatusConflict {
		t.Fatalf("Complete mit falschem expected_due_date: %d %s, erwartet 409", rec.Code, rec.Body)
	}
	if got := taskByID(t, s, task.ID); got.Done {
		t.Fatal("Task trotz 409 abgehakt")
	}
}

func TestCompleteWithMatchingExpectedDueDateSucceeds(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Zahnarzt","due_date":"2026-03-01"}`)
	id := fmt.Sprint(task.ID)

	rec := call(t, s.handleCompleteTask, http.MethodPost, "/", strings.NewReader(`{"expected_due_date":"2026-03-01"}`), "id", id)
	if rec.Code != http.StatusOK {
		t.Fatalf("Complete mit passendem expected_due_date: %d %s", rec.Code, rec.Body)
	}
	if got := taskByID(t, s, task.ID); !got.Done {
		t.Fatal("Task trotz passendem expected_due_date nicht abgehakt")
	}
}

// Without expected_due_date (in-app), previous behavior is unchanged.
func TestCompleteWithoutExpectedDueDateSucceedsAsBefore(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Zahnarzt","due_date":"2026-03-01"}`)
	id := fmt.Sprint(task.ID)

	rec := call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", id)
	if rec.Code != http.StatusOK {
		t.Fatalf("Complete ohne expected_due_date: %d %s", rec.Code, rec.Body)
	}
	if got := taskByID(t, s, task.ID); !got.Done {
		t.Fatal("Task nicht abgehakt")
	}
}

func TestSnoozeWithMismatchingExpectedDueDateIs409(t *testing.T) {
	s := testServer(t)
	due := s.nowFor(testUID).AddDate(0, 0, 30).Format(dateFmt)
	task := mustCreate(t, s, fmt.Sprintf(`{"title":"Zahnarzt","due_date":%q}`, due))
	id := fmt.Sprint(task.ID)

	rec := call(t, s.handleSnoozeTask, http.MethodPost, "/", strings.NewReader(`{"days":1,"expected_due_date":"2026-02-01"}`), "id", id)
	if rec.Code != http.StatusConflict {
		t.Fatalf("Snooze mit falschem expected_due_date: %d %s, erwartet 409", rec.Code, rec.Body)
	}
	if got := taskByID(t, s, task.ID); got.DueDate == nil || *got.DueDate != due {
		t.Fatalf("Fälligkeit trotz 409 geändert: %+v", got.DueDate)
	}
}

func TestSnoozeWithMatchingExpectedDueDateSucceeds(t *testing.T) {
	s := testServer(t)
	due := s.nowFor(testUID).AddDate(0, 0, 30).Format(dateFmt)
	want := s.nowFor(testUID).AddDate(0, 0, 31).Format(dateFmt)
	task := mustCreate(t, s, fmt.Sprintf(`{"title":"Zahnarzt","due_date":%q}`, due))
	id := fmt.Sprint(task.ID)

	rec := call(t, s.handleSnoozeTask, http.MethodPost, "/", strings.NewReader(fmt.Sprintf(`{"days":1,"expected_due_date":%q}`, due)), "id", id)
	if rec.Code != http.StatusOK {
		t.Fatalf("Snooze mit passendem expected_due_date: %d %s", rec.Code, rec.Body)
	}
	if got := taskByID(t, s, task.ID); got.DueDate == nil || *got.DueDate != want {
		t.Fatalf("Fälligkeit nicht verschoben: %+v, erwartet %s", got.DueDate, want)
	}
}

func TestSnoozeWithoutExpectedDueDateSucceedsAsBefore(t *testing.T) {
	s := testServer(t)
	due := s.nowFor(testUID).AddDate(0, 0, 30).Format(dateFmt)
	want := s.nowFor(testUID).AddDate(0, 0, 31).Format(dateFmt)
	task := mustCreate(t, s, fmt.Sprintf(`{"title":"Zahnarzt","due_date":%q}`, due))
	id := fmt.Sprint(task.ID)

	rec := call(t, s.handleSnoozeTask, http.MethodPost, "/", strings.NewReader(`{"days":1}`), "id", id)
	if rec.Code != http.StatusOK {
		t.Fatalf("Snooze ohne expected_due_date: %d %s", rec.Code, rec.Body)
	}
	if got := taskByID(t, s, task.ID); got.DueDate == nil || *got.DueDate != want {
		t.Fatalf("Fälligkeit nicht verschoben: %+v, erwartet %s", got.DueDate, want)
	}
}

func mustCreateAs(t *testing.T, s *server, uid int64, body string) Task {
	t.Helper()
	rec := callAs(t, uid, s.handleCreateTask, http.MethodPost, "/api/tasks", strings.NewReader(body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("anlegen %s: %d %s", body, rec.Code, rec.Body)
	}
	var task Task
	json.Unmarshal(rec.Body.Bytes(), &task)
	return task
}

// asTestUser sets the test user on the request, the way requireAuth would —
// for tests with a real HTTP server, without logging in.
func asTestUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { h(w, withUser(r, testUID)) }
}
