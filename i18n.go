package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

// languages are the supported UI languages. New language: create the file
// static/locales/<code>.json and add it here, plus to SUPPORTED and
// LANG_NAMES in static/i18n.js (a test keeps the lists in sync).
var languages = []string{"de", "en"}

// refLang is the reference: every key exists there; if it's missing in
// another language, the text from here is used.
const refLang = "de"

// defaultLang applies to requests without a matching language (unknown browser).
const defaultLang = "en"

// catalogs: language -> key -> text, loaded from the embedded FS at
// startup. Broken catalogs abort startup (and thus every test).
var catalogs = mustLoadCatalogs(staticFS)

func mustLoadCatalogs(fsys fs.FS) map[string]map[string]string {
	c, err := loadCatalogs(fsys)
	if err != nil {
		panic(err)
	}
	return c
}

func loadCatalogs(fsys fs.FS) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, lang := range languages {
		data, err := fs.ReadFile(fsys, "static/locales/"+lang+".json")
		if err != nil {
			return nil, fmt.Errorf("catalog %s: %w", lang, err)
		}
		var m map[string]string
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", lang, err)
		}
		out[lang] = m
	}
	return out, nil
}

func supportedLang(l string) bool { return slices.Contains(languages, l) }

var placeholder = regexp.MustCompile(`\{[a-z]+\}`)

// T translates key into lang; kv are name/value pairs for {name}
// placeholders. If key has no entry of its own but has key.one/key.other,
// it's a plural: chosen by the int parameter n (key.zero for 0, if
// present). Substitution happens in a single pass — a value containing
// "{title}" stays literal. Unknown key: the key itself (tests prevent
// this, see i18n_test.go).
func T(lang, key string, kv ...any) string {
	params := map[string]string{}
	n, hasN := 0, false
	for i := 0; i+1 < len(kv); i += 2 {
		name, _ := kv[i].(string)
		params[name] = fmt.Sprint(kv[i+1])
		if v, ok := kv[i+1].(int); ok && name == "n" {
			n, hasN = v, true
		}
	}
	msg, ok := lookup(lang, key)
	if !ok && hasN {
		form := "other"
		if n == 1 {
			form = "one"
		}
		if _, zero := lookup(lang, key+".zero"); zero && n == 0 {
			form = "zero"
		}
		msg, ok = lookup(lang, key+"."+form)
	}
	if !ok {
		return key
	}
	return placeholder.ReplaceAllStringFunc(msg, func(m string) string {
		if v, ok := params[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

func lookup(lang, key string) (string, bool) {
	if m, ok := catalogs[lang][key]; ok {
		return m, true
	}
	m, ok := catalogs[refLang][key]
	return m, ok
}

// requestLang: the language of the response to r — X-Lang (the language
// the app is currently running in), otherwise the first supported one from
// Accept-Language (browsers send it sorted by preference), otherwise
// defaultLang.
func requestLang(r *http.Request) string {
	if l := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Lang"))); supportedLang(l) {
		return l
	}
	for part := range strings.SplitSeq(r.Header.Get("Accept-Language"), ",") {
		tag, _, _ := strings.Cut(part, ";")
		prim, _, _ := strings.Cut(strings.TrimSpace(tag), "-")
		if l := strings.ToLower(prim); supportedLang(l) {
			return l
		}
	}
	return defaultLang
}

// Keys for weekday and month names, as literals (the unused-keys test
// searches for them). Long weekday names (date.wd.*) are only needed by
// the client.
var (
	wdShortKeys = [...]string{"date.wds.0", "date.wds.1", "date.wds.2", "date.wds.3", "date.wds.4", "date.wds.5", "date.wds.6"}
	monthKeys   = [...]string{"", "date.mon.1", "date.mon.2", "date.mon.3", "date.mon.4", "date.mon.5", "date.mon.6",
		"date.mon.7", "date.mon.8", "date.mon.9", "date.mon.10", "date.mon.11", "date.mon.12"}
	monShortKeys = [...]string{"", "date.mons.1", "date.mons.2", "date.mons.3", "date.mons.4", "date.mons.5", "date.mons.6",
		"date.mons.7", "date.mons.8", "date.mons.9", "date.mons.10", "date.mons.11", "date.mons.12"}
)

// formatDate: "YYYY-MM-DD" short with weekday — de "Do 24.09.", en "Thu Sep
// 24". The pattern lives in the catalog (fmt.date_short) and always
// receives all parameters, matching the client (dateParams in app.js).
func formatDate(lang, date string) string {
	d, err := time.Parse(dateFmt, date)
	if err != nil {
		return date
	}
	return T(lang, "fmt.date_short",
		"wd", T(lang, wdShortKeys[d.Weekday()]),
		"d", d.Day(),
		"dd", fmt.Sprintf("%02d", d.Day()),
		"mm", fmt.Sprintf("%02d", int(d.Month())),
		"mon", T(lang, monShortKeys[d.Month()]),
		"month", T(lang, monthKeys[d.Month()]))
}

// userError is a message to the user: a catalog key with its parameters.
// It's only translated when responding (writeErr), in the request's
// language.
type userError struct {
	Key string
	KV  []any
}

func (e *userError) Error() string { return e.Key }

func uerr(key string, kv ...any) *userError { return &userError{Key: key, KV: kv} }

// writeErr reports err: a userError is translated with code, everything
// else becomes a 500 "internal error" — an internal error text never goes
// to the client.
func writeErr(w http.ResponseWriter, r *http.Request, code int, err error) {
	if ue, ok := errors.AsType[*userError](err); ok {
		writeError(w, r, code, ue.Key, ue.KV...)
		return
	}
	writeError(w, r, http.StatusInternalServerError, "error.internal")
}

// handleLocale serves a catalog (public like css/js: login and setup need
// it without being logged in). Only known languages, no path.
func handleLocale(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	lang, ok := strings.CutSuffix(file, ".json")
	if !ok || !supportedLang(lang) {
		http.NotFound(w, r)
		return
	}
	staticFile("static/locales/"+file)(w, r)
}
