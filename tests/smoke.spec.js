// Smoke test: one pass through the whole app, against a real server with a
// fresh DB. Covers what the Go tests can't see — passkey ceremonies in the
// browser, rendering, sheet behavior.
//
// The tests build on each other (serial, shared page): the virtual
// authenticator lives in the browser context, and a fresh context would lose
// the registered passkey and couldn't log back in.
const fs = require('node:fs');
const path = require('node:path');
const { test, expect } = require('@playwright/test');

test.describe.configure({ mode: 'serial' });

let context;
let page;

test.beforeAll(async ({ browser }) => {
  context = await browser.newContext();
  page = await context.newPage();
  const cdp = await context.newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
});

test.afterAll(async () => {
  await context?.close();
});

// After every change the server sends an SSE event, and the client reloads
// 150ms later and replaces the whole list (list.replaceChildren). A click
// landing in that window would hit a node that's about to disappear — so
// let things settle briefly before interacting.
const beruhigen = () => page.waitForTimeout(400);

// `eingabe` is what gets typed; `erwarteterTitel` is what ends up in the
// list afterward — when a date is recognized, the app cuts that part out of
// the title.
async function aufgabeAnlegen(eingabe, erwarteterTitel = eingabe) {
  await page.fill('#new-title', eingabe);
  await page.press('#new-title', 'Enter');
  await expect(page.locator('#list li.task', { hasText: erwarteterTitel }).first()).toBeVisible();
  await beruhigen();
}

test('leere Instanz führt ins Setup und legt den ersten Passkey an', async () => {
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.locator('html')).toHaveAttribute('lang', 'de');
  await expect(page.locator('h1')).toHaveText('Willkommen');

  // The setup code sits next to the DB that playwright.config.js creates.
  const code = fs.readFileSync(path.join(process.env.E2E_DB_DIR, 'setup-code.txt'), 'utf8').trim();
  await page.fill('#setup-code', code);
  await page.fill('#device-name', 'Testgerät');
  await page.click('#setup-form button[type=submit]');

  await expect(page).toHaveURL(/\/$/);
  await expect(page.locator('#hdr-date')).not.toBeEmpty();

  // The browser only accepts __Host- from its own host: another subdomain
  // can't plant a session cookie that would take precedence over the real one.
  const namen = (await context.cookies()).map((c) => c.name);
  expect(namen).toContain('__Host-session');
});

// AMOLED: black pixels stay off. No color blotches behind the glass, cards
// black with a thin line, status bar and splash screen black.
test('AMOLED: alles Schwarz, Karten nur mit feiner Linie', async () => {
  await expect(page.locator('.bg')).toHaveCount(0);
  await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(0, 0, 0)');
  const karte = page.locator('#add-form');
  await expect(karte).toHaveCSS('background-color', 'rgb(0, 0, 0)');
  await expect(karte).toHaveCSS('border-top-color', 'rgb(42, 42, 42)');
  await expect(page.locator('meta[name="theme-color"]')).toHaveAttribute('content', '#000000');
  const manifest = await page.evaluate(async () => (await fetch('/manifest.webmanifest')).json());
  expect([manifest.theme_color, manifest.background_color]).toEqual(['#000000', '#000000']);

  // Same for the grouped lists in the sheets
  await page.click('#open-settings');
  const gruppe = page.locator('#settings .group').first();
  await expect(gruppe).toHaveCSS('background-color', 'rgb(0, 0, 0)');
  await expect(gruppe).toHaveCSS('border-top-color', 'rgb(42, 42, 42)');
  await page.click('#close-settings');
  await expect(page.locator('#settings')).not.toBeVisible();
});

test('Schnelleingabe erkennt Datum und Uhrzeit im Text', async () => {
  await aufgabeAnlegen('Milch kaufen morgen 18:00', 'Milch kaufen');

  const zeile = page.locator('#list li.task', { hasText: 'Milch kaufen' });
  await expect(zeile).toBeVisible();
  // Title without the date part, date shown as meta below
  await expect(zeile.locator('.title').first()).toHaveText('Milch kaufen');
  await expect(zeile.locator('.meta').first()).toContainText('morgen');
  await expect(zeile.locator('.meta').first()).toContainText('18:00');
  // The "Erkannt" (Recognized) message disappears again. It used to be that
  // display:flex overrode the hidden attribute, leaving the last toast stuck
  // forever.
  await expect(page.locator('#toast')).toBeVisible();
  await expect(page.locator('#toast')).toBeHidden({ timeout: 6000 });
});

