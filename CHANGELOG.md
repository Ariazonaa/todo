# Changelog

All notable changes to this project. Versions follow
[Semantic Versioning](https://semver.org/); every release is also published as
a Docker image `ghcr.io/ariazonaa/todo:X.Y.Z` (plus `:X.Y` and `:X`).

## 1.1.2 — 2026-10-19

### Changed

- **Faster with many tasks.** Loading the list no longer slows down
  quadratically once tasks are linked: with 1000 tasks it takes about 6 ms
  instead of 130 ms, with 5000 about 30 ms instead of 4.4 s. Backups export
  and sharing changes get the same speed-up.
- Checking off a task in a long list no longer freezes the page: the
  animation only covers the rows on screen, and each row costs less to
  build. Search stays smooth while typing.
- App files (JavaScript, styles, translations, icons) are revalidated
  instead of downloaded again on every start.
- On a stalled network the app opens from its offline copy after a few
  seconds instead of waiting until the browser gives up.
- Saving changes no longer waits for the disk after every write (SQLite
  `synchronous=NORMAL` with WAL). A power cut can lose the last few seconds
  of changes, but never corrupts the database.
- The example nginx config now compresses JSON, JavaScript and CSS. If you
  run your own proxy, add the `gzip` block from
  `deploy/nginx/todo.example.com.conf` — never for `text/event-stream`.

## 1.1.1 — 2026-10-19

### Fixed

- iPhone (home screen app): after typing in the edit sheet, "Save" and
  "Cancel" ignored taps. The sheet header no longer scrolls with the sheet;
  only its content does.

## 1.1.0 — 2026-10-15

### Changed

- **Sharing a list is now an invitation.** The invited account sees the list
  (and gets its reminders) only after accepting; declining or leaving works
  the same way. Lists shared before this release stay shared.
- Creating or removing accounts and setup codes, sharing a list, changing the
  Discord webhook or the archive period, and importing a backup now require a
  fresh sign-in (at most 10 minutes old), like managing passkeys already did.
  A stolen session cookie alone can't turn into lasting access.
- Upper bounds per list owner: tasks, image storage and links per task. They
  are large enough that every backup can be imported again.
- Moving a task under a parent in someone else's shared list asks first — the
  task moves into that list and everyone there sees it.
- A push device registered to one account can't be taken over by another
  without its keys.
- Names and titles drop control, bidi and invisible characters, so
  "‮nimda" can't pass for "admin".
- nginx example: no HSTS `preload`, fixed redirect host, no proxy temp files.
- Go 1.26.9.

### Fixed

- An edit sheet that stayed open no longer writes back what another device
  changed in the meantime: only the fields you changed are saved, and other
  people's changes show up in the fields you didn't touch.
- Concurrent edits, completions and snoozes of the same task (two devices, or
  members of a shared list) no longer overwrite each other or book the daily
  statistic to the wrong person.
- Snoozing answers with an error instead of pretending to succeed when the
  task was completed elsewhere at the same moment.
- Creating a task no longer reports an error after it was saved (a retry
  created it twice).
- Due dates stay within a sane range; extreme years no longer sort wrong.
- Backup import: memory per element is capped, timestamps are normalized,
  and push reminders that were never sent are no longer marked as sent.
- Thumbnails no longer load the full original image each time.
- A retired session can't be renewed by a request that was already in flight.
- One broken passkey record no longer blocks sign-in for every account.
- Deleting an account also removes its half-finished import.
- Web push uses an https contact even when the app was opened via another
  origin (Apple's push service rejected those).
- Clicking a task that was just deleted elsewhere no longer closes the edit
  sheet and drops unsaved input; no more stray confetti after switching
  lists.

## 1.0.0 — 2026-09-24

First public release.
