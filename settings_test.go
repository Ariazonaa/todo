package main

import "testing"

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