// Same cause as the toast: .add-details (display:grid) stayed open despite
// hidden, so the button next to it did nothing.
test('Datum/Wiederholung der Schnelleingabe ist einklappbar', async () => {
  await expect(page.locator('#add-details')).toBeHidden();
  await page.click('#toggle-details');
  await expect(page.locator('#add-details')).toBeVisible();
  await page.click('#toggle-details');
  await expect(page.locator('#add-details')).toBeHidden();
});

test('Schnelleingabe hält Wortteile nicht für Datumsangaben', async () => {
  const eingaben = ['To-do Liste aufräumen', 'Morgen-Routine planen', 'Kapitel 3.2 lesen', '2.5 kg Mehl',
    'İstanbul Reise morgen', 'Zahnarzt am 3.2.', 'Arzt Freitag um 9 uhr', 'Meeting (morgen 10 uhr)',
    'jeden Montag Müll raus', 'Miete in 3 tagen', 'Arzt fr 9:30'];
  const r = await page.evaluate((xs) => Object.fromEntries(xs.map((s) => [s, parseNaturalDate(s)])), eingaben);
  for (const s of eingaben.slice(0, 4)) {
    expect(r[s].date, s).toBeNull();
    expect(r[s].title, s).toBe(s);
  }
  // toLowerCase() turns "İ" into two characters — the cut used to land in the wrong place
  expect(r['İstanbul Reise morgen']).toMatchObject({ title: 'İstanbul Reise' });
  expect(r['İstanbul Reise morgen'].date).not.toBeNull();
  expect(r['Zahnarzt am 3.2.'].title).toBe('Zahnarzt');
  expect(r['Zahnarzt am 3.2.'].date).toMatch(/-02-03$/);
  expect(r['Arzt Freitag um 9 uhr']).toMatchObject({ title: 'Arzt', time: '09:00' });
  // what used to work still works
  expect(r['Meeting (morgen 10 uhr)']).toMatchObject({ time: '10:00' });
  expect(r['Meeting (morgen 10 uhr)'].date).not.toBeNull();
  expect(r['jeden Montag Müll raus']).toMatchObject({ title: 'Müll raus', rec: 'weekly' });
  expect(r['Miete in 3 tagen'].title).toBe('Miete');
  expect(r['Miete in 3 tagen'].date).not.toBeNull();
  expect(r['Arzt fr 9:30']).toMatchObject({ title: 'Arzt', time: '09:30' });
});

test('Abhaken verschiebt die Aufgabe nach Erledigt', async () => {
  const zeile = page.locator('#list li.task', { hasText: 'Milch kaufen' });
  await zeile.locator('.check').first().click();
  await beruhigen();

  const erledigt = page.locator('#list details');
  await expect(erledigt).toBeVisible();
  await expect(erledigt.locator('summary')).toContainText('Erledigt (1)');
  await expect(erledigt.locator('li.task', { hasText: 'Milch kaufen' })).toHaveCount(1);
});

test('Unteraufgabe anlegen und im Fortschrittsring zählen', async () => {
  await aufgabeAnlegen('Umzug');
  const zeile = page.locator('#list li.task', { hasText: 'Umzug' });

  await zeile.locator('.body').first().click();
  await expect(page.locator('#edit')).toBeVisible();
  await page.fill('#new-sub', 'Kartons besorgen');
  await page.click('#add-sub');
  await expect(page.locator('#sub-list li')).toHaveCount(1);
  await page.click('#edit-cancel');
  await beruhigen();

  await expect(zeile.locator('.subs li.task')).toHaveCount(1);
  await expect(zeile.locator('.meta').first()).toContainText('0/1');
});

