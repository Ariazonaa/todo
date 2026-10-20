package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// maxTasksPerOwner: tasks in one person's lists — as many as a backup
	// may contain, so every export can be imported again.
	maxTasksPerOwner = maxImportTasks
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	jsonHeader(w)
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonHeader(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	// Tasks, settings and passkey lists have no business in the browser
	// cache — they'd still be there after logging out.
	w.Header().Set("Cache-Control", "no-store")
}

// jsonStream writes a JSON response one value at a time, through a buffer
// and an idleWriter: a long list never sits in memory as one document (for
// the task list it used to be hundreds of MB). Without HTML escaping —
// every "<" would otherwise take six bytes (\u003c), in memory and on the
// wire; the responses are application/json with nosniff, never HTML.
type jsonStream struct {
	bw  *bufio.Writer
	buf bytes.Buffer
	enc *json.Encoder
	err error // first write error: the client is gone, the rest is skipped
}

// newJSONStream: until as for idleWriter (zero: no overall deadline).
func newJSONStream(w http.ResponseWriter, until time.Time) *jsonStream {
	js := &jsonStream{bw: bufio.NewWriterSize(newIdleWriter(w, until), 64<<10)}
	js.enc = json.NewEncoder(&js.buf)
	js.enc.SetEscapeHTML(false)
	return js
}

// raw writes JSON syntax as is (keys, brackets, commas).
func (js *jsonStream) raw(s string) {
	if js.err == nil {
		_, js.err = js.bw.WriteString(s)
	}
}

// value writes v. The status is already out by then: on an encoding error
// the connection is aborted, so nobody keeps a broken document.
func (js *jsonStream) value(v any) {
	if js.err != nil {
		return
	}
	js.buf.Reset()
	if err := js.enc.Encode(v); err != nil {
		panic(http.ErrAbortHandler)
	}
	_, js.err = js.bw.Write(bytes.TrimSuffix(js.buf.Bytes(), []byte("\n")))
}

// flush sends what's buffered; the error is the first one of the stream.
func (js *jsonStream) flush() error {
	if js.err == nil {
		js.err = js.bw.Flush()
	}
	return js.err
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
		si, ok := s.authSession(w, r)
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
		uid := si.uid
		r = withUser(r, uid)
		if !si.authAt.IsZero() {
			r = withAuthAt(r, si.authAt)
		}
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
	si, ok := s.authSession(w, r)
	return si.uid, ok
}

// authSession is renewedAuth with the whole session (including the time of
// the passkey login).
func (s *server) authSession(w http.ResponseWriter, r *http.Request) (sessionInfo, bool) {
	token := s.sessionToken(r)
	si, ok := sessionLookup(s.db, token)
	if !ok {
		return si, false
	}
	// Only renew once less than half the lifetime is left: otherwise every
	// request would write to the single DB connection. And no longer once
	// within the grace period of a replaced session (sessionRetireGrace).
	if left := time.Until(si.expires); left > sessionIdleTimeout/2 || left <= sessionRetireGrace {
		return si, true
	}
	if err := renewSession(s.db, token, time.Now().Add(sessionIdleTimeout)); err != nil {
		return si, true // renewing is a convenience, not a reason to reject
	}
	s.setSessionCookie(w, token)
	return si, true
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
	resp.WebhookURL = maskWebhook(resp.WebhookURL)
	var admin int
	s.db.QueryRow("SELECT id, name, is_admin FROM users WHERE id = ?", uid).Scan(&resp.Me.ID, &resp.Me.Name, &admin)
	resp.Me.IsAdmin = admin == 1
	return resp
}

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsOf(userID(r)))
}

// settingsReq: PUT /api/settings. The settings only show the webhook
// masked (maskWebhook): leaving webhook_url out or sending a mask back keeps
// the current one — also an old mask, which an old cached form sends after
// the webhook was changed elsewhere ("…" is never part of a webhook URL).
type settingsReq struct {
	appSettings
	WebhookURL *string `json:"webhook_url"`
}

