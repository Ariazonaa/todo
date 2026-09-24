package main

import (
	"fmt"
	"testing"
)

// Owner and member each get one Discord ping, not two.
func TestSharedTaskRemindsEveryone(t *testing.T) {
	sc := schedulerFixture(t)
	s := &server{db: sc.db, store: sc.store, hub: sc.hub}
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	callAs(t, testUID, s.handleAddMember, "PUT", "/", nil, "id", fmt.Sprint(haus.ID), "uid", fmt.Sprint(bea))
	mustCreate(t, s, fmt.Sprintf(`{"title":"Müll","list_id":%d,"due_date":"2026-03-01","due_time":"08:00"}`, haus.ID))
	dA, urlA := newDiscordStub(t)
	dB, urlB := newDiscordStub(t)
	for range 2 {
		sc.exactReminders(testUID, urlA, "2026-03-01", "09:00")
		sc.exactReminders(bea, urlB, "2026-03-01", "09:00")
	}
	if len(dA.contents()) != 1 || len(dB.contents()) != 1 {
		t.Fatalf("Pings: Admin %d, Bea %d — erwartet je 1", len(dA.contents()), len(dB.contents()))
	}
}

// New due date → new ping; a stamp for the old one doesn't block it.
func TestNewDueRemindsAgain(t *testing.T) {
	sc := schedulerFixture(t)
	id := insertDue(t, sc.db, "Termin", "2026-03-01", "08:00")
	d, url := newDiscordStub(t)
	sc.exactReminders(testUID, url, "2026-03-01", "09:00")
	task, _ := getTask(sc.db, id)
	sc.db.Exec("UPDATE tasks SET due_date = '2026-03-02' WHERE id = ?", id)
	// late stamp for the old due date (the ping was in flight)
	if err := stampReminded(sc.db, testUID, chDiscord, task); err != nil {
		t.Fatal(err)
	}
	sc.exactReminders(testUID, url, "2026-03-02", "09:00")
	if n := len(d.contents()); n != 2 {
		t.Fatalf("%d Pings, erwartet 2 (alte und neue Fälligkeit)", n)
	}
}

// Digest includes shared tasks.
func TestDigestIncludesSharedTasks(t *testing.T) {
	sc := schedulerFixture(t)
	s := &server{db: sc.db, store: sc.store, hub: sc.hub}
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	callAs(t, testUID, s.handleAddMember, "PUT", "/", nil, "id", fmt.Sprint(haus.ID), "uid", fmt.Sprint(bea))
	mustCreate(t, s, fmt.Sprintf(`{"title":"Müll","list_id":%d,"due_date":"2026-03-01"}`, haus.ID))
	today, _, err := sc.digestTasks(bea, "2026-03-01")
	if err != nil || len(today) != 1 {
		t.Fatalf("Beas Digest: %v %v", today, err)
	}
}

// Old stamps from the task columns become rows owned by the task owner.
func TestMigrationMovesReminderStamps(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createUser(db, "Admin", true, []byte("todo-single-user"))
	db.Exec("INSERT INTO tasks (title, created_at, due_date, due_time, reminded_at, push_reminded_at) VALUES ('alt', 'z', '2026-03-01', '08:00', 'x', 'y')")
	for range 2 {
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM task_reminders WHERE user_id = 1 AND due = '2026-03-01 08:00'").Scan(&n)
	if n != 2 {
		t.Fatalf("%d Stempel, erwartet 2", n)
	}
}

// Deleted task: stamps gone (the ID gets reused).
func TestDeletedTaskDropsStamps(t *testing.T) {
	sc := schedulerFixture(t)
	id := insertDue(t, sc.db, "Termin", "2026-03-01", "08:00")
	_, url := newDiscordStub(t)
	sc.exactReminders(testUID, url, "2026-03-01", "09:00")
	sc.db.Exec("DELETE FROM tasks WHERE id = ?", id)
	var n int
	sc.db.QueryRow("SELECT COUNT(*) FROM task_reminders").Scan(&n)
	if n != 0 {
		t.Fatalf("%d Stempel übrig", n)
	}
}