// Regression: the list behind the open sheet used to be scrollable.
test('offenes Sheet hält die Seite dahinter fest', async () => {
  await page.evaluate(async () => {
    for (let i = 1; i <= 30; i++) {
      await fetch('/api/tasks', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: 'Aufgabe ' + i }),
      });
    }
  });
  await page.reload();
  await expect(page.locator('#list li.task').first()).toBeVisible();
  await beruhigen();

  await page.evaluate(() => window.scrollTo(0, 400));
  await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(300);
  // The header shrinks while scrolling — measure only after that's done, or
  // we'd be comparing positions from two different layouts. The class alone
  // isn't enough: the padding keeps animating for 0.25s, and while it does,
  // scroll anchoring shifts scrollY by a few pixels (that was the occasional
  // flake with a 4–7px deviation).
  await expect(page.locator('body')).toHaveClass(/scrolled/);
  await expect
    .poll(() => page.evaluate(() => document.querySelector('header').getAnimations({ subtree: true }).length))
    .toBe(0);

  const vorher = await page.evaluate(() => window.scrollY);

  // dispatchEvent instead of click(): a normal click would first scroll the
  // row into view, shifting exactly the position this test is measuring.
  await page.locator('#list li.task .body').first().dispatchEvent('click');
  await expect(page.locator('#edit')).toBeVisible();

  // try to scroll behind it: mouse wheel and programmatically
  await page.mouse.move(200, 80);
  await page.mouse.wheel(0, 900);
  await page.evaluate(() => window.scrollBy(0, 900));
  await page.waitForTimeout(200);

  // Deliberately no pixel comparisons of the list: those depend on the header,
  // which shrinks while scrolling, and on SSE re-renders — that gets flaky.
  // These three assertions catch the regression just as well and don't
  // depend on timing.
  expect(await page.evaluate(() => getComputedStyle(document.body).position)).toBe('fixed');
  expect(await page.evaluate(() => window.scrollY)).toBe(0);

  await page.click('#edit-cancel');
  await expect(page.locator('#edit')).toBeHidden();
  // Tolerance of 3px: Chrome rounds scrollY depending on device pixels. A
  // broken restore would be off by hundreds, not by two.
  await expect
    .poll(async () => Math.abs((await page.evaluate(() => window.scrollY)) - vorher))
    .toBeLessThanOrEqual(3);
  expect(await page.evaluate(() => getComputedStyle(document.body).position)).toBe('static');
});

test('Suche filtert die Liste', async () => {
  await page.click('#toggle-search');
  await page.fill('#search', 'Umzug');
  await expect(page.locator('#list li.task', { hasText: 'Umzug' })).toHaveCount(1);
  await expect(page.locator('#list li.task', { hasText: 'Aufgabe 1' })).toHaveCount(0);
  await page.click('#toggle-search'); // closing clears the search
  await expect(page.locator('#list li.task').first()).toBeVisible();
});

// A modal dialog makes everything outside it inert. That's why the toast
// used to sit invisibly behind the sheet ("Titel fehlt" went unseen), and
// its undo button wasn't clickable.
test('Toast liegt über dem offenen Sheet und ist bedienbar', async () => {
  const zeile = page.locator('#list li.task', { hasText: 'Umzug' });
  await zeile.locator('.body').first().click();
  await expect(page.locator('#edit')).toBeVisible();

  const obenauf = () => page.evaluate(() => {
    const t = document.querySelector('#toast');
    const r = t.getBoundingClientRect();
    return t.contains(document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2));
  });
  await page.fill('#edit-title', '');
  await page.click('#edit-save');
  await expect(page.locator('#toast')).toHaveText('Titel fehlt');
  expect(await obenauf()).toBe(true);
  await page.fill('#edit-title', 'Umzug');

  // Delete a subtask in the sheet: it disappears there immediately, undo brings it back
  await expect(page.locator('#sub-list li')).toHaveCount(1);
  await page.locator('#sub-list li .del').click();
  await expect(page.locator('#sub-list li')).toHaveCount(0);
  await page.locator('#toast .toastbtn').click();
  await expect(page.locator('#sub-list li')).toHaveCount(1);

  // Closing the sheet moves the toast along instead of dismissing it with it —
  // undo still has to work afterward.
  await page.locator('#sub-list li .del').click();
  await page.click('#edit-cancel');
  await expect(page.locator('#edit')).toBeHidden();
  await expect(page.locator('#toast .toastbtn')).toBeVisible();
  await page.locator('#toast .toastbtn').click();
  await expect(zeile.locator('.subs li.task')).toHaveCount(1);
  await beruhigen();
});

test('Verschieben im Sheet verwirft keine ungespeicherten Eingaben', async () => {
  await aufgabeAnlegen('Snooze-Probe');
  await page.locator('#list li.task', { hasText: 'Snooze-Probe' }).locator('.body').first().click();
  await expect(page.locator('#edit')).toBeVisible();
  await page.fill('#edit-note', 'noch nicht gespeichert');
  await page.click('#snooze-1');
  await expect(page.locator('#edit-date')).not.toHaveValue('');
  await expect(page.locator('#edit-note')).toHaveValue('noch nicht gespeichert');
  await page.click('#edit-cancel');
  await beruhigen();
});

