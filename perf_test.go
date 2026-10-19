package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// linksPlan returns the plan lines of q that touch the links table.
func linksPlan(t *testing.T, s *server, q string) []string {
	t.Helper()
	args := make([]any, strings.Count(q, "?"))
	for i := range args {
		args[i] = 1
	}
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("EXPLAIN %q: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "links") {
			out = append(out, detail)
		}
	}
	return out
}

// Links queries that constrain a and b with IN must not seek the primary key
// with both columns: that is one seek per pair of the two lists (n×n; 3000
// tasks took ~1 s per GET /api/tasks). The unary plus (`+b IN`) prevents it.
func TestLinksQueryPlans(t *testing.T) {
	s := testServer(t)
	// Control: without the plus SQLite does pick the n×n plan — otherwise
	// this test would no longer notice anything.
	ctrl := strings.ReplaceAll(attachLinksQuery, "+b IN", "b IN")
	if p := strings.Join(linksPlan(t, s, ctrl), " | "); !strings.Contains(p, "a=? AND b=?") {
		t.Fatalf("Kontrolle: Plan ohne Plus zeigt kein a=? AND b=? mehr: %s", p)
	}
	for name, q := range map[string]string{
		"attachLinks":  attachLinksQuery,
		"export":       exportLinksQuery,
		"unlinkAcross": unlinkAcrossQuery,
		"visibleLinks": "SELECT " + visibleLinks,
	} {
		plan := linksPlan(t, s, q)
		if len(plan) == 0 {
			t.Errorf("%s: kein Planschritt auf links", name)
		}
		for _, d := range plan {
			if strings.Contains(d, "a=? AND b=?") {
				t.Errorf("%s: n×n-Plan auf links: %s", name, d)
			}
		}
	}
}

// Embedded files carry an ETag (embed.FS has no ModTime): the no-cache
// revalidation then answers 304 instead of the whole file again.
func TestStaticETag(t *testing.T) {
	s := testServer(t)
	h := s.routes(newRateLimiter(1000, 1000), newRateLimiter(1000, 1000))
	get := func(path, inm string, serve func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		rec := httptest.NewRecorder()
		serve(rec, req)
		return rec
	}
	for _, path := range []string{"/app.js", "/style.css", "/sw.js", "/manifest.webmanifest", "/locales/de.json", "/icons/icon-192.png", "/"} {
		serve := h.ServeHTTP
		if path == "/" { // behind requireAuth — the handler itself
			serve = s.handleIndex
		}
		first := get(path, "", serve)
		tag := first.Header().Get("ETag")
		if first.Code != http.StatusOK || !strings.HasPrefix(tag, `"`) || first.Body.Len() == 0 {
			t.Errorf("%s: %d, ETag %q, %d Bytes", path, first.Code, tag, first.Body.Len())
			continue
		}
		if cc := first.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q", path, cc)
		}
		if again := get(path, tag, serve); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
			t.Errorf("%s: mit If-None-Match %d, %d Bytes", path, again.Code, again.Body.Len())
		}
		// Weak form as nginx's gzip passes it on.
		if weak := get(path, "W/"+tag, serve); weak.Code != http.StatusNotModified {
			t.Errorf("%s: mit schwachem ETag %d", path, weak.Code)
		}
		if other := get(path, `"anders"`, serve); other.Code != http.StatusOK {
			t.Errorf("%s: mit fremdem ETag %d", path, other.Code)
		}
	}
	// Different content, different tag.
	if staticETags["static/app.js"] == staticETags["static/style.css"] {
		t.Error("app.js und style.css haben dasselbe ETag")
	}
}

// synchronous(NORMAL) from the DSN applies to the connection (1 = NORMAL).
func TestSynchronousNormal(t *testing.T) {
	s := testServer(t)
	var v int
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&v); err != nil || v != 1 {
		t.Fatalf("PRAGMA synchronous = %d (%v), want 1", v, err)
	}
}
