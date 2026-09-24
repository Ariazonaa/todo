package main

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func taskByID(t *testing.T, s *server, id int64) Task {
	t.Helper()
	task, err := getTask(s.db, id)
	if err != nil {
		t.Fatalf("getTask %d: %v", id, err)
	}
	return task
}

// When a recurring parent task starts a new occurrence, its subtasks reset —
// but dated ones used to keep their old date: the next scheduler tick would
// immediately ping them again, and from the next day on they'd show up as
// overdue in the digest every day.
func TestRecurringParentShiftsDatedSubs(t *testing.T) {
	s := testServer(t)
	today := s.nowFor(testUID).Format(dateFmt)
	parent := mustCreate(t, s, fmt.Sprintf(`{"title":"Putzen","due_date":%q,"recurrence":"weekly"}`, today))
	dated := mustCreate(t, s, fmt.Sprintf(`{"title":"Bad","due_date":%q,"due_time":"00:00","parent_id":%d}`, today, parent.ID))
	undated := mustCreate(t, s, fmt.Sprintf(`{"title":"Küche","parent_id":%d}`, parent.ID))
	s.db.Exec("UPDATE tasks SET done = 1, completed_at = 'z' WHERE id IN (?, ?)", dated.ID, undated.ID)
	s.db.Exec("INSERT INTO task_reminders (task_id, user_id, channel, due, sent_at) SELECT id, ?, 'discord', due_date || ' ' || due_time, 'z' FROM tasks WHERE id = ?", testUID, dated.ID)

	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(parent.ID))

	p := taskByID(t, s, parent.ID)
	d := taskByID(t, s, dated.ID)
	u := taskByID(t, s, undated.ID)
	if p.DueDate == nil || *p.DueDate == today {
		t.Fatalf("Eltern-Task nicht verschoben: %v", p.DueDate)
	}
	var reminded *string
	if v := column(t, s.db, "reminded_at", dated.ID); v.Valid {
		reminded = &v.String
	}
	if d.DueDate == nil || *d.DueDate != *p.DueDate || d.Done || reminded != nil {
		t.Fatalf("datierte Unteraufgabe: due=%v done=%v reminded=%v, erwartet due=%s offen ungepingt", d.DueDate, d.Done, reminded, *p.DueDate)
	}
	if d.DueTime == nil || *d.DueTime != "00:00" {
		t.Fatalf("Uhrzeit der Unteraufgabe verändert: %v", d.DueTime)
	}
	if u.DueDate != nil || u.Done {
		t.Fatalf("undatierte Unteraufgabe: due=%v done=%v", u.DueDate, u.Done)
	}
}

// The digest used to cut off after ~1900 characters but still marked the day
// as done: the rest never showed up in any digest.
func TestDigestSummarizesWhatDoesNotFit(t *testing.T) {
	s := testServer(t)
	stub, hook := newDiscordStub(t)
	today := s.nowFor(testUID).Format(dateFmt)
	const total = 60
	for i := range total {
		mustCreate(t, s, fmt.Sprintf(`{"title":"Überfällige Aufgabe Nummer %02d mit etwas längerem Titel","due_date":"2020-01-01"}`, i))
	}
	newScheduler(s.db, s.store, s.hub, s.vapid).morningDigest(testUID, hook, "00:00", today, "23:59")

	msgs := stub.contents()
	if len(msgs) != 1 {
		t.Fatalf("erwartet eine Nachricht, bekam %d", len(msgs))
	}
	msg := msgs[0]
	if utf8.RuneCountInString(msg) > discordMaxRunes {
		t.Fatalf("Nachricht zu lang (%d Zeichen), postDiscord schneidet ab", utf8.RuneCountInString(msg))
	}
	listed := strings.Count(msg, "• ")
	m := regexp.MustCompile(`und (\d+) weitere`).FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("kein Hinweis auf nicht gelistete Aufgaben (%d gelistet):\n%s", listed, msg)
	}
	rest, _ := strconv.Atoi(m[1])
	if listed+rest != total {
		t.Fatalf("%d gelistet + %d weitere != %d", listed, rest, total)
	}
}