// The "Unteraufgabe von" dropdown didn't know about completed tasks. For a
// task whose own parent was already checked off, it therefore showed
// "keine", and saving silently dropped the subtask from the checklist.
test('Unteraufgabe eines erledigten Eltern-Tasks bleibt beim Speichern dran', async () => {
  const ids = await page.evaluate(async () => {
    const post = (b) => fetch('/api/tasks', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(b),
    }).then((r) => r.json());
    const p = await post({ title: 'Checkliste' });
    const s = await post({ title: 'Punkt A', parent_id: p.id });
    await fetch(`/api/tasks/${p.id}/complete`, { method: 'POST' });
    return { p: p.id, s: s.id };
  });
  await beruhigen();

  const erledigt = page.locator('#list details');
  if (!(await erledigt.evaluate((d) => d.open))) await erledigt.locator('summary').click();
  await erledigt.locator('li.subtask', { hasText: 'Punkt A' }).locator('.body').first().click();
  await expect(page.locator('#edit')).toBeVisible();
  await page.fill('#edit-note', 'nur die Notiz geändert');
  await page.click('#edit-save');
  await expect(page.locator('#edit')).toBeHidden();

  const sub = await page.evaluate(async (id) =>
    (await (await fetch('/api/tasks')).json()).tasks.find((t) => t.id === id), ids.s);
  expect(sub.parent_id).toBe(ids.p);
  await beruhigen();
});

// If the server rejects the delete, the task has to reappear. But the list
// itself is unchanged at that point, so reloading alone doesn't re-render.
test('Fehlgeschlagenes Löschen zeigt die Aufgabe wieder an', async () => {
  await aufgabeAnlegen('Bleibt stehen');
  await aufgabeAnlegen('Löst aus');
  // Replace fetch in the browser instead of using page.route: the DELETE goes
  // out with keepalive, and page.route doesn't intercept requests like that.
  // The response comes back with a delay like over a real network — answered
  // immediately, the (deferred via View Transition) rendering of the second
  // delete would already show the corrected state, and the test would never
  // see the failure.
  await page.evaluate(() => {
    window.__fetch = window.fetch;
    window.fetch = (url, opts = {}) => (opts.method === 'DELETE'
      ? new Promise((ok) => setTimeout(() => ok(new Response('{"error":"kaputt"}',
        { status: 500, headers: { 'Content-Type': 'application/json' } })), 300))
      : window.__fetch(url, opts));
  });
  try {
    const bleibt = page.locator('#list li.task', { hasText: 'Bleibt stehen' });
    await bleibt.locator('.del').first().click();
    await expect(bleibt).toHaveCount(0);
    // a second delete sends the first one out immediately (there's only one undo window)
    await page.locator('#list li.task', { hasText: 'Löst aus' }).locator('.del').first().click();
    await expect(page.locator('#toast')).toContainText('Löschen fehlgeschlagen');
    await expect(bleibt).toHaveCount(1);
  } finally {
    await page.evaluate(() => { window.fetch = window.__fetch; });
  }
  await beruhigen();
});

