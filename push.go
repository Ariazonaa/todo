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

// maxPushDevicesPerUser: every notification goes to every device, one
// after the other in the scheduler's tick for all accounts.
const maxPushDevicesPerUser = 10

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

// errPushTaken: the endpoint belongs to another account, and the request
// doesn't carry that device's keys.
var errPushTaken = errors.New("push: endpoint registered to another account")

// errPushLimit: the account already has maxPushDevicesPerUser devices.
var errPushLimit = errors.New("push: device limit reached")

// savePushSub registers a device for uid or updates it (reload,
// self-healing, new passkey) — one endpoint, one row. If the same device
// registers under a different person — a shared tablet — it belongs to that
// person afterward. But only with the same keys: the browser keeps them for
// its subscription, whereas someone who merely learned the endpoint doesn't
// know the auth secret — and could otherwise unhook another person's device.
//
// A device that isn't uid's yet only gets in below maxPushDevicesPerUser —
// within the INSERT itself.
func savePushSub(db *sql.DB, endpoint string, p256dh, auth []byte, origin string, credentialID, uid int64) error {
	var cred any
	if credentialID > 0 {
		cred = credentialID
	}
	const mine = "EXISTS (SELECT 1 FROM push_subscriptions WHERE endpoint = ? AND user_id = ?)"
	const room = "(SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?) < ?"
	res, err := db.Exec(`INSERT INTO push_subscriptions (endpoint, p256dh, auth, origin, credential_id, created_at, user_id)
		SELECT ?, ?, ?, ?, ?, ?, ? WHERE `+mine+` OR `+room+`
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth,
			origin = excluded.origin, credential_id = excluded.credential_id, user_id = excluded.user_id
		WHERE push_subscriptions.user_id IS excluded.user_id
			OR (push_subscriptions.p256dh = excluded.p256dh AND push_subscriptions.auth = excluded.auth)`,
		endpoint, b64.EncodeToString(p256dh), b64.EncodeToString(auth), origin, cred,
		time.Now().UTC().Format(time.RFC3339), uid, endpoint, uid, uid, maxPushDevicesPerUser)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		var full bool
		if err := db.QueryRow("SELECT NOT "+mine+" AND NOT "+room, endpoint, uid, uid, maxPushDevicesPerUser).Scan(&full); err == nil && full {
			return errPushLimit
		}
		return errPushTaken
	}
	return nil
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

// pushSubReq is PushSubscription.toJSON() from the browser. OldEndpoint:
// the subscription this one replaces (the browser renewed it, or the
// server key changed).
type pushSubReq struct {
	Endpoint    string `json:"endpoint"`
	OldEndpoint string `json:"old_endpoint"`
	Keys        struct {
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
	if u, err := url.Parse(origin); err != nil || u.Scheme != "https" || u.Host == "" {
		origin = "https://" + r.Host
	}
	// A device registered with a stolen session cookie would get task
	// titles for good: it outlives the session and shows up nowhere. So a
	// new device needs a fresh login — refreshing one of the account's own
	// (healPush on every start) or replacing it with its renewed
	// subscription doesn't. Endpoints are secret, the thief knows none.
	uid := userID(r)
	if !pushDeviceKnown(s.db, uid, req.Endpoint, req.OldEndpoint) && !s.requireFreshAuth(w, r) {
		return
	}
	// The browser has given up the old subscription: it goes first, so the
	// successor doesn't count against the device limit.
	if req.OldEndpoint != "" && req.OldEndpoint != req.Endpoint {
		if err := deletePushSubOf(s.db, uid, req.OldEndpoint); err != nil {
			writeError(w, r, http.StatusInternalServerError, "error.internal")
			return
		}
	}
	cred := sessionCredential(s.db, s.sessionToken(r))
	switch err := savePushSub(s.db, req.Endpoint, p256dh, auth, origin, cred, uid); {
	case errors.Is(err, errPushTaken):
		writeError(w, r, http.StatusConflict, "error.push_taken")
		return
	case errors.Is(err, errPushLimit):
		writeError(w, r, http.StatusConflict, "error.push_limit", "n", maxPushDevicesPerUser)
		return
	case err != nil:
		writeError(w, r, http.StatusInternalServerError, "error.internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushDeviceKnown: endpoint or old (the subscription it replaces) is
// already one of uid's devices.
func pushDeviceKnown(db *sql.DB, uid int64, endpoint, old string) bool {
	var ok bool
	return db.QueryRow("SELECT EXISTS (SELECT 1 FROM push_subscriptions WHERE user_id = ? AND endpoint IN (?, ?))",
		uid, endpoint, old).Scan(&ok) == nil && ok
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
