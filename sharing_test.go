package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// share: owner invites member, and member accepts — a fully shared list.
func share(t *testing.T, s *server, owner int64, list, member int64) *httptest.ResponseRecorder {
	t.Helper()
	rec := invite(t, s, owner, list, member)
	if rec.Code == http.StatusNoContent {
		if acc := callAs(t, member, s.handleAcceptInvite, "POST", "/", nil, "id", fmt.Sprint(list)); acc.Code != http.StatusNoContent {
			t.Fatalf("annehmen: %d %s", acc.Code, acc.Body)
		}
	}
	return rec
}

func invite(t *testing.T, s *server, owner int64, list, member int64) *httptest.ResponseRecorder {
	t.Helper()
	return callAs(t, owner, s.handleAddMember, "PUT", "/", nil, "id", fmt.Sprint(list), "uid", fmt.Sprint(member))
}

// An invitation alone shows nothing: neither the list nor its tasks, no
// reminders, no edits — until it's accepted. Declining removes it.
func TestInvitationNeedsAcceptance(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	task := mustCreate(t, s, fmt.Sprintf(`{"title":"Müll","list_id":%d,"due_date":"2026-03-01","due_time":"08:00"}`, haus.ID))
	ch := s.hub.subscribe(bea)
	if rec := invite(t, s, testUID, haus.ID, bea); rec.Code != http.StatusNoContent {
		t.Fatalf("einladen: %d %s", rec.Code, rec.Body)
	}
	if !notified(ch) {
		t.Fatal("Bea erfährt nichts von der Einladung")
	}
	var got struct {
		Tasks   []Task   `json:"tasks"`
		Lists   []List   `json:"lists"`
		Invites []Invite `json:"invites"`
	}
	load := func(uid int64) {
		got.Tasks, got.Lists, got.Invites = nil, nil, nil
		json.Unmarshal(callAs(t, uid, s.handleListTasks, "GET", "/", nil).Body.Bytes(), &got)
	}
	load(bea)
	if len(got.Tasks) != 0 || len(got.Lists) != 1 {
		t.Fatalf("vor dem Annehmen sichtbar: %+v / %+v", got.Tasks, got.Lists)
	}
	if len(got.Invites) != 1 || got.Invites[0].ListID != haus.ID || got.Invites[0].OwnerName != "Admin" || *got.Invites[0].Name != "Haushalt" {
		t.Fatalf("Einladungen %+v", got.Invites)
	}
	if rec := callAs(t, bea, s.handleCompleteTask, "POST", "/", nil, "id", fmt.Sprint(task.ID)); rec.Code != http.StatusNotFound {
		t.Fatalf("abhaken vor dem Annehmen: %d", rec.Code)
	}
	if due, _ := newScheduler(s.db, s.store, s.hub, s.vapid).queryTasks("SELECT "+taskCols+" FROM tasks WHERE list_id "+visibleLists, bea); len(due) != 0 {
		t.Fatalf("Erinnerungen vor dem Annehmen: %+v", due)
	}
	// The owner sees the person as invited
	load(testUID)
	if m := got.Lists[1].Members; len(m) != 1 || m[0].ID != bea || !m[0].Pending {
		t.Fatalf("Mitglieder beim Eigentümer %+v", m)
	}
	// Someone without an invitation can't accept
	cleo := mustUser(t, s, "Cleo")
	if rec := callAs(t, cleo, s.handleAcceptInvite, "POST", "/", nil, "id", fmt.Sprint(haus.ID)); rec.Code != http.StatusNotFound {
		t.Fatalf("ohne Einladung angenommen: %d", rec.Code)
	}
	if rec := callAs(t, bea, s.handleAcceptInvite, "POST", "/", nil, "id", fmt.Sprint(haus.ID)); rec.Code != http.StatusNoContent {
		t.Fatalf("annehmen: %d %s", rec.Code, rec.Body)
	}
	load(bea)
	if len(got.Tasks) != 1 || len(got.Invites) != 0 {
		t.Fatalf("nach dem Annehmen: %+v / %+v", got.Tasks, got.Invites)
	}
	// Declining = leaving with one's own ID; works before accepting too
	beas := mustListAs(t, s, bea, "Beas")
	invite(t, s, bea, beas.ID, cleo)
	if rec := callAs(t, cleo, s.handleRemoveMember, "DELETE", "/", nil, "id", fmt.Sprint(beas.ID), "uid", fmt.Sprint(cleo)); rec.Code != http.StatusNoContent {
		t.Fatalf("ablehnen: %d %s", rec.Code, rec.Body)
	}
	load(cleo)
	if len(got.Invites) != 0 {
		t.Fatalf("Einladung nach dem Ablehnen: %+v", got.Invites)
	}
}

