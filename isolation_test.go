package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Two people: B may neither see nor change A's data — through any route.
func TestIsolation(t *testing.T) {
	s := testServer(t)
	a := testUID
	b := mustUser(t, s, "Bea")
	arbeit := mustList(t, s, "Arbeit")
	task := mustCreate(t, s, fmt.Sprintf(`{"title":"Geheim","list_id":%d,"due_date":"2026-03-01"}`, arbeit.ID))
	sub := mustCreate(t, s, fmt.Sprintf(`{"title":"Teil","parent_id":%d}`, task.ID))
	other := mustCreate(t, s, `{"title":"Zweite"}`)
	linkTasks(s.db, a, task.ID, other.ID)
	s.db.Exec("INSERT INTO attachments (task_id, mime, created_at, data) VALUES (?, 'image/png', 'z', x'89504e47')", task.ID)
	var att int64
	s.db.QueryRow("SELECT id FROM attachments").Scan(&att)
	bTask := mustCreateAs(t, s, b, `{"title":"B's"}`)

	id, subID, attID, list := fmt.Sprint(task.ID), fmt.Sprint(sub.ID), fmt.Sprint(att), fmt.Sprint(arbeit.ID)

	// Reading: B sees nothing of A
	var got struct {
		Tasks []Task `json:"tasks"`
		Lists []List `json:"lists"`
	}
	json.Unmarshal(callAs(t, b, s.handleListTasks, "GET", "/", nil).Body.Bytes(), &got)
	for _, x := range got.Tasks {
		if x.ID != bTask.ID {
			t.Fatalf("B sieht Aufgabe %d", x.ID)
		}
	}
	if len(got.Lists) != 1 || !got.Lists[0].IsDefault {
		t.Fatalf("B sieht Listen %+v", got.Lists)
	}

	// Every route with A's IDs → 404 (or no effect)
	for _, c := range []struct {
		name   string
		h      http.HandlerFunc
		method string
		body   string
		pv     []string
	}{
		{"ändern", s.handleUpdateTask, "PUT", `{"title":"gekapert"}`, []string{"id", id}},
		{"abhaken", s.handleCompleteTask, "POST", "", []string{"id", id}},
		{"öffnen", s.handleUncompleteTask, "POST", "", []string{"id", id}},
		{"verschieben", s.handleSnoozeTask, "POST", `{"days":1}`, []string{"id", id}},
		{"in Arbeit", s.handleToggleProgress, "POST", `{"on":true}`, []string{"id", id}},
		{"verknüpfen", s.handleAddLink, "POST", fmt.Sprintf(`{"other":%d}`, bTask.ID), []string{"id", id}},
		{"eigene mit fremder verknüpfen", s.handleAddLink, "POST", fmt.Sprintf(`{"other":%d}`, task.ID), []string{"id", fmt.Sprint(bTask.ID)}},
		{"Verknüpfung lösen", s.handleRemoveLink, "DELETE", "", []string{"id", id, "other", fmt.Sprint(other.ID)}},
		{"Bild hochladen", s.handleAttachmentUpload, "POST", "x", []string{"id", id}},
		{"Bild", s.handleAttachmentGet, "GET", "", []string{"task", id, "id", attID}},
		{"Vorschau", s.handleAttachmentThumb, "GET", "", []string{"task", id, "id", attID}},
		{"Bild löschen", s.handleAttachmentDelete, "DELETE", "", []string{"task", id, "id", attID}},
		{"löschen", s.handleDeleteTask, "DELETE", "", []string{"id", id}},
		{"Unteraufgabe löschen", s.handleDeleteTask, "DELETE", "", []string{"id", subID}},
		{"Liste umbenennen", s.handleRenameList, "PUT", `{"name":"x"}`, []string{"id", list}},
		{"Liste löschen", s.handleDeleteList, "DELETE", "", []string{"id", list}},
		{"Liste teilen", s.handleAddMember, "PUT", "", []string{"id", list, "uid", fmt.Sprint(b)}},
		{"Mitglied entfernen", s.handleRemoveMember, "DELETE", "", []string{"id", list, "uid", fmt.Sprint(a)}},
		{"Unteraufgabe unter fremde", s.handleCreateTask, "POST", fmt.Sprintf(`{"title":"x","parent_id":%d}`, task.ID), nil},
		{"in fremde Liste", s.handleCreateTask, "POST", fmt.Sprintf(`{"title":"x","list_id":%d}`, arbeit.ID), nil},
		{"eigene in fremde Liste", s.handleUpdateTask, "PUT", fmt.Sprintf(`{"title":"B's","list_id":%d}`, arbeit.ID), []string{"id", fmt.Sprint(bTask.ID)}},
	} {
		rec := callAs(t, b, c.h, c.method, "/", strings.NewReader(c.body), c.pv...)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	// Reordering with someone else's IDs changes nothing
	before, _ := getTask(s.db, task.ID)
	callAs(t, b, s.handleReorder, "PUT", "/", strings.NewReader(fmt.Sprintf(`{"ids":[%d,%d]}`, bTask.ID, task.ID)))
	after, _ := getTask(s.db, task.ID)
	if after.SortOrder != before.SortOrder {
		t.Error("Umsortieren hat A's Aufgabe verändert")
	}
	// Stats: B's counters stay empty, A's tasks don't count for B
	var st map[string]int
	json.Unmarshal(callAs(t, b, s.handleStats, "GET", "/", nil).Body.Bytes(), &st)
	if st["open"] != 1 || st["done"] != 0 {
		t.Errorf("B's Statistik %+v", st)
	}
	// A's data unchanged
	if a2, err := getTask(s.db, task.ID); err != nil || a2.Title != "Geheim" || a2.Done {
		t.Fatalf("A's Aufgabe: %+v %v", a2, err)
	}
	var links, atts int
	s.db.QueryRow("SELECT COUNT(*) FROM links").Scan(&links)
	s.db.QueryRow("SELECT COUNT(*) FROM attachments").Scan(&atts)
	if links != 1 || atts != 1 || !listVisible(s.db, a, arbeit.ID) {
		t.Fatalf("A's Links %d, Bilder %d", links, atts)
	}
}

// Every route from main.go is in the isolation test or explicitly
// exempted (public, or only ever touches the caller's own data by
// construction).
func TestIsolationCoversAllRoutes(t *testing.T) {
	src, _ := os.ReadFile("main.go")
	test, _ := os.ReadFile("isolation_test.go")
	covered := string(test)
	exempt := map[string]bool{
		// public / no personal data
		"handleHealthz": true, "handleLoginPage": true, "handleSetupPage": true, "handleIndex": true,
		"handleLoginBegin": true, "handleLoginFinish": true, "handleSetupBegin": true, "handleSetupFinish": true,
		"handleSetupMode": true, "handleLocale": true, "handlePushKey": true,
		// own session / own account by construction (tests in users_test.go)
		"handleLogout": true, "handleLogoutAll": true, "handleEvents": true,
		"handleGetSettings": true, "handlePutSettings": true, "handleSetLanguage": true, "handleTestWebhook": true,
		"handlePasskeyList": true, "handlePasskeyBegin": true, "handlePasskeyFinish": true, "handlePasskeyDelete": true,
		"handlePushSubscribe": true, "handlePushUnsubscribe": true, "handlePushTest": true,
		"handleExport": true, "handleImport": true, "handleListLists": true, "handleCreateList": true,
		// only account names, intentionally visible to all logged-in users
		// (sharing)
		"handlePeople":    true,
		"handleListUsers": true, "handleCreateUser": true, "handleNewUserCode": true, "handleDeleteUser": true,
	}
	for _, m := range regexp.MustCompile(`s\.(handle[A-Za-z]+)`).FindAllStringSubmatch(string(src), -1) {
		if !exempt[m[1]] && !strings.Contains(covered, "s."+m[1]) {
			t.Errorf("Route %s fehlt im Trennungs-Test", m[1])
		}
	}
}
