'use strict';
// Translations. Loads as the first script on every page: determines the
// language, fetches the catalog (static/locales/*.json — the server uses the
// same files), translates the static HTML, and then reveals the page
// (.i18n-ready, see style.css). Anything that renders after this waits on i18nReady.

// Supported languages — must match languages in i18n.go (tested).
const SUPPORTED = ['de', 'en'];
// Names shown in the language switcher, each written in itself.
const LANG_NAMES = { de: 'Deutsch', en: 'English' };

function storedLang() {
  try { return localStorage.getItem('lang'); } catch { return null; }
}
// setStoredLang mirrors the DB setting ('auto' = no mirror), so login and
// setup — without a session, without settings — already know it.
function setStoredLang(v) {
  try {
    if (v && v !== 'auto') localStorage.setItem('lang', v);
    else localStorage.removeItem('lang');
  } catch {}
}
// browserLang: the first supported language of the browser, else English.
function browserLang() {
  const list = navigator.languages && navigator.languages.length ? navigator.languages : [navigator.language];
  for (const l of list) {
    const p = String(l || '').toLowerCase().split('-')[0];
    if (SUPPORTED.includes(p)) return p;
  }
  return 'en';
}
function detectLang() {
  const s = storedLang();
  return SUPPORTED.includes(s) ? s : browserLang();
}

const LANG = detectLang();
// Locale for Intl: the browser's, if it matches the UI language (en-GB
// stays en-GB), otherwise the UI language itself.
const FMT_LOCALE = String(navigator.language || '').toLowerCase().split('-')[0] === LANG ? navigator.language : LANG;

let CATALOG = {};
let FALLBACK = {}; // German — the reference, in case a key is missing in LANG
const warned = new Set();

function lookup(key) {
  if (Object.hasOwn(CATALOG, key)) return CATALOG[key];
  if (Object.hasOwn(FALLBACK, key)) {
    if (!warned.has(key)) { warned.add(key); console.warn(`i18n: "${key}" fehlt in ${LANG}`); }
    return FALLBACK[key];
  }
  return undefined;
}

const pluralRules = new Intl.PluralRules(LANG);

// t translates key; params replace {name}. If key has no entry of its own
// and params.n is a number, it's treated as a plural: key.zero (for 0, if
// present), else key.one/key.other per Intl.PluralRules. Substitution happens
// in a single pass; the result is text — never insert it as HTML.
function t(key, params = {}) {
  let msg = lookup(key);
  if (msg === undefined && typeof params.n === 'number') {
    if (params.n === 0) msg = lookup(key + '.zero');
    if (msg === undefined) msg = lookup(key + '.' + pluralRules.select(params.n));
    if (msg === undefined) msg = lookup(key + '.other');
  }
  if (msg === undefined) return key;
  return msg.replace(/\{([a-z]+)\}/g, (m, name) => (Object.hasOwn(params, name) ? String(params[name]) : m));
}

// translateInto sets node's text from key. Children with data-i18n-slot
// replace their placeholder ({cmd} → <code data-i18n-slot="cmd">) — this
// keeps markup possible within the text, without HTML coming from the catalog.
function translateInto(node, key) {
  const slots = {};
  for (const c of node.querySelectorAll(':scope > [data-i18n-slot]')) slots[c.dataset.i18nSlot] = c;
  if (!Object.keys(slots).length) { node.textContent = t(key); return; }
  const parts = t(key).split(/(\{[a-z]+\})/).map((piece) => {
    const m = /^\{([a-z]+)\}$/.exec(piece);
    return m && slots[m[1]] ? slots[m[1]] : document.createTextNode(piece);
  });
  node.replaceChildren(...parts);
}

function applyI18n(root = document) {
  for (const n of root.querySelectorAll('[data-i18n]')) translateInto(n, n.dataset.i18n);
  for (const n of root.querySelectorAll('[data-i18n-attr]')) {
    for (const pair of n.dataset.i18nAttr.split(';')) {
      const [attr, key] = pair.split(':').map((s) => s.trim());
      if (attr && key) n.setAttribute(attr, t(key));
    }
  }
}

async function fetchCatalog(lang) {
  const res = await fetch(`/locales/${lang}.json`);
  if (!res.ok) throw new Error('locale ' + lang);
  return res.json();
}

// i18nReady: catalog loaded and HTML translated — or given up after 1.5s
// (offline without a cache). In that case, the German text from the HTML
// stays as is, and t() returns keys; if the catalog does arrive later, the
// HTML gets translated retroactively and 'i18n-late' fires — the app then re-renders.
let i18nSettled = false;
const i18nReady = (async () => {
  document.documentElement.lang = LANG;
  const load = (async () => {
    const [cat, de] = await Promise.all([
      fetchCatalog(LANG).catch(() => null),
      LANG === 'de' ? null : fetchCatalog('de').catch(() => null),
    ]);
    CATALOG = cat || {};
    FALLBACK = LANG === 'de' ? CATALOG : (de || {});
    // No catalog at all: the German text from the HTML stays — better than keys.
    if (!cat && !de) return;
    applyI18n(document);
    if (i18nSettled) document.dispatchEvent(new Event('i18n-late'));
  })();
  await Promise.race([load, new Promise((r) => setTimeout(r, 1500))]);
  i18nSettled = true;
  document.documentElement.classList.add('i18n-ready');
})();