// In a shared list, Bea reopening the admin's completion used to take the
// count away from Bea — and the admin's next completion counted twice.
func TestReopenTakesBackCompletersStat(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	share(t, s, testUID, haus.ID, bea)
	task := mustCreate(t, s, fmt.Sprintf(`{"title":"Müll","list_id":%d}`, haus.ID))
	id := fmt.Sprint(task.ID)
	stats := func(uid int64) int {
		var st map[string]int
		json.Unmarshal(callAs(t, uid, s.handleStats, "GET", "/", nil).Body.Bytes(), &st)
		return st["today"]
	}
	// Bea completed something of her own today
	beas := mustCreateAs(t, s, bea, `{"title":"Beas"}`)
	callAs(t, bea, s.handleCompleteTask, "POST", "/", nil, "id", fmt.Sprint(beas.ID))

	callAs(t, testUID, s.handleCompleteTask, "POST", "/", nil, "id", id)
	callAs(t, bea, s.handleUncompleteTask, "POST", "/", nil, "id", id)
	if a, b := stats(testUID), stats(bea); a != 0 || b != 1 {
		t.Fatalf("nach Beas Wiederöffnen: Admin %d (erwartet 0), Bea %d (erwartet 1)", a, b)
	}
	callAs(t, testUID, s.handleCompleteTask, "POST", "/", nil, "id", id)
	if a := stats(testUID); a != 1 {
		t.Fatalf("Admin nach erneutem Erledigen: %d, erwartet 1", a)
	}
	// Completer deleted: reopening takes back nobody's count
	other := mustCreate(t, s, fmt.Sprintf(`{"title":"Spülen","list_id":%d}`, haus.ID))
	callAs(t, bea, s.handleCompleteTask, "POST", "/", nil, "id", fmt.Sprint(other.ID))
	if err := deleteUser(s.db, bea); err != nil {
		t.Fatal(err)
	}
	callAs(t, testUID, s.handleUncompleteTask, "POST", "/", nil, "id", fmt.Sprint(other.ID))
	if a := stats(testUID); a != 1 {
		t.Fatalf("Admin nach Wiederöffnen einer Erledigung der gelöschten Bea: %d, erwartet 1", a)
	}
}

// Sharing is lasting access for another account: like managing passkeys,
// only with a fresh login.
func TestShareNeedsFreshAuth(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	rec := serveAuthed(s, s.handleAddMember, staleRequest(t, s, testUID, "PUT", "", "id", fmt.Sprint(haus.ID), "uid", fmt.Sprint(bea)))
	if rec.Code != http.StatusForbidden || isMember(s.db, haus.ID, bea) {
		t.Fatalf("teilen mit alter Sitzung: %d", rec.Code)
	}
}