// An open subtask under a completed parent task used to sit only in the
// collapsed "Done" section — the header counted it as open, but it was
// visible nowhere. If a subtask becomes open, the parent task does too.
func TestReopeningSubReopensParent(t *testing.T) {
	s := testServer(t)
	parent := mustCreate(t, s, `{"title":"Eltern"}`)
	sub := mustCreate(t, s, fmt.Sprintf(`{"title":"Sub","parent_id":%d}`, parent.ID))
	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(parent.ID))
	if got := statToday(t, s); got != 2 {
		t.Fatalf("nach dem Abhaken: Statistik %d, erwartet 2", got)
	}

	call(t, s.handleUncompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(sub.ID))

	if p := taskByID(t, s, parent.ID); p.Done {
		t.Fatal("Eltern-Task blieb erledigt")
	}
	if got := statToday(t, s); got != 0 {
		t.Fatalf("Statistik nach dem Wiederöffnen: %d, erwartet 0", got)
	}
}

func TestNewSubReopensDoneParent(t *testing.T) {
	s := testServer(t)
	parent := mustCreate(t, s, `{"title":"Eltern"}`)
	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(parent.ID))
	mustCreate(t, s, fmt.Sprintf(`{"title":"neu","parent_id":%d}`, parent.ID))
	if p := taskByID(t, s, parent.ID); p.Done {
		t.Fatal("Eltern-Task blieb trotz neuer offener Unteraufgabe erledigt")
	}
	if got := statToday(t, s); got != 0 {
		t.Fatalf("Statistik: %d, erwartet 0", got)
	}
}

func TestMovingOpenTaskUnderDoneParentReopensIt(t *testing.T) {
	s := testServer(t)
	parent := mustCreate(t, s, `{"title":"Eltern"}`)
	other := mustCreate(t, s, `{"title":"wird Sub"}`)
	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(parent.ID))
	rec := call(t, s.handleUpdateTask, http.MethodPut, "/", strings.NewReader(fmt.Sprintf(`{"title":"wird Sub","parent_id":%d}`, parent.ID)), "id", fmt.Sprint(other.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("Umhängen: %d %s", rec.Code, rec.Body)
	}
	if p := taskByID(t, s, parent.ID); p.Done {
		t.Fatal("Eltern-Task blieb trotz offener Unteraufgabe erledigt")
	}
}

// The streak calculation used wall-clock time: in zones with a DST switch at
// midnight (e.g. America/Santiago), "minus one day" between 00:00 and 01:00
// would skip an entire day.
func TestStreakAcrossMidnightDST(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skip("keine Zonendaten")
	}
	// 2026-09-06 00:00 jumps to 01:00 in Santiago (start of DST).
	now := time.Date(2026, 9, 7, 0, 30, 0, 0, loc)
	counts := map[string]int{"2026-09-07": 1, "2026-09-06": 1, "2026-09-05": 1, "2026-09-04": 1}
	if _, streak := weekAndStreak(counts, now); streak != 4 {
		t.Fatalf("Streak %d, erwartet 4", streak)
	}
}

