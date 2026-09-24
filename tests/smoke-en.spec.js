// English run against its own server (playwright.config.js, project "en"):
// browser set to en-US, the app follows it.
// Structured like smoke.spec.js: serial, shared page with a virtual authenticator.
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

const beruhigen = () => page.waitForTimeout(400);

test('setup is English, including server errors', async () => {
  await page.goto('/');
  await expect(page).toHaveURL(/\/setup$/);
  await expect(page.locator('html')).toHaveAttribute('lang', 'en');
  await expect(page).toHaveTitle('Todo — Setup');
  await expect(page.locator('h1')).toHaveText('Welcome');
  await expect(page.locator('#setup-code')).toHaveAttribute('placeholder', 'Setup code (XXXX-XXXX-XXXX-XXXX)');
  // The hint keeps its <code> elements
  await expect(page.locator('.hint code')).toHaveCount(2);

  await page.fill('#setup-code', 'FALSCH');
  await page.click('#setup-form button[type=submit]');
  await expect(page.locator('#err')).toHaveText("Setup code missing or wrong — it's in the server log");

  const code = fs.readFileSync(path.join(process.env.E2E_DB_DIR_EN, 'setup-code.txt'), 'utf8').trim();
  await page.fill('#setup-code', code);
  await page.click('#setup-form button[type=submit]');
  await expect(page).toHaveURL(/\/$/);
});

// An unsupported browser language falls back to English.
test('unsupported browser language falls back to English', async ({ browser }) => {
  const fr = await browser.newContext({ locale: 'fr-FR' });
  const p = await fr.newPage();
  await p.goto('/login');
  await expect(p.locator('html')).toHaveAttribute('lang', 'en');
  await expect(p.locator('h1')).toHaveText('Sign in');
  await expect(p.locator('#login-form button')).toContainText('Sign in with passkey');
  await fr.close();
});

test('app speaks English', async () => {
  await expect(page.locator('#hdr-count')).toHaveText('nothing open');
  await expect(page.locator('#hdr-weekday')).toHaveText(/^(Sun|Mon|Tues|Wednes|Thurs|Fri|Satur)day · Good (morning|afternoon|evening|night)$/);
  await expect(page.locator('#new-title')).toHaveAttribute('placeholder', 'New task…');
});

test('quick add understands English and shows 12-hour times', async () => {
  await page.fill('#new-title', 'Buy milk tomorrow 6pm');
  await page.press('#new-title', 'Enter');
  await expect(page.locator('#toast')).toHaveText('Recognized: tomorrow · 6:00 PM');
  const row = page.locator('#list li.task', { hasText: 'Buy milk' });
  await expect(row.locator('.meta')).toContainText('tomorrow · 6:00 PM');
  await expect(row.locator('.check')).toHaveAttribute('aria-label', 'Complete Buy milk');
  await expect(page.locator('#hdr-count')).toHaveText('1 task open');
  await beruhigen();
});

// Titles are user input: placeholders and markup inside them stay literal text.
test('titles with braces and markup stay literal', async () => {
  const title = '{title} <img src=x onerror=window.hacked=1>';
  await page.fill('#new-title', title);
  await page.press('#new-title', 'Enter');
  const row = page.locator('#list li.task', { hasText: '{title}' });
  await expect(row.locator('.check')).toHaveAttribute('aria-label', `Complete ${title}`);
  await beruhigen();
  const deleted = page.waitForResponse((r) => r.request().method() === 'DELETE', { timeout: 10_000 });
  await row.locator('.rowbtn.del').click();
  await expect(page.locator('#toast')).toContainText(`“${title}” deleted`);
  expect(await page.evaluate(() => window.hacked)).toBeUndefined();
  // The delete only goes out after the undo window — wait for it, otherwise
  // the language-switch test below would still count the task.
  await deleted;
  await beruhigen();
});

test('server errors come back in English', async () => {
  await page.locator('#list li.task', { hasText: 'Buy milk' }).locator('.body').click();
  await page.fill('#edit-title', '');
  await page.click('#edit-save');
  await expect(page.locator('#toast')).toHaveText('Title is missing');
  await page.click('#edit-cancel');
  await beruhigen();
});

// Without any catalog at all (offline, nothing cached), the German text from
// the HTML stays as is — no raw keys — and the page still becomes visible.
test('without any catalog the German HTML stays and the page shows', async ({ browser }) => {
  const ctx = await browser.newContext();
  await ctx.route('**/locales/**', (r) => r.abort());
  const p = await ctx.newPage();
  await p.goto('/login');
  await expect(p.locator('h1')).toBeVisible();
  await expect(p.locator('h1')).toHaveText('Anmelden');
  await ctx.close();
});

// Even if i18n.js fails to load at all, the CSS still reveals the page.
test('page shows even when i18n.js fails to load', async ({ browser }) => {
  const ctx = await browser.newContext();
  await ctx.route('**/i18n.js', (r) => r.abort());
  const p = await ctx.newPage();
  await p.goto('/login');
  await expect(p.locator('h1')).toBeVisible({ timeout: 5000 });
  await ctx.close();
});

// If the catalog arrives only after the 1.5s deadline (slow network), the
// already-rendered list gets translated afterward. Uses its own context with
// the page's session, without a service worker, so the delay actually takes
// effect.
test('a late catalog still translates the rendered list', async ({ browser }) => {
  const ctx = await browser.newContext({ serviceWorkers: 'block' });
  await ctx.addCookies(await context.cookies());
  await ctx.route('**/locales/en.json', async (r) => {
    await new Promise((res) => setTimeout(res, 2500));
    await r.continue();
  });
  const p = await ctx.newPage();
  await p.goto('/');
  await expect(p.locator('#list h2', { hasText: 'Upcoming' })).toBeVisible({ timeout: 8000 });
  await expect(p.locator('#hdr-count')).toHaveText('1 task open');
  await ctx.close();
});

test('switching to German reloads in German and sticks', async () => {
  await page.click('#open-settings');
  await expect(page.locator('#set-lang option[value=auto]')).toHaveText('Automatic (English)');
  await Promise.all([page.waitForEvent('load'), page.selectOption('#set-lang', 'de')]);
  await expect(page.locator('html')).toHaveAttribute('lang', 'de');
  await expect(page.locator('#hdr-count')).toHaveText('1 Aufgabe offen');
  expect(await page.evaluate(() => localStorage.getItem('lang'))).toBe('de');
});