// A member sees the list together with its tasks and can fully edit them.
func TestMemberEditsSharedList(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	task := mustCreate(t, s, fmt.Sprintf(`{"title":"Müll","list_id":%d}`, haus.ID))
	if rec := share(t, s, testUID, haus.ID, bea); rec.Code != http.StatusNoContent {
		t.Fatalf("teilen: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Tasks []Task `json:"tasks"`
		Lists []List `json:"lists"`
	}
	json.Unmarshal(callAs(t, bea, s.handleListTasks, "GET", "/", nil).Body.Bytes(), &got)
	if len(got.Tasks) != 1 || got.Tasks[0].ID != task.ID {
		t.Fatalf("Bea sieht %+v", got.Tasks)
	}
	if len(got.Lists) != 2 || got.Lists[1].ID != haus.ID || got.Lists[1].OwnerName != "Admin" {
		t.Fatalf("Beas Listen %+v", got.Lists)
	}
	id := fmt.Sprint(task.ID)
	for _, c := range []struct {
		name string
		h    http.HandlerFunc
		m    string
		body string
	}{
		{"ändern", s.handleUpdateTask, "PUT", fmt.Sprintf(`{"title":"Müll raus","list_id":%d}`, haus.ID)},
		{"abhaken", s.handleCompleteTask, "POST", ""},
	} {
		if rec := callAs(t, bea, c.h, c.m, "/", strings.NewReader(c.body), "id", id); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if rec := callAs(t, bea, s.handleCreateTask, "POST", "/", strings.NewReader(fmt.Sprintf(`{"title":"Spülen","list_id":%d}`, haus.ID))); rec.Code != http.StatusCreated {
		t.Fatalf("anlegen: %d %s", rec.Code, rec.Body)
	}
	// Management is owner-only
	for _, c := range []struct {
		name string
		h    http.HandlerFunc
		m    string
		body string
		pv   []string
	}{
		{"umbenennen", s.handleRenameList, "PUT", `{"name":"x"}`, []string{"id", fmt.Sprint(haus.ID)}},
		{"löschen", s.handleDeleteList, "DELETE", "", []string{"id", fmt.Sprint(haus.ID)}},
		{"weiter teilen", s.handleAddMember, "PUT", "", []string{"id", fmt.Sprint(haus.ID), "uid", fmt.Sprint(testUID)}},
	} {
		if rec := callAs(t, bea, c.h, c.m, "/", strings.NewReader(c.body), c.pv...); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d, erwartet 404", c.name, rec.Code)
		}
	}
}

func TestShareGuards(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	var def int64
	s.db.QueryRow("SELECT id FROM lists WHERE owner_id = ? AND is_default = 1", testUID).Scan(&def)
	for _, c := range []struct {
		name       string
		list, user int64
		want       int
	}{
		{"Standardliste", def, bea, http.StatusConflict},
		{"sich selbst", haus.ID, testUID, http.StatusBadRequest},
		{"unbekanntes Konto", haus.ID, 999, http.StatusNotFound},
		{"unbekannte Liste", 999, bea, http.StatusNotFound},
	} {
		if rec := share(t, s, testUID, c.list, c.user); rec.Code != c.want {
			t.Errorf("%s: %d, erwartet %d", c.name, rec.Code, c.want)
		}
	}
	// sharing twice is not an error
	share(t, s, testUID, haus.ID, bea)
	if rec := share(t, s, testUID, haus.ID, bea); rec.Code != http.StatusNoContent {
		t.Fatalf("zweites Teilen: %d", rec.Code)
	}
}

// Leaving (member) and removing (owner): 404 everywhere afterward.
func TestLeaveAndRemoveMember(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	cleo := mustUser(t, s, "Cleo")
	haus := mustList(t, s, "Haushalt")
	task := mustCreate(t, s, fmt.Sprintf(`{"title":"Müll","list_id":%d}`, haus.ID))
	share(t, s, testUID, haus.ID, bea)
	share(t, s, testUID, haus.ID, cleo)
	list := fmt.Sprint(haus.ID)
	// Bea may not remove Cleo, only herself
	if rec := callAs(t, bea, s.handleRemoveMember, "DELETE", "/", nil, "id", list, "uid", fmt.Sprint(cleo)); rec.Code != http.StatusNotFound {
		t.Fatalf("Mitglied entfernt anderes: %d", rec.Code)
	}
	if rec := callAs(t, bea, s.handleRemoveMember, "DELETE", "/", nil, "id", list, "uid", fmt.Sprint(bea)); rec.Code != http.StatusNoContent {
		t.Fatalf("verlassen: %d %s", rec.Code, rec.Body)
	}
	if rec := callAs(t, testUID, s.handleRemoveMember, "DELETE", "/", nil, "id", list, "uid", fmt.Sprint(cleo)); rec.Code != http.StatusNoContent {
		t.Fatalf("entfernen: %d %s", rec.Code, rec.Body)
	}
	for _, uid := range []int64{bea, cleo} {
		if rec := callAs(t, uid, s.handleCompleteTask, "POST", "/", nil, "id", fmt.Sprint(task.ID)); rec.Code != http.StatusNotFound {
			t.Errorf("früheres Mitglied %d hakt ab: %d", uid, rec.Code)
		}
	}
	if rec := callAs(t, testUID, s.handleRemoveMember, "DELETE", "/", nil, "id", list, "uid", fmt.Sprint(bea)); rec.Code != http.StatusNotFound {
		t.Fatalf("Nicht-Mitglied entfernen: %d", rec.Code)
	}
}

// Deleting a list and removing an account clean up memberships.
func TestSharingCleanup(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	mustCreate(t, s, fmt.Sprintf(`{"title":"Müll","list_id":%d}`, haus.ID))
	share(t, s, testUID, haus.ID, bea)
	beas := mustListAs(t, s, bea, "Beas")
	share(t, s, bea, beas.ID, testUID)
	if rec := callAs(t, testUID, s.handleDeleteList, "DELETE", "/?tasks=move", nil, "id", fmt.Sprint(haus.ID)); rec.Code != http.StatusNoContent {
		t.Fatalf("Liste löschen: %d %s", rec.Code, rec.Body)
	}
	var tasks []Task
	json.Unmarshal(callAs(t, bea, s.handleListTasks, "GET", "/", nil).Body.Bytes(), &struct{ Tasks *[]Task }{&tasks})
	if len(tasks) != 0 {
		t.Fatalf("Bea sieht nach dem Löschen %+v", tasks)
	}
	if err := deleteUser(s.db, bea); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM list_members").Scan(&n)
	if n != 0 {
		t.Fatalf("%d Mitgliedschaften übrig", n)
	}
}

func TestPeopleListsOthers(t *testing.T) {
	s := testServer(t)
	mustUser(t, s, "Bea")
	var got []Person
	json.Unmarshal(call(t, s.handlePeople, "GET", "/", nil).Body.Bytes(), &got)
	if len(got) != 1 || got[0].Name != "Bea" {
		t.Fatalf("people %+v", got)
	}
}

// A change in A's private list does not reach B, but one in the shared list does.
func TestChangedReachesOnlyParticipants(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	cleo := mustUser(t, s, "Cleo")
	haus := mustList(t, s, "Haushalt")
	share(t, s, testUID, haus.ID, bea)
	chB, chC := s.hub.subscribe(bea), s.hub.subscribe(cleo)
	drain := func(ch chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	drain(chB)
	drain(chC)
	mustCreate(t, s, fmt.Sprintf(`{"title":"geteilt","list_id":%d}`, haus.ID))
	if !drain(chB) || drain(chC) {
		t.Fatal("geteilte Änderung: Bea muss, Cleo darf nicht")
	}
	mustCreateAs(t, s, cleo, `{"title":"privat"}`)
	if drain(chB) {
		t.Fatal("Cleos private Änderung erreicht Bea")
	}
}

func notified(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Whoever loses access (list deleted, left, replaced by an import), and
// whoever shared the list with them, must reload — even though the
// membership is already gone by the time the write happens.
func TestLosingAccessIsNotified(t *testing.T) {
	setup := func(t *testing.T) (*server, int64, int64, List) {
		s := testServer(t)
		bea, cleo := mustUser(t, s, "Bea"), mustUser(t, s, "Cleo")
		haus := mustList(t, s, "Haushalt")
		share(t, s, testUID, haus.ID, bea)
		share(t, s, testUID, haus.ID, cleo)
		return s, bea, cleo, haus
	}
	t.Run("Liste gelöscht", func(t *testing.T) {
		s, bea, _, haus := setup(t)
		ch := s.hub.subscribe(bea)
		callAs(t, testUID, s.handleDeleteList, "DELETE", "/?tasks=move", nil, "id", fmt.Sprint(haus.ID))
		if !notified(ch) {
			t.Fatal("Bea nicht benachrichtigt")
		}
	})
	t.Run("verlassen", func(t *testing.T) {
		s, bea, cleo, haus := setup(t)
		owner, other := s.hub.subscribe(testUID), s.hub.subscribe(cleo)
		callAs(t, bea, s.handleRemoveMember, "DELETE", "/", nil, "id", fmt.Sprint(haus.ID), "uid", fmt.Sprint(bea))
		if !notified(owner) || !notified(other) {
			t.Fatal("Eigentümer oder übriges Mitglied nicht benachrichtigt")
		}
	})
	t.Run("Import ersetzt Listen", func(t *testing.T) {
		s, bea, _, _ := setup(t)
		ch := s.hub.subscribe(bea)
		if rec := postImport(t, s, exportData{}); rec.Code != http.StatusNoContent {
			t.Fatalf("Import: %d %s", rec.Code, rec.Body)
		}
		if !notified(ch) {
			t.Fatal("Bea nicht benachrichtigt")
		}
	})
}

// A link between a shared and a private task belongs to whoever can see
// both — a member cannot remove it.
func TestMemberCannotUnlinkHiddenLink(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	shared := mustCreate(t, s, fmt.Sprintf(`{"title":"geteilt","list_id":%d}`, haus.ID))
	private := mustCreate(t, s, `{"title":"privat"}`)
	linkTasks(s.db, testUID, shared.ID, private.ID)
	share(t, s, testUID, haus.ID, bea)
	rec := callAs(t, bea, s.handleRemoveLink, "DELETE", "/", nil, "id", fmt.Sprint(shared.ID), "other", fmt.Sprint(private.ID))
	var n int
	s.db.QueryRow("SELECT COUNT(*) FROM links").Scan(&n)
	if n != 1 {
		t.Fatalf("Verknüpfung gelöscht (Antwort %d)", rec.Code)
	}
}

// Accepting an invitation is lasting access the other way round: with
// Bea's stolen cookie, the thief accepts an invitation into a list of his
// own and moves her tasks there — they're his after "sign out everywhere"
// too. Like inviting, it needs a fresh login.
func TestAcceptingInviteNeedsFreshAuth(t *testing.T) {
	s := testServer(t)
	bea := mustUser(t, s, "Bea")
	haus := mustList(t, s, "Haushalt")
	if rec := invite(t, s, testUID, haus.ID, bea); rec.Code != http.StatusNoContent {
		t.Fatalf("einladen: %d %s", rec.Code, rec.Body)
	}
	rec := serveAuthed(s, s.handleAcceptInvite, staleRequest(t, s, bea, "POST", "", "id", fmt.Sprint(haus.ID)))
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body["reauth"] != true {
		t.Fatalf("annehmen mit alter Sitzung: %d %s, erwartet 403 reauth", rec.Code, rec.Body)
	}
	if listVisible(s.db, bea, haus.ID) {
		t.Fatal("Liste trotz 403 angenommen")
	}
	if rec := callAs(t, bea, s.handleAcceptInvite, "POST", "/", nil, "id", fmt.Sprint(haus.ID)); rec.Code != http.StatusNoContent {
		t.Fatalf("annehmen frisch angemeldet: %d %s", rec.Code, rec.Body)
	}
	if !listVisible(s.db, bea, haus.ID) {
		t.Fatal("Liste nach dem Annehmen nicht sichtbar")
	}
}