// A DB error is not the user's fault: 500, not 400 with a raw message.
func TestSettingsDBErrorIs500(t *testing.T) {
	s := testServer(t)
	s.db.Close()
	rec := call(t, s.handlePutSettings, http.MethodPut, "/", strings.NewReader(`{"tz":"Europe/Berlin","digest_time":"09:00","archive_days":7}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("DB zu: %d %s, erwartet 500", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "sql:") {
		t.Fatalf("interne Fehlermeldung beim Client: %s", rec.Body)
	}
}

func TestSettingsValidationErrorIs400(t *testing.T) {
	s := testServer(t)
	rec := call(t, s.handlePutSettings, http.MethodPut, "/", strings.NewReader(`{"tz":"Mars/Olympus","digest_time":"09:00"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ungültige Zeitzone: %d %s, erwartet 400", rec.Code, rec.Body)
	}
}

// The import used to report success even when the settings could not be saved.
func TestImportReportsFailedSettingsSave(t *testing.T) {
	s := testServer(t)
	for _, ev := range []string{"INSERT", "UPDATE"} {
		if _, err := s.db.Exec("CREATE TRIGGER no_tz_" + ev + " BEFORE " + ev + " ON user_settings WHEN NEW.key = 'tz' BEGIN SELECT RAISE(ABORT, 'boom'); END"); err != nil {
			t.Fatal(err)
		}
	}
	rec := postImport(t, s, exportData{
		Tasks:    []exportTask{{ID: 1, Title: "t", CreatedAt: "z"}},
		Settings: appSettings{TZ: "Europe/Berlin", DigestTime: "09:00"},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Import mit gescheiterten Einstellungen: %d %s, erwartet 500", rec.Code, rec.Body)
	}
}

type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	k := min(len(p), f.n)
	f.n -= k
	return k, nil
}

// An aborted upload is not "image too big".
func TestUploadAbortIsNotTooBig(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"mit Bild"}`)
	rec := call(t, s.handleAttachmentUpload, http.MethodPost, "/", &failingReader{n: 100}, "id", fmt.Sprint(task.ID))
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "zu groß") {
		t.Fatalf("abgebrochener Upload: %d %s", rec.Code, rec.Body)
	}
}

func TestUploadTooBigIs413(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"mit Bild"}`)
	rec := call(t, s.handleAttachmentUpload, http.MethodPost, "/", &failingReader{n: maxAttachmentSize + 10}, "id", fmt.Sprint(task.ID))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("zu großer Upload: %d %s, erwartet 413", rec.Code, rec.Body)
	}
}

// Snoozing reopened a completed task and moved it in two steps: if the
// second one failed, the task stayed open but unmoved.
func TestSnoozeIsAtomic(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"später"}`)
	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(task.ID))
	if _, err := s.db.Exec("CREATE TRIGGER no_move BEFORE UPDATE OF due_date ON tasks BEGIN SELECT RAISE(ABORT, 'boom'); END"); err != nil {
		t.Fatal(err)
	}
	rec := call(t, s.handleSnoozeTask, http.MethodPost, "/", strings.NewReader(`{"days":1}`), "id", fmt.Sprint(task.ID))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Snooze mit Fehler: %d %s, erwartet 500", rec.Code, rec.Body)
	}
	if got := taskByID(t, s, task.ID); !got.Done {
		t.Fatal("Task wurde geöffnet, obwohl das Verschieben scheiterte")
	}
	if got := statToday(t, s); got != 1 {
		t.Fatalf("Statistik %d, erwartet 1 (nichts zurückgebucht)", got)
	}
}

// A backup can also contain an open subtask under a completed parent task —
// the import straightens that out just like the API does.
func TestImportReopensParentOfOpenSub(t *testing.T) {
	s := testServer(t)
	one := int64(1)
	rec := postImport(t, s, exportData{Tasks: []exportTask{
		{ID: 1, Title: "Eltern", CreatedAt: "z", Done: true, CompletedAt: ptr("2026-01-01T00:00:00Z")},
		{ID: 2, Title: "offen", CreatedAt: "z", ParentID: &one},
	}})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Import: %d %s", rec.Code, rec.Body)
	}
	if p := taskByID(t, s, 1); p.Done {
		t.Fatal("Eltern-Task blieb erledigt")
	}
}

// Snoozing also reopens a completed task — for a subtask under a completed
// parent task, it used to end up invisible in "Done" afterward.
func TestSnoozingDoneSubReopensParent(t *testing.T) {
	s := testServer(t)
	p := mustCreate(t, s, `{"title":"Eltern"}`)
	sub := mustCreate(t, s, fmt.Sprintf(`{"title":"Sub","parent_id":%d}`, p.ID))
	call(t, s.handleCompleteTask, http.MethodPost, "/", nil, "id", fmt.Sprint(p.ID))
	rec := call(t, s.handleSnoozeTask, http.MethodPost, "/", strings.NewReader(`{"days":1}`), "id", fmt.Sprint(sub.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("Snooze: %d %s", rec.Code, rec.Body)
	}
	if taskByID(t, s, p.ID).Done {
		t.Fatal("Eltern-Task blieb erledigt, Unteraufgabe ist offen")
	}
}
