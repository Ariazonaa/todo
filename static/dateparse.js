'use strict';
// Natural-language date input in quick-add, one vocabulary per language:
//   de  "Milch kaufen morgen 18:00", "Reifen fr", "Miete in 3 tagen", "jeden montag Müll raus"
//   en  "Buy milk tomorrow 6pm", "Report fri", "Rent in 3 days", "Trash every monday"
//   both: "!1" high, "!2" medium, "!3" low (priority, language-independent)
//   list: "#Einkauf" (parseListTag)
// Only the active language is understood — both at once would cause false
// matches ("do the dishes" would become Thursday). A separate file without
// app context, so tests/dateparse.spec.js can test it directly.

function localDateStr(d) {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}
function addDays(dateStr, n) {
  const d = new Date(dateStr + 'T00:00:00');
  d.setDate(d.getDate() + n);
  return localDateStr(d);
}

function nextWeekdayStr(wd, now) {
  const d = new Date(now);
  d.setHours(0, 0, 0, 0);
  let diff = (wd - d.getDay() + 7) % 7;
  if (diff === 0) diff = 7;
  d.setDate(d.getDate() + diff);
  return localDateStr(d);
}

// Vocabulary. times: tried in order; h12 = hour with am/pm (group 3).
// date: 'dmy' ("am 3.10.", "3.2.2027") or 'iso' ("2026-10-03").
const DATE_VOCAB = {
  de: {
    times: [
      { re: /(?:um\s+)?([01]?\d|2[0-3])(?::([0-5]\d))?\s*uhr/ },
      { re: /(?:um\s+)?([01]?\d|2[0-3]):([0-5]\d)/ },
    ],
    weekdays: { montag: 1, mo: 1, dienstag: 2, di: 2, mittwoch: 3, mi: 3, donnerstag: 4, do: 4, freitag: 5, fr: 5, samstag: 6, sa: 6, sonntag: 0 },
    everyWeekday: /jeden?\s+(montag|dienstag|mittwoch|donnerstag|freitag|samstag|sonntag)/,
    daily: /täglich/,
    weekly: /wöchentlich/,
    monthly: /monatlich/,
    dayAfter: /übermorgen/,
    tomorrow: /morgen/,
    today: /heute/,
    inDays: /in\s+(\d{1,3})\s+tag(?:en)?/,
    inWeeks: /in\s+(\d{1,2})\s+wochen?/,
    weekday: /(?:am\s+)?(montag|dienstag|mittwoch|donnerstag|freitag|samstag|sonntag|mo|di|mi|do|fr|sa)/,
    date: 'dmy',
    trailing: /\s+(am|um)$/i,
  },
  en: {
    times: [
      { re: /(?:at\s+)?(1[0-2]|0?[1-9])(?::([0-5]\d))?\s*(am|pm)/, h12: true },
      { re: /(?:at\s+)?([01]?\d|2[0-3]):([0-5]\d)/ },
    ],
    // sat/sun are deliberately missing: "sun cream", "Sat nav" aren't weekdays.
    weekdays: { monday: 1, mon: 1, tuesday: 2, tue: 2, wednesday: 3, wed: 3, thursday: 4, thu: 4, friday: 5, fri: 5, saturday: 6, sunday: 0 },
    everyWeekday: /every\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday)/,
    daily: /daily/,
    weekly: /weekly/,
    monthly: /monthly/,
    dayAfter: null,
    tomorrow: /tomorrow/,
    today: /today/,
    inDays: /in\s+(\d{1,3})\s+days?/,
    inWeeks: /in\s+(\d{1,2})\s+weeks?/,
    weekday: /(?:on\s+)?(monday|tuesday|wednesday|thursday|friday|saturday|sunday|mon|tue|wed|thu|fri)/,
    date: 'iso',
    // No cleanup at the end: the patterns already consume on/at, and English
    // titles often end on one of them ("Turn the heating on").
    trailing: null,
  },
};

