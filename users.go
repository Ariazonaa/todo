package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SQL building blocks for visibility. visibleLists is the single place that
// extends part 2 (shared lists) via the list_access view; ownedLists is for
// the scheduler and backup, which only deal with a person's own lists.
const (
	visibleLists = "IN (SELECT list_id FROM list_access WHERE user_id = ?)"
	ownedLists   = "IN (SELECT id FROM lists WHERE owner_id = ?)"
)

type ctxKey int

const userKey ctxKey = 1

// withUser stores the logged-in person on the request (requireAuth).
func withUser(r *http.Request, uid int64) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userKey, uid))
}

// userID: the logged-in person; 0 when not logged in — then there is no data.
func userID(r *http.Request) int64 {
	uid, _ := r.Context().Value(userKey).(int64)
	return uid
}

// errSetupDone: by the time setup ran, an account already existed.
var errSetupDone = errors.New("setup done")

// User: an account as shown by the API.
type User struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	IsAdmin     bool   `json:"is_admin"`
	HasPasskey  bool   `json:"has_passkey"`
	CodePending bool   `json:"code_pending"`
}

const maxUserNameLen = 40

func userName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if n := utf8.RuneCountInString(name); n == 0 || n > maxUserNameLen {
		return "", uerr("error.user_name")
	}
	return name, nil
}

func newWebAuthnID() []byte {
	b := make([]byte, 32)
	rand.Read(b)
	return b
}

// createUser creates an account along with a default list. The first
// account (admin) takes over ownerless lists from an instance predating
// accounts.
func createUser(ex execer, name string, admin bool, webauthnID []byte) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := ex.Exec("INSERT INTO users (name, name_key, is_admin, webauthn_id, created_at) VALUES (?, ?, ?, ?, ?)",
		name, strings.ToLower(name), boolInt(admin), webauthnID, now)
	if err != nil {
		return 0, err
	}
	uid, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if admin {
		if _, err := ex.Exec("UPDATE lists SET owner_id = ? WHERE owner_id IS NULL", uid); err != nil {
			return 0, err
		}
	}
	_, err = ex.Exec("INSERT INTO lists (owner_id, name, is_default, created_at) SELECT ?, NULL, 1, ? "+
		"WHERE NOT EXISTS (SELECT 1 FROM lists WHERE owner_id = ? AND is_default = 1)", uid, now, uid)
	return uid, err
}

func userIsAdmin(db *sql.DB, uid int64) bool {
	var admin int
	return db.QueryRow("SELECT is_admin FROM users WHERE id = ?", uid).Scan(&admin) == nil && admin == 1
}

const setupCodeTTL = 7 * 24 * time.Hour

// setUserCode generates a new setup code (the old one becomes invalid) and
// stores only its hash.
func setUserCode(db *sql.DB, uid int64) (string, error) {
	code := newSetupCode()
	_, err := db.Exec("UPDATE users SET setup_hash = ?, setup_expires = ? WHERE id = ?",
		hashToken(normalizeSetupCode(code)), time.Now().Add(setupCodeTTL).UTC().Format(time.RFC3339), uid)
	return code, err
}

// userForCode: the account with this pending, unexpired code.
func userForCode(db *sql.DB, code string) (uid int64, handle []byte, name string, ok bool) {
	norm := normalizeSetupCode(code)
	if len(norm) != 16 {
		return 0, nil, "", false
	}
	err := db.QueryRow("SELECT id, webauthn_id, name FROM users WHERE setup_hash = ? AND setup_expires > ?",
		hashToken(norm), time.Now().UTC().Format(time.RFC3339)).Scan(&uid, &handle, &name)
	return uid, handle, name, err == nil
}