// Tapping a subtask in the sheet used to open it immediately — title, note,
// and date of the task currently being edited were gone without asking.
test('Wechsel im Sheet fragt bei ungespeicherten Eingaben nach', async () => {
  await page.evaluate(async () => {
    const post = (b) => fetch('/api/tasks', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(b),
    }).then((r) => r.json());
    const p = await post({ title: 'Wechsel-Eltern' });
    await post({ title: 'Wechsel-Kind', parent_id: p.id });
  });
  await beruhigen();
  await page.locator('#list li.task', { hasText: 'Wechsel-Eltern' }).locator('.body').first().click();
  await expect(page.locator('#edit')).toBeVisible();
  await page.fill('#edit-note', 'nicht verlieren');
  await page.locator('#sub-list .title', { hasText: 'Wechsel-Kind' }).click();
  await expect(page.locator('#confirm-dialog')).toBeVisible();
  await page.click('#confirm-cancel');
  await expect(page.locator('#edit-title')).toHaveValue('Wechsel-Eltern');
  await expect(page.locator('#edit-note')).toHaveValue('nicht verlieren');

  await page.locator('#sub-list .title', { hasText: 'Wechsel-Kind' }).click();
  await page.click('#confirm-ok');
  await expect(page.locator('#edit-title')).toHaveValue('Wechsel-Kind');
  await page.click('#edit-cancel');
  await beruhigen();

  // No prompt when nothing changed — not even when the saved note has Windows
  // line endings and trailing spaces (the textarea normalizes both; comparing
  // against the raw data would otherwise look like a change).
  await page.evaluate(async () => {
    const tasks = (await (await fetch('/api/tasks')).json()).tasks;
    const p = tasks.find((x) => x.title === 'Wechsel-Eltern');
    await fetch(`/api/tasks/${p.id}`, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ title: p.title, note: 'Zeile 1\r\nZeile 2  ' }),
    });
  });
  await beruhigen();
  await page.locator('#list li.task', { hasText: 'Wechsel-Eltern' }).locator('.body').first().click();
  await page.locator('#sub-list .title', { hasText: 'Wechsel-Kind' }).click();
  await expect(page.locator('#edit-title')).toHaveValue('Wechsel-Kind');
  await expect(page.locator('#confirm-dialog')).toBeHidden();
  await page.click('#edit-cancel');
  await beruhigen();
});

// Several actions used to swallow server errors: nothing happened, no message.
test('Fehler beim In-Arbeit-Umschalten erscheint als Hinweis', async () => {
  await aufgabeAnlegen('Arbeitsprobe');
  await page.evaluate(() => {
    window.__fetch = window.fetch;
    window.fetch = (url, opts = {}) => (String(url).endsWith('/progress')
      ? Promise.resolve(new Response('{"error":"kaputt"}', { status: 500, headers: { 'Content-Type': 'application/json' } }))
      : window.__fetch(url, opts));
  });
  try {
    await page.locator('#list li.task', { hasText: 'Arbeitsprobe' }).locator('.progressbtn').first().click();
    await expect(page.locator('#toast')).toContainText('kaputt');
  } finally {
    await page.evaluate(() => { window.fetch = window.__fetch; });
  }
  await beruhigen();
});

// While dragging, the drag used to respond to every finger: lifting a second
// one ended it mid-move.
test('Zweiter Finger beendet das Ziehen nicht', async () => {
  await aufgabeAnlegen('Zieh-Probe');
  await aufgabeAnlegen('Zieh-Nachbar');
  const griff = page.locator('#list li.task', { hasText: 'Zieh-Probe' }).locator('.draghandle').first();
  await griff.dispatchEvent('pointerdown', { pointerId: 1, pointerType: 'touch', isPrimary: true, clientY: 100, bubbles: true });
  await expect(page.locator('body')).toHaveClass(/dragging-on/);
  // second finger on a different handle: no second drag
  const andererGriff = page.locator('#list li.task', { hasText: 'Zieh-Nachbar' }).locator('.draghandle').first();
  await andererGriff.dispatchEvent('pointerdown', { pointerId: 2, pointerType: 'touch', clientY: 200, bubbles: true });
  await page.evaluate(() => document.dispatchEvent(new PointerEvent('pointerup', { pointerId: 2, pointerType: 'touch', bubbles: true })));
  await expect(page.locator('body')).toHaveClass(/dragging-on/);
  await page.evaluate(() => document.dispatchEvent(new PointerEvent('pointerup', { pointerId: 1, pointerType: 'touch', bubbles: true })));
  await expect(page.locator('body')).not.toHaveClass(/dragging-on/);
  await beruhigen();
});

