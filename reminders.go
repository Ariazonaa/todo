package main

import (
	"database/sql"
	"time"
)

// Reminder stamp per user and channel. "due" records which due date was
// reported: a new due date (snooze, recurrence, edit sheet) has no stamp and
// fires again — without a reset, and a late stamp for the old one doesn't
// block it.

const (
	chDiscord = "discord"
	chPush    = "push"
)

// notReminded: SQL condition on tasks — not yet reported to the user (one
// placeholder) on this channel for this due date.
func notReminded(channel string) string {
	return "NOT EXISTS (SELECT 1 FROM task_reminders r WHERE r.task_id = tasks.id AND r.user_id = ? AND r.channel = '" + channel +
		"' AND r.due = tasks.due_date || ' ' || tasks.due_time)"
}

// stampReminded: after successful delivery — only if the task still has
// exactly this due date and the user can still see it.
func stampReminded(db *sql.DB, uid int64, channel string, t Task) error {
	if t.DueDate == nil || t.DueTime == nil {
		return nil
	}
	_, err := db.Exec("INSERT INTO task_reminders (task_id, user_id, channel, due, sent_at) "+
		"SELECT id, ?, ?, due_date || ' ' || due_time, ? FROM tasks WHERE id = ? AND due_date = ? AND due_time = ? AND done = 0 AND list_id "+visibleLists+
		" ON CONFLICT (task_id, user_id, channel) DO UPDATE SET due = excluded.due, sent_at = excluded.sent_at",
		uid, channel, time.Now().UTC().Format(time.RFC3339), t.ID, *t.DueDate, *t.DueTime, uid)
	return err
}

// clearReminders: the due date was deliberately reset (edit sheet) — even
// back to a time already reported, it should fire again.
func clearReminders(ex execer, taskID int64) error {
	_, err := ex.Exec("DELETE FROM task_reminders WHERE task_id = ?", taskID)
	return err
}
