// Smoke test configuration. Starts the Go server itself with a fresh,
// disposable DB and shuts it down again afterward.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { defineConfig, devices } = require('@playwright/test');

const PORT = process.env.E2E_PORT || '8390';

// The test DB has to live on local disk: if the repo sits on a network
// drive, SQLite (WAL, locking) runs into trouble there.
// A fresh directory per run — the tests assume an empty instance. The path
// is stored in the environment so the worker processes (which reload the
// config) use the same directory instead of trying to tear down the DB
// that's still running.
if (!process.env.E2E_DB_DIR) {
  process.env.E2E_DB_DIR = fs.mkdtempSync(path.join(os.tmpdir(), 'todo-e2e-'));
}
const dbDir = process.env.E2E_DB_DIR;

// Second server for the English run: its own empty instance, otherwise it
// would run into the German run's passkey.
const PORT_EN = String(Number(PORT) + 1);
const dbDirEn = path.join(dbDir, 'en');
fs.mkdirSync(dbDirEn, { recursive: true });
process.env.E2E_DB_DIR_EN = dbDirEn;

module.exports = defineConfig({
  testDir: './tests',
  // One server, one DB, one single-user login — nothing here can run in parallel.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? 'list' : [['list']],
  use: {
    trace: 'retain-on-failure',
    // The server defaults to Europe/Berlin, and so do real users. Without
    // this, the browser would run in the container's UTC: Berlin is ahead of
    // UTC, so just before midnight UTC, the browser's "tomorrow" is already
    // the server's "today" — in summer (UTC+2) between 22:00 and 24:00 UTC,
    // in winter (UTC+1) only between 23:00 and 24:00 UTC.
    timezoneId: 'Europe/Berlin',
  },
  // baseURL: localhost, not 127.0.0.1 — WebAuthn derives the RP ID from the
  // host, and an IP address isn't a valid RP ID.
  projects: [
    {
      name: 'de',
      testMatch: /(smoke|dateparse)\.spec\.js/,
      // Without a locale, Chromium would run on en-US — the app would be in English.
      use: { ...devices['Desktop Chrome'], locale: 'de-DE', baseURL: `http://localhost:${PORT}` },
    },
    {
      name: 'en',
      testMatch: /smoke-en\.spec\.js/,
      use: { ...devices['Desktop Chrome'], locale: 'en-US', baseURL: `http://localhost:${PORT_EN}` },
    },
  ],
  webServer: [PORT, PORT_EN].map((port) => ({
    command: 'go run .',
    url: `http://localhost:${port}/healthz`,
    env: {
      PORT: port,
      DB_PATH: path.join(port === PORT ? dbDir : dbDirEn, 'todo.db'),
      // Chromium accepts Secure and __Host- cookies from http://localhost too —
      // the test checks the cookies just like in production. Empty instead of
      // omitted: an INSECURE_COOKIE=1 exported in the shell would otherwise
      // leak through.
      INSECURE_COOKIE: '',
    },
    reuseExistingServer: false,
    timeout: 120_000, // the first run has to compile first
    stdout: 'pipe',
    stderr: 'pipe',
  })),
});