// Web push: headless Chromium can't reach a push service. An init script
// fakes the permission and subscription (with a real P-256 key that the
// server validates). This checks that the app subscribes and unsubscribes
// the device.
test('Push lässt sich auf dem Gerät ein- und ausschalten', async () => {
  await page.addInitScript(() => {
    let sub = null;
    let perm = 'default';
    Object.defineProperty(Notification, 'permission', { get: () => perm });
    Notification.requestPermission = async () => (perm = 'granted');
    const b64 = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf)))
      .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
    PushManager.prototype.getSubscription = async () => sub;
    PushManager.prototype.subscribe = async (opts) => {
      const pair = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']);
      const p256dh = b64(await crypto.subtle.exportKey('raw', pair.publicKey));
      const auth = b64(crypto.getRandomValues(new Uint8Array(16)));
      const endpoint = 'https://fcm.googleapis.com/fcm/send/smoke-' + Date.now();
      const key = opts.applicationServerKey;
      sub = {
        endpoint,
        options: { applicationServerKey: key.buffer ? key.buffer : key },
        toJSON: () => ({ endpoint, keys: { p256dh, auth } }),
        unsubscribe: async () => { sub = null; return true; },
      };
      return sub;
    };
  });
  await page.reload();
  await expect(page.locator('#hdr-date')).not.toBeEmpty();
  await beruhigen();

  await page.click('#open-settings');
  const schalter = page.locator('#push-toggle');
  await expect(schalter).toHaveAttribute('aria-checked', 'false');
  await expect(page.locator('#push-test')).toBeHidden();

  const an = page.waitForResponse((r) => r.url().endsWith('/api/push/subscription') && r.request().method() === 'PUT');
  await schalter.click();
  expect((await an).status()).toBe(204);
  await expect(schalter).toHaveAttribute('aria-checked', 'true');
  await expect(page.locator('#push-test')).toBeVisible();

  const aus = page.waitForResponse((r) => r.url().endsWith('/api/push/subscription') && r.request().method() === 'DELETE');
  await schalter.click();
  expect((await aus).status()).toBe(204);
  await expect(schalter).toHaveAttribute('aria-checked', 'false');
  await page.click('#close-settings');
  await expect(page.locator('#settings')).not.toBeVisible();
});

// The app may have been suspended while the task was created on another
// device: TASKS would then be stale, and byId(id) would fail. Without the
// fix, this showed the "gibt es nicht mehr" toast and dropped the pending
// task instead of still opening it after the reload.
test('Antippen einer Benachrichtigung öffnet eine erst danach geladene Aufgabe', async () => {
  const titel = 'Push-Ziel ' + Date.now();
  await page.evaluate(async (titel) => {
    const t = await fetch('/api/tasks', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: titel }),
    }).then((r) => r.json());
    // Deliberately before the SSE reload (150ms debounce): the message arrives
    // while TASKS doesn't know about the new task yet.
    navigator.serviceWorker.dispatchEvent(new MessageEvent('message', { data: { type: 'open-task', id: t.id } }));
  }, titel);
  await expect(page.locator('#edit')).toBeVisible();
  await expect(page.locator('#edit-title')).toHaveValue(titel);
  await expect(page.locator('#toast')).not.toContainText('gibt es nicht mehr');
  await page.click('#edit-cancel');
  await beruhigen();
});

// Priority is a pure marker: colored ring, screen-reader announcement, but
// the list order doesn't change.
test('Listen: anlegen, befüllen, wechseln, verschieben, löschen', async () => {
  const chip = (name) => page.locator('#lists .listchip', { hasText: name });
  await page.click('#lists .listchip.add');
  await page.fill('#list-name', 'Einkauf');
  await page.click('#list-save');
  await expect(page.locator('#lists .listchip.active')).toHaveText('Einkauf');
  await expect(page.locator('#hdr-count')).toHaveText('nichts offen');
  await beruhigen();

  // from "Alle" into the list via #Name
  await page.click('#lists .listchip[data-list="all"]');
  await aufgabeAnlegen('Milch #einkauf', 'Milch');
  const milch = page.locator('#list li.task', { hasText: 'Milch' }).first();
  await expect(milch.locator('.meta')).toContainText('Einkauf');

  await chip('Einkauf').click();
  await expect(page.locator('#list li.task')).toHaveCount(1);
  await expect(page.locator('#hdr-count')).toHaveText('1 Aufgabe offen');

  // move to the default list from within the sheet
  await milch.locator('.body').first().click();
  await page.selectOption('#edit-list', { label: 'Aufgaben' });
  await page.click('#edit-save');
  await beruhigen();
  await expect(page.locator('#list li.task')).toHaveCount(0);

  // Create a task in the open list, rename the list, and delete it via
  // "verschieben"
  await aufgabeAnlegen('Brot');
  await chip('Einkauf').click({ button: 'right' });
  await page.fill('#list-name', 'Einkäufe');
  await page.click('#list-save');
  await expect(chip('Einkäufe')).toBeVisible();
  await beruhigen();
  await chip('Einkäufe').click({ button: 'right' });
  await page.click('#list-delete');
  await page.locator('#choice-dialog button', { hasText: 'In „Aufgaben“ verschieben' }).click();
  await expect(chip('Einkäufe')).toHaveCount(0);
  await expect(page.locator('#lists .listchip.active')).toHaveText('Alle');
  await expect(page.locator('#list li.task', { hasText: 'Brot' })).toBeVisible();
  await beruhigen();
});

