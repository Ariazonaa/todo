package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Lists: every task belongs to exactly one. The default list (is_default)
// is created by the migration; it cannot be deleted, and its name is NULL
// until renamed (until then the catalog text lists.default is shown).
// Subtasks always live in their parent task's list — that is enforced in
// the writing statements themselves (createTask, handleUpdateTask).
type List struct {
	ID        int64    `json:"id"`
	Name      *string  `json:"name"`
	IsDefault bool     `json:"is_default"`
	OwnerID   int64    `json:"owner_id,omitempty"`
	OwnerName string   `json:"owner_name,omitempty"` // only set for other people's lists
	Members   []Person `json:"members,omitempty"`    // excludes the owner
}

// Person: an account as others see it (for sharing). Pending: invited, but
// not accepted yet.
type Person struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Pending bool   `json:"pending,omitempty"`
}

// Invite: a list someone shared with uid that uid hasn't accepted yet —
// until then it's not visible (list_access), and its tasks don't remind.
type Invite struct {
	ListID    int64   `json:"list_id"`
	Name      *string `json:"name"`
	OwnerName string  `json:"owner_name"`
}

func invitesOf(db *sql.DB, uid int64) ([]Invite, error) {
	rows, err := db.Query("SELECT l.id, l.name, u.name FROM list_members m JOIN lists l ON l.id = m.list_id "+
		"JOIN users u ON u.id = l.owner_id WHERE m.user_id = ? AND m.accepted_at IS NULL ORDER BY m.added_at, l.id", uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Invite{}
	for rows.Next() {
		var inv Invite
		var name sql.NullString
		if err := rows.Scan(&inv.ListID, &name, &inv.OwnerName); err != nil {
			return nil, err
		}
		if name.Valid {
			inv.Name = &name.String
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// errListInvalid: the given list no longer existed at write time.
var errListInvalid = errors.New("list missing")

// listLists: uid's lists (part 1: their own), default list first.
func listLists(db *sql.DB, uid int64) ([]List, error) {
	rows, err := db.Query("SELECT id, name, is_default FROM lists WHERE owner_id = ? ORDER BY is_default DESC, id", uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []List{}
	for rows.Next() {
		var l List
		var name sql.NullString
		var def int
		if err := rows.Scan(&l.ID, &name, &def); err != nil {
			return nil, err
		}
		if name.Valid {
			l.Name = &name.String
		}
		l.IsDefault = def != 0
		out = append(out, l)
	}
	return out, rows.Err()
}

// visibleListsOf: all lists uid can see — own lists first (default list at
// the front), then shared ones, ordered by ID; includes owner and members.
// The ordering also matters for "#Name": own lists win.
func visibleListsOf(db *sql.DB, uid int64) ([]List, error) {
	rows, err := db.Query("SELECT l.id, l.name, l.is_default, l.owner_id, u.name FROM lists l JOIN users u ON u.id = l.owner_id "+
		"WHERE l.id IN (SELECT list_id FROM list_access WHERE user_id = ?) ORDER BY l.owner_id != ?, l.is_default DESC, l.id", uid, uid)
	if err != nil {
		return nil, err
	}
	out := []List{}
	index := map[int64]int{}
	for rows.Next() {
		var l List
		var name sql.NullString
		var def int
		var owner string
		if err := rows.Scan(&l.ID, &name, &def, &l.OwnerID, &owner); err != nil {
			rows.Close()
			return nil, err
		}
		if name.Valid {
			l.Name = &name.String
		}
		l.IsDefault = def != 0
		if l.OwnerID != uid {
			l.OwnerName = owner
		}
		l.Members = []Person{}
		index[l.ID] = len(out)
		out = append(out, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mrows, err := db.Query("SELECT m.list_id, u.id, u.name, m.accepted_at IS NULL FROM list_members m JOIN users u ON u.id = m.user_id "+
		"WHERE m.list_id IN (SELECT list_id FROM list_access WHERE user_id = ?) ORDER BY u.name_key", uid)
	if err != nil {
		return nil, err
	}
	defer mrows.Close()
	for mrows.Next() {
		var list int64
		var p Person
		if err := mrows.Scan(&list, &p.ID, &p.Name, &p.Pending); err != nil {
			return nil, err
		}
		if i, ok := index[list]; ok {
			out[i].Members = append(out[i].Members, p)
		}
	}
	return out, mrows.Err()
}

// listVisible: uid is allowed to see the list (list_access).
func listVisible(db querier, uid, id int64) bool {
	var n int
	return db.QueryRow("SELECT COUNT(*) FROM list_access WHERE list_id = ? AND user_id = ?", id, uid).Scan(&n) == nil && n > 0
}

const (
	maxLists       = 50
	maxListNameLen = 40
)

// listKey: comparison key for list names. SQLite's NOCASE only folds ASCII
// ("Ärger" != "ärger") — the API and import therefore compare this key
// instead (column name_key, unique).
func listKey(name string) string { return strings.ToLower(name) }

// listName validates and normalizes a list name.
func listName(raw string) (string, error) {
	name := strings.TrimSpace(cleanName(raw))
	if n := utf8.RuneCountInString(name); n == 0 || n > maxListNameLen {
		return "", uerr("error.list_name")
	}
	return name, nil
}

func decodeListName(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return "", false
	}
	name, err := listName(req.Name)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return "", false
	}
	return name, true
}

func (s *server) handleListLists(w http.ResponseWriter, r *http.Request) {
	lists, err := visibleListsOf(s.db, userID(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	writeJSON(w, http.StatusOK, lists)
}

func (s *server) handleCreateList(w http.ResponseWriter, r *http.Request) {
	name, ok := decodeListName(w, r)
	if !ok {
		return
	}
	// Uniqueness and the upper limit are enforced in the INSERT itself —
	// otherwise two devices could create "Groceries" at the same time, or
	// the 51st list.
	uid := userID(r)
	// Name uniqueness and the per-person limit.
	res, err := s.db.Exec("INSERT INTO lists (owner_id, name, name_key, is_default, created_at) SELECT ?, ?, ?, 0, ? "+
		"WHERE NOT EXISTS (SELECT 1 FROM lists WHERE owner_id = ? AND name_key = ?) AND (SELECT COUNT(*) FROM lists WHERE owner_id = ?) < ?",
		uid, name, listKey(name), time.Now().UTC().Format(time.RFC3339), uid, listKey(name), uid, maxLists)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var count int
		s.db.QueryRow("SELECT COUNT(*) FROM lists WHERE owner_id = ?", uid).Scan(&count)
		if count >= maxLists {
			writeError(w, r, http.StatusBadRequest, "error.list_limit")
		} else {
			writeError(w, r, http.StatusBadRequest, "error.list_exists")
		}
		return
	}
	id, _ := res.LastInsertId()
	s.changed(userID(r))
	writeJSON(w, http.StatusCreated, List{ID: id, Name: &name})
}

func (s *server) listFromPath(w http.ResponseWriter, r *http.Request) (List, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return List{}, false
	}
	var l List
	var name sql.NullString
	var def int
	// Only manage own lists: other people's lists don't exist here (404).
	err = s.db.QueryRow("SELECT id, name, is_default FROM lists WHERE id = ? AND owner_id = ?", id, userID(r)).Scan(&l.ID, &name, &def)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "error.list_not_found")
		return List{}, false
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return List{}, false
	}
	if name.Valid {
		l.Name = &name.String
	}
	l.IsDefault = def != 0
	return l, true
}

func (s *server) handleRenameList(w http.ResponseWriter, r *http.Request) {
	l, ok := s.listFromPath(w, r)
	if !ok {
		return
	}
	name, ok := decodeListName(w, r)
	if !ok {
		return
	}
	uid := userID(r)
	res, err := s.db.Exec("UPDATE lists SET name = ?, name_key = ? WHERE id = ? AND owner_id = ? "+
		"AND NOT EXISTS (SELECT 1 FROM lists WHERE owner_id = ? AND name_key = ? AND id != ?)", name, listKey(name), l.ID, uid, uid, listKey(name), l.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if !listVisible(s.db, uid, l.ID) {
			writeError(w, r, http.StatusNotFound, "error.list_not_found")
		} else {
			writeError(w, r, http.StatusBadRequest, "error.list_exists")
		}
		return
	}
	// Open invitations show the name too, but changed doesn't reach them.
	s.changed(uid, userIDs(s.db, "SELECT user_id FROM list_members WHERE list_id = ?", l.ID)...)
	l.Name = &name
	writeJSON(w, http.StatusOK, l)
}

// handleDeleteList deletes a list. ?tasks=move moves its tasks to the
// default list, ?tasks=delete deletes them along with their subtasks,
// images, and links. Without a parameter, deletion only succeeds if the
// list is empty — so an old or buggy client never deletes anything without
// being asked.
func (s *server) handleDeleteList(w http.ResponseWriter, r *http.Request) {
	l, ok := s.listFromPath(w, r)
	if !ok {
		return
	}
	if l.IsDefault {
		writeError(w, r, http.StatusConflict, "error.list_default")
		return
	}
	mode := r.URL.Query().Get("tasks")
	if mode != "" && mode != "move" && mode != "delete" {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE list_id = ?", l.ID).Scan(&count); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if count > 0 && mode == "" {
		writeError(w, r, http.StatusConflict, "error.list_not_empty")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer tx.Rollback()
	// Members lose access to the list — record them before the change (in
	// the transaction, so nobody accepting in between is missed); afterward
	// they're no longer in list_access but still need to reload. Their links
	// into the list go with them.
	members := userIDs(tx, "SELECT user_id FROM list_members WHERE list_id = ?", l.ID)
	for _, m := range members {
		if err := unlinkAcross(tx, l.ID, m); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
	}
	const inList = "SELECT id FROM tasks WHERE list_id = ?"
	type stmt struct {
		sql  string
		args []any
	}
	var stmts []stmt
	if mode == "delete" {
		stmts = append(stmts,
			stmt{"DELETE FROM links WHERE a IN (" + inList + ") OR b IN (" + inList + ")", []any{l.ID, l.ID}},
			stmt{"DELETE FROM attachments WHERE task_id IN (" + inList + ")", []any{l.ID}},
			stmt{"DELETE FROM tasks WHERE list_id = ?", []any{l.ID}})
	}
	// Also for "move" and an empty list: anything another device added in
	// the meantime ends up in the default list instead of on a dead ID. Read
	// first: without a default list the tasks would get NULL, and the start-up
	// heal would hand them to the admin.
	var defaultList int64
	if err := tx.QueryRow("SELECT id FROM lists WHERE owner_id = ? AND is_default = 1", userID(r)).Scan(&defaultList); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	stmts = append(stmts,
		stmt{"UPDATE tasks SET list_id = ? WHERE list_id = ?", []any{defaultList, l.ID}},
		stmt{"DELETE FROM list_members WHERE list_id = ?", []any{l.ID}},
		stmt{"DELETE FROM lists WHERE id = ? AND owner_id = ? AND is_default = 0", []any{l.ID, userID(r)}})
	for _, st := range stmts {
		if _, err := tx.Exec(st.sql, st.args...); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r), members...)
	w.WriteHeader(http.StatusNoContent)
}

// --- Sharing ---

// handleAddMember: the owner invites another account to a list. It only
// becomes visible to them once they accept (handleAcceptInvite) — nobody
// gets someone else's tasks and reminders pushed on them. All rules (own
// list, not the default list, account exists, not the owner) are enforced
// in the INSERT itself. Already invited or a member: not an error. Needs a
// fresh login: with a stolen session cookie, sharing a list with an
// account of one's own would be access that survives "sign out everywhere".
func (s *server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	l, ok := s.listFromPath(w, r)
	if !ok {
		return
	}
	member, err := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return
	}
	uid := userID(r)
	switch {
	case l.IsDefault:
		writeError(w, r, http.StatusConflict, "error.list_default")
		return
	case member == uid:
		writeError(w, r, http.StatusBadRequest, "error.list_share_self")
		return
	}
	if !s.requireFreshAuth(w, r) {
		return
	}
	res, err := s.db.Exec("INSERT INTO list_members (list_id, user_id, added_at) SELECT l.id, u.id, ? FROM lists l, users u "+
		"WHERE l.id = ? AND l.owner_id = ? AND l.is_default = 0 AND u.id = ? AND u.id != l.owner_id "+
		"ON CONFLICT (list_id, user_id) DO NOTHING", time.Now().UTC().Format(time.RFC3339), l.ID, uid, member)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 && !isMember(s.db, l.ID, member) {
		// neither newly invited nor already invited: the list or account no longer exists
		if !listVisible(s.db, uid, l.ID) {
			writeError(w, r, http.StatusNotFound, "error.list_not_found")
		} else {
			writeError(w, r, http.StatusNotFound, "error.user_not_found")
		}
		return
	}
	// The invitee doesn't have access yet (changed wouldn't reach them), but
	// has a new invitation to show.
	s.changed(uid, member)
	w.WriteHeader(http.StatusNoContent)
}

// isMember: member is invited to list (accepted or not).
func isMember(db *sql.DB, list, member int64) bool {
	var n int
	return db.QueryRow("SELECT COUNT(*) FROM list_members WHERE list_id = ? AND user_id = ?", list, member).Scan(&n) == nil && n > 0
}

// handleAcceptInvite: uid accepts an invitation — from then on, the list is
// visible to them. Declining is leaving (handleRemoveMember with their own ID).
// Accepting needs a fresh login, like inviting: with a stolen session cookie
// the thief would accept an invitation into a list of his own and move the
// account's tasks there — they'd stay his after "sign out everywhere".
func (s *server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	list, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return
	}
	if !s.requireFreshAuth(w, r) {
		return
	}
	uid := userID(r)
	local := s.nowFor(uid) // before the transaction: may read settings through s.db
	today, hhmm := local.Format(dateFmt), local.Format("15:04")
	sentAt := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE list_members SET accepted_at = ? WHERE list_id = ? AND user_id = ? AND accepted_at IS NULL",
		sentAt, list, uid)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n, _ := res.RowsAffected(); n == 1 {
		// What was already due before joining counts as reported: otherwise
		// the first tick would ping every old overdue task of the list at once.
		// The morning digest still lists them.
		if _, err := tx.Exec("INSERT OR IGNORE INTO task_reminders (task_id, user_id, channel, due, sent_at) "+
			"SELECT t.id, ?, c.channel, t.due_date || ' ' || t.due_time, ? FROM tasks t, "+
			"(SELECT ? AS channel UNION ALL SELECT ?) c WHERE t.list_id = ? AND t.done = 0 AND t.due_time IS NOT NULL "+
			"AND (t.due_date < ? OR (t.due_date = ? AND t.due_time <= ?))",
			uid, sentAt, chDiscord, chPush, list, today, today, hhmm); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
	} else if !listVisible(tx, uid, list) {
		writeError(w, r, http.StatusNotFound, "error.list_not_found")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(uid)
	w.WriteHeader(http.StatusNoContent)
}

// handleRemoveMember: the owner removes a member, or a member leaves the
// list themselves (uid = the member). Who is allowed is enforced in the
// DELETE itself.
func (s *server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	list, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	member, err2 := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err1 != nil || err2 != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return
	}
	uid := userID(r)
	// Notification is routed via the owner: their list still exists, so
	// changed reaches the remaining members through it; the departing
	// member is added on top.
	var owner int64
	s.db.QueryRow("SELECT owner_id FROM lists WHERE id = ?", list).Scan(&owner)
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec("DELETE FROM list_members WHERE list_id = ? AND user_id = ? "+
		"AND (user_id = ? OR list_id IN (SELECT id FROM lists WHERE owner_id = ?))", list, member, uid, uid)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, r, http.StatusNotFound, "error.list_not_found")
		return
	}
	// Links between the list and the leaving member's own tasks: afterward
	// nobody could see (or remove) them anymore.
	if err := unlinkAcross(tx, list, member); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if owner == 0 {
		owner = uid
	}
	s.changed(owner, member, uid)
	w.WriteHeader(http.StatusNoContent)
}

// handlePeople: the instance's other accounts (names, for sharing).
func (s *server) handlePeople(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query("SELECT id, name FROM users WHERE id != ? ORDER BY name_key", userID(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer rows.Close()
	out := []Person{}
	for rows.Next() {
		var p Person
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		out = append(out, p)
	}
	if rows.Err() != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// userIDs: the account IDs returned by query (for notifications; on error
// returns whatever was gathered so far).
func userIDs(db querier, query string, args ...any) []int64 {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}