function parseNaturalDate(raw, lang = 'de', now = new Date()) {
  const V = DATE_VOCAB[lang] || DATE_VOCAB.en;
  let title = raw;
  let date = null;
  let time = null;
  let rec = '';
  // Word boundaries are whitespace, brackets and punctuation — not \b: that
  // also splits on hyphens ("To-do Liste", "Morgen-Routine") and doesn't work
  // before umlauts. After times, ":" doesn't count as a boundary, and after
  // dates neither does ".", because both belong to the value there.
  const PRE = String.raw`(?<=^|[\s(])`;
  const POST = String.raw`(?=[\s,;:!?.)]|$)`;
  const TIME_POST = String.raw`(?=[\s,;!?.)]|$)`;
  const DATE_POST = String.raw`(?=[\s,;:!?)]|$)`;
  // Search the original text (/i), not toLowerCase(): that changes the length
  // of some characters ("İ" becomes two), and strip() would then cut at the
  // wrong spot.
  const re = (r, post = POST) => title.match(new RegExp(`${PRE}(?:${r.source})${post}`, 'i'));
  const strip = (m) => { title = title.slice(0, m.index) + ' ' + title.slice(m.index + m[0].length); };
  const todayStr = localDateStr(now);

  let m;
  // Priority: "!1" high … "!3" low as its own word, only the first one counts
  let prio = 0;
  if ((m = re(/!([1-3])/))) {
    prio = 4 - Number(m[1]);
    strip(m);
  }
  for (const tm of V.times) {
    if (!(m = re(tm.re, TIME_POST))) continue;
    let h = Number(m[1]);
    if (tm.h12) h = (h % 12) + (m[3].toLowerCase() === 'pm' ? 12 : 0);
    time = String(h).padStart(2, '0') + ':' + (m[2] || '00');
    strip(m);
    break;
  }

  if ((m = re(V.everyWeekday))) {
    rec = 'weekly';
    date = nextWeekdayStr(V.weekdays[m[1].toLowerCase()], now);
    strip(m);
  } else if ((m = re(V.daily))) { rec = 'daily'; date = todayStr; strip(m); }
  else if ((m = re(V.weekly))) { rec = 'weekly'; date = todayStr; strip(m); }
  else if ((m = re(V.monthly))) { rec = 'monthly'; date = todayStr; strip(m); }

  if (!date) {
    if (V.dayAfter && (m = re(V.dayAfter))) { date = addDays(todayStr, 2); strip(m); }
    else if ((m = re(V.tomorrow))) { date = addDays(todayStr, 1); strip(m); }
    else if ((m = re(V.today))) { date = todayStr; strip(m); }
    else if ((m = re(V.inDays))) { date = addDays(todayStr, Number(m[1])); strip(m); }
    else if ((m = re(V.inWeeks))) { date = addDays(todayStr, 7 * Number(m[1])); strip(m); }
    else if ((m = re(V.weekday))) { date = nextWeekdayStr(V.weekdays[m[1].toLowerCase()], now); strip(m); }
    else if (V.date === 'dmy') {
      if ((m = re(/am\s+([0-3]?\d)\.([01]?\d)\.?(\d{4})?/, DATE_POST)) ||
          // without "am", only with a period after the month: "3.2." is a date,
          // "Kapitel 3.2" or "2.5 kg" aren't
          (m = re(/([0-3]?\d)\.([01]?\d)\.(\d{4})?/, DATE_POST))) {
        const d = Number(m[1]);
        const mo = Number(m[2]);
        const y = m[3] ? Number(m[3]) : now.getFullYear();
        let cand = new Date(y, mo - 1, d);
        if (d >= 1 && mo >= 1 && mo <= 12 && cand.getDate() === d) {
          if (!m[3] && localDateStr(cand) < todayStr) cand = new Date(y + 1, mo - 1, d);
          date = localDateStr(cand);
          strip(m);
        }
      }
    } else if ((m = re(/(?:on\s+)?(\d{4})-(\d{2})-(\d{2})/, DATE_POST))) {
      const cand = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
      // "2026-02-30" doesn't exist — Date would silently roll it into March
      if (localDateStr(cand) === `${m[1]}-${m[2]}-${m[3]}`) {
        date = localDateStr(cand);
        strip(m);
      }
    }
  }
  title = title.replace(/\s+/g, ' ').trim();
  if (V.trailing) title = title.replace(V.trailing, '').trim();
  return { title, date, time, rec, prio };
}

// parseListTag: "#Einkauf" selects the list with that name — case-insensitive
// and ignoring spaces in the list name ("#zuhause" matches "Zu Hause").
// Only the first #word counts; without a match, the text is left unchanged.
// lists: [{id, label}] with the displayed name.
function parseListTag(raw, lists) {
  const m = raw.match(/(?<=^|[\s(])#([^\s#,;:!?.()]+)(?=[\s,;:!?.)]|$)/u);
  if (!m) return { title: raw, listId: null };
  const key = (s) => s.toLocaleLowerCase().replace(/\s+/g, '');
  const hit = lists.find((l) => key(l.label) === key(m[1]));
  if (!hit) return { title: raw, listId: null };
  const title = (raw.slice(0, m.index) + ' ' + raw.slice(m.index + m[0].length)).replace(/\s+/g, ' ').trim();
  return { title, listId: hit.id };
}
