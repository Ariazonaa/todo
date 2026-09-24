package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// discordStub accepts webhook posts and remembers the messages.
type discordStub struct {
	mu       sync.Mutex
	payloads []map[string]any
}

func newDiscordStub(t *testing.T) (*discordStub, string) {
	t.Helper()
	d := &discordStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("Webhook-Body ist kein JSON: %v", err)
		}
		d.mu.Lock()
		d.payloads = append(d.payloads, p)
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return d, srv.URL + "/api/webhooks/1/token"
}

func (d *discordStub) contents() []string {
	var out []string
	for _, p := range d.all() {
		s, _ := p["content"].(string)
		out = append(out, s)
	}
	return out
}

// all reads under the lock: the handler writes from the server goroutine,
// and across the network the race detector sees no ordering.
func (d *discordStub) all() []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]map[string]any(nil), d.payloads...)
}

// The HTTP client's *url.Error contains the full URL — including the token
// for a webhook. The message ends up in the log (logErr) and in the
// test-ping response.
func TestPostDiscordErrorHidesWebhookToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hard-cut the connection: transport error instead of HTTP status
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()

	err := postDiscord(srv.URL+"/api/webhooks/123/GEHEIMER-TOKEN", "hallo")
	if err == nil {
		t.Fatal("erwartet einen Fehler bei gekappter Verbindung")
	}
	if strings.Contains(err.Error(), "GEHEIMER-TOKEN") {
		t.Fatalf("Webhook-Token steht in der Fehlermeldung: %v", err)
	}
}

// Task titles are free text — an "@everyone" in one must not ping the
// whole Discord server.
func TestPostDiscordSuppressesMentions(t *testing.T) {
	d, url := newDiscordStub(t)
	if err := postDiscord(url, "@everyone Müll raus"); err != nil {
		t.Fatalf("postDiscord: %v", err)
	}
	p := d.all()[0]
	am, ok := p["allowed_mentions"].(map[string]any)
	if !ok {
		t.Fatalf("allowed_mentions fehlt: %v", p)
	}
	parse, ok := am["parse"].([]any)
	if !ok || len(parse) != 0 {
		t.Fatalf("allowed_mentions.parse muss eine leere Liste sein, ist %v", am["parse"])
	}
}

func schedulerFixture(t *testing.T) *scheduler {
	t.Helper()
	s := testServer(t)
	return newScheduler(s.db, s.store, s.hub, s.vapid)
}

// If not all due tasks fit into one Discord message, only the ones that
// were actually included may be stamped — the rest come in the next tick.
// postDiscord used to truncate, and the truncated tasks still counted as
// reminded: they were never pinged.
func TestExactRemindersOnlyStampsWhatWasSent(t *testing.T) {
	sc := schedulerFixture(t)
	d, url := newDiscordStub(t)
	const total = 40
	for i := range total {
		title := fmt.Sprintf("Aufgabe %02d %s", i, strings.Repeat("x", 100))
		if _, err := sc.db.Exec("INSERT INTO tasks (title, due_date, due_time, created_at) VALUES (?, '2026-03-01', '08:00', 'z')", title); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	stamped := func() int {
		var n int
		sc.db.QueryRow("SELECT COUNT(*) FROM task_reminders WHERE channel = 'discord'").Scan(&n)
		return n
	}

	sc.exactReminders(testUID, url, "2026-03-01", "09:00")
	msgs := d.contents()
	if len(msgs) != 1 {
		t.Fatalf("erwartet eine Nachricht, bekam %d", len(msgs))
	}
	if n := utf8.RuneCountInString(msgs[0]); n > discordMaxRunes {
		t.Fatalf("Nachricht mit %d Zeichen über dem Budget von %d", n, discordMaxRunes)
	}
	sent := strings.Count(msgs[0], "⏰")
	if sent == 0 || sent == total {
		t.Fatalf("Testaufbau: erwartet eine Teilmenge, gesendet %d von %d", sent, total)
	}
	if got := stamped(); got != sent {
		t.Fatalf("%d Tasks gesendet, aber %d als erinnert gestempelt", sent, got)
	}

	// The following ticks catch up on the rest — each task exactly once.
	for range total {
		if stamped() == total {
			break
		}
		sc.exactReminders(testUID, url, "2026-03-01", "09:01")
	}
	all := strings.Join(d.contents(), "\n")
	for i := range total {
		if c := strings.Count(all, fmt.Sprintf("Aufgabe %02d ", i)); c != 1 {
			t.Fatalf("Aufgabe %02d wurde %d-mal gepingt", i, c)
		}
	}
}

// A subtask added (or reopened) after the parent was checked off still
// hangs off the completed parent task. Auto-archive used to silently
// delete it along with the parent.
func TestAutoArchiveKeepsParentWithOpenSubtask(t *testing.T) {
	sc := schedulerFixture(t)
	old := time.Now().UTC().AddDate(0, 0, -60).Format(time.RFC3339)
	for _, stmt := range []string{
		"INSERT INTO tasks (id, title, created_at, done, completed_at) VALUES (1, 'Umzug', 'z', 1, '" + old + "')",
		"INSERT INTO tasks (id, title, created_at, parent_id) VALUES (2, 'Kaution zurückfordern', 'z', 1)",
		"INSERT INTO tasks (id, title, created_at, done, completed_at) VALUES (3, 'Steuer', 'z', 1, '" + old + "')",
		"INSERT INTO tasks (id, title, created_at, parent_id, done, completed_at) VALUES (4, 'Belege', 'z', 3, 1, '" + old + "')",
	} {
		if _, err := sc.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	sc.autoArchive(30, "2026-03-01")

	exists := func(id int) bool {
		var n int
		sc.db.QueryRow("SELECT COUNT(*) FROM tasks WHERE id = ?", id).Scan(&n)
		return n == 1
	}
	if !exists(1) || !exists(2) {
		t.Fatal("erledigter Eltern-Task mit offener Unteraufgabe wurde archiviert")
	}
	if exists(3) || exists(4) {
		t.Fatal("komplett erledigter Task hätte archiviert werden müssen")
	}
}
