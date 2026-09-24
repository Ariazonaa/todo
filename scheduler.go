package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Discord accepts 2000 characters per message; the rest is headroom for the
// truncation notice in postDiscord.
const discordMaxRunes = 1900

type scheduler struct {
	db         *sql.DB
	store      *settingsStore
	hub        *hub
	vapid      *vapidKey // sender for web push
	lastErrLog time.Time
	done       chan struct{} // closed once run() has returned
}

func newScheduler(db *sql.DB, store *settingsStore, h *hub, vapid *vapidKey) *scheduler {
	return &scheduler{db: db, store: store, hub: h, vapid: vapid, done: make(chan struct{})}
}

// run ticks every minute until ctx is canceled. A tick already in progress is
// not interrupted — it's allowed to finish writing before the DB closes.
func (s *scheduler) run(ctx context.Context) {
	defer close(s.done)
	s.tick(time.Now())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.tick(now)
		}
	}
}

func (s *scheduler) tick(now time.Time) {
	purgeSessions(s.db)
	s.autoArchive(s.store.archiveDays(), now.UTC().Format(dateFmt))
	rows, err := s.db.Query("SELECT id FROM users ORDER BY id")
	if err != nil {
		s.logErr(fmt.Errorf("tick users: %w", err))
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, uid := range ids {
		s.tickUser(uid, now)
	}
}

// tickUser: one user's reminders — their webhook, their devices, their
// timezone, tasks from all lists they can see (own and shared).
func (s *scheduler) tickUser(uid int64, now time.Time) {
	set := s.store.get(uid)
	local := now.In(s.store.location(uid))
	today := local.Format(dateFmt)
	hhmm := local.Format("15:04")
	// Discord and push run independently: each channel has its own
	// dedup state, a failure in one doesn't hold up the other.
	if set.WebhookURL != "" {
		s.exactReminders(uid, set.WebhookURL, today, hhmm)
		s.morningDigest(uid, set.WebhookURL, set.DigestTime, today, hhmm)
	}
	if n, err := countPushSubs(s.db, uid); err != nil {
		s.logErr(fmt.Errorf("push: %w", err))
	} else if n > 0 && s.vapid != nil {
		s.pushReminders(uid, today, hhmm)
		s.pushDigest(uid, set.DigestTime, today, hhmm)
	}
}

// autoArchive deletes, once a day, completed top-level tasks (including subs/links)
// whose completion is older than archiveDays. The stats stay,
// they keep counting in the stats table.
func (s *scheduler) autoArchive(archiveDays int, today string) {
	if archiveDays <= 0 || getMeta(s.db, "last_archive_date") == today {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -archiveDays).Format(time.RFC3339)
	// Open subtasks (added after checking off, or reopened)
	// hang off a completed parent task — those must not be silently
	// swept away with it.
	rows, err := s.db.Query("SELECT id FROM tasks WHERE done = 1 AND parent_id IS NULL AND completed_at < ? "+
		"AND NOT EXISTS (SELECT 1 FROM tasks s WHERE s.parent_id = tasks.id AND s.done = 0)", cutoff)
	if err != nil {
		s.logErr(fmt.Errorf("autoArchive query: %w", err))
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Err() // a partial set is fine, the rest gets picked up on the next run
	rows.Close()
	for _, id := range ids {
		if err := archiveTask(s.db, id); err != nil {
			s.logErr(fmt.Errorf("autoArchive delete %d: %w", id, err))
		}
	}
	if len(ids) > 0 {
		s.hub.notifyAll()
	}
	if err := setMeta(s.db, "last_archive_date", today); err != nil {
		s.logErr(fmt.Errorf("autoArchive setMeta: %w", err))
	}
}

// exactReminders pings tasks with a date+time whose moment has arrived/passed
// and that haven't been pinged for this user yet. Timestamping only happens after a Discord 2xx,
// so errors retry automatically and missed pings fire exactly once after downtime.
func (s *scheduler) exactReminders(uid int64, webhook, today, hhmm string) {
	due, err := s.queryTasks(
		"SELECT "+taskCols+" FROM tasks WHERE list_id "+visibleLists+" AND done = 0 AND due_time IS NOT NULL AND "+notReminded(chDiscord)+" AND (due_date < ? OR (due_date = ? AND due_time <= ?)) ORDER BY due_date, due_time, id",
		uid, uid, today, today, hhmm)
	if err != nil {
		s.logErr(fmt.Errorf("exactReminders query: %w", err))
		return
	}
	if len(due) == 0 {
		return
	}
	lang := s.store.notifyLang(uid)
	var lines []string
	var sent []Task
	size := 0
	for _, t := range due {
		line := T(lang, "notify.ping_line", "title", notifyTitle(t), "time", *t.DueTime)
		if *t.DueDate < today {
			line += T(lang, "notify.ping_overdue", "date", formatDate(lang, *t.DueDate))
		}
		// Only as many tasks as fit in one message: postDiscord would truncate
		// the rest, but it would still get stamped — and never actually pinged. The
		// overflow gets picked up on the next tick.
		n := utf8.RuneCountInString(line) + 1 // + Zeilenumbruch
		if len(lines) > 0 && size+n > discordMaxRunes {
			break
		}
		size += n
		lines = append(lines, line)
		sent = append(sent, t)
	}
	if err := postDiscord(webhook, strings.Join(lines, "\n")); err != nil {
		s.logErr(fmt.Errorf("exactReminders webhook: %w", err))
		return
	}
	// The due date that gets stamped is the one that was reported: if someone
	// checked off a recurrence during the Discord POST (new due date), the
	// new one stays unstamped and gets reported next time.
	for _, t := range sent {
		if err := stampReminded(s.db, uid, chDiscord, t); err != nil {
			s.logErr(fmt.Errorf("exactReminders stamp: %w", err))
		}
	}
}

// digestTasks: date-only tasks due today, plus everything overdue — for the
// digest via Discord and via push.
func (s *scheduler) digestTasks(uid int64, today string) (dueToday, overdue []Task, err error) {
	dueToday, err = s.queryTasks(
		"SELECT "+taskCols+" FROM tasks WHERE list_id "+visibleLists+" AND done = 0 AND due_date = ? AND due_time IS NULL", uid, today)
	if err != nil {
		return nil, nil, fmt.Errorf("today: %w", err)
	}
	overdue, err = s.queryTasks(
		"SELECT "+taskCols+" FROM tasks WHERE list_id "+visibleLists+" AND done = 0 AND due_date < ? ORDER BY due_date", uid, today)
	if err != nil {
		return nil, nil, fmt.Errorf("overdue: %w", err)
	}
	return dueToday, overdue, nil
}

// morningDigest sends a once-a-day summary starting at the configured digest time: date-only
// tasks due today plus everything overdue. Dedup via meta.last_digest_date.
func (s *scheduler) morningDigest(uid int64, webhook, digestTime, today, hhmm string) {
	if hhmm < digestTime || getUserSetting(s.db, uid, "last_digest_date") == today {
		return
	}
	dueToday, overdue, err := s.digestTasks(uid, today)
	if err != nil {
		s.logErr(fmt.Errorf("morningDigest %w", err))
		return
	}
	if len(dueToday) == 0 && len(overdue) == 0 {
		// nothing to report, still mark the day as done
		if err := setUserSetting(s.db, uid, "last_digest_date", today); err != nil {
			s.logErr(fmt.Errorf("morningDigest setMeta: %w", err))
		}
		return
	}
	lang := s.store.notifyLang(uid)
	var entries []digestEntry
	for _, t := range dueToday {
		entries = append(entries, digestEntry{T(lang, "notify.digest_today"), "• " + notifyTitle(t)})
	}
	for _, t := range overdue {
		entries = append(entries, digestEntry{T(lang, "notify.digest_overdue"),
			T(lang, "notify.digest_overdue_line", "title", notifyTitle(t), "date", formatDate(lang, *t.DueDate))})
	}
	msg := digestMessage(lang, T(lang, "notify.digest_title", "date", formatDate(lang, today)), entries)
	if err := postDiscord(webhook, msg); err != nil {
		s.logErr(fmt.Errorf("morningDigest webhook: %w", err))
		return
	}
	if err := setUserSetting(s.db, uid, "last_digest_date", today); err != nil {
		s.logErr(fmt.Errorf("morningDigest setMeta: %w", err))
	}
}

type digestEntry struct{ section, line string }

// digestMessage builds the digest so it fits into a single Discord message.
// Whatever no longer fits gets counted instead of cut off: postDiscord
// would otherwise silently truncate, and the day would still count as reported. The
// overdue ones come back tomorrow — until then, at least the count is shown.
func digestMessage(lang, title string, entries []digestEntry) string {
	const reserve = 40 // room for "… and N more"
	lines := []string{title}
	size := utf8.RuneCountInString(title)
	section := ""
	for i, e := range entries {
		add := []string{e.line}
		if e.section != section {
			add = []string{e.section, e.line}
		}
		n := 0
		for _, l := range add {
			n += utf8.RuneCountInString(l) + 1 // + line break
		}
		limit := discordMaxRunes
		if i < len(entries)-1 {
			limit -= reserve
		}
		if size+n > limit {
			lines = append(lines, T(lang, "notify.more", "n", len(entries)-i))
			break
		}
		lines = append(lines, add...)
		size += n
		section = e.section
	}
	return strings.Join(lines, "\n")
}

func (s *scheduler) queryTasks(query string, args ...any) ([]Task, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// discordClient doesn't follow redirects: the webhook URL is restricted to
// discord.com, a redirect (an open redirect there) would otherwise send the
// POST — including task titles — to an arbitrary destination. A 3xx therefore
// counts as an error.
var discordClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func postDiscord(webhook, content string) error {
	// Discord limit: 2000 characters
	if len(content) > discordMaxRunes {
		runes := []rune(content)
		if len(runes) > discordMaxRunes {
			content = string(runes[:discordMaxRunes]) + "\n…"
		}
	}
	body, err := json.Marshal(map[string]any{
		"content": content,
		// Task titles are free text: an "@everyone" or a
		// role mention in one shouldn't ping the entire server.
		"allowed_mentions": map[string][]string{"parse": {}},
	})
	if err != nil {
		return err
	}
	resp, err := discordClient.Post(webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		// The client's *url.Error carries the complete URL, including the webhook token,
		// in its message — and that ends up in the log. Only pass along the underlying cause.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			return fmt.Errorf("discord webhook: %w", ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("discord webhook: status %d", resp.StatusCode)
	}
	return nil
}

// logErr throttles to at most one line per 15 minutes, so a dead webhook
// doesn't flood the logs (the loop retries every minute).
func (s *scheduler) logErr(err error) {
	if time.Since(s.lastErrLog) < 15*time.Minute {
		return
	}
	s.lastErrLog = time.Now()
	log.Printf("scheduler: %v", err)
}

const (
	// More simultaneously due pings than this → a grouped notification
	// instead of a stack of them (e.g. after the server was down for a while).
	pushGroupAbove = 3
	pushTitleRunes = 120
)

// pushReminders is exactReminders for push: due tasks with a time that
// have never been reported via push. Timestamping only happens after delivery.
func (s *scheduler) pushReminders(uid int64, today, hhmm string) {
	due, err := s.queryTasks(
		"SELECT "+taskCols+" FROM tasks WHERE list_id "+visibleLists+" AND done = 0 AND due_time IS NOT NULL AND "+notReminded(chPush)+" AND (due_date < ? OR (due_date = ? AND due_time <= ?)) ORDER BY due_date, due_time, id",
		uid, uid, today, today, hhmm)
	if err != nil {
		s.logErr(fmt.Errorf("pushReminders query: %w", err))
		return
	}
	lang := s.store.notifyLang(uid)
	if len(due) > pushGroupAbove {
		var titles []string
		for i, t := range due {
			if i < pushGroupAbove {
				titles = append(titles, clipRunes(notifyTitle(t), 40))
			}
		}
		body := T(lang, "notify.group_body", "titles", strings.Join(titles, ", "), "n", len(due)-pushGroupAbove)
		msg := pushMsg{Title: T(lang, "notify.group_title", "n", len(due)), Body: body, Tag: "reminders"}
		if s.deliverPush(uid, msg, "pushReminders") {
			s.stampPushed(uid, due)
		}
		return
	}
	for _, t := range due {
		msg := pushMsg{
			Title:   clipRunes(notifyTitle(t), pushTitleRunes),
			Body:    pushPingBody(lang, t, today),
			Tag:     fmt.Sprintf("task-%d", t.ID),
			TaskID:  t.ID,
			DueDate: *t.DueDate,
			Actions: pushActions(lang, "done", "snooze"),
		}
		if open, err := hasOpenSubs(s.db, t.ID); err != nil {
			s.logErr(fmt.Errorf("pushReminders hasOpenSubs: %w", err))
			msg.Actions = pushActions(lang, "snooze") // when in doubt, no "Done" button
		} else if open {
			msg.Actions = pushActions(lang, "snooze")
		}
		if s.deliverPush(uid, msg, "pushReminders") {
			s.stampPushed(uid, []Task{t})
		}
	}
}

// deliverPush sends to all devices and logs errors with throttling.
func (s *scheduler) deliverPush(uid int64, msg pushMsg, step string) bool {
	urgency := "high"
	if msg.Tag == "digest" {
		urgency = "normal"
	}
	ok, err := pushAll(s.db, s.vapid, uid, msg, 12*time.Hour, urgency)
	if err != nil {
		s.logErr(fmt.Errorf("%s: %w", step, err))
	}
	return ok
}

// stampPushed stamps the reported due dates — same as Discord: a
// due date that's changed in the meantime (recurrence checked off) stays unstamped.
func (s *scheduler) stampPushed(uid int64, tasks []Task) {
	for _, t := range tasks {
		if err := stampReminded(s.db, uid, chPush, t); err != nil {
			s.logErr(fmt.Errorf("pushReminders stamp: %w", err))
		}
	}
}

// notifyTitle: the title for Discord and push — high-priority tasks
// stand out with "‼️ ". Length limits apply to prefix plus title.
func notifyTitle(t Task) string {
	if t.Priority == 3 {
		return "‼️ " + t.Title
	}
	return t.Title
}

// pushActions builds the buttons with labels in lang.
func pushActions(lang string, names ...string) []pushAction {
	labels := map[string]string{"done": "notify.action_done", "snooze": "notify.action_snooze"}
	out := make([]pushAction, 0, len(names))
	for _, n := range names {
		out = append(out, pushAction{Action: n, Title: T(lang, labels[n])})
	}
	return out
}

func pushPingBody(lang string, t Task, today string) string {
	if *t.DueDate < today {
		return T(lang, "notify.push_overdue", "date", formatDate(lang, *t.DueDate), "time", *t.DueTime)
	}
	return T(lang, "notify.push_due", "time", *t.DueTime)
}

// pushDigest is morningDigest for push, with its own daily stamp.
func (s *scheduler) pushDigest(uid int64, digestTime, today, hhmm string) {
	if hhmm < digestTime || getUserSetting(s.db, uid, "last_push_digest_date") == today {
		return
	}
	dueToday, overdue, err := s.digestTasks(uid, today)
	if err != nil {
		s.logErr(fmt.Errorf("pushDigest %w", err))
		return
	}
	if len(dueToday) > 0 || len(overdue) > 0 {
		lang := s.store.notifyLang(uid)
		msg := pushMsg{Title: T(lang, "notify.push_digest_title", "date", formatDate(lang, today)),
			Body: digestPushBody(lang, dueToday, overdue), Tag: "digest"}
		if !s.deliverPush(uid, msg, "pushDigest") {
			return
		}
	}
	if err := setUserSetting(s.db, uid, "last_push_digest_date", today); err != nil {
		s.logErr(fmt.Errorf("pushDigest setMeta: %w", err))
	}
}

// digestPushBody: "3 today, 2 overdue: Groceries, Taxes, …"
func digestPushBody(lang string, dueToday, overdue []Task) string {
	var counts, titles []string
	if n := len(dueToday); n > 0 {
		counts = append(counts, T(lang, "notify.count_today", "n", n))
	}
	if n := len(overdue); n > 0 {
		counts = append(counts, T(lang, "notify.count_overdue", "n", n))
	}
	for _, t := range append(append([]Task(nil), dueToday...), overdue...) {
		titles = append(titles, notifyTitle(t))
	}
	return clipRunes(strings.Join(counts, ", ")+": "+strings.Join(titles, ", "), 300)
}