// deleteUser removes an account along with everything it owns — one transaction.
func deleteUser(db *sql.DB, uid int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const own = "SELECT id FROM tasks WHERE list_id IN (SELECT id FROM lists WHERE owner_id = ?)"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM links WHERE a IN (" + own + ") OR b IN (" + own + ")", []any{uid, uid}},
		{"DELETE FROM attachments WHERE task_id IN (" + own + ")", []any{uid}},
		{"DELETE FROM tasks WHERE list_id IN (SELECT id FROM lists WHERE owner_id = ?)", []any{uid}},
		{"DELETE FROM list_members WHERE user_id = ? OR list_id IN (SELECT id FROM lists WHERE owner_id = ?)", []any{uid, uid}},
		{"DELETE FROM lists WHERE owner_id = ?", []any{uid}},
		{"DELETE FROM credentials WHERE user_id = ?", []any{uid}},
		{"DELETE FROM sessions WHERE user_id = ?", []any{uid}},
		{"DELETE FROM push_subscriptions WHERE user_id = ?", []any{uid}},
		{"DELETE FROM user_settings WHERE user_id = ?", []any{uid}},
		{"DELETE FROM stats WHERE user_id = ?", []any{uid}},
		{"DELETE FROM task_reminders WHERE user_id = ?", []any{uid}},
		{"DELETE FROM users WHERE id = ? AND is_admin = 0", []any{uid}},
	} {
		if _, err := tx.Exec(q.sql, q.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// requireAdmin: only the admin manages accounts; for everyone else, the
// admin area doesn't exist (404, no information given).
func (s *server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !userIsAdmin(s.db, userID(r)) {
		writeError(w, r, http.StatusNotFound, "error.user_not_found")
		return false
	}
	return true
}

func (s *server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	rows, err := s.db.Query("SELECT u.id, u.name, u.is_admin, EXISTS (SELECT 1 FROM credentials c WHERE c.user_id = u.id), "+
		"u.setup_hash IS NOT NULL AND u.setup_expires > ? FROM users u ORDER BY u.id", time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var admin, pk, pending int
		if err := rows.Scan(&u.ID, &u.Name, &admin, &pk, &pending); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		u.IsAdmin, u.HasPasskey, u.CodePending = admin == 1, pk == 1, pending == 1
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	name, err := userName(req.Name)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	uid, err := func() (int64, error) {
		tx, err := s.db.Begin()
		if err != nil {
			return 0, err
		}
		defer tx.Rollback()
		uid, err := createUser(tx, name, false, newWebAuthnID())
		if err != nil {
			return 0, err
		}
		return uid, tx.Commit()
	}()
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, r, http.StatusBadRequest, "error.user_exists")
		} else {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
		}
		return
	}
	code, err := setUserCode(s.db, uid)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.hub.notifyAll()
	writeJSON(w, http.StatusCreated, map[string]any{
		"user": User{ID: uid, Name: name, CodePending: true},
		"code": code,
	})
}

// handleNewUserCode: a new setup code (the old one becomes invalid) — only
// for accounts that don't have a passkey yet.
func (s *server) handleNewUserCode(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return
	}
	var exists, creds int
	if err := s.db.QueryRow("SELECT (SELECT COUNT(*) FROM users WHERE id = ?), (SELECT COUNT(*) FROM credentials WHERE user_id = ?)", id, id).Scan(&exists, &creds); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if exists == 0 {
		writeError(w, r, http.StatusNotFound, "error.user_not_found")
		return
	}
	if creds > 0 {
		writeError(w, r, http.StatusConflict, "error.user_has_passkey")
		return
	}
	code, err := setUserCode(s.db, id)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"code": code})
}

// handleDeleteUser removes an account along with everything that belongs
// to it. Requires a fresh login (like managing passkeys); never the
// caller's own account.
func (s *server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return
	}
	if !s.requireFreshAuth(w, r) {
		return
	}
	if id == userID(r) {
		writeError(w, r, http.StatusConflict, "error.user_self")
		return
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users WHERE id = ? AND is_admin = 0", id).Scan(&n); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n == 0 {
		writeError(w, r, http.StatusNotFound, "error.user_not_found")
		return
	}
	if err := deleteUser(s.db, id); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.store.forget(id)
	s.hub.notifyAll()
	w.WriteHeader(http.StatusNoContent)
}

// handleSetupMode tells the setup page whether it's setting up the
// instance (first account, code from the server log) or an account using
// a code from the admin.
func (s *server) handleSetupMode(w http.ResponseWriter, r *http.Request) {
	first, err := s.setupMode()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"first": first})
}
