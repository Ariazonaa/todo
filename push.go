package main

import (
	"crypto/ecdh"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// pushSub is a device's registration with the push service. Endpoint and
// keys act like credentials: never log them.
type pushSub struct {
	ID       int64
	Endpoint string
	P256dh   []byte
	Auth     []byte
	Origin   string
}

const pushSubCols = "id, endpoint, p256dh, auth, origin"

func scanPushSub(row interface{ Scan(...any) error }) (pushSub, error) {
	var sub pushSub
	var p256dh, auth string
	if err := row.Scan(&sub.ID, &sub.Endpoint, &p256dh, &auth, &sub.Origin); err != nil {
		return sub, err
	}
	var err error
	if sub.P256dh, err = b64.DecodeString(p256dh); err != nil {
		return sub, err
	}
	sub.Auth, err = b64.DecodeString(auth)
	return sub, err
}

// savePushSub creates a device or updates it (reload, self-healing, new
// passkey) — one endpoint, one row.
// savePushSub registers a device for uid. If the same device (endpoint)
// registers under a different person — a shared tablet — it belongs to
// that person afterward.
func savePushSub(db *sql.DB, endpoint string, p256dh, auth []byte, origin string, credentialID, uid int64) error {
	var cred any
	if credentialID > 0 {
		cred = credentialID
	}
	_, err := db.Exec(`INSERT INTO push_subscriptions (endpoint, p256dh, auth, origin, credential_id, created_at, user_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth,
			origin = excluded.origin, credential_id = excluded.credential_id, user_id = excluded.user_id`,
		endpoint, b64.EncodeToString(p256dh), b64.EncodeToString(auth), origin, cred,
		time.Now().UTC().Format(time.RFC3339), uid)
	return err
}

// getPushSub: the device endpoint belonging to uid.
func getPushSub(db *sql.DB, uid int64, endpoint string) (pushSub, error) {
	return scanPushSub(db.QueryRow("SELECT "+pushSubCols+" FROM push_subscriptions WHERE endpoint = ? AND user_id = ?", endpoint, uid))
}

func listPushSubs(db *sql.DB, uid int64) ([]pushSub, error) {
	rows, err := db.Query("SELECT "+pushSubCols+" FROM push_subscriptions WHERE user_id = ? ORDER BY id", uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []pushSub
	for rows.Next() {
		sub, err := scanPushSub(rows)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

func countPushSubs(db *sql.DB, uid int64) (int, error) {
	var n int
	err := db.QueryRow("SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?", uid).Scan(&n)
	return n, err
}

// deletePushSubOf unregisters the device endpoint of uid — others are left alone.
func deletePushSubOf(db *sql.DB, uid int64, endpoint string) error {
	_, err := db.Exec("DELETE FROM push_subscriptions WHERE endpoint = ? AND user_id = ?", endpoint, uid)
	return err
}

func deletePushSub(db *sql.DB, endpoint string) error {
	_, err := db.Exec("DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint)
	return err
}

// deletePushSubsOfCredential: a deleted passkey also locks out the devices
// that were registered with it — notifications carry task titles. As with
// sessions, unassigned ones go too.
func deletePushSubsOfCredential(db *sql.DB, uid, credentialID int64) error {
	_, err := db.Exec("DELETE FROM push_subscriptions WHERE user_id = ? AND (credential_id = ? OR credential_id IS NULL)", uid, credentialID)
	return err
}

// pushAll sends msg to all devices. delivered: at least one responded
// 2xx — then the message counts as delivered. A device that only failed
// temporarily misses it; otherwise the others would get it twice on
// retry. Unregistered devices (404/410) are deleted.
// pushAll sends msg to all of uid's devices.
func pushAll(db *sql.DB, k *vapidKey, uid int64, msg pushMsg, ttl time.Duration, urgency string) (delivered bool, err error) {
	subs, err := listPushSubs(db, uid)
	if err != nil {
		return false, err
	}
	var errs []error
	for _, sub := range subs {
		switch err := sendPush(k, sub, msg, ttl, urgency); {
		case err == nil:
			delivered = true
		case errors.Is(err, errPushGone):
			if err := deletePushSub(db, sub.Endpoint); err != nil {
				errs = append(errs, err)
			}
		default:
			errs = append(errs, err)
		}
	}
	return delivered, errors.Join(errs...)
}

func (s *server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"public_key": s.vapid.publicKey()})
}

// pushSubReq is PushSubscription.toJSON() from the browser.
type pushSubReq struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// decodeKey accepts base64url with or without padding (browsers differ).
func decodeKey(s string) ([]byte, error) {
	return b64.DecodeString(strings.TrimRight(s, "="))
}

func (s *server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req pushSubReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	u, err := url.Parse(req.Endpoint)
	if err != nil || len(req.Endpoint) > 1024 {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	if !pushHostAllowed(u) {
		// Host, never the path (that's the device's secret) — by design:
		// 400 with the host, so the service list can be extended for an
		// unusual browser.
		writeError(w, r, http.StatusBadRequest, "error.push_host", "host", u.Host)
		return
	}
	p256dh, err1 := decodeKey(req.Keys.P256dh)
	auth, err2 := decodeKey(req.Keys.Auth)
	if err1 != nil || err2 != nil || len(auth) != 16 {
		writeError(w, r, http.StatusBadRequest, "error.bad_keys")
		return
	}
	if _, err := ecdh.P256().NewPublicKey(p256dh); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_keys")
		return
	}
	// Origin: contact for the VAPID JWT (Apple requires an https sub).
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "https://" + r.Host
	}
	cred := sessionCredential(s.db, s.sessionToken(r))
	if err := savePushSub(s.db, req.Endpoint, p256dh, auth, origin, cred, userID(r)); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	if err := deletePushSubOf(s.db, userID(r), req.Endpoint); err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// hasOpenSubs: does the task have open subtasks? If so, the push has no
// "Done" button (the app asks first, a push notification can't).
func hasOpenSubs(db *sql.DB, id int64) (bool, error) {
	var ok bool
	err := db.QueryRow("SELECT EXISTS (SELECT 1 FROM tasks WHERE parent_id = ? AND done = 0)", id).Scan(&ok)
	return ok, err
}

// clipRunes truncates to at most n characters (with …) — notifications
// only show the beginning anyway, and the message must stay under
// maxPushPlaintext.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// handlePushTest sends a test notification to exactly the device that
// asked — not to all of them.
func (s *server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	sub, err := getPushSub(s.db, userID(r), req.Endpoint)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "error.push_not_registered")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	err = sendPush(s.vapid, sub, pushMsg{Title: "Todo", Body: T(requestLang(r), "notify.push_test"), Tag: "test"}, time.Minute, "normal")
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errPushGone):
		deletePushSub(s.db, sub.Endpoint)
		writeError(w, r, http.StatusBadGateway, "error.push_gone")
	default:
		// err never names the endpoint (sendPush), only status or cause
		writeError(w, r, http.StatusBadGateway, "error.push_rejected", "detail", err.Error())
	}
}
