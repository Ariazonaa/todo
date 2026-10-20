package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Saved settings must survive a restart — all values, not just some.
func TestSettingsSaveSurvivesRestart(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	st, err := newSettingsStore(db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	want := appSettings{WebhookURL: "https://discord.com/api/webhooks/1/x", TZ: "Europe/Vienna", DigestTime: "07:30", ArchiveDays: 30, Language: "auto"} // archive retention is instance-wide, save leaves it as is
	if err := st.save(testUID, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded, err := newSettingsStore(db)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.get(testUID); got != want {
		t.Fatalf("nach Neustart %+v, gespeichert war %+v", got, want)
	}
}

func TestValidateSettingsRejectsNonDiscordWebhook(t *testing.T) {
	err := validateSettings(appSettings{
		WebhookURL:  "https://evil.example.com/api/webhooks/123/token",
		TZ:          "Europe/Berlin",
		DigestTime:  "09:00",
		ArchiveDays: 30,
	})
	if err == nil {
		t.Fatal("expected error for non-Discord webhook")
	}
}

func TestValidateSettingsAllowsDiscordWebhook(t *testing.T) {
	err := validateSettings(appSettings{
		WebhookURL:  "https://discord.com/api/webhooks/123/token",
		TZ:          "Europe/Berlin",
		DigestTime:  "09:00",
		ArchiveDays: 30,
	})
	if err != nil {
		t.Fatalf("expected Discord webhook to be accepted: %v", err)
	}
}

func TestLanguageSettingSurvivesRestartAndFormSave(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, _ := newSettingsStore(db)
	if got := st.get(testUID).Language; got != "auto" {
		t.Fatalf("Standard %q, erwartet auto", got)
	}
	if err := st.saveLanguage(testUID, "en"); err != nil {
		t.Fatal(err)
	}
	// The form doesn't send a language — it must not fall back to the
	// default in the process.
	if err := st.save(testUID, appSettings{TZ: "Europe/Berlin", DigestTime: "09:00", ArchiveDays: 30}); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := newSettingsStore(db)
	if got := reloaded.get(testUID).Language; got != "en" {
		t.Fatalf("nach Neustart %q, erwartet en", got)
	}
	for _, bad := range []string{"", "fr", "EN", "de-DE"} {
		if err := st.saveLanguage(testUID, bad); err == nil {
			t.Errorf("%q angenommen", bad)
		}
	}
}

func TestNotifyLang(t *testing.T) {
	db, _ := openDB(":memory:")
	defer db.Close()
	st, _ := newSettingsStore(db)
	if got := st.notifyLang(testUID); got != "de" {
		t.Fatalf("frisch: %q", got)
	}
	st.noteLang(testUID, "en")
	st.noteLang(testUID, "xx") // unknown: ignored
	if got := st.notifyLang(testUID); got != "en" {
		t.Fatalf("nach Nutzung auf Englisch: %q", got)
	}
	st.saveLanguage(testUID, "de")
	if got := st.notifyLang(testUID); got != "de" {
		t.Fatalf("fest eingestellt: %q", got)
	}
	reloaded, _ := newSettingsStore(db)
	reloaded.saveLanguage(testUID, "auto")
	if got := reloaded.notifyLang(testUID); got != "en" {
		t.Fatalf("last_lang nach Neustart: %q", got)
	}
}

// noteLang runs on every request: no write when the language is unchanged.
func TestNoteLangWritesOnlyOnChange(t *testing.T) {
	db, _ := openDB(":memory:")
	defer db.Close()
	st, _ := newSettingsStore(db)
	st.noteLang(testUID, "en")
	db.Exec("DELETE FROM user_settings WHERE key = 'last_lang'")
	st.noteLang(testUID, "en")
	if getUserSetting(db, testUID, "last_lang") != "" {
		t.Fatal("gleiche Sprache erneut geschrieben")
	}
	st.noteLang(testUID, "de")
	if getUserSetting(db, testUID, "last_lang") != "de" {
		t.Fatal("Wechsel nicht geschrieben")
	}
}

// "Local" loads, but would mean the server's zone, not the user's.
func TestValidateSettingsRejectsLocalTZ(t *testing.T) {
	if validateSettings(appSettings{TZ: "Local", DigestTime: "09:00", ArchiveDays: 30}) == nil {
		t.Fatal(`expected error for TZ "Local"`)
	}
}

// The webhook URL is a credential: whoever has it can post to the channel
// (with any name), rename or delete the webhook — even after "sign out
// everywhere". A stolen session cookie must not be able to read it, neither
// from the settings nor from a backup. The form gets the URL with the token
// masked; sending that back (or leaving the field out) keeps the webhook.
func TestSettingsDoNotRevealWebhook(t *testing.T) {
	s := testServer(t)
	hook := "https://discord.com/api/webhooks/123456/geheimes-token"
	if err := s.store.save(testUID, appSettings{WebhookURL: hook, TZ: "Europe/Berlin", DigestTime: "09:00"}); err != nil {
		t.Fatal(err)
	}
	rec := serveAuthed(s, s.handleGetSettings, staleRequest(t, s, testUID, "GET", ""))
	var got struct {
		WebhookURL string `json:"webhook_url"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if strings.Contains(rec.Body.String(), "geheimes-token") {
		t.Fatalf("Einstellungen verraten den Webhook: %s", rec.Body)
	}
	if !strings.Contains(got.WebhookURL, "/123456/") {
		t.Fatalf("Webhook nicht wiederzuerkennen: %q", got.WebhookURL)
	}
	for name, body := range map[string]string{
		"maskiert zurück": `{"webhook_url":"` + got.WebhookURL + `","tz":"Europe/Vienna","digest_time":"08:00","archive_days":30}`,
		"ohne Feld":       `{"tz":"Europe/Vienna","digest_time":"08:00","archive_days":30}`,
	} {
		if rec := serveAuthed(s, s.handlePutSettings, staleRequest(t, s, testUID, "PUT", body)); rec.Code != http.StatusOK {
			t.Fatalf("%s mit alter Sitzung: %d %s", name, rec.Code, rec.Body)
		}
		if cur := s.store.get(testUID).WebhookURL; cur != hook {
			t.Fatalf("%s: Webhook verändert zu %q", name, cur)
		}
	}
	if rec := call(t, s.handleExport, http.MethodGet, "/", nil); strings.Contains(rec.Body.String(), "geheimes-token") {
		t.Fatal("Backup enthält den Webhook")
	}
}

// The mask hides the token in every URL form Discord accepts — also with a
// suffix (/slack, /github), a trailing slash or an API version.
func TestMaskWebhookHidesTokenInEveryForm(t *testing.T) {
	for _, raw := range []string{
		"https://discord.com/api/webhooks/123/tok",
		"https://discord.com/api/webhooks/123/tok/slack",
		"https://discord.com/api/webhooks/123/tok//",
		"https://discord.com/api/v10/webhooks/123/tok?wait=true",
		"https://discord.com/irgendwas/tok",
	} {
		if got := maskWebhook(raw); strings.Contains(got, "tok") {
			t.Errorf("%s → %s", raw, got)
		}
	}
	if got := maskWebhook("https://discord.com/api/webhooks/123/tok/slack"); !strings.Contains(got, "/123/") {
		t.Errorf("Webhook-ID fehlt in %q", got)
	}
}

// A form opened before the webhook changed elsewhere sends the old mask
// back: that's never a webhook URL, it keeps the current one.
func TestStaleWebhookMaskKeepsWebhook(t *testing.T) {
	s := testServer(t)
	hook := "https://discord.com/api/webhooks/2/neu"
	s.store.save(testUID, appSettings{WebhookURL: hook, TZ: "Europe/Berlin", DigestTime: "09:00"})
	body := `{"webhook_url":"https://discord.com/api/webhooks/1/…","tz":"Europe/Berlin","digest_time":"09:00","archive_days":30}`
	rec := call(t, s.handlePutSettings, "PUT", "/", strings.NewReader(body))
	if rec.Code != http.StatusOK || s.store.get(testUID).WebhookURL != hook {
		t.Fatalf("alte Maske: %d, Webhook jetzt %q", rec.Code, s.store.get(testUID).WebhookURL)
	}
	if strings.Contains(rec.Body.String(), "neu") {
		t.Fatalf("Antwort verrät den Webhook: %s", rec.Body)
	}
}
