package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	sessionCookie = "session"
	// Idle timeout, no fixed expiration date: as long as the app is used, the
	// session keeps renewing (see maybeRenewSession). An abandoned or stolen
	// cookie, on the other hand, dies after 30 days — it used to be 180 days
	// from login regardless of use.
	sessionIdleTimeout = 30 * 24 * time.Hour
	// sessionRetireGrace: how long a session replaced by a new login stays
	// valid (retireSession). renewedAuth does not renew sessions with this
	// little time left — otherwise a final request could bring the replaced
	// session back to life for another 30 days.
	sessionRetireGrace = time.Minute
	maxTitleLen        = 500
	maxNoteLen         = 5000
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Tasks, settings (webhook token) and passkey lists have no business in
	// the browser cache — they'd still be there after logging out.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeError responds with {"error": "<message>"} — key is a catalog key,
// translated into the request's language (requestLang).
func writeError(w http.ResponseWriter, r *http.Request, code int, key string, kv ...any) {
	writeJSON(w, code, map[string]string{"error": T(requestLang(r), key, kv...)})
}

// sameOrigin blocks same-site CSRF (SameSite=Lax does not protect against
// *.example.com siblings): browsers always send Origin on cross-origin POSTs;
// if the header is missing (curl, same-origin GET), we let it through.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

func (s *server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && !sameOrigin(r) {
			writeError(w, r, http.StatusForbidden, "error.origin")
			return
		}
		uid, ok := s.renewedAuth(w, r)
		if !ok {
			target := "/login"
			if none, err := s.noPasskeys(); err == nil && none {
				target = "/setup"
			}
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			} else {
				http.Redirect(w, r, target, http.StatusFound)
			}
			return
		}
		r = withUser(r, uid)
		if l := r.Header.Get("X-Lang"); l != "" {
			s.store.noteLang(uid, strings.ToLower(strings.TrimSpace(l)))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) authed(r *http.Request) bool {
	token := s.sessionToken(r)
	return token != "" && sessionValid(s.db, token)
}

// sessionToken returns the token of the session making the request.
func (s *server) sessionToken(r *http.Request) string {
	return s.cookie(r, sessionCookie)
}

// renewedAuth checks the session, returns its user, and pushes its
// expiration date forward while doing so.
func (s *server) renewedAuth(w http.ResponseWriter, r *http.Request) (int64, bool) {
	token := s.sessionToken(r)
	uid, expires, ok := sessionUser(s.db, token)
	if !ok {
		return 0, false
	}
	// Only renew once less than half the lifetime is left: otherwise every
	// request would write to the single DB connection. And no longer once
	// within the grace period of a replaced session (sessionRetireGrace).
	if left := time.Until(expires); left > sessionIdleTimeout/2 || left <= sessionRetireGrace {
		return uid, true
	}
	if err := renewSession(s.db, token, time.Now().Add(sessionIdleTimeout)); err != nil {
		return uid, true // renewing is a convenience, not a reason to reject
	}
	s.setSessionCookie(w, token)
	return uid, true
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// startSession creates a token + cookie after a successful passkey
// login/setup. credentialID is the passkey used to log in, uid its user.
func (s *server) startSession(w http.ResponseWriter, credentialID, uid int64) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	if err := createSession(s.db, token, time.Now().Add(sessionIdleTimeout), credentialID, uid); err != nil {
		return err
	}
	s.setSessionCookie(w, token)
	return nil
}

func (s *server) setSessionCookie(w http.ResponseWriter, token string) {
	s.setCookie(w, sessionCookie, token, int(sessionIdleTimeout.Seconds()))
}

// cookieName: with Secure, the app's cookies carry the __Host- prefix. The
// browser only accepts such a cookie from its own host (Secure, Path=/, no
// Domain). Without the prefix, any subdomain of the same domain could set a
// same-named cookie for the whole domain; it took precedence over the real
// one, and the app kept responding 401 — again after every re-login. Without
// Secure (INSECURE_COOKIE, local over http) the browser would reject the
// prefix.
func (s *server) cookieName(name string) string {
	if s.cfg.insecureCookie {
		return name
	}
	return "__Host-" + name
}

