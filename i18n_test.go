package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

var placeholderRe = regexp.MustCompile(`\{([a-z]+)\}`)

func placeholders(s string) []string {
	var out []string
	for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// Every language has exactly the reference language's keys — a missing key
// would silently fall back to German, an extra one would be a typo.
func TestCatalogsHaveSameKeys(t *testing.T) {
	ref := slices.Sorted(maps.Keys(catalogs[refLang]))
	for _, lang := range languages {
		got := slices.Sorted(maps.Keys(catalogs[lang]))
		if !slices.Equal(got, ref) {
			for _, k := range ref {
				if _, ok := catalogs[lang][k]; !ok {
					t.Errorf("%s: fehlt %q", lang, k)
				}
			}
			for _, k := range got {
				if _, ok := catalogs[refLang][k]; !ok {
					t.Errorf("%s: überzählig %q", lang, k)
				}
			}
		}
	}
}

// The same placeholders in every language — otherwise the code sets a value
// the translation never shows, or the translation shows "{name}" raw.
// fmt.* always get the full set (fmtParams) and may pick freely from it.
func TestCatalogsHaveSameParams(t *testing.T) {
	fmtParams := []string{"d", "dd", "mm", "mon", "month", "wd"}
	for _, lang := range languages {
		for k, v := range catalogs[lang] {
			if strings.HasPrefix(k, "fmt.") {
				for _, p := range placeholders(v) {
					if !slices.Contains(fmtParams, p) {
						t.Errorf("%s %s: unbekannter Parameter {%s}", lang, k, p)
					}
				}
				continue
			}
			if want := placeholders(catalogs[refLang][k]); !slices.Equal(placeholders(v), want) {
				t.Errorf("%s %s: Parameter %v, Referenz %v", lang, k, placeholders(v), want)
			}
		}
	}
}

func TestCatalogPluralsComplete(t *testing.T) {
	for k := range catalogs[refLang] {
		for _, form := range []string{".one", ".zero"} {
			if base, ok := strings.CutSuffix(k, form); ok {
				if _, ok := catalogs[refLang][base+".other"]; !ok {
					t.Errorf("%s ohne %s.other", k, base)
				}
			}
		}
		if base, ok := strings.CutSuffix(k, ".other"); ok {
			if _, ok := catalogs[refLang][base+".one"]; !ok {
				t.Errorf("%s ohne %s.one", k, base)
			}
		}
	}
}

func TestT(t *testing.T) {
	cases := []struct {
		lang, key string
		kv        []any
		want      string
	}{
		{"en", "header.open", []any{"n", 0}, "nothing open"},
		{"en", "header.open", []any{"n", 1}, "1 task open"},
		{"en", "header.open", []any{"n", 3}, "3 tasks open"},
		{"de", "header.open", []any{"n", 3}, "3 Aufgaben offen"},
		{"de", "stats.days", []any{"n", 0}, "0 Tage"}, // no .zero → falls back to .other
		{"de", "error.max_images", []any{"n", 20}, "maximal 20 Bilder pro Aufgabe"},
		// Values aren't substituted again and aren't escaped
		{"en", "task.deleted", []any{"title", "{title} <b>"}, "“{title} <b>” deleted"},
		// A missing parameter stays visible instead of silently disappearing
		{"en", "task.deleted", nil, "“{title}” deleted"},
		{"en", "gibt.es.nicht", nil, "gibt.es.nicht"},
		{"xx", "common.save", nil, "Speichern"}, // unknown language → reference (German)
	}
	for _, c := range cases {
		if got := T(c.lang, c.key, c.kv...); got != c.want {
			t.Errorf("T(%s, %s, %v) = %q, erwartet %q", c.lang, c.key, c.kv, got, c.want)
		}
	}
}

// If a key is missing in a language, the German text applies.
func TestTFallsBackToReference(t *testing.T) {
	orig := catalogs["en"]
	t.Cleanup(func() { catalogs["en"] = orig })
	en := maps.Clone(orig)
	delete(en, "common.save")
	catalogs["en"] = en
	if got := T("en", "common.save"); got != "Speichern" {
		t.Fatalf("got %q", got)
	}
}

func TestRequestLang(t *testing.T) {
	cases := []struct{ xlang, accept, want string }{
		{"en", "de-DE", "en"},
		{"DE", "", "de"},
		{" de ", "", "de"},
		{"xx", "de-CH,de;q=0.9", "de"},
		{"", "fr-FR, en;q=0.5", "en"},
		{"", "fr-FR", "en"},
		{"", "", "en"},
		{"", "*", "en"},
		{"", "de-CH;q=0.9, *", "de"},
		{"", ";;,", "en"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.xlang != "" {
			r.Header.Set("X-Lang", c.xlang)
		}
		if c.accept != "" {
			r.Header.Set("Accept-Language", c.accept)
		}
		if got := requestLang(r); got != c.want {
			t.Errorf("X-Lang %q, Accept-Language %q: %q, erwartet %q", c.xlang, c.accept, got, c.want)
		}
	}
}

func TestFormatDate(t *testing.T) {
	cases := []struct{ lang, in, want string }{
		{"de", "2026-09-24", "Do 24.09."},
		{"en", "2026-09-24", "Thu Sep 24"},
		{"de", "2026-03-01", "So 01.03."},
		{"en", "2026-03-01", "Sun Mar 1"},
		{"en", "kaputt", "kaputt"},
	}
	for _, c := range cases {
		if got := formatDate(c.lang, c.in); got != c.want {
			t.Errorf("formatDate(%s, %s) = %q, erwartet %q", c.lang, c.in, got, c.want)
		}
	}
}

func TestLoadCatalogsRejectsBrokenFiles(t *testing.T) {
	ok := `{"a": "b"}`
	for name, fsys := range map[string]fstest.MapFS{
		"kaputtes JSON": {"static/locales/de.json": {Data: []byte(ok)}, "static/locales/en.json": {Data: []byte(`{"a": `)}},
		"Datei fehlt":   {"static/locales/de.json": {Data: []byte(ok)}},
	} {
		if _, err := loadCatalogs(fsys); err == nil {
			t.Errorf("%s: kein Fehler", name)
		}
	}
}

// errorOf calls h with X-Lang set to lang and returns the error message.
func errorOf(t *testing.T, h http.HandlerFunc, lang, method, target, body string, pv ...string) (int, string) {
	t.Helper()
	req := withUser(httptest.NewRequest(method, target, strings.NewReader(body)), testUID)
	req.Header.Set("X-Lang", lang)
	for i := 0; i+1 < len(pv); i += 2 {
		req.SetPathValue(pv[i], pv[i+1])
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	var d struct{ Error string }
	json.Unmarshal(rec.Body.Bytes(), &d)
	return rec.Code, d.Error
}

func TestAPIErrorsFollowRequestLanguage(t *testing.T) {
	s := testServer(t)
	task := mustCreate(t, s, `{"title":"Bild-Task"}`)
	cases := []struct {
		name   string
		h      http.HandlerFunc
		method string
		body   string
		pv     []string
		de, en string
	}{
		{"Titel fehlt", s.handleCreateTask, "POST", `{"title":" "}`, nil, "Titel fehlt", "Title is missing"},
		{"Webhook", s.handlePutSettings, "PUT", `{"webhook_url":"http://x","tz":"Europe/Berlin","digest_time":"09:00"}`, nil,
			"Webhook-URL muss eine https-URL sein", "Webhook URL must be an https URL"},
		{"Zeitzone mit Parameter", s.handlePutSettings, "PUT", `{"tz":"Mars/Base","digest_time":"09:00"}`, nil,
			"unbekannte Zeitzone „Mars/Base“", "unknown time zone “Mars/Base”"},
		{"kein Bild", s.handleAttachmentUpload, "POST", "hallo", []string{"id", fmt.Sprint(task.ID)},
			"nur Bilder (JPEG, PNG, WebP, GIF)", "images only (JPEG, PNG, WebP, GIF)"},
		{"Task fehlt", s.handleDeleteTask, "DELETE", "", []string{"id", "999"}, "Task nicht gefunden", "Task not found"},
	}
	for _, c := range cases {
		for lang, want := range map[string]string{"de": c.de, "en": c.en} {
			code, got := errorOf(t, c.h, lang, c.method, "/", c.body, c.pv...)
			if code < 400 || got != want {
				t.Errorf("%s (%s): %d %q, erwartet %q", c.name, lang, code, got, want)
			}
		}
	}
}

// Every key used by Go code is in the catalog — either as a key or as a
// plural base. And writeError never gets a literal text instead of a key.
func TestGoKeysExist(t *testing.T) {
	keyRe := regexp.MustCompile(`"((?:error|notify|date|fmt)\.[a-z0-9_.]+)"`)
	callRe := regexp.MustCompile(`writeError\(w, r, [^,]+, "([^"]*)"`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range keyRe.FindAllStringSubmatch(string(src), -1) {
			// Hostnames look like keys (pushHosts: "notify.windows.com")
			if strings.HasSuffix(m[1], ".com") {
				continue
			}
			if !keyExists(m[1]) {
				t.Errorf("%s: Key %q fehlt im Katalog", f, m[1])
			}
		}
		for _, m := range callRe.FindAllStringSubmatch(string(src), -1) {
			if !keyExists(m[1]) {
				t.Errorf("%s: writeError mit %q — das ist kein Katalog-Key", f, m[1])
			}
		}
		if bytes.Contains(src, []byte("writeError(w, http.")) {
			t.Errorf("%s: writeError ohne Request (alte Signatur)", f)
		}
	}
}

func keyExists(k string) bool {
	_, ok := catalogs[refLang][k]
	_, plural := catalogs[refLang][k+".other"]
	return ok || plural
}

func TestSetLanguageRoute(t *testing.T) {
	s := testServer(t)
	rec := call(t, s.handleSetLanguage, http.MethodPut, "/", strings.NewReader(`{"language":"en"}`))
	if rec.Code != http.StatusOK || s.store.get(testUID).Language != "en" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	code, msg := errorOf(t, s.handleSetLanguage, "en", "PUT", "/", `{"language":"fr"}`)
	if code != http.StatusBadRequest || msg != "unknown language" {
		t.Fatalf("%d %q", code, msg)
	}
	var got appSettings
	json.Unmarshal(call(t, s.handleGetSettings, http.MethodGet, "/", nil).Body.Bytes(), &got)
	if got.Language != "en" {
		t.Fatalf("GET /api/settings: %+v", got)
	}
}

// requireAuth remembers the app's language (X-Lang) for notifications.
func TestRequireAuthNotesLanguage(t *testing.T) {
	s := testServer(t)
	mustCreateSession(t, s.db, "tok", time.Now().Add(time.Hour), 0, testUID)
	h := s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(&http.Cookie{Name: s.cookieName(sessionCookie), Value: "tok"})
	req.Header.Set("X-Lang", "en")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := s.store.notifyLang(testUID); got != "en" {
		t.Fatalf("notifyLang %q", got)
	}
	// Without a session, X-Lang doesn't count.
	anon := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	anon.Header.Set("X-Lang", "de")
	h.ServeHTTP(httptest.NewRecorder(), anon)
	if got := s.store.notifyLang(testUID); got != "en" {
		t.Fatalf("anonym überschrieben: %q", got)
	}
}

// Old backups don't know a language — the configured one stays.
func TestImportKeepsLanguageWithoutOne(t *testing.T) {
	s := testServer(t)
	s.store.saveLanguage(testUID, "en")
	old := exportData{Settings: appSettings{TZ: "Europe/Berlin", DigestTime: "09:00", ArchiveDays: 30}}
	if rec := postImport(t, s, old); rec.Code != http.StatusNoContent {
		t.Fatalf("Import: %d %s", rec.Code, rec.Body)
	}
	if got := s.store.get(testUID).Language; got != "en" {
		t.Fatalf("nach altem Backup %q", got)
	}
	withLang := old
	withLang.Settings.Language = "de"
	postImport(t, s, withLang)
	if got := s.store.get(testUID).Language; got != "de" {
		t.Fatalf("Backup mit Sprache: %q", got)
	}
}

// Every key from JS (t('…')) and HTML (data-i18n, data-i18n-attr) is in the
// catalog. The scan can't see dynamically assembled keys — that's why such
// keys appear in the code as literals (lists, maps).
func TestStaticKeysExist(t *testing.T) {
	jsRe := regexp.MustCompile(`\bt\('([a-z0-9_.]+)'`)
	textRe := regexp.MustCompile(`data-i18n="([^"]+)"`)
	attrRe := regexp.MustCompile(`data-i18n-attr="([^"]+)"`)
	files, _ := filepath.Glob("static/*.js")
	html, _ := filepath.Glob("static/*.html")
	for _, f := range append(files, html...) {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, m := range jsRe.FindAllStringSubmatch(string(src), -1) {
			keys = append(keys, m[1])
		}
		for _, m := range textRe.FindAllStringSubmatch(string(src), -1) {
			keys = append(keys, m[1])
		}
		for _, m := range attrRe.FindAllStringSubmatch(string(src), -1) {
			for pair := range strings.SplitSeq(m[1], ";") {
				_, key, ok := strings.Cut(pair, ":")
				if !ok {
					t.Errorf("%s: data-i18n-attr %q ohne attr:key", f, pair)
					continue
				}
				keys = append(keys, strings.TrimSpace(key))
			}
		}
		for _, k := range keys {
			if !keyExists(k) {
				t.Errorf("%s: Key %q fehlt im Katalog", f, k)
			}
		}
	}
}

// SUPPORTED in i18n.js and languages in i18n.go are the same list.
func TestClientLanguagesMatchServer(t *testing.T) {
	src, err := os.ReadFile("static/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const SUPPORTED = \[([^\]]*)\]`).FindSubmatch(src)
	if m == nil {
		t.Fatal("SUPPORTED nicht gefunden")
	}
	var got []string
	for _, q := range regexp.MustCompile(`'([a-z]+)'`).FindAllSubmatch(m[1], -1) {
		got = append(got, string(q[1]))
	}
	if !slices.Equal(got, languages) {
		t.Fatalf("i18n.js %v, i18n.go %v", got, languages)
	}
	for _, l := range languages {
		if !bytes.Contains(src, []byte(l+": '")) {
			t.Errorf("LANG_NAMES ohne %s", l)
		}
	}
}

func TestLocaleRoute(t *testing.T) {
	for path, want := range map[string]int{
		"en.json": http.StatusOK, "de.json": http.StatusOK,
		"fr.json": http.StatusNotFound, "en": http.StatusNotFound, "..%2Fmain.go": http.StatusNotFound,
	} {
		rec := call(t, handleLocale, http.MethodGet, "/locales/"+path, nil, "file", path)
		if rec.Code != want {
			t.Errorf("%s: %d, erwartet %d", path, rec.Code, want)
		}
		if want == http.StatusOK && !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: Content-Type %q", path, rec.Header().Get("Content-Type"))
		}
	}
}

// Every catalog key is used somewhere — as a literal in Go/JS/HTML, in
// data-i18n-attr, or as a plural base. Otherwise the catalog accumulates
// dead entries that translators maintain for nothing.
func TestNoUnusedKeys(t *testing.T) {
	litRe := regexp.MustCompile(`['"]([a-z0-9_]+(?:\.[a-z0-9_]+)+)['"]`)
	attrRe := regexp.MustCompile(`data-i18n-attr="([^"]+)"`)
	used := map[string]bool{}
	var files []string
	for _, pat := range []string{"*.go", "static/*.js", "static/*.html"} {
		m, _ := filepath.Glob(pat)
		files = append(files, m...)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range litRe.FindAllStringSubmatch(string(src), -1) {
			used[m[1]] = true
		}
		for _, m := range attrRe.FindAllStringSubmatch(string(src), -1) {
			for pair := range strings.SplitSeq(m[1], ";") {
				if _, k, ok := strings.Cut(pair, ":"); ok {
					used[strings.TrimSpace(k)] = true
				}
			}
		}
	}
	for k := range catalogs[refLang] {
		base := k
		for _, form := range []string{".one", ".other", ".zero"} {
			base = strings.TrimSuffix(base, form)
		}
		if !used[k] && !used[base] {
			t.Errorf("Key %q wird nirgends benutzt", k)
		}
	}
}

// The 403 from requireFreshAuth carries a message alongside "reauth" that
// the client can display — also in the request's language.
func TestFreshAuthMessageFollowsLanguage(t *testing.T) {
	s := testServer(t)
	mustCreateSession(t, s.db, "alt", time.Now().Add(sessionIdleTimeout), 1, testUID)
	stale := time.Now().Add(-reauthWindow - time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec("UPDATE sessions SET auth_at = ? WHERE token = ?", stale, hashToken("alt")); err != nil {
		t.Fatal(err)
	}
	req := passkeyReq(s, http.MethodPost, "alt", "1")
	req.Header.Set("X-Lang", "en")
	rec := httptest.NewRecorder()
	s.handlePasskeyBegin(rec, req)
	var body struct {
		Error  string
		Reauth bool
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || !body.Reauth || body.Error != "For security, confirm with an existing passkey first" {
		t.Fatalf("%d %+v", rec.Code, body)
	}
}
