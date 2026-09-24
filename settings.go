package main

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// appSettings are the settings editable in the control panel. Webhook,
// timezone, digest time and language belong to a user (user_settings); the
// archive retention period belongs to the whole instance
// (meta.set_archive_days, admin only).
type appSettings struct {
	WebhookURL  string `json:"webhook_url"`
	TZ          string `json:"tz"`
	DigestTime  string `json:"digest_time"`
	ArchiveDays int    `json:"archive_days"` // 0 = never clean up automatically
	Language    string `json:"language"`     // "auto" or a code from languages
}

// userSettings: cache entry for a user.
type userSettings struct {
	s   appSettings
	loc *time.Location
	// lastLang: the language the app last ran in for this user — used for
	// notifications when Language is "auto". Kept in memory so noteLang
	// doesn't hit the DB on every request.
	lastLang string
}

type settingsStore struct {
	mu      sync.RWMutex
	db      *sql.DB
	users   map[int64]*userSettings // lazy aus user_settings
	archive int                     // instanzweit
	// saveMu serializes save operations. A separate mutex instead of mu: save
	// waits on the single DB connection, and whoever is holding it must not
	// get stuck on mu while settings are being read.
	saveMu sync.Mutex
}

func newSettingsStore(db *sql.DB) (*settingsStore, error) {
	st := &settingsStore{db: db, users: map[int64]*userSettings{}, archive: 30}
	if v := getMeta(db, "set_archive_days"); v != "" {
		fmt.Sscanf(v, "%d", &st.archive)
	}
	return st, nil
}

func getUserSetting(db *sql.DB, uid int64, key string) string {
	var v string
	db.QueryRow("SELECT value FROM user_settings WHERE user_id = ? AND key = ?", uid, key).Scan(&v)
	return v
}

func setUserSetting(ex execer, uid int64, key, value string) error {
	_, err := ex.Exec("INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?) "+
		"ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value", uid, key, value)
	return err
}

// load fetches a user's settings (cache).
func (st *settingsStore) load(uid int64) *userSettings {
	st.mu.RLock()
	u := st.users[uid]
	st.mu.RUnlock()
	if u != nil {
		return u
	}
	s := appSettings{
		WebhookURL: getUserSetting(st.db, uid, "webhook_url"),
		TZ:         getUserSetting(st.db, uid, "tz"),
		DigestTime: getUserSetting(st.db, uid, "digest_time"),
		Language:   getUserSetting(st.db, uid, "language"),
	}
	if s.TZ == "" {
		s.TZ = "Europe/Berlin"
	}
	if s.DigestTime == "" {
		s.DigestTime = "09:00"
	}
	if s.Language == "" {
		s.Language = "auto"
	}
	loc, err := time.LoadLocation(s.TZ)
	if err != nil {
		// broken TZ in the DB (validation should prevent this): fall back instead of crashing
		s.TZ = "Europe/Berlin"
		if loc, err = time.LoadLocation(s.TZ); err != nil {
			loc = time.UTC
		}
	}
	u = &userSettings{s: s, loc: loc, lastLang: getUserSetting(st.db, uid, "last_lang")}
	st.mu.Lock()
	if cur := st.users[uid]; cur != nil {
		u = cur
	} else {
		st.users[uid] = u
	}
	st.mu.Unlock()
	return u
}

// get: uid's settings including the instance-wide archive retention period.
func (st *settingsStore) get(uid int64) appSettings {
	u := st.load(uid)
	st.mu.RLock()
	defer st.mu.RUnlock()
	s := u.s
	s.ArchiveDays = st.archive
	return s
}

func (st *settingsStore) location(uid int64) *time.Location {
	u := st.load(uid)
	st.mu.RLock()
	defer st.mu.RUnlock()
	return u.loc
}

func (st *settingsStore) archiveDays() int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.archive
}

// forget drops a user's cache entry (account removed).
func (st *settingsStore) forget(uid int64) {
	st.mu.Lock()
	delete(st.users, uid)
	st.mu.Unlock()
}

