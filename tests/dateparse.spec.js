// Quick-add parser directly, without a server: load dateparse.js into a
// blank page and call it with a fixed "now" — Thu 2026-09-24, 10:00.
const path = require('node:path');
const { test, expect } = require('@playwright/test');

const NOW = [2026, 8, 24, 10, 0];
const none = (title) => ({ title, date: null, time: null, rec: '' });

const cases = [
  // German — exactly the previous behavior
  ['de', 'Milch kaufen morgen 18:00', { title: 'Milch kaufen', date: '2026-09-25', time: '18:00', rec: '' }],
  ['de', 'Reifen fr', { title: 'Reifen', date: '2026-09-25', time: null, rec: '' }],
  ['de', 'Miete in 3 tagen', { title: 'Miete', date: '2026-09-27', time: null, rec: '' }],
  ['de', 'jeden montag Müll raus', { title: 'Müll raus', date: '2026-09-28', time: null, rec: 'weekly' }],
  ['de', 'Zahnarzt am 3.10. um 9 uhr', { title: 'Zahnarzt', date: '2026-10-03', time: '09:00', rec: '' }],
  ['de', 'Party übermorgen', { title: 'Party', date: '2026-09-26', time: null, rec: '' }],
  ['de', 'Morgen-Routine planen', none('Morgen-Routine planen')],
  ['de', 'Kapitel 3.2 lesen', none('Kapitel 3.2 lesen')],
  ['de', 'Buy milk tomorrow', none('Buy milk tomorrow')],
  // English
  ['en', 'Buy milk tomorrow 6pm', { title: 'Buy milk', date: '2026-09-25', time: '18:00', rec: '' }],
  ['en', 'Call mom at 6:30 am', { title: 'Call mom', date: null, time: '06:30', rec: '' }],
  ['en', 'Pay rent in 3 days', { title: 'Pay rent', date: '2026-09-27', time: null, rec: '' }],
  ['en', 'Review in 2 weeks', { title: 'Review', date: '2026-10-08', time: null, rec: '' }],
  ['en', 'Trash every monday', { title: 'Trash', date: '2026-09-28', time: null, rec: 'weekly' }],
  ['en', 'Water plants daily', { title: 'Water plants', date: '2026-09-24', time: null, rec: 'daily' }],
  ['en', 'Report fri', { title: 'Report', date: '2026-09-25', time: null, rec: '' }],
  ['en', 'Dentist on 2026-10-03 at 14:00', { title: 'Dentist', date: '2026-10-03', time: '14:00', rec: '' }],
  ['en', 'Standup today 12am', { title: 'Standup', date: '2026-09-24', time: '00:00', rec: '' }],
  ['en', 'Lunch today 12pm', { title: 'Lunch', date: '2026-09-24', time: '12:00', rec: '' }],
  ['en', 'Tomorrow-list cleanup', none('Tomorrow-list cleanup')],
  ['en', 'do the dishes', none('do the dishes')],
  ['en', 'Buy sun cream', none('Buy sun cream')],
  ['en', 'Sat nav update', none('Sat nav update')],
  ['en', 'Meeting at 5', none('Meeting at 5')],
  ['en', 'Invoice 2026-02-30', none('Invoice 2026-02-30')],
  ['en', 'Milch kaufen morgen', none('Milch kaufen morgen')],
  // Phrasal verbs with on/at at the end stay intact
  ['en', 'Turn the heating on tomorrow', { title: 'Turn the heating on', date: '2026-09-25', time: null, rec: '' }],
  ['en', 'Put the kettle on at 6pm', { title: 'Put the kettle on', date: null, time: '18:00', rec: '' }],
  ['en', 'Tomorrow check what to look at', { title: 'check what to look at', date: '2026-09-25', time: null, rec: '' }],
  // Priority: !1 high, !2 medium, !3 low — language-independent
  ['de', 'Steuer !1 morgen', { title: 'Steuer', date: '2026-09-25', time: null, rec: '', prio: 3 }],
  ['de', 'morgen !1 18:00 Steuer', { title: 'Steuer', date: '2026-09-25', time: '18:00', rec: '', prio: 3 }],
  ['en', 'Taxes !2', { title: 'Taxes', date: null, time: null, rec: '', prio: 2 }],
  ['en', 'Tidy up !3', { title: 'Tidy up', date: null, time: null, rec: '', prio: 1 }],
  ['de', 'Erst !2 dann !1', { title: 'Erst dann !1', date: null, time: null, rec: '', prio: 2 }],
  ['de', 'Level !4', none('Level !4')],
  ['de', 'Seite !10', none('Seite !10')],
  ['en', 'Wow!1', none('Wow!1')],
  // Just the marker alone: the title would end up empty — the app then creates it uncut (recognized = false)
  ['de', '!1', { title: '', date: null, time: null, rec: '', prio: 3 }],
];

test.beforeEach(async ({ page }) => {
  await page.addScriptTag({ path: path.join(__dirname, '..', 'static', 'dateparse.js') });
});

for (const [lang, raw, want] of cases) {
  test(`${lang}: ${raw}`, async ({ page }) => {
    const got = await page.evaluate(([raw, lang, now]) => parseNaturalDate(raw, lang, new Date(...now)), [raw, lang, NOW]);
    expect(got).toEqual({ prio: 0, ...want });
  });
}

// #Name selects the list — case-insensitive, spaces in the list name don't
// count; only the first #word is checked, an unknown one is left as is.
const LISTS = [{ id: 1, label: 'Aufgaben' }, { id: 2, label: 'Einkauf' }, { id: 3, label: 'Zu Hause' }];
const tagCases = [
  ['Milch #Einkauf morgen', { title: 'Milch morgen', listId: 2 }],
  ['Milch #einkauf', { title: 'Milch', listId: 2 }],
  ['Staubsaugen #zuhause', { title: 'Staubsaugen', listId: 3 }],
  ['Notiz #aufgaben', { title: 'Notiz', listId: 1 }],
  ['Bug in #Frontend', { title: 'Bug in #Frontend', listId: null }],
  ['C# lernen', { title: 'C# lernen', listId: null }],
  ['#Einkauf Brot #Zu', { title: 'Brot #Zu', listId: 2 }],
  ['Brot #Unbekannt #Einkauf', { title: 'Brot #Unbekannt #Einkauf', listId: null }],
  ['Brot (#Einkauf)', { title: 'Brot ( )', listId: 2 }],
];
for (const [raw, want] of tagCases) {
  test(`#Liste: ${raw}`, async ({ page }) => {
    const got = await page.evaluate(([raw, lists]) => parseListTag(raw, lists), [raw, LISTS]);
    expect(got).toEqual(want);
  });
}