// cookie reads an app cookie; "" if it is missing.
func (s *server) cookie(r *http.Request, name string) string {
	if c, err := r.Cookie(s.cookieName(name)); err == nil {
		return c.Value
	}
	return ""
}

// setCookie sets one of the app's cookies — all with the same flags. maxAge
// < 0 deletes it.
func (s *server) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(name),
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   !s.cfg.insecureCookie,
	})
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := s.sessionToken(r); token != "" {
		deleteSession(s.db, token)
	}
	s.setCookie(w, sessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// nowFor: the current time in uid's timezone — "today", overdue status and
// stats days are all computed in it.
func (s *server) nowFor(uid int64) time.Time {
	return time.Now().In(s.store.location(uid))
}

func (s *server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	// Also all push devices: notifications carry task titles. Whoever logs
	// back in afterwards has their device re-register itself (app.js). One
	// transaction: otherwise a crash in between could delete sessions but
	// leave devices behind (or vice versa).
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer tx.Rollback()
	// Only the user's own sessions and devices.
	for _, q := range []string{"DELETE FROM sessions WHERE user_id = ?", "DELETE FROM push_subscriptions WHERE user_id = ?"} {
		if _, err := tx.Exec(q, userID(r)); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.setCookie(w, sessionCookie, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// settingsResponse: the user's own settings plus who they are (the app only
// shows admin parts to the admin).
type settingsResponse struct {
	appSettings
	Me User `json:"me"`
}

func (s *server) settingsOf(uid int64) settingsResponse {
	resp := settingsResponse{appSettings: s.store.get(uid)}
	var admin int
	s.db.QueryRow("SELECT id, name, is_admin FROM users WHERE id = ?", uid).Scan(&resp.Me.ID, &resp.Me.Name, &admin)
	resp.Me.IsAdmin = admin == 1
	return resp
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsOf(userID(r)))
}

func (s *server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req appSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	if err := validateSettings(req); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	uid := userID(r)
	if err := s.store.save(uid, req); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	// The archive retention period applies to the whole instance: only the admin sets it.
	if userIsAdmin(s.db, uid) {
		if err := s.store.saveArchiveDays(req.ArchiveDays); err != nil {
			writeErr(w, r, http.StatusBadRequest, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.settingsOf(uid))
}

// handleSetLanguage sets the language on its own — the settings form
// (handlePutSettings) leaves it untouched.
func (s *server) handleSetLanguage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	var req struct {
		Language string `json:"language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	if err := s.store.saveLanguage(userID(r), req.Language); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.settingsOf(userID(r)))
}

func (s *server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	url := s.store.get(userID(r)).WebhookURL
	if url == "" {
		writeError(w, r, http.StatusBadRequest, "error.no_webhook")
		return
	}
	if err := postDiscord(url, T(requestLang(r), "notify.discord_test")); err != nil {
		writeError(w, r, http.StatusBadGateway, "error.webhook_unreachable", "detail", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	now := s.nowFor(userID(r))
	uid := userID(r)
	tasks, err := listTasks(s.db, uid, now)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	lists, err := visibleListsOf(s.db, uid)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": tasks,
		"today": now.Format(dateFmt),
		"lists": lists,
	})
}

type taskReq struct {
	Title      string `json:"title"`
	DueDate    string `json:"due_date"`
	DueTime    string `json:"due_time"`
	Recurrence string `json:"recurrence"`
	Note       string `json:"note"`
	Pinned     bool   `json:"pinned"`
	InProgress bool   `json:"in_progress"`
	// Priority nil = not sent: 0 when creating, unchanged when updating — a
	// tab with an old app.js would otherwise silently reset it.
	Priority *int `json:"priority"`
	// ListID nil = not sent: the default list when creating, unchanged when
	// updating. Does not apply to subtasks.
	ListID   *int64 `json:"list_id"`
	ParentID *int64 `json:"parent_id"`
}

// validateTaskReq validates and normalizes the fields; selfID 0 = new task.
// errKey is a catalog key for a user-facing message (400), err a genuine
// server error (500) —
// the two must not get mixed up, or the app would report "task with
// subtasks ..." instead of an error on a DB outage.
func (s *server) validateTaskReq(req *taskReq, uid, selfID int64) (dueDate, dueTime *string, errKey string, err error) {
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		return nil, nil, "error.title_missing", nil
	}
	if utf8.RuneCountInString(req.Title) > maxTitleLen {
		return nil, nil, "error.title_too_long", nil
	}
	if utf8.RuneCountInString(req.Note) > maxNoteLen {
		return nil, nil, "error.note_too_long", nil
	}
	if req.DueDate != "" {
		if _, err := time.Parse(dateFmt, req.DueDate); err != nil {
			return nil, nil, "error.date_invalid", nil
		}
		dueDate = &req.DueDate
	}
	if req.DueTime != "" {
		if dueDate == nil {
			return nil, nil, "error.time_needs_date", nil
		}
		parsed, err := time.Parse("15:04", req.DueTime)
		if err != nil {
			return nil, nil, "error.time_invalid", nil
		}
		// store normalized: Go also accepts "9:30", which would break all
		// lexical wall-clock-time comparisons
		norm := parsed.Format("15:04")
		dueTime = &norm
	}
	switch req.Recurrence {
	case "", "daily", "weekly", "monthly":
	default:
		return nil, nil, "error.recurrence_invalid", nil
	}
	if req.Recurrence != "" && dueDate == nil {
		return nil, nil, "error.recurrence_needs_date", nil
	}
	if req.Priority != nil && (*req.Priority < 0 || *req.Priority > 3) {
		return nil, nil, "error.priority_invalid", nil
	}
	if req.ListID != nil && req.ParentID == nil && !listVisible(s.db, uid, *req.ListID) {
		return nil, nil, "error.list_not_found", nil
	}
	if req.ParentID != nil {
		if *req.ParentID == selfID {
			return nil, nil, "error.parent_self", nil
		}
		parent, err := taskFor(s.db, uid, *req.ParentID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, "error.parent_missing", nil
		}
		if err != nil {
			return nil, nil, "", err
		}
		// only one level deep
		if parent.ParentID != nil {
			return nil, nil, "error.parent_nested", nil
		}
		if selfID > 0 {
			subs, err := hasSubs(s.db, selfID)
			if err != nil {
				return nil, nil, "", err
			}
			if subs {
				return nil, nil, "error.parent_has_subs", nil
			}
		}
		if req.Recurrence != "" {
			return nil, nil, "error.sub_recurrence", nil
		}
		req.Pinned = false
		req.InProgress = false
	}
	return dueDate, dueTime, "", nil
}

func (s *server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var req taskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	uid := userID(r)
	dueDate, dueTime, msg, err := s.validateTaskReq(&req, uid, 0)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if msg != "" {
		writeError(w, r, http.StatusBadRequest, msg)
		return
	}
	t, err := createTask(s.db, uid, req.Title, dueDate, dueTime, req.Recurrence, req.Note, req.Pinned, derefInt(req.Priority), req.ListID, req.ParentID)
	if errors.Is(err, errListInvalid) {
		writeError(w, r, http.StatusBadRequest, "error.list_not_found")
		return
	}
	if errors.Is(err, errParentInvalid) {
		writeError(w, r, http.StatusConflict, msgParentChanged)
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if err := s.reopenParentOf(uid, t.ParentID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	s.respondTask(w, userID(r), http.StatusCreated, t)
}

// msgParentChanged: validateTaskReq saw a valid parent task, but by the time
// of the write it was gone or had itself become a subtask (second device).
const msgParentChanged = "error.parent_changed"

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// ptrEq: both nil, or both set with the same value.
func ptrEq[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s *server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var req taskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	uid := userID(r)
	dueDate, dueTime, msg, err := s.validateTaskReq(&req, uid, t.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if msg != "" {
		writeError(w, r, http.StatusBadRequest, msg)
		return
	}
	q := "UPDATE tasks SET title = ?, due_date = ?, due_time = ?, recurrence = ?, note = ?, pinned = ?, in_progress = ?, priority = COALESCE(?, priority), " +
		// Subtasks follow their parent task's list; without list_id the list stays unchanged.
		"list_id = CASE WHEN ? IS NOT NULL THEN (SELECT p.list_id FROM tasks p WHERE p.id = ?) ELSE COALESCE(?, list_id) END, " +
		"parent_id = ?"
	args := []any{req.Title, dueDate, dueTime, req.Recurrence, req.Note, boolInt(req.Pinned), boolInt(req.InProgress), req.Priority,
		req.ParentID, req.ParentID, req.ListID, req.ParentID}
	// Due date changed -> re-arm the reminder (even back to a time already
	// reported): clear the stamp, below in the same transaction.
	dueChanged := !ptrEq(t.DueDate, dueDate) || !ptrEq(t.DueTime, dueTime)
	// Different parent task (or moved out of one): goes to the end of the new
	// list, same as on creation — the old position came from a different set
	// of siblings.
	if !ptrEq(t.ParentID, req.ParentID) {
		q += ", sort_order = (SELECT COALESCE(MAX(sort_order), 0) + 10 FROM tasks WHERE parent_id IS ? AND id != ?)"
		args = append(args, req.ParentID, t.ID)
	}
	q += " WHERE id = ? AND list_id " + visibleLists
	args = append(args, t.ID, uid)
	if req.ListID != nil && req.ParentID == nil {
		// Check the list within the write itself: a second device may have just
		// deleted it.
		q += " AND ? " + visibleLists
		args = append(args, *req.ListID, uid)
	}
	if req.ParentID != nil {
		// Enforce the one-level rule within the write itself, not just in
		// validateTaskReq: a second device could reparent the parent task or
		// give this task a subtask in between.
		q += " AND EXISTS (SELECT 1 FROM tasks WHERE id = ? AND parent_id IS NULL AND list_id " + visibleLists + ") AND NOT EXISTS (SELECT 1 FROM tasks WHERE parent_id = ?)"
		args = append(args, *req.ParentID, uid, t.ID)
	}
	// A task and its subtasks change list together.
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec(q, args...)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		tx.Rollback() // single connection: release first, then look it up via s.db
		if _, err := taskFor(s.db, uid, t.ID); errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, "error.task_not_found")
		} else if req.ListID != nil && req.ParentID == nil && !listVisible(s.db, uid, *req.ListID) {
			writeError(w, r, http.StatusBadRequest, "error.list_not_found")
		} else {
			writeError(w, r, http.StatusConflict, msgParentChanged)
		}
		return
	}
	if _, err := tx.Exec("UPDATE tasks SET list_id = (SELECT list_id FROM tasks WHERE id = ?) WHERE parent_id = ?", t.ID, t.ID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if dueChanged {
		if err := clearReminders(tx, t.ID); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	t, err = getTask(s.db, t.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if !t.Done {
		if err := s.reopenParentOf(uid, t.ParentID); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
	}
	s.changed(userID(r))
	s.respondTask(w, userID(r), http.StatusOK, t)
}

func (s *server) handleSnoozeTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Days            int    `json:"days"`
		ExpectedDueDate string `json:"expected_due_date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Days < 1 || req.Days > 365 {
		writeError(w, r, http.StatusBadRequest, "error.snooze_days")
		return
	}
	// Optional: the due date the snooze is relative to (buttons on a
	// notification, see handleCompleteTask). tasks.id gets reused after
	// deletion — a day-old notification must not silently snooze a different,
	// newer task.
	if req.ExpectedDueDate != "" {
		cur := ""
		if t.DueDate != nil {
			cur = *t.DueDate
		}
		if cur != req.ExpectedDueDate {
			writeError(w, r, http.StatusConflict, "error.task_changed")
			return
		}
	}
	today := s.nowFor(userID(r)).Format(dateFmt)
	base := today
	if t.DueDate != nil && *t.DueDate > today {
		base = *t.DueDate
	}
	bt, err := time.Parse(dateFmt, base)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	newDate := bt.AddDate(0, 0, req.Days).Format(dateFmt)
	// Reopen and snooze together or not at all: doing them separately could
	// leave a reopened but unsnoozed task behind on error.
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer tx.Rollback()
	// A completed task becomes open again after snoozing — so also roll back
	// the daily counter, or the next completion would count it twice.
	uid := userID(r)
	if err := s.reopenDone(tx, uid, t); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	// Snoozing means "I'll do it later" -> out of "in progress". The
	// expected_due_date check is additionally placed here in the WHERE clause
	// (not just in the check above): otherwise a concurrent request could
	// interfere (rules belong in the write statement itself).
	res, err := tx.Exec("UPDATE tasks SET due_date = ?, in_progress = 0 WHERE id = ? AND list_id "+visibleLists+" AND (? = '' OR due_date IS ?)",
		newDate, t.ID, uid, req.ExpectedDueDate, req.ExpectedDueDate)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if n, _ := res.RowsAffected(); n != 1 && req.ExpectedDueDate != "" {
		writeError(w, r, http.StatusConflict, "error.task_changed")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	// After the commit: reopenParentOf goes through s.db (single connection).
	if err := s.reopenParentOf(uid, t.ParentID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	t, err = getTask(s.db, t.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	s.respondTask(w, userID(r), http.StatusOK, t)
}

func (s *server) handleAddLink(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Other int64 `json:"other"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Other == t.ID {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	if _, err := taskFor(s.db, userID(r), req.Other); errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "error.task_not_found")
		return
	} else if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if err := linkTasks(s.db, userID(r), t.ID, req.Other); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleRemoveLink(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	other, err := strconv.ParseInt(r.PathValue("other"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return
	}
	if err := unlinkTasks(s.db, userID(r), t.ID, other); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	w.WriteHeader(http.StatusNoContent)
}

// handleToggleProgress: move a task into / out of the "in progress" section.
func (s *server) handleToggleProgress(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	if t.ParentID != nil {
		writeError(w, r, http.StatusBadRequest, "error.sub_progress")
		return
	}
	if _, err := s.db.Exec("UPDATE tasks SET in_progress = ? WHERE id = ? AND list_id "+visibleLists, boolInt(req.On), t.ID, userID(r)); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	t, err := getTask(s.db, t.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	s.respondTask(w, userID(r), http.StatusOK, t)
}

// handleReorder sets the manual ordering: the client sends the IDs of a list
// (section or subtasks) in display order.
func (s *server) handleReorder(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 || len(req.IDs) > 1000 {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	// check for duplicates
	seen := make(map[int64]bool)
	for _, id := range req.IDs {
		if seen[id] {
			writeError(w, r, http.StatusBadRequest, "error.duplicate_ids")
			return
		}
		seen[id] = true
	}
	// Gaps of 10: neighboring lists keep their relative order among each
	// other. In one transaction, or the list could be left half-reordered on
	// error.
	tx, err := s.db.Begin()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	defer tx.Rollback()
	for i, id := range req.IDs {
		// IDs come from the client: only reorder the user's own (visible) tasks.
		if _, err := tx.Exec("UPDATE tasks SET sort_order = ? WHERE id = ? AND list_id "+visibleLists, (i+1)*10, id, userID(r)); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	now := s.nowFor(userID(r))
	today := now.Format(dateFmt)
	counts := map[string]int{}
	rows, err := s.db.Query("SELECT date, done FROM stats WHERE user_id = ?", userID(r))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	for rows.Next() {
		var d string
		var n int
		if err := rows.Scan(&d, &n); err != nil {
			rows.Close()
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		counts[d] = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	rows.Close()
	week, streak := weekAndStreak(counts, now)
	var open, done, inProgress int
	if err := s.db.QueryRow("SELECT COALESCE(SUM(done = 0), 0), COALESCE(SUM(done = 1), 0), "+
		"COALESCE(SUM(done = 0 AND in_progress = 1 AND parent_id IS NULL), 0) FROM tasks WHERE list_id "+visibleLists, userID(r)).Scan(&open, &done, &inProgress); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{
		"today":       counts[today],
		"week":        week,
		"streak":      streak,
		"open":        open,
		"done":        done,
		"in_progress": inProgress,
	})
}

// weekAndStreak counts by calendar day, computed from 12:00 that day: on
// wall-clock time, "minus one day" from 00:30 would skip a whole day in
// zones with a DST change at midnight (e.g. America/Santiago).
func weekAndStreak(counts map[string]int, now time.Time) (week, streak int) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
	monday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	for i := range 7 {
		week += counts[monday.AddDate(0, 0, i).Format(dateFmt)]
	}
	d := day
	if counts[d.Format(dateFmt)] == 0 {
		d = d.AddDate(0, 0, -1) // nothing done today yet doesn't count as a break
	}
	for counts[d.Format(dateFmt)] > 0 {
		streak++
		d = d.AddDate(0, 0, -1)
	}
	return week, streak
}

func (s *server) taskFromPath(w http.ResponseWriter, r *http.Request) (Task, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_id")
		return Task{}, false
	}
	// Other people's tasks don't exist for this user: 404, no further detail.
	t, err := taskFor(s.db, userID(r), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "error.task_not_found")
		return Task{}, false
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return Task{}, false
	}
	return t, true
}

func (s *server) handleCompleteTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	// Optional: the due date the client is completing from. If it no longer
	// matches (double tap, retry after a lost response), the task was already
	// advanced — then just return the current state instead of skipping
	// another occurrence.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		ExpectedDueDate string `json:"expected_due_date"`
	}
	json.NewDecoder(r.Body).Decode(&req) // an empty body is fine

	uid := userID(r)
	today := s.nowFor(userID(r)).Format(dateFmt)
	if t.Recurrence != "" && t.DueDate != nil {
		alreadyMoved := req.ExpectedDueDate != "" && req.ExpectedDueDate != *t.DueDate
		if !alreadyMoved {
			next, err := nextOccurrence(*t.DueDate, t.Recurrence, today)
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "error.internal")
				return
			}
			// The new due date has no reminder stamp, so it will fire again. The
			// due_date in the WHERE clause catches concurrent requests (0 rows =
			// already advanced).
			res, err := s.db.Exec("UPDATE tasks SET due_date = ?, in_progress = 0 WHERE id = ? AND list_id "+visibleLists+" AND done = 0 AND due_date = ?", next, t.ID, uid, *t.DueDate)
			if err != nil {
				writeError(w, r, http.StatusInternalServerError, "error.internal")
				return
			}
			if n, _ := res.RowsAffected(); n == 1 {
				if err := bumpStat(s.db, uid, today, 1); err != nil {
					log.Printf("stats: %v", err)
				}
				// Checklist pattern: a new occurrence starts with fresh subtasks.
				// Dated ones shift by the same offset as the parent task — with the
				// old date they'd be immediately due again (ping) and overdue every
				// day starting tomorrow (digest).
				shift := fmt.Sprintf("%+d days", daysBetween(*t.DueDate, next))
				if _, err := s.db.Exec("UPDATE tasks SET done = 0, completed_at = NULL, "+
					"due_date = CASE WHEN due_date IS NULL THEN NULL ELSE date(due_date, ?) END WHERE parent_id = ? AND list_id "+visibleLists, shift, t.ID, uid); err != nil {
					log.Printf("recurrence: subtasks of %d: %v", t.ID, err)
				}
			}
		}
	} else {
		// Same protection as snoozing (handleSnoozeTask): a notification with an
		// old due date must not silently complete a different, newer task
		// because tasks.id gets reused. Without expected_due_date (in-app use)
		// no check applies, as before.
		if req.ExpectedDueDate != "" {
			cur := ""
			if t.DueDate != nil {
				cur = *t.DueDate
			}
			if cur != req.ExpectedDueDate {
				writeError(w, r, http.StatusConflict, "error.task_changed")
				return
			}
		}
		completed := time.Now().UTC().Format(time.RFC3339)
		// Check additionally placed in the WHERE clause: closes the gap between
		// reading (above) and writing against a concurrent request.
		res, err := s.db.Exec("UPDATE tasks SET done = 1, completed_at = ?, in_progress = 0 WHERE id = ? AND list_id "+visibleLists+" AND done = 0 AND (? = '' OR due_date IS ?)",
			completed, t.ID, uid, req.ExpectedDueDate, req.ExpectedDueDate)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			if err := bumpStat(s.db, uid, today, 1); err != nil {
				log.Printf("stats: %v", err)
			}
		} else if req.ExpectedDueDate != "" {
			writeError(w, r, http.StatusConflict, "error.task_changed")
			return
		}
		// Completing the parent also completes its open subtasks (the client asks first)
		if res2, err := s.db.Exec("UPDATE tasks SET done = 1, completed_at = ? WHERE parent_id = ? AND list_id "+visibleLists+" AND done = 0", completed, t.ID, uid); err == nil {
			if n2, _ := res2.RowsAffected(); n2 > 0 {
				if err := bumpStat(s.db, uid, today, int(n2)); err != nil {
					log.Printf("stats: %v", err)
				}
			}
		}
	}
	t, err := getTask(s.db, t.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	s.respondTask(w, userID(r), http.StatusOK, t)
}

func (s *server) handleUncompleteTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	uid := userID(r)
	if err := s.reopenDone(s.db, uid, t); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if err := s.reopenParentOf(uid, t.ParentID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	t, err := getTask(s.db, t.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	s.respondTask(w, userID(r), http.StatusOK, t)
}

// reopenDone reverts a completion, including the daily counter on the
// original completion day. Only what the UPDATE actually reopened gets
// booked — done = 1 in the WHERE clause catches duplicate requests. ex is
// s.db or a transaction (in that case, nothing inside it via s.db — single
// connection, deadlock).
func (s *server) reopenDone(ex execer, uid int64, t Task) error {
	res, err := ex.Exec("UPDATE tasks SET done = 0, completed_at = NULL WHERE id = ? AND list_id "+visibleLists+" AND done = 1", t.ID, uid)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		date := s.nowFor(uid).Format(dateFmt)
		if t.CompletedAt != nil {
			if ct, err := time.Parse(time.RFC3339, *t.CompletedAt); err == nil {
				date = ct.In(s.store.location(uid)).Format(dateFmt)
			}
		}
		return bumpStat(ex, uid, date, -1)
	}
	return nil
}

// reopenParentOf: an open subtask under a completed parent task would only
// sit inside the collapsed "done" section — the header would count it as
// open, but it wouldn't be visible anywhere. So the parent task gets
// reopened along with it.
func (s *server) reopenParentOf(uid int64, parentID *int64) error {
	if parentID == nil {
		return nil
	}
	p, err := taskFor(s.db, uid, *parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.reopenDone(s.db, uid, p)
}

func (s *server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	if err := deleteTaskCascade(s.db, userID(r), t.ID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.delete_failed")
		return
	}
	s.changed(userID(r))
	w.WriteHeader(http.StatusNoContent)
}

// deleteTaskCascade deletes a task along with its subtasks, links and images
// in one transaction: if one step fails, everything stays as is. Done
// separately, orphans would otherwise be left behind and inherited by the
// next task with the same ID (tasks.id gets reused). Use only tx inside it —
// with a single DB connection, any access via db would deadlock.
func deleteTaskCascade(db *sql.DB, uid, id int64) error {
	return deleteFamily(db, "SELECT id FROM tasks WHERE (id = ? OR parent_id = ?) AND list_id "+visibleLists, id, id, uid)
}

// archiveTask: like deleteTaskCascade, but without a user — only for the
// instance-wide auto-archive in the scheduler.
func archiveTask(db *sql.DB, id int64) error {
	return deleteFamily(db, "SELECT id FROM tasks WHERE id = ? OR parent_id = ?", id, id)
}

// deleteFamily deletes the tasks from family (a SELECT id …) along with
// their links and images.
func deleteFamily(db *sql.DB, family string, args ...any) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"DELETE FROM links WHERE a IN (" + family + ") OR b IN (" + family + ")", append(append([]any{}, args...), args...)},
		{"DELETE FROM attachments WHERE task_id IN (" + family + ")", args},
		{"DELETE FROM tasks WHERE id IN (" + family + ")", args},
	} {
		if _, err := tx.Exec(q.sql, q.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *server) respondTask(w http.ResponseWriter, uid int64, code int, t Task) {
	now := s.nowFor(uid)
	t.Overdue = isOverdue(t, now.Format(dateFmt), now.Format("15:04"))
	if t.Links == nil {
		t.Links = []int64{}
	}
	if t.Attachments == nil {
		t.Attachments = []int64{}
	}
	writeJSON(w, code, t)
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("ok"))
}