func (s *server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body settingsReq
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	uid := userID(r)
	cur := s.store.get(uid).WebhookURL
	req := body.appSettings
	req.WebhookURL = cur
	if body.WebhookURL != nil && !strings.Contains(*body.WebhookURL, "…") {
		req.WebhookURL = *body.WebhookURL
	}
	if err := validateSettings(req); err != nil {
		writeErr(w, r, http.StatusBadRequest, err)
		return
	}
	admin := userIsAdmin(s.db, uid)
	// The webhook decides where task titles go — even after "sign out
	// everywhere"; the archive period decides when everyone's completed
	// tasks get deleted. Changing either needs a fresh login, like passkeys.
	webhookChanged := req.WebhookURL != cur
	archiveChanged := admin && req.ArchiveDays != s.store.archiveDays()
	if (webhookChanged || archiveChanged) && !s.requireFreshAuth(w, r) {
		return
	}
	if err := s.store.save(uid, req); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	// The archive retention period applies to the whole instance: only the admin sets it.
	if admin {
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

// taskLoads: list loads one at a time per person (the app reloads after
// every change, in every tab), two at a time overall — each holds the
// person's whole visible list in memory until it's written out.
var taskLoads = newWaitSlots(1, 2)

func (s *server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	release, ok := taskLoads.acquire(r.Context(), uid)
	if !ok {
		return // client gone while waiting
	}
	defer release()
	now := s.nowFor(uid)
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
	invites, err := invitesOf(s.db, uid)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	jsonHeader(w)
	w.WriteHeader(http.StatusOK)
	js := newJSONStream(w, time.Now().Add(taskListWriteMax))
	js.raw(`{"today":`)
	js.value(now.Format(dateFmt))
	js.raw(`,"lists":`)
	js.value(lists)
	js.raw(`,"invites":`)
	js.value(invites)
	js.raw(`,"tasks":[`)
	for i := range tasks {
		if i > 0 {
			js.raw(",")
		}
		js.value(&tasks[i])
	}
	js.raw("]}\n")
	js.flush() // an error here only means: client gone
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
	req.Title = strings.TrimSpace(cleanTitle(req.Title))
	req.Note = cleanNote(req.Note)
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
		if !dueDateOK(req.DueDate) {
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
	if errors.Is(err, errTaskLimit) {
		writeError(w, r, http.StatusConflict, "error.task_limit", "n", maxTasksPerOwner)
		return
	}
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
	// The task exists by now: a 500 here would make the client retry and
	// create it twice.
	if err := s.reopenParentOf(uid, t.ParentID); err != nil {
		log.Printf("create task: reopen parent: %v", err)
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

// mergeTaskReq: an update only changes the fields it sends — everything else
// keeps the current value. Otherwise an edit sheet that was open for a while
// would write back what another device (a member of a shared list, the
// "Done" button of a notification) changed in the meantime. null counts as
// sent (clears the field). priority and list_id already mean "unchanged"
// when absent.
func mergeTaskReq(req *taskReq, present map[string]json.RawMessage, t Task) {
	has := func(key string) bool {
		for k := range present {
			// encoding/json matches field names case-insensitively, so must we
			if strings.EqualFold(k, key) {
				return true
			}
		}
		return false
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if !has("title") {
		req.Title = t.Title
	}
	if !has("note") {
		req.Note = t.Note
	}
	if !has("due_date") {
		req.DueDate = deref(t.DueDate)
	}
	if !has("due_time") {
		req.DueTime = deref(t.DueTime)
	}
	if !has("recurrence") {
		req.Recurrence = t.Recurrence
	}
	if !has("pinned") {
		req.Pinned = t.Pinned
	}
	if !has("in_progress") {
		req.InProgress = t.InProgress
	}
	if !has("parent_id") {
		req.ParentID = t.ParentID
	}
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
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	var sent taskReq
	var present map[string]json.RawMessage
	if json.Unmarshal(body, &sent) != nil || json.Unmarshal(body, &present) != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	uid := userID(r)
	// The fields not sent come from t. If another request changes the task
	// before the write, the UPDATE's guard finds nothing to change; then t is
	// read again and merged anew — a concurrent change is never written back
	// over.
	for attempt := 1; ; attempt++ {
		req := sent
		mergeTaskReq(&req, present, t)
		dueDate, dueTime, msg, err := s.validateTaskReq(&req, uid, t.ID)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		if msg != "" {
			writeError(w, r, http.StatusBadRequest, msg)
			return
		}
		n, err := s.writeTaskUpdate(uid, t, req, dueDate, dueTime)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		if n == 1 {
			break
		}
		cur, err := taskFor(s.db, uid, t.ID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, "error.task_not_found")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		if !sameTaskFields(cur, t) {
			if attempt < 3 {
				t = cur
				continue
			}
			writeError(w, r, http.StatusConflict, "error.task_changed")
			return
		}
		if req.ListID != nil && req.ParentID == nil && !listVisible(s.db, uid, *req.ListID) {
			writeError(w, r, http.StatusBadRequest, "error.list_not_found")
			return
		}
		if key, n := s.moveLimitHit(t, req); key != "" {
			writeError(w, r, http.StatusConflict, key, "n", n)
			return
		}
		writeError(w, r, http.StatusConflict, msgParentChanged)
		return
	}
	t, err = taskFor(s.db, uid, t.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "error.task_not_found")
		return
	}
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

// sameTaskFields: the fields an update writes are the same in a and b.
func sameTaskFields(a, b Task) bool {
	return a.Title == b.Title && a.Note == b.Note && ptrEq(a.DueDate, b.DueDate) && ptrEq(a.DueTime, b.DueTime) &&
		a.Recurrence == b.Recurrence && a.Pinned == b.Pinned && a.InProgress == b.InProgress &&
		a.Priority == b.Priority && a.ListID == b.ListID && ptrEq(a.ParentID, b.ParentID)
}

// writeTaskUpdate writes the merged req over t in one transaction: the task,
// its subtasks' list, its reminder stamps. 0 rows: a rule in the statement
// didn't hold, or the row no longer holds what t says.
func (s *server) writeTaskUpdate(uid int64, t Task, req taskReq, dueDate, dueTime *string) (int64, error) {
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
		q += ", sort_order = (SELECT COALESCE(MAX(sort_order), 0) + 10 FROM tasks WHERE parent_id IS ? AND id != ? AND list_id " + visibleLists + ")"
		args = append(args, req.ParentID, t.ID, uid)
	}
	q += " WHERE id = ? AND list_id " + visibleLists
	args = append(args, t.ID, uid)
	// The row must still hold what the fields not sent were taken from.
	q += " AND title IS ? AND note IS ? AND due_date IS ? AND due_time IS ? AND recurrence IS ? AND pinned = ? AND in_progress = ? " +
		"AND priority = ? AND list_id IS ? AND parent_id IS ?"
	args = append(args, t.Title, t.Note, t.DueDate, t.DueTime, t.Recurrence, boolInt(t.Pinned), boolInt(t.InProgress),
		t.Priority, t.ListID, t.ParentID)
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
	if movesList(t, req) {
		ml := newMoveLimits(req)
		q += " AND (" + ml.same + " OR (" + ml.tasks + " AND " + ml.storage + "))"
		args = append(append(append(args, ml.sameArgs...), ml.tasksArgs...), ml.storageArgs...)
	}
	// A task and its subtasks change list together.
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil
	}
	if _, err := tx.Exec("UPDATE tasks SET list_id = (SELECT list_id FROM tasks WHERE id = ?) WHERE parent_id = ?", t.ID, t.ID); err != nil {
		return 0, err
	}
	if dueChanged {
		if err := clearReminders(tx, t.ID); err != nil {
			return 0, err
		}
	}
	return 1, tx.Commit()
}

// movesList: the update can put the task into another list.
func movesList(t Task, req taskReq) bool {
	if req.ParentID != nil {
		return !ptrEq(t.ParentID, req.ParentID)
	}
	return req.ListID != nil && *req.ListID != t.ListID
}

// moveLimits: SQL conditions for the task UPDATE. A task moving into another
// owner's lists takes itself, its subtasks and their images along — the new
// owner must stay within maxTasksPerOwner and maxAttachmentBytesPerOwner,
// otherwise members could unload any amount onto them (and their backup no
// longer fit back in). Moves within one owner's lists are always fine.
type moveLimits struct {
	same, tasks, storage             string
	sameArgs, tasksArgs, storageArgs []any
}

func newMoveLimits(req taskReq) moveLimits {
	// The target list as the UPDATE sets it (tasks = the row being updated).
	const target = "CASE WHEN ? IS NOT NULL THEN (SELECT p.list_id FROM tasks p WHERE p.id = ?) ELSE COALESCE(?, tasks.list_id) END"
	const owner = "(SELECT owner_id FROM lists WHERE id = " + target + ")"
	const ownerTasks = "(SELECT o.id FROM tasks o WHERE o.list_id IN (SELECT id FROM lists WHERE owner_id = " + owner + "))"
	const family = "(SELECT f.id FROM tasks f WHERE f.id = tasks.id OR f.parent_id = tasks.id)"
	ta := []any{req.ParentID, req.ParentID, req.ListID}
	return moveLimits{
		same:     "(" + owner + " IS (SELECT owner_id FROM lists WHERE id = tasks.list_id))",
		sameArgs: ta,
		tasks: "((SELECT COUNT(*) FROM tasks c WHERE c.id IN " + ownerTasks + ") + " +
			"(SELECT COUNT(*) FROM tasks c WHERE c.id IN " + family + ") <= ?)",
		tasksArgs: append(append([]any{}, ta...), maxTasksPerOwner),
		storage: "((SELECT COALESCE(SUM(length(a.data)), 0) FROM attachments a WHERE a.task_id IN " + ownerTasks + ") + " +
			"(SELECT COALESCE(SUM(length(a.data)), 0) FROM attachments a WHERE a.task_id IN " + family + ") <= ?)",
		storageArgs: append(append([]any{}, ta...), maxAttachmentBytesPerOwner),
	}
}

// moveLimitHit: after an update came back empty — was it the target owner's
// task or image limit? Returns the catalog key and the limit for it.
func (s *server) moveLimitHit(t Task, req taskReq) (string, any) {
	if !movesList(t, req) {
		return "", nil
	}
	ml := newMoveLimits(req)
	args := append(append(append(append(append([]any{}, ml.sameArgs...), ml.tasksArgs...), ml.sameArgs...), ml.storageArgs...), t.ID)
	var tasksOK, storageOK bool
	if err := s.db.QueryRow("SELECT "+ml.same+" OR "+ml.tasks+", "+ml.same+" OR "+ml.storage+" FROM tasks WHERE id = ?", args...).
		Scan(&tasksOK, &storageOK); err != nil {
		return "", nil
	}
	switch {
	case !tasksOK:
		return "error.task_limit", maxTasksPerOwner
	case !storageOK:
		return "error.storage_limit", maxAttachmentBytesPerOwner >> 20
	}
	return "", nil
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
	// notification, see handleCompleteTask). A day-old notification must not
	// snooze the task once more after its due date has moved on — nor, with
	// an ID from before migrateTasksAutoincrement, a different task.
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
	uid := userID(r)
	for attempt := 1; ; attempt++ {
		err := s.snoozeOnce(uid, t, req.Days, req.ExpectedDueDate)
		if errors.Is(err, errStaleCompletion) && attempt < 3 {
			// completed again by someone else meanwhile: reread and redo
			if t, err = taskFor(s.db, uid, t.ID); errors.Is(err, sql.ErrNoRows) {
				writeError(w, r, http.StatusNotFound, "error.task_not_found")
				return
			} else if err != nil {
				writeError(w, r, http.StatusInternalServerError, "error.internal")
				return
			}
			continue
		}
		// Still completed by someone else after the retries: the snooze
		// didn't happen, don't answer as if it had.
		if errors.Is(err, errTaskChanged) || errors.Is(err, errStaleCompletion) {
			writeError(w, r, http.StatusConflict, "error.task_changed")
			return
		}
		if errors.Is(err, errDateRange) {
			writeError(w, r, http.StatusBadRequest, "error.date_invalid")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
		break
	}
	// After the commit: reopenParentOf goes through s.db (single connection).
	if err := s.reopenParentOf(uid, t.ParentID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	t, err := taskFor(s.db, uid, t.ID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "error.task_not_found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	s.respondTask(w, userID(r), http.StatusOK, t)
}

// errTaskChanged: the due date no longer matches expected_due_date.
var errTaskChanged = errors.New("task changed")

// snoozeOnce moves t's due date by days (from today, or from a due date in
// the future) and reopens it if it was done — together or not at all.
func (s *server) snoozeOnce(uid int64, t Task, days int, expected string) error {
	today := s.nowFor(uid).Format(dateFmt)
	base := today
	if t.DueDate != nil && *t.DueDate > today {
		base = *t.DueDate
	}
	bt, err := time.Parse(dateFmt, base)
	if err != nil {
		return err
	}
	newDate := bt.AddDate(0, 0, days).Format(dateFmt)
	if newDate > maxDueDate {
		return errDateRange
	}
	// Before the transaction: reopenStat may read settings through s.db.
	statUID, statDate := s.reopenStat(uid, t)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A completed task becomes open again after snoozing — so also roll back
	// the daily counter, or the next completion would count it twice.
	if err := reopenDone(tx, uid, t, statUID, statDate); err != nil {
		return err
	}
	// Snoozing means "I'll do it later" -> out of "in progress". The
	// expected_due_date check is additionally placed here in the WHERE clause
	// (not just in the check above): otherwise a concurrent request could
	// interfere (rules belong in the write statement itself).
	res, err := tx.Exec("UPDATE tasks SET due_date = ?, in_progress = 0 WHERE id = ? AND list_id "+visibleLists+" AND (? = '' OR due_date IS ?)",
		newDate, t.ID, uid, expected, expected)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 && expected != "" {
		return errTaskChanged
	}
	return tx.Commit()
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
	if err := linkTasks(s.db, userID(r), t.ID, req.Other); errors.Is(err, errLinkLimit) {
		writeError(w, r, http.StatusConflict, "error.link_limit", "n", maxLinksPerTask)
		return
	} else if err != nil {
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
	if _, err := s.db.Exec("UPDATE tasks SET in_progress = ? WHERE id = ? AND parent_id IS NULL AND list_id "+visibleLists, boolInt(req.On), t.ID, userID(r)); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	s.respondCurrent(w, r, t.ID, http.StatusOK)
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
			if next > maxDueDate {
				writeError(w, r, http.StatusBadRequest, "error.date_invalid")
				return
			}
			// The new due date has no reminder stamp, so it will fire again. The
			// due_date in the WHERE clause catches concurrent requests (0 rows =
			// already advanced).
			res, err := s.db.Exec("UPDATE tasks SET due_date = ?, in_progress = 0 WHERE id = ? AND list_id "+visibleLists+" AND done = 0 AND due_date = ? AND recurrence = ?", next, t.ID, uid, *t.DueDate, t.Recurrence)
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
				if _, err := s.db.Exec("UPDATE tasks SET done = 0, completed_at = NULL, completed_by = NULL, "+
					"due_date = CASE WHEN due_date IS NULL THEN NULL ELSE date(due_date, ?) END WHERE parent_id = ? AND list_id "+visibleLists, shift, t.ID, uid); err != nil {
					log.Printf("recurrence: subtasks of %d: %v", t.ID, err)
				}
			}
		}
	} else {
		// Same protection as snoozing (handleSnoozeTask): a notification with an
		// old due date must not silently complete the task once its due date
		// has moved on — nor, with an ID from before migrateTasksAutoincrement,
		// a different task. Without expected_due_date (in-app use) no check
		// applies, as before.
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
		// reading (above) and writing against a concurrent request — also one
		// that just made the task recurring (those are never done = 1).
		res, err := s.db.Exec("UPDATE tasks SET done = 1, completed_at = ?, completed_by = ?, in_progress = 0 WHERE id = ? AND list_id "+visibleLists+" AND done = 0 AND (? = '' OR due_date IS ?) "+
			"AND (recurrence = '' OR due_date IS NULL)",
			completed, uid, t.ID, uid, req.ExpectedDueDate, req.ExpectedDueDate)
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
		if res2, err := s.db.Exec("UPDATE tasks SET done = 1, completed_at = ?, completed_by = ? WHERE parent_id = ? AND list_id "+visibleLists+" AND done = 0", completed, uid, t.ID, uid); err == nil {
			if n2, _ := res2.RowsAffected(); n2 > 0 {
				if err := bumpStat(s.db, uid, today, int(n2)); err != nil {
					log.Printf("stats: %v", err)
				}
			}
		}
	}
	s.changed(userID(r))
	s.respondCurrent(w, r, t.ID, http.StatusOK)
}

func (s *server) handleUncompleteTask(w http.ResponseWriter, r *http.Request) {
	t, ok := s.taskFromPath(w, r)
	if !ok {
		return
	}
	uid := userID(r)
	if err := s.reopen(uid, t); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	if err := s.reopenParentOf(uid, t.ParentID); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.changed(userID(r))
	s.respondCurrent(w, r, t.ID, http.StatusOK)
}

// reopenStat: whose daily counter a reopen of t takes back, and for which
// day — the person who checked it off (in a shared list that's not
// necessarily the one reopening it), on their completion day. statUID 0:
// that account no longer exists, nobody's counter changes. Call outside a
// transaction: it may read settings through s.db.
func (s *server) reopenStat(uid int64, t Task) (statUID int64, date string) {
	statUID = uid // legacy rows without completed_by: from before accounts
	if t.CompletedBy != nil {
		statUID = *t.CompletedBy
	}
	if statUID == 0 {
		return 0, ""
	}
	loc := s.store.location(statUID)
	date = time.Now().In(loc).Format(dateFmt)
	if t.CompletedAt != nil {
		if ct, err := time.Parse(time.RFC3339, *t.CompletedAt); err == nil {
			date = ct.In(loc).Format(dateFmt)
		}
	}
	return statUID, date
}

// errStaleCompletion: the task was completed again (by someone else) after
// t was read — the stat would go back to the wrong person. Reread, retry.
var errStaleCompletion = errors.New("completion changed")

// reopenDone reverts a completion, including the daily counter from
// reopenStat. Only what the UPDATE actually reopened gets booked — done = 1
// in the WHERE clause catches duplicate requests, and the completion read
// into t must still be the one in the row. ex is s.db or a transaction (in
// that case, nothing inside it via s.db — single connection, deadlock).
func reopenDone(ex dbtx, uid int64, t Task, statUID int64, statDate string) error {
	res, err := ex.Exec("UPDATE tasks SET done = 0, completed_at = NULL, completed_by = NULL WHERE id = ? AND list_id "+visibleLists+
		" AND done = 1 AND completed_at IS ? AND completed_by IS ?", t.ID, uid, t.CompletedAt, t.CompletedBy)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if statUID != 0 {
			return bumpStat(ex, statUID, statDate, -1)
		}
		return nil
	}
	var done bool
	if err := ex.QueryRow("SELECT done = 1 FROM tasks WHERE id = ? AND list_id "+visibleLists, t.ID, uid).Scan(&done); err == nil && done {
		return errStaleCompletion
	}
	return nil
}

// reopen is reopenDone outside a transaction, rereading t when it was
// completed again in the meantime.
func (s *server) reopen(uid int64, t Task) error {
	for attempt := 1; ; attempt++ {
		statUID, statDate := s.reopenStat(uid, t)
		err := reopenDone(s.db, uid, t, statUID, statDate)
		if !errors.Is(err, errStaleCompletion) {
			return err
		}
		if attempt == 3 {
			return nil // completed over and over by others: leave it done
		}
		if t, err = taskFor(s.db, uid, t.ID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
	}
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
	return s.reopen(uid, p)
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
// separately, orphans would otherwise be left behind. Use only tx inside it —
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

// respondCurrent answers with task id as the requesting person sees it now
// — through taskFor, not getTask: between the write and this read the task
// could have been deleted and its ID handed to someone else's new task.
func (s *server) respondCurrent(w http.ResponseWriter, r *http.Request, id int64, code int) {
	t, err := taskFor(s.db, userID(r), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "error.task_not_found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	s.respondTask(w, userID(r), code, t)
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