// isHost checks host against a domain including its subdomains. A plain
// strings.HasSuffix would be bypassable: "evildiscord.com" also ends in
// "discord.com" — the dot boundary must be explicit.
func isHost(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func validateSettings(s appSettings) error {
	if s.WebhookURL != "" {
		u, err := url.Parse(s.WebhookURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return uerr("error.webhook_https")
		}
		if len(s.WebhookURL) > 500 {
			return uerr("error.webhook_long")
		}
		host := strings.ToLower(u.Hostname())
		if host == "" {
			return uerr("error.webhook_host")
		}
		if ip := net.ParseIP(host); ip != nil {
			return uerr("error.webhook_ip")
		}
		if !isHost(host, "discord.com") && !isHost(host, "discordapp.com") {
			return uerr("error.webhook_discord")
		}
	}
	if _, err := time.LoadLocation(s.TZ); err != nil || s.TZ == "" {
		return uerr("error.tz_unknown", "tz", s.TZ)
	}
	if _, err := time.Parse("15:04", s.DigestTime); err != nil {
		return uerr("error.digest_time")
	}
	if s.ArchiveDays < 0 || s.ArchiveDays > 3650 {
		return uerr("error.archive_days")
	}
	// Empty is allowed: old backups and the form don't know a language.
	if s.Language != "" && !validLanguage(s.Language) {
		return uerr("error.language")
	}
	return nil
}

// save stores uid's webhook, timezone and digest time. Language
// (saveLanguage) and archive retention period (saveArchiveDays) are left
// untouched.
func (st *settingsStore) save(uid int64, s appSettings) error {
	if err := validateSettings(s); err != nil {
		return err
	}
	// normalize (e.g. "9:05" -> "09:05")
	t, _ := time.Parse("15:04", s.DigestTime)
	s.DigestTime = t.Format("15:04")
	loc, err := time.LoadLocation(s.TZ)
	if err != nil {
		return err
	}
	// All values in one transaction, and one save after another: two
	// concurrent saves (two devices, or an import) would otherwise leave a
	// mixed state in the DB that takes effect after the next restart.
	st.saveMu.Lock()
	defer st.saveMu.Unlock()
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range map[string]string{
		"webhook_url": s.WebhookURL,
		"tz":          s.TZ,
		"digest_time": s.DigestTime,
	} {
		if err := setUserSetting(tx, uid, k, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	u := st.load(uid)
	st.mu.Lock()
	s.Language = u.s.Language // belongs to saveLanguage, not the form
	s.ArchiveDays = 0         // instance-wide, lives in st.archive
	u.s = s
	u.loc = loc
	st.mu.Unlock()
	return nil
}

// saveArchiveDays sets the instance-wide archive retention period (admin only).
func (st *settingsStore) saveArchiveDays(n int) error {
	if n < 0 || n > 3650 {
		return uerr("error.archive_days")
	}
	st.saveMu.Lock()
	defer st.saveMu.Unlock()
	if err := setMeta(st.db, "set_archive_days", fmt.Sprintf("%d", n)); err != nil {
		return err
	}
	st.mu.Lock()
	st.archive = n
	st.mu.Unlock()
	return nil
}

func validLanguage(l string) bool { return l == "auto" || supportedLang(l) }

// saveLanguage sets uid's language ("auto", "de", "en"). Its own route, so
// saving the rest of the form never resets it.
func (st *settingsStore) saveLanguage(uid int64, lang string) error {
	if !validLanguage(lang) {
		return uerr("error.language")
	}
	st.saveMu.Lock()
	defer st.saveMu.Unlock()
	if err := setUserSetting(st.db, uid, "language", lang); err != nil {
		return err
	}
	u := st.load(uid)
	st.mu.Lock()
	u.s.Language = lang
	st.mu.Unlock()
	return nil
}

// noteLang records the language the app is currently running in for uid
// (X-Lang of an authenticated request). Only written on a change.
func (st *settingsStore) noteLang(uid int64, lang string) {
	if !supportedLang(lang) {
		return
	}
	u := st.load(uid)
	st.mu.RLock()
	same := u.lastLang == lang
	st.mu.RUnlock()
	if same {
		return
	}
	if err := setUserSetting(st.db, uid, "last_lang", lang); err != nil {
		return // convenience: retried on the next request
	}
	st.mu.Lock()
	u.lastLang = lang
	st.mu.Unlock()
}

// notifyLang: the language for Discord and push notifications to uid — the
// configured one, or on "auto" the last used one, or the reference language
// if neither is set.
func (st *settingsStore) notifyLang(uid int64) string {
	u := st.load(uid)
	st.mu.RLock()
	defer st.mu.RUnlock()
	if supportedLang(u.s.Language) {
		return u.s.Language
	}
	if u.lastLang != "" {
		return u.lastLang
	}
	return refLang
}