// Long press (touch): the dialog opens exactly once, even when Android also
// fires a contextmenu event afterward — and the chip responds to taps again
// afterward.
test('Langes Drücken auf einen Listen-Chip', async () => {
  const fehler = [];
  page.on('pageerror', (e) => fehler.push(e.message));
  const chip = page.locator('#lists .listchip', { hasText: 'Aufgaben' });
  await chip.dispatchEvent('pointerdown', { pointerType: 'touch', isPrimary: true });
  await page.waitForTimeout(650);
  await chip.dispatchEvent('contextmenu');
  await expect(page.locator('#list-dialog')).toBeVisible();
  await chip.dispatchEvent('pointerup', { pointerType: 'touch' });
  await page.click('#list-cancel');
  await chip.click();
  await expect(page.locator('#lists .listchip.active')).toHaveText('Aufgaben');
  expect(fehler).toEqual([]);
  await page.click('#lists .listchip[data-list="all"]');
  await beruhigen();
});

test('Priorität per Schnelleingabe und im Sheet', async () => {
  await aufgabeAnlegen('Steuererklärung !1', 'Steuererklärung');
  const zeile = page.locator('#list li.task', { hasText: 'Steuererklärung' }).first();
  const kreis = zeile.locator('.check').first();
  await expect(kreis).toHaveClass(/prio-3/);
  await expect(kreis).toHaveAttribute('aria-label', 'Steuererklärung erledigen, Priorität hoch');
  // priority only, no date
  await expect(zeile.locator('.meta')).toHaveCount(0);

  const vorher = await page.locator('#list li.task .title').allTextContents();
  await zeile.locator('.body').first().click();
  await expect(page.locator('#edit-prio button[data-v="3"]')).toHaveClass(/active/);
  await page.click('#edit-prio button[data-v="1"]');
  await page.click('#edit-save');
  await beruhigen();
  await expect(kreis).toHaveClass(/prio-1/);
  expect(await page.locator('#list li.task .title').allTextContents()).toEqual(vorher);

  // saving without a change leaves the level as is
  await zeile.locator('.body').first().click();
  await page.click('#edit-save');
  await beruhigen();
  await expect(kreis).toHaveClass(/prio-1/);
});

// Admin creates an account; a second person sets it up with the code in
// their own browser context and sees only their own data.
test('Konten: anlegen, einrichten, getrennt, entfernen', async ({ browser }) => {
  await page.click('#open-settings');
  await page.fill('#user-name', 'Bea');
  await page.click('#add-user');
  await expect(page.locator('#choice-text')).toContainText('Bea');
  const codeText = await page.locator('#choice-text').textContent();
  const code = codeText.match(/[A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4}/)[0];
  await page.locator('#choice-dialog button', { hasText: 'Schließen' }).click();
  await expect(page.locator('#user-list li', { hasText: 'Bea' })).toContainText('wartet auf Einrichtung');
  await page.click('#close-settings');

  const beaCtx = await browser.newContext();
  const bea = await beaCtx.newPage();
  const cdp = await beaCtx.newCDPSession(bea);
  await cdp.send('WebAuthn.enable');
  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
  });
  await bea.goto('/setup');
  await expect(bea.locator('.hint')).toContainText('Setup-Code');
  await bea.fill('#setup-code', code);
  await bea.click('#setup-form button[type=submit]');
  await expect(bea).toHaveURL(/\/$/);
  await expect(bea.locator('#hdr-count')).toHaveText('nichts offen');
  await expect(bea.locator('#list li.task')).toHaveCount(0);
  await bea.fill('#new-title', 'Beas Aufgabe');
  await bea.press('#new-title', 'Enter');
  await expect(bea.locator('#list li.task', { hasText: 'Beas Aufgabe' })).toBeVisible();

  await beruhigen();
  await expect(page.locator('#list li.task', { hasText: 'Beas Aufgabe' })).toHaveCount(0);

  // Removal (the admin already has a fresh login in this run)
  await page.click('#open-settings');
  await page.locator('#user-list li', { hasText: 'Bea' }).locator('.rowbtn.del').click();
  await page.click('#confirm-ok');
  await expect(page.locator('#user-list li', { hasText: 'Bea' })).toHaveCount(0);
  await page.click('#close-settings');
  await bea.reload();
  await expect(bea).toHaveURL(/\/login$/);
  await beaCtx.close();
});

// After 7 days the code has expired, but the account still has no passkey:
// it must be recognizable as such and get a new code.
test('Konten: abgelaufener Code lässt sich erneuern', async () => {
  await page.route('**/api/users', (route) => route.fulfill({
    json: [
      { id: 1, name: 'Admin', is_admin: true, has_passkey: true, code_pending: false },
      { id: 999, name: 'Cleo', is_admin: false, has_passkey: false, code_pending: false },
    ],
  }));
  await page.click('#open-settings');
  const cleo = page.locator('#user-list li', { hasText: 'Cleo' });
  await expect(cleo).toContainText('Code abgelaufen');
  await expect(cleo.locator('button', { hasText: 'Neuer Code' })).toBeVisible();
  await page.click('#close-settings');
  await page.unroute('**/api/users');
});

// A shared list: Cleo sees it, checks something off, the admin sees it; Cleo leaves it.
test('Geteilte Liste: teilen, gemeinsam abhaken, verlassen', async ({ browser }) => {
  await page.click('#open-settings');
  await page.fill('#user-name', 'Cleo');
  await page.click('#add-user');
  await expect(page.locator('#choice-text')).toContainText('Cleo');
  const code = (await page.locator('#choice-text').textContent()).match(/[A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4}-[A-Z0-9]{4}/)[0];
  await page.locator('#choice-dialog button', { hasText: 'Schließen' }).click();
  await page.click('#close-settings');

  // Admin: create list, task, share
  await page.locator('#lists .listchip.add').click();
  await page.fill('#list-name', 'WG');
  await page.click('#list-save');
  await beruhigen();
  await page.fill('#new-title', 'Bad putzen');
  await page.press('#new-title', 'Enter');
  await beruhigen();
  await page.locator('#lists .listchip', { hasText: 'WG' }).click({ button: 'right' });
  await page.selectOption('#share-person', { label: 'Cleo' });
  await page.click('#share-add');
  await expect(page.locator('#list-members li', { hasText: 'Cleo' })).toBeVisible();
  await page.click('#list-cancel');

  const ctx = await browser.newContext();
  const cleo = await ctx.newPage();
  const cdp = await ctx.newCDPSession(cleo);
  await cdp.send('WebAuthn.enable');
  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: { protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true },
  });
  await cleo.goto('/setup');
  await cleo.fill('#setup-code', code);
  await cleo.click('#setup-form button[type=submit]');
  await expect(cleo).toHaveURL(/\/$/);
  await expect(cleo.locator('#lists .listchip.shared', { hasText: 'WG' })).toBeVisible();
  const zeile = cleo.locator('#list li.task', { hasText: 'Bad putzen' });
  await expect(zeile).toBeVisible();
  // In the sheet, a list owned by someone else is recognizable as such
  // (otherwise you might confuse it with one of your own with the same name
  // and share a private task)
  await zeile.locator('.body').first().click();
  await expect(cleo.locator('#edit-list option:checked')).toHaveText('WG (Admin)');
  await cleo.click('#edit-cancel');
  await zeile.locator('.check').first().click();
  await expect(page.locator('#list details li.task', { hasText: 'Bad putzen' })).toHaveCount(1, { timeout: 5000 });

  // Cleo leaves the list
  await cleo.locator('#lists .listchip', { hasText: 'WG' }).click({ button: 'right' });
  await expect(cleo.locator('#list-dialog-title')).toContainText('Admin');
  await cleo.click('#list-leave');
  await cleo.click('#confirm-ok');
  await expect(cleo.locator('#lists .listchip', { hasText: 'WG' })).toHaveCount(0);
  await ctx.close();
  // reset the view back to all lists for the following tests
  await page.locator('#lists .listchip[data-list="all"]').click();
});

test('Abmelden und wieder per Passkey anmelden', async () => {
  await page.click('#open-settings');
  await expect(page.locator('#settings')).toBeVisible();
  await page.click('#logout');
  await expect(page).toHaveURL(/\/login$/);

  // A protected route without a session redirects back to login
  await page.goto('/');
  await expect(page).toHaveURL(/\/login$/);

  await page.click('#login-form button[type=submit]');
  await expect(page).toHaveURL(/\/$/);
  await expect(page.locator('#list li.task', { hasText: 'Umzug' })).toHaveCount(1);
});
