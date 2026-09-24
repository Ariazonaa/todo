'use strict';

const $ = (sel) => document.querySelector(sel);
const motionOK = !matchMedia('(prefers-reduced-motion: reduce)').matches;
// Catalog keys as literals — the test for unused keys searches for them.
const WD_KEYS = ['date.wd.0', 'date.wd.1', 'date.wd.2', 'date.wd.3', 'date.wd.4', 'date.wd.5', 'date.wd.6'];
const WDS_KEYS = ['date.wds.0', 'date.wds.1', 'date.wds.2', 'date.wds.3', 'date.wds.4', 'date.wds.5', 'date.wds.6'];
const MON_KEYS = ['date.mon.1', 'date.mon.2', 'date.mon.3', 'date.mon.4', 'date.mon.5', 'date.mon.6',
  'date.mon.7', 'date.mon.8', 'date.mon.9', 'date.mon.10', 'date.mon.11', 'date.mon.12'];
const MONS_KEYS = ['date.mons.1', 'date.mons.2', 'date.mons.3', 'date.mons.4', 'date.mons.5', 'date.mons.6',
  'date.mons.7', 'date.mons.8', 'date.mons.9', 'date.mons.10', 'date.mons.11', 'date.mons.12'];
const REC_KEYS = { daily: 'rec.daily', weekly: 'rec.weekly', monthly: 'rec.monthly' };
const PRIO_KEYS = ['', 'prio.aria.1', 'prio.aria.2', 'prio.aria.3'];
const prioClass = (x) => (x.priority ? ' prio-' + x.priority : '');
// withPrio appends the priority level to an aria-label — the color alone
// tells screen readers nothing.
const withPrio = (label, x) => (x.priority ? `${label}, ${t(PRIO_KEYS[x.priority])}` : label);

let TODAY = localDateStr(new Date());
let TOMORROW = addDays(TODAY, 1);
let TASKS = [];
const inflight = new Set(); // task IDs with a request in flight (double-tap protection)
let doneOpen = false;       // state of the Done section across re-renders
let loadSeq = 0;            // discards late, stale /api/tasks responses
let searchTerm = '';
let editId = null;          // task currently being edited in the sheet
let editRec = '';
let editPinned = false;
let dragActive = false;     // is a drag in progress right now? (a re-render would rip the row out from under it)
let LISTS = [];             // from /api/tasks, default list first
let currentList = storedList(); // 'all' or a list ID; remembered per device
let lockedScrollY = null;   // scroll position while a sheet is open (see syncScrollLock)

const delay = (ms) => new Promise((r) => setTimeout(r, ms));
const byId = (id) => TASKS.find((t) => t.id === id);
const subsOf = (id) => TASKS.filter((t) => t.parent_id === id && !hiddenIds.has(t.id));

async function api(path, opts = {}) {
  let res;
  try {
    // X-Lang: error messages come back in the app's language, and the server
    // remembers it for notifications (noteLang).
    const headers = { 'Content-Type': 'application/json', ...opts.headers, 'X-Lang': LANG };
    res = await fetch(path, { ...opts, headers });
  } catch (e) {
    $('#offline').hidden = false;
    throw e;
  }
  $('#offline').hidden = true;
  if (res.status === 401) {
    location.href = '/login';
    throw new Error('unauthorized');
  }
  return res;
}

// failed shows the server's message as a toast on an error response.
async function failed(res, fallback = t('common.error')) {
  if (res.ok) return false;
  const d = await res.json().catch(() => ({}));
  toast(d.error || fallback);
  return true;
}

// dateParams: all parameters for the fmt.* patterns — each language picks what it needs.
function dateParams(d) {
  return {
    wd: t(WDS_KEYS[d.getDay()]),
    d: d.getDate(),
    dd: String(d.getDate()).padStart(2, '0'),
    mm: String(d.getMonth() + 1).padStart(2, '0'),
    mon: t(MONS_KEYS[d.getMonth()]),
    month: t(MON_KEYS[d.getMonth()]),
  };
}

function fmtDate(dateStr) {
  if (dateStr === TODAY) return t('date.today');
  if (dateStr === TOMORROW) return t('date.tomorrow');
  return t('fmt.date_short', dateParams(new Date(dateStr + 'T00:00:00')));
}

// fmtTime: 24-hour locales (de, en-GB) keep "HH:MM" exactly as stored;
// 12-hour locales (en-US) get "6:30 PM" from Intl.
const hour12 = !['h23', 'h24'].includes(new Intl.DateTimeFormat(FMT_LOCALE, { hour: 'numeric' }).resolvedOptions().hourCycle);
function fmtTime(hhmm) {
  if (!hour12) return hhmm;
  const [h, m] = hhmm.split(':').map(Number);
  return new Intl.DateTimeFormat(FMT_LOCALE, { hour: 'numeric', minute: '2-digit' }).format(new Date(2000, 0, 1, h, m));
}

function greeting() {
  const h = new Date().getHours();
  if (h < 5) return t('greet.night');
  if (h < 11) return t('greet.morning');
  if (h < 18) return t('greet.day');
  if (h < 23) return t('greet.evening');
  return t('greet.night');
}

// openCount null = no data yet (e.g. started offline) — better to show
// nothing than to claim "nothing open".
function renderHeader(openCount) {
  const d = new Date(TODAY + 'T00:00:00');
  $('#hdr-weekday').textContent = `${t(WD_KEYS[d.getDay()])} · ${greeting()}`;
  $('#hdr-date').textContent = t('fmt.header_date', dateParams(d));
  $('#hdr-count').textContent = openCount === null ? '' : t('header.open', { n: openCount });
}

function storedList() {
  try {
    const v = localStorage.getItem('list');
    return v && v !== 'all' ? Number(v) : 'all';
  } catch { return 'all'; }
}
function setCurrentList(v) {
  currentList = v;
  try { localStorage.setItem('list', String(v)); } catch {}
  renderChips();
  rerender();
}
const listLabel = (l) => l.name ?? t('lists.default');
// Other people's lists that have an owner — wherever you file a task, a
// foreign "Einkauf" list must not look like your own (it's a shared list).
const listLabelFull = (l) => (l.owner_name ? t('lists.with_owner', { name: listLabel(l), owner: l.owner_name }) : listLabel(l));
const defaultList = () => LISTS.find((l) => l.is_default);
const listById = (id) => LISTS.find((l) => l.id === id);
// inView: does the task belong to the selected view? Subtasks live in
// their parent task's list, so they're filtered along with it.
const inView = (x) => currentList === 'all' || x.list_id === currentList;

// Large-title behavior: header shrinks into a compact glass bar while scrolling
let scrolledState = false;
window.addEventListener('scroll', () => {
  // While the page is locked (a sheet is open), scrollY sits at 0 — the header
  // would expand and shift the scroll position when the sheet closes.
  if (lockedScrollY !== null) return;
  const y = window.scrollY;
  const next = scrolledState ? y > 30 : y > 70; // hysteresis against flickering
  if (next !== scrolledState) {
    scrolledState = next;
    document.body.classList.toggle('scrolled', next);
  }
}, { passive: true });

// iOS renders empty date/time inputs completely blank — .has-value drives
// the CSS placeholder (see the @supports block in the stylesheet)
function syncPickers() {
  for (const i of document.querySelectorAll('input[type="date"], input[type="time"]')) {
    i.classList.toggle('has-value', !!i.value);
  }
}
document.addEventListener('input', (e) => {
  if (e.target.matches?.('input[type="date"], input[type="time"]')) {
    e.target.classList.toggle('has-value', !!e.target.value);
  }
});

function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined) node.textContent = text;
  return node;
}

// SF Symbols-style line icons (inline SVG, stroke = currentColor)
const ICONS = {
  check: '<polyline points="5 13 10 18 19 7" pathLength="1"/>',
  xmark: '<line x1="6" y1="6" x2="18" y2="18"/><line x1="18" y1="6" x2="6" y2="18"/>',
  forward: '<polyline points="5 6 11 12 5 18"/><polyline points="12 6 18 12 12 18"/>',
  repeat: '<polyline points="17 1 21 5 17 9"/><path d="M3 11V9a4 4 0 0 1 4-4h14"/><polyline points="7 23 3 19 7 15"/><path d="M21 13v2a4 4 0 0 1-4 4H3"/>',
  note: '<line x1="4" y1="7" x2="20" y2="7"/><line x1="4" y1="12" x2="16" y2="12"/><line x1="4" y1="17" x2="11" y2="17"/>',
  link: '<path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/>',
  checksquare: '<rect x="4" y="4" width="16" height="16" rx="4"/><polyline points="8 12 11 15 16 9"/>',
  inbox: '<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>',
  checkcircle: '<circle cx="12" cy="12" r="9"/><polyline points="8 12.5 11 15.5 16 9.5"/>',
  search: '<circle cx="11" cy="11" r="7"/><line x1="16.5" y1="16.5" x2="21" y2="21"/>',
  handle: '<line x1="5" y1="9" x2="19" y2="9"/><line x1="5" y1="15" x2="19" y2="15"/>',
  photo: '<rect x="3" y="5" width="18" height="14" rx="3"/><circle cx="9" cy="10" r="1.6"/><path d="M4 17l4.5-4 3.5 3 4-4 4 4.5"/>',
  play: '<path d="M8 5.5v13l11-6.5z"/>',
  pause: '<line x1="9.5" y1="6" x2="9.5" y2="18"/><line x1="14.5" y1="6" x2="14.5" y2="18"/>',
  pin: '<path d="M9 4h6v5l3 3v2H6v-2l3-3z"/><line x1="12" y1="14" x2="12" y2="20"/>',
  alert: '<path d="M12 4 3 19.5h18z"/><line x1="12" y1="10.5" x2="12" y2="14.5"/><line x1="12" y1="16.8" x2="12" y2="17"/>',
  clock: '<circle cx="12" cy="12" r="8.5"/><polyline points="12 7.5 12 12 15.5 14"/>',
  calendar: '<rect x="3" y="5" width="18" height="16" rx="3"/><line x1="3" y1="9.5" x2="21" y2="9.5"/><line x1="8" y1="3" x2="8" y2="7"/><line x1="16" y1="3" x2="16" y2="7"/>',
  people: '<circle cx="9" cy="8" r="3.2"/><path d="M3.5 19a5.5 5.5 0 0 1 11 0"/><circle cx="17" cy="9" r="2.6"/><path d="M15.5 14.2A4.6 4.6 0 0 1 21 18.5"/>',
  list: '<line x1="9" y1="7" x2="20" y2="7"/><line x1="9" y1="12" x2="20" y2="12"/><line x1="9" y1="17" x2="20" y2="17"/><circle cx="4.5" cy="7" r="1"/><circle cx="4.5" cy="12" r="1"/><circle cx="4.5" cy="17" r="1"/>',
};
function icon(name, cls = 'ic') {
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  s.setAttribute('viewBox', '0 0 24 24');
  s.setAttribute('aria-hidden', 'true');
  s.setAttribute('class', cls);
  s.innerHTML = ICONS[name];
  return s;
}
function chip(iconName, text) {
  const span = el('span', 'chipflex');
  span.append(icon(iconName));
  if (text) span.append(document.createTextNode(text));
  return span;
}

// Progress ring (Apple Fitness style) for subtask progress
function ringChip(done, total) {
  const span = el('span', 'chipflex');
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('class', 'ring');
  svg.setAttribute('aria-hidden', 'true');
  const C = 2 * Math.PI * 6.5;
  const bg = document.createElementNS(NS, 'circle');
  bg.setAttribute('cx', '8'); bg.setAttribute('cy', '8'); bg.setAttribute('r', '6.5');
  bg.setAttribute('class', 'ringbg');
  const fg = document.createElementNS(NS, 'circle');
  fg.setAttribute('cx', '8'); fg.setAttribute('cy', '8'); fg.setAttribute('r', '6.5');
  fg.setAttribute('class', 'ringfg');
  fg.setAttribute('stroke-dasharray', String(C));
  fg.setAttribute('stroke-dashoffset', String(C * (1 - (total ? done / total : 0))));
  fg.setAttribute('transform', 'rotate(-90 8 8)');
  svg.append(bg, fg);
  span.append(svg, document.createTextNode(`${done}/${total}`));
  return span;
}

// Subtle pop sound on completion (WebAudio, no asset file; can be turned off in Settings)
let audioCtx = null;
function playPop() {
  if (localStorage.getItem('soundOff') === '1') return;
  try {
    audioCtx = audioCtx || new (window.AudioContext || window.webkitAudioContext)();
    if (audioCtx.state === 'suspended') audioCtx.resume();
    const t = audioCtx.currentTime;
    const o = audioCtx.createOscillator();
    const g = audioCtx.createGain();
    o.type = 'sine';
    o.frequency.setValueAtTime(620, t);
    o.frequency.exponentialRampToValueAtTime(960, t + 0.09);
    g.gain.setValueAtTime(0.16, t);
    g.gain.exponentialRampToValueAtTime(0.0001, t + 0.17);
    o.connect(g).connect(audioCtx.destination);
    o.start(t);
    o.stop(t + 0.19);
  } catch {}
}

// Confetti when the last open task gets checked off (max once per day)
let prevOpenTotal = null;
function confetti() {
  if (!motionOK) return;
  const c = document.createElement('canvas');
  c.className = 'confetti';
  c.width = innerWidth * devicePixelRatio;
  c.height = innerHeight * devicePixelRatio;
  document.body.append(c);
  const ctx = c.getContext('2d');
  ctx.scale(devicePixelRatio, devicePixelRatio);
  const colors = ['#30d158', '#0a84ff', '#ffd60a', '#ff453a', '#bf5af2', '#ff9f0a'];
  const parts = Array.from({ length: 90 }, () => ({
    x: innerWidth / 2 + (Math.random() - 0.5) * innerWidth * 0.6,
    y: -20 - Math.random() * 50,
    vx: (Math.random() - 0.5) * 3.2,
    vy: 2 + Math.random() * 2.5,
    s: 5 + Math.random() * 5,
    r: Math.random() * Math.PI,
    vr: (Math.random() - 0.5) * 0.3,
    col: colors[Math.floor(Math.random() * colors.length)],
  }));
  const t0 = performance.now();
  requestAnimationFrame(function frame(now) {
    ctx.clearRect(0, 0, innerWidth, innerHeight);
    for (const p of parts) {
      p.x += p.vx;
      p.y += p.vy;
      p.vy += 0.05;
      p.r += p.vr;
      ctx.save();
      ctx.translate(p.x, p.y);
      ctx.rotate(p.r);
      ctx.fillStyle = p.col;
      ctx.fillRect(-p.s / 2, -p.s / 2, p.s, p.s * 0.62);
      ctx.restore();
    }
    if (now - t0 < 2300) requestAnimationFrame(frame);
    else c.remove();
  });
}
function maybeConfetti(openTotal) {
  if (prevOpenTotal !== null && prevOpenTotal > 0 && openTotal === 0 &&
      localStorage.getItem('confettiDay') !== TODAY) {
    localStorage.setItem('confettiDay', TODAY);
    confetti();
  }
  prevOpenTotal = openTotal;
}

// Smooth transitions: re-renders run as a View Transition (where supported),
// so tasks glide animated to their new spot instead of jumping abruptly.
function rerender() {
  // Don't re-render during a drag — the dragged row would otherwise no longer
  // be in the DOM, and the drop would save the wrong order.
  if (dragActive) return; // finish() reloads fresh afterwards anyway
  // Render without a View Transition while a sheet is open: the root transition
  // snapshots the whole page and would flash the list visibly behind the
  // backdrop (e.g. on every image upload via an SSE event).
  if (document.startViewTransition && motionOK && !document.querySelector('dialog[open]')) {
    document.startViewTransition(() => render());
  } else {
    render();
  }
  // Update the open sheet too — otherwise a subtask deleted inside it would
  // only disappear from its list once the undo window expires.
  if (editId !== null && editDlg.open) refreshEditLists();
}

// In-app confirmation dialog (iOS action-sheet style)
function confirmDlg(message, okLabel = t('common.delete')) {
  return new Promise((resolve) => {
    const d = $('#confirm-dialog');
    $('#confirm-text').textContent = message;
    const okBtn = $('#confirm-ok');
    okBtn.textContent = okLabel;
    let result = false;
    const onOk = () => { result = true; d.close(); };
    okBtn.addEventListener('click', onOk, { once: true });
    d.addEventListener('close', () => {
      okBtn.removeEventListener('click', onOk);
      resolve(result);
    }, { once: true });
    d.showModal();
  });
}
$('#confirm-cancel').addEventListener('click', () => $('#confirm-dialog').close());
$('#confirm-dialog').addEventListener('click', (e) => {
  if (e.target === $('#confirm-dialog')) $('#confirm-dialog').close();
});

// --- Scroll lock while a dialog is open ---
// showModal() doesn't hold the page behind it in place. Instead of hooking
// into each of the four open/close call sites, we watch the open attribute —
// that also covers Esc and stacked dialogs (a confirmation inside the sheet).
// lockedScrollY is also read in the scroll listener above, so don't rename it
// here without updating that too.
function syncScrollLock() {
  const anyOpen = !!document.querySelector('dialog[open]');
  if (anyOpen && lockedScrollY === null) {
    lockedScrollY = window.scrollY;
    document.body.style.top = `-${lockedScrollY}px`;
    document.body.classList.add('scroll-locked');
  } else if (!anyOpen && lockedScrollY !== null) {
    const y = lockedScrollY;
    lockedScrollY = null;
    document.body.classList.remove('scroll-locked');
    document.body.style.top = '';
    window.scrollTo(0, y);
  }
}
const dialogObserver = new MutationObserver(() => { syncScrollLock(); rehostToast(); });
for (const d of document.querySelectorAll('dialog')) {
  dialogObserver.observe(d, { attributes: true, attributeFilter: ['open'] });
}

const canPopover = typeof HTMLElement.prototype.showPopover === 'function';

// The toast lives in the topmost open dialog (see toast()). If that dialog
// closes, or a new one opens above it, the toast moves — otherwise it would
// vanish along with the dialog (display:none is inherited into the top layer
// too), taking its undo action with it.
function rehostToast() {
  const t = $('#toast');
  if (!canPopover || t.hidden) return;
  const host = [...document.querySelectorAll('dialog[open]')].pop() || document.body;
  if (t.parentElement === host) return;
  if (t.matches(':popover-open')) t.hidePopover();
  host.append(t);
  t.showPopover();
}

function toast(msg, action) {
  const t = $('#toast');
  t.replaceChildren(document.createTextNode(msg));
  if (action) {
    const btn = el('button', 'toastbtn', action.label);
    btn.addEventListener('click', () => { hideToast(); action.fn(); });
    t.append(btn);
  }
  if (canPopover) {
    // A modal dialog sits in the top layer and makes everything outside it
    // inert: a regular toast would be invisible behind it ("title missing" in
    // the sheet) and its undo button unclickable. As a popover in the topmost
    // open dialog, it sits above and stays usable.
    const host = [...document.querySelectorAll('dialog[open]')].pop() || document.body;
    if (t.matches(':popover-open')) t.hidePopover();
    if (t.parentElement !== host) host.append(t);
    t.hidden = false;
    t.showPopover();
  } else {
    t.hidden = false;
  }
  clearTimeout(toast._timer);
  toast._timer = setTimeout(hideToast, action ? 6000 : 3500);
}
function hideToast() {
  const t = $('#toast');
  if (canPopover && t.matches(':popover-open')) t.hidePopover();
  t.hidden = true;
}

// --- Search ---

function matchesSearch(t) {
  if (!searchTerm) return true;
  const q = searchTerm.toLowerCase();
  return t.title.toLowerCase().includes(q) || (t.note || '').toLowerCase().includes(q);
}
function visibleTop(t) {
  return matchesSearch(t) || subsOf(t.id).some(matchesSearch);
}

// --- List rendering ---

function metaFor(task) {
  const meta = el('div', 'meta');
  if (task.due_date) {
    let txt = fmtDate(task.due_date);
    if (task.due_time) txt += ` · ${fmtTime(task.due_time)}`;
    meta.append(el('span', task.overdue ? 'overdue' : '', txt));
  }
  if (task.recurrence) meta.append(chip('repeat', t(REC_KEYS[task.recurrence])));
  // In "All": name the list, except for the default list and subtasks
  const home = listById(task.list_id);
  if (currentList === 'all' && home && !home.is_default && !task.parent_id) meta.append(chip('list', listLabelFull(home)));
  const subs = subsOf(task.id);
  if (subs.length) meta.append(ringChip(subs.filter((s) => s.done).length, subs.length));
  if (task.links.length) meta.append(chip('link', String(task.links.length)));
  return meta;
}

function openLightbox(url) {
  $('#lightbox-img').src = url;
  $('#lightbox').showModal();
}

// Drag and drop via a handle: move a row within its list,
// the new order is persisted on release.
function attachDrag(handle, li) {
  handle.addEventListener('pointerdown', (e) => {
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    if (dragActive) return; // a second finger doesn't start a second drag
    e.preventDefault();
    const ul = li.parentElement;
    // Capture is nice-to-have (touch-action already suppresses touch scrolling);
    // the listeners are on document so the drag never "drops"
    try { handle.setPointerCapture(e.pointerId); } catch {}
    dragActive = true;
    li.classList.add('dragging');
    document.body.classList.add('dragging-on');
    navigator.vibrate?.(8);

    let lastY = e.clientY;
    const flips = new Map(); // node -> running FLIP animation (to cancel it)

    // FLIP: displaced neighbors glide to their new spot instead of jumping.
    // getBoundingClientRect returns the *visible* position (including
    // transform), so cancel all running animations first, then re-measure.
    const snapshot = () => new Map([...ul.children].map((c) => [c, c.getBoundingClientRect().top]));
    const playFlip = (before) => {
      if (!motionOK) return;
      for (const a of flips.values()) a.cancel();
      flips.clear();
      for (const [node, top] of before) {
        if (node === li || !node.isConnected) continue;
        const dy = top - node.getBoundingClientRect().top;
        if (!dy) continue;
        const anim = node.animate(
          [{ transform: `translateY(${dy}px)` }, { transform: 'translateY(0)' }],
          { duration: 200, easing: 'cubic-bezier(0.2, 0, 0, 1)' },
        );
        flips.set(node, anim);
        anim.finished.then(() => flips.delete(node), () => {});
      }
    };

    const reorder = (y) => {
      let target = null;
      for (const sib of ul.children) {
        if (sib === li) continue;
        const r = sib.getBoundingClientRect();
        if (y < r.top + r.height / 2) { target = sib; break; }
      }
      if (target === li || target === li.nextSibling) return;
      const before = snapshot();
      ul.insertBefore(li, target);
      playFlip(before);
    };

    // Auto-scroll: near the top/bottom edge, the page scrolls along so you
    // can drag a row from the very bottom to the very top in one motion.
    const EDGE = 110;  // edge zone in px
    const SPEED = 16;  // max px per frame
    let raf = requestAnimationFrame(function step() {
      raf = requestAnimationFrame(step);
      const overTop = lastY - EDGE;
      const overBottom = lastY - (innerHeight - EDGE);
      let v = 0;
      if (overTop < 0) v = Math.max(-SPEED, (overTop / EDGE) * SPEED);
      else if (overBottom > 0) v = Math.min(SPEED, (overBottom / EDGE) * SPEED);
      if (!v) return;
      const before = scrollY;
      scrollBy(0, v);
      if (scrollY !== before) reorder(lastY); // the list moved under the finger
    });

    const onMove = (ev) => {
      if (ev.pointerId !== e.pointerId) return; // a different finger
      ev.preventDefault();
      lastY = ev.clientY;
      reorder(lastY);
    };
    const finish = async (ev) => {
      if (ev.pointerId !== e.pointerId) return; // a second finger doesn't end the drag
      document.removeEventListener('pointermove', onMove);
      document.removeEventListener('pointerup', finish);
      document.removeEventListener('pointercancel', finish);
      cancelAnimationFrame(raf);
      for (const a of flips.values()) a.cancel();
      li.classList.remove('dragging');
      document.body.classList.remove('dragging-on');
      dragActive = false;
      const ids = [...ul.children].map((c) => Number(c.dataset.id)).filter(Boolean);
      try {
        await api('/api/tasks/reorder', { method: 'PUT', body: JSON.stringify({ ids }) });
      } catch {}
      // The rows were reordered locally — always redraw, even if the server
      // responds unchanged (e.g. because it rejected the new order).
      lastRendered = '';
      loadTasks();
    };
    document.addEventListener('pointermove', onMove);
    document.addEventListener('pointerup', finish);
    document.addEventListener('pointercancel', finish);
  });
}

function taskRow(task, isSub, prio) {
  const li = el('li', 'task' + (task.done ? ' done' : '') + (isSub ? ' subtask' : ''));
  li.dataset.id = String(task.id);
  li.style.viewTransitionName = 'task-' + task.id;
  const row = el('div', 'rowline');

  // Priority number: counted top to bottom within each section
  if (prio) row.append(el('span', 'prio', String(prio)));

  // Move handle to the left of the task. Not during a search: rows are
  // missing then, and saving the order of the visible ones would scramble
  // the hidden ones.
  if (!task.done && !searchTerm) {
    const drag = el('button', 'rowbtn draghandle');
    drag.append(icon('handle'));
    drag.setAttribute('aria-label', t('task.move', { title: task.title }));
    attachDrag(drag, li);
    row.append(drag);
  }

  const check = el('button', 'check' + prioClass(task));
  check.append(icon('check'));
  check.setAttribute('aria-label', withPrio(task.done ? t('task.reopen', { title: task.title }) : t('task.complete', { title: task.title }), task));
  check.addEventListener('click', () => toggleTask(task, li));

  const body = el('div', 'body');
  body.append(el('div', 'title', task.title));
  // Note sits directly under the todo
  if (task.note) body.append(el('div', 'notepreview', task.note));
  // Images directly in the row, tapping opens the lightbox
  if (task.attachments.length) {
    const strip = el('div', 'rowthumbs');
    for (const aid of task.attachments) {
      const img = document.createElement('img');
      img.src = `/api/tasks/${task.id}/attachments/${aid}/thumb`; // small preview, full size only in the lightbox
      img.alt = t('image.alt');
      img.loading = 'lazy';
      img.addEventListener('click', (e) => {
        e.stopPropagation(); // don't also open the edit sheet
        openLightbox(`/api/tasks/${task.id}/attachments/${aid}`);
      });
      strip.append(img);
    }
    body.append(strip);
  }
  const meta = metaFor(task);
  if (meta.childNodes.length) body.append(meta);
  body.addEventListener('click', () => openEdit(task.id));

  row.append(check, body);

  // "In progress" toggle (top-level tasks only)
  if (!task.done && !isSub && !task.parent_id) {
    const prog = el('button', 'rowbtn progressbtn' + (task.in_progress ? ' active' : ''));
    prog.append(icon(task.in_progress ? 'pause' : 'play'));
    prog.setAttribute('aria-label', task.in_progress ? t('task.progress_off', { title: task.title }) : t('task.progress_on', { title: task.title }));
    prog.title = task.in_progress ? t('task.progress_stop') : t('section.in_progress');
    prog.addEventListener('click', async () => {
      if (inflight.has(task.id)) return;
      inflight.add(task.id);
      try {
        const res = await api(`/api/tasks/${task.id}/progress`, { method: 'POST', body: JSON.stringify({ on: !task.in_progress }) });
        if (await failed(res)) return;
      } catch {
        return;
      } finally {
        inflight.delete(task.id);
      }
      loadTasks();
    });
    row.append(prog);
  }

  if (!task.done) {
    const snoozeBtn = el('button', 'rowbtn');
    snoozeBtn.append(icon('forward'));
    snoozeBtn.setAttribute('aria-label', t('task.snooze', { title: task.title }));
    snoozeBtn.title = t('snooze.day');
    snoozeBtn.addEventListener('click', () => snooze(task, 1));
    row.append(snoozeBtn);
  }
  const del = el('button', 'rowbtn del');
  del.append(icon('xmark'));
  del.setAttribute('aria-label', t('task.delete', { title: task.title }));
  del.addEventListener('click', () => deleteTask(task));
  row.append(del);

  li.append(row);

  if (!isSub) {
    const subs = subsOf(task.id).filter((s) => !searchTerm || matchesSearch(s) || matchesSearch(task));
    if (subs.length) {
      const ul = el('ul', 'subs');
      subs.sort((a, b) => (a.done - b.done) || (a.sort_order - b.sort_order) || (a.id - b.id));
      subs.forEach((s) => ul.append(taskRow(s, true)));
      li.append(ul);
    }
  }
  return li;
}

// Delete without confirmation, but with undo: the task disappears from the
// view immediately, and the DELETE request only goes out once the undo
// window has expired.
const hiddenIds = new Set();
let pendingDelete = null; // { taskId, ids, timer }

async function commitPendingDelete() {
  if (!pendingDelete) return;
  const p = pendingDelete;
  pendingDelete = null;
  clearTimeout(p.timer);
  let res;
  try {
    // keepalive: the request still goes out even if the page is being
    // switched away from or closed right now (see pagehide/visibilitychange)
    res = await api(`/api/tasks/${p.taskId}`, { method: 'DELETE', keepalive: true });
  } catch {
    // Network error: show the task again, nothing was lost
    p.ids.forEach((id) => hiddenIds.delete(id));
    rerender();
    return;
  }
  p.ids.forEach((id) => hiddenIds.delete(id));
  if (!res.ok && res.status !== 404) {
    // Server rejects it: show the task again. Redraw it ourselves — loadTasks
    // would see unchanged data and leave the list (without the task) as is.
    toast(t('error.delete_failed'));
    rerender();
  }
  loadTasks();
}

function deleteTask(task) {
  if (hiddenIds.has(task.id)) return;
  commitPendingDelete(); // only one undo window at a time
  const ids = [task.id, ...TASKS.filter((s) => s.parent_id === task.id).map((s) => s.id)];
  ids.forEach((id) => hiddenIds.add(id));
  pendingDelete = { taskId: task.id, ids, timer: setTimeout(commitPendingDelete, 6000) };
  if (editId !== null && ids.includes(editId)) $('#edit').close();
  rerender();
  toast(t('task.deleted', { title: task.title }), {
    label: t('common.undo'),
    fn: () => {
      if (!pendingDelete || pendingDelete.taskId !== task.id) return;
      clearTimeout(pendingDelete.timer);
      pendingDelete.ids.forEach((id) => hiddenIds.delete(id));
      pendingDelete = null;
      rerender();
    },
  });
}
// Send any pending delete immediately on leaving — already when switching
// away, not just on unload. A frozen page (background tab, iOS PWA) no longer
// gets a pagehide event once the browser later discards it; the delete would
// then be lost. So the undo window ends as soon as you switch away.
window.addEventListener('pagehide', commitPendingDelete);

async function snooze(task, days) {
  if (inflight.has(task.id)) return;
  inflight.add(task.id);
  let updated;
  try {
    const res = await api(`/api/tasks/${task.id}/snooze`, { method: 'POST', body: JSON.stringify({ days }) });
    if (await failed(res)) return;
    updated = await res.json();
    toast(t('task.snoozed', { date: fmtDate(updated.due_date) }));
  } catch {
    return;
  } finally {
    inflight.delete(task.id);
  }
  // In the open sheet, only update what the snooze actually changes — title
  // and note may be edited there right now, unsaved. Use the values from the
  // response, not from TASKS: a concurrent SSE reload can leave TASKS on the
  // old state, and saving would write the old date back.
  if (editId === task.id) {
    // What the snooze already saved is not a pending change: only update
    // these fields in the snapshot, leave the rest.
    const snap = JSON.parse(editSnapshot);
    $('#edit-date').value = updated.due_date || '';
    setProgress(updated.in_progress);
    syncPickers();
    snap[2] = $('#edit-date').value;
    snap[7] = editProgress;
    editSnapshot = JSON.stringify(snap);
  }
  loadTasks();
}

async function toggleTask(task, li) {
  if (inflight.has(task.id)) return;
  if (!task.done && !task.recurrence) {
    const openSubs = subsOf(task.id).filter((s) => !s.done).length;
    if (openSubs > 0 && !(await confirmDlg(t('confirm.complete_subs', { n: openSubs }), t('confirm.complete')))) return;
  }
  if (inflight.has(task.id)) return;
  inflight.add(task.id);
  try {
    if (!task.done) {
      li.classList.add('checking');
      navigator.vibrate?.(10); // brief haptic feedback (Android)
      playPop();
      // expected_due_date: protects recurring tasks server-side against a
      // double complete (a retry after a lost response would otherwise skip an occurrence)
      const opts = { method: 'POST' };
      if (task.recurrence && task.due_date) opts.body = JSON.stringify({ expected_due_date: task.due_date });
      const res = await api(`/api/tasks/${task.id}/complete`, opts);
      const updated = await res.json().catch(() => ({}));
      if (!res.ok) {
        li.classList.remove('checking');
        toast(updated.error || t('task.complete_failed'));
        return;
      }
      if (task.recurrence && updated.due_date) toast(t('task.next', { date: fmtDate(updated.due_date) }));
      if (motionOK) await delay(320);
    } else {
      const res = await api(`/api/tasks/${task.id}/uncomplete`, { method: 'POST' });
      if (!res.ok) { toast(t('task.reopen_failed')); return; }
    }
  } catch {
    li.classList.remove('checking');
    return;
  } finally {
    inflight.delete(task.id);
  }
  loadTasks();
}

function render() {
  const top = TASKS.filter((x) => !x.parent_id && !hiddenIds.has(x.id) && inView(x));
  const allOpen = TASKS.filter((x) => !x.done && !hiddenIds.has(x.id) && inView(x)).length;
  renderHeader(allOpen);

  const allOpenTop = top.filter((t) => !t.done && visibleTop(t));
  const inProg = allOpenTop.filter((t) => t.in_progress);
  const open = allOpenTop.filter((t) => !t.in_progress);
  const done = top.filter((t) => t.done && visibleTop(t));
  const groups = [
    { label: t('section.in_progress'), cls: 'inprogress', icon: 'play', items: inProg },
    { label: t('section.pinned'), cls: 'pinned', icon: 'pin', items: open.filter((t) => t.pinned) },
    { label: t('section.overdue'), cls: 'overdue', icon: 'alert', items: open.filter((t) => !t.pinned && t.overdue) },
    { label: t('section.today'), cls: '', icon: 'clock', items: open.filter((t) => !t.pinned && !t.overdue && t.due_date === TODAY) },
    { label: t('section.upcoming'), cls: '', icon: 'calendar', items: open.filter((t) => !t.pinned && !t.overdue && t.due_date && t.due_date > TODAY) },
    { label: t('section.someday'), cls: '', icon: 'inbox', items: open.filter((t) => !t.pinned && !t.due_date) },
  ];

  const list = $('#list');
  list.replaceChildren();

  const emptyState = (name, text) => {
    const empty = el('div', 'empty');
    const big = el('div', 'big');
    big.append(icon(name, 'ic bigic'));
    empty.append(big, el('div', '', text));
    list.append(empty);
  };
  if (searchTerm && allOpenTop.length === 0 && done.length === 0) {
    emptyState('search', t('empty.search'));
    return;
  }
  if (!searchTerm && !TASKS.some(inView)) {
    emptyState('inbox', t('empty.none'));
    return;
  }
  if (!searchTerm && allOpen === 0) emptyState('checkcircle', t('empty.all_done'));

  for (const g of groups) {
    if (!g.items.length) continue;
    const sec = el('section', g.cls);
    const h = el('h2');
    h.append(icon(g.icon), document.createTextNode(g.label));
    h.append(el('span', 'n', String(g.items.length)));
    const ul = el('ul');
    // Each section counts from 1 — "In progress" has its own numbering
    g.items.forEach((t, i) => ul.append(taskRow(t, false, i + 1)));
    sec.append(h, ul);
    list.append(sec);
  }

  if (done.length) {
    const det = el('details');
    det.open = doneOpen || !!searchTerm;
    // Setting it above triggers an (asynchronous) toggle event itself — during
    // a search that must not stick as "the user expanded it".
    det.addEventListener('toggle', () => { if (!searchTerm) doneOpen = det.open; });
    const sum = el('summary');
    sum.append(icon('checkcircle'), document.createTextNode(` ${t('section.done', { n: done.length })}`));
    det.append(sum);
    const ul = el('ul');
    done.forEach((t) => ul.append(taskRow(t, false)));
    det.append(ul);
    list.append(det);
  }
}

let lastRendered = ''; // the response that was actually rendered last

async function loadTasks() {
  const seq = ++loadSeq;
  try {
    const res = await api('/api/tasks');
    if (await failed(res, t('task.load_failed'))) return;
    const raw = await res.text();
    if (seq !== loadSeq) return; // a newer request is already running / already finished
    // Unchanged (e.g. the SSE echo of our own change): don't redraw. Every
    // redraw is a View Transition, and the browser swallows taps during one —
    // checking off several tasks quickly would lose them. Except the header:
    // its greeting depends on the time of day, not on the data.
    if (raw === lastRendered) {
      renderHeader(TASKS.filter((x) => !x.done && !hiddenIds.has(x.id) && inView(x)).length);
      return;
    }
    const data = JSON.parse(raw);
    TODAY = data.today;
    TOMORROW = addDays(TODAY, 1);
    TASKS = data.tasks;
    LISTS = data.lists;
    // The selected list no longer exists (another device deleted it)
    if (currentList !== 'all' && !listById(currentList)) {
      currentList = 'all';
      try { localStorage.setItem('list', 'all'); } catch {}
    }
    renderChips();
    if (!dragActive) lastRendered = raw; // rerender doesn't redraw during a drag
    rerender();
    maybeConfetti(TASKS.filter((x) => !x.done && inView(x)).length);
    openPendingTask();
  } catch {
    /* the offline banner is already showing, the last view stays as is */
  }
}

// --- Edit sheet ---

const editDlg = $('#edit');

function setSegmented(value) {
  editRec = value;
  $('#edit-rec').querySelectorAll('button').forEach((b) => {
    b.classList.toggle('active', b.dataset.v === value);
    b.setAttribute('aria-checked', String(b.dataset.v === value));
  });
}
$('#edit-rec').querySelectorAll('button').forEach((b) => {
  b.addEventListener('click', () => setSegmented(b.dataset.v));
});

function setPin(value) {
  editPinned = value;
  $('#edit-pin').setAttribute('aria-checked', String(value));
  $('#edit-pin').classList.toggle('on', value);
}
$('#edit-pin').addEventListener('click', () => setPin(!editPinned));

let editProgress = false;
function setProgress(value) {
  editProgress = value;
  $('#edit-progress').setAttribute('aria-checked', String(value));
  $('#edit-progress').classList.toggle('on', value);
}
$('#edit-progress').addEventListener('click', () => setProgress(!editProgress));

let editPrio = 0;
function setPrio(value) {
  editPrio = value;
  $('#edit-prio').querySelectorAll('button').forEach((b) => {
    const on = Number(b.dataset.v) === value;
    b.classList.toggle('active', on);
    b.setAttribute('aria-checked', String(on));
  });
}
$('#edit-prio').querySelectorAll('button').forEach((b) => {
  b.addEventListener('click', () => setPrio(Number(b.dataset.v)));
});

function fillEditForm(t) {
  $('#edit-title').value = t.title;
  $('#edit-note').value = t.note || '';
  $('#edit-date').value = t.due_date || '';
  $('#edit-time').value = t.due_time || '';
  setSegmented(t.recurrence || '');
  setPin(t.pinned);
  setProgress(t.in_progress);
  setPrio(t.priority || 0);
  fillListSelect(t);
  syncPickers();
}

// taskOption: entry for a task picker, long titles truncated.
function taskOption(c) {
  const o = el('option', '', c.title.length > 40 ? c.title.slice(0, 40) + '…' : c.title);
  o.value = String(c.id);
  return o;
}

function fillListSelect(task) {
  const sel = $('#edit-list');
  sel.replaceChildren(...LISTS.map((l) => {
    const o = el('option', '', listLabelFull(l));
    o.value = String(l.id);
    return o;
  }));
  sel.value = String(task.list_id);
}

function fillParentSelect(task) {
  const sel = $('#edit-parent');
  sel.replaceChildren();
  const none = el('option', '', t('edit.parent_none'));
  none.value = '';
  sel.append(none);
  const iHaveSubs = subsOf(task.id).length > 0;
  for (const c of TASKS) {
    // Completed tasks drop out as a target — except the current parent task:
    // if it were missing, the selection would fall back to "none", and saving
    // would silently detach the subtask from its (completed) checklist.
    if (c.id === task.id || c.parent_id || (c.done && c.id !== task.parent_id)) continue;
    sel.append(taskOption(c));
  }
  sel.value = task.parent_id ? String(task.parent_id) : '';
  sel.disabled = iHaveSubs;
  sel.title = iHaveSubs ? t('error.parent_has_subs') : '';
  applyParentConstraints();
}

// Subtasks can't recur and can't be pinned
function applyParentConstraints() {
  const isSub = $('#edit-parent').value !== '';
  $('#rec-wrap').classList.toggle('disabled', isSub);
  $('#pin-row').classList.toggle('disabled', isSub);
  $('#progress-row').classList.toggle('disabled', isSub);
  $('#list-row').hidden = isSub;
  if (isSub) { setSegmented(''); setPin(false); setProgress(false); }
  $('#subs-wrap').hidden = isSub;
}
$('#edit-parent').addEventListener('change', applyParentConstraints);

// Images: resize client-side to a max of 2048px JPEG before upload
async function compressImage(file) {
  // Upload GIFs unchanged: the canvas would only see the first frame, turning
  // an animation into a still image (createImageBitmap doesn't throw for this).
  if (file.type === 'image/gif') return file;
  try {
    const bmp = await createImageBitmap(file);
    // Keep the original if it's small — in bytes AND in pixels: a low-detail
    // PNG (a scan, a long screenshot) can easily have more pixels at 200 KB
    // than the server allows for (maxImagePixels).
    if (file.size < 300 * 1024 && Math.max(bmp.width, bmp.height) <= 2048) {
      bmp.close();
      return file;
    }
    const scale = Math.min(1, 2048 / Math.max(bmp.width, bmp.height));
    const canvas = document.createElement('canvas');
    canvas.width = Math.round(bmp.width * scale);
    canvas.height = Math.round(bmp.height * scale);
    const ctx = canvas.getContext('2d');
    // JPEG has no transparency — transparent areas would turn black. The
    // color matches the server's thumbnails (makeThumb).
    ctx.fillStyle = '#1c1c1e';
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.drawImage(bmp, 0, 0, canvas.width, canvas.height);
    bmp.close();
    const blob = await new Promise((r) => canvas.toBlob(r, 'image/jpeg', 0.85));
    return blob && blob.size < file.size ? blob : file;
  } catch {
    return file; // a format the browser can't decode: upload it uncompressed
  }
}

function renderAttachments(task) {
  const grid = $('#attach-grid');
  grid.replaceChildren();
  for (const aid of task.attachments) {
    const cell = el('div', 'thumb');
    const img = document.createElement('img');
    img.src = `/api/tasks/${task.id}/attachments/${aid}/thumb`;
    img.alt = t('image.alt');
    img.loading = 'lazy';
    img.addEventListener('click', () => openLightbox(`/api/tasks/${task.id}/attachments/${aid}`));
    const del = el('button', 'thumbdel');
    del.append(icon('xmark'));
    del.setAttribute('aria-label', t('image.delete'));
    del.addEventListener('click', async () => {
      if (!(await confirmDlg(t('image.delete_confirm')))) return;
      const res = await api(`/api/tasks/${task.id}/attachments/${aid}`, { method: 'DELETE' }).catch(() => null);
      if (res) await failed(res, t('image.delete_failed'));
      loadTasks();
    });
    cell.append(img, del);
    grid.append(cell);
  }
}

$('#add-attach').addEventListener('click', () => $('#attach-file').click());
// Multiple images at once: upload them one after another (there's only one
// writer in SQLite), progress shown in the toast, exactly one reload at the end.
$('#attach-file').addEventListener('change', async () => {
  const files = Array.from($('#attach-file').files);
  $('#attach-file').value = '';
  const task = byId(editId);
  if (!files.length || !task) return;

  let uploaded = 0;
  let lastError = '';
  for (const [i, file] of files.entries()) {
    toast(files.length > 1 ? t('image.uploading_n', { i: i + 1, n: files.length }) : t('image.uploading'));
    try {
      const blob = await compressImage(file);
      // Set the header explicitly: api() would otherwise append application/json
      const res = await api(`/api/tasks/${task.id}/attachments`, {
        method: 'POST', body: blob, headers: { 'Content-Type': blob.type || 'application/octet-stream' },
      });
      if (!res.ok) {
        const d = await res.json().catch(() => ({}));
        lastError = d.error || t('image.upload_failed');
        continue;
      }
      uploaded++;
    } catch {
      lastError = t('image.upload_failed');
    }
  }

  if (lastError) toast(uploaded ? t('image.partial', { done: uploaded, total: files.length, error: lastError }) : lastError);
  else toast(t('image.added', { n: uploaded }));
  if (uploaded > 0) loadTasks(); // also refreshes the open sheet
});
$('#lightbox').addEventListener('click', () => $('#lightbox').close());
$('#lightbox-close').addEventListener('click', () => $('#lightbox').close());

function refreshEditLists() {
  const task = byId(editId);
  if (!task) { editDlg.close(); return; }
  renderAttachments(task);
  // Subtasks
  const subs = subsOf(task.id);
  $('#sub-count').textContent = subs.length ? `${subs.filter((s) => s.done).length}/${subs.length}` : '';
  const ul = $('#sub-list');
  ul.replaceChildren();
  for (const s of subs) {
    const li = el('li', 'minirow' + (s.done ? ' done' : ''));
    const check = el('button', 'check small' + prioClass(s));
    check.append(icon('check'));
    check.setAttribute('aria-label', withPrio(s.done ? t('task.reopen', { title: s.title }) : t('task.complete', { title: s.title }), s));
    check.addEventListener('click', async () => {
      const res = await api(`/api/tasks/${s.id}/${s.done ? 'uncomplete' : 'complete'}`, { method: 'POST' }).catch(() => null);
      if (res) await failed(res);
      loadTasks();
    });
    const title = el('div', 'title', s.title);
    title.addEventListener('click', () => switchEdit(s.id));
    const del = el('button', 'rowbtn del');
    del.append(icon('xmark'));
    del.setAttribute('aria-label', t('task.delete', { title: s.title }));
    del.addEventListener('click', () => deleteTask(s));
    li.append(check, title, del);
    ul.append(li);
  }
  // Links
  const linkUl = $('#link-list');
  linkUl.replaceChildren();
  for (const lid of task.links) {
    const other = byId(lid);
    if (!other) continue;
    const li = el('li', 'minirow');
    const title = el('div', 'title' + (other.done ? ' struck' : ''), other.title);
    title.addEventListener('click', () => switchEdit(other.id));
    const del = el('button', 'rowbtn del');
    del.append(icon('xmark'));
    del.setAttribute('aria-label', t('link.remove', { title: other.title }));
    del.addEventListener('click', async () => {
      const res = await api(`/api/tasks/${task.id}/links/${lid}`, { method: 'DELETE' }).catch(() => null);
      if (res) await failed(res, t('link.remove_failed'));
      loadTasks();
    });
    const mark = el('span', 'linkmark');
    mark.append(icon('link'));
    li.append(mark, title, del);
    linkUl.append(li);
  }
  const sel = $('#link-select');
  const chosen = sel.value; // a background refresh (SSE) shouldn't discard the selection
  sel.replaceChildren();
  const ph = el('option', '', t('link.choose'));
  ph.value = '';
  sel.append(ph);
  for (const c of TASKS) {
    if (c.id === task.id || task.links.includes(c.id) || c.done) continue;
    sel.append(taskOption(c));
  }
  if ([...sel.options].some((o) => o.value === chosen)) sel.value = chosen;
}

// editState: what "Save" would currently send. editSnapshot holds the state
// right after opening — the form is compared against the form, not against
// the raw data (textareas normalize line endings, subtasks reset pin/
// recurrence; either would otherwise register as a false "change").
let editSnapshot = '';
function editState() {
  return JSON.stringify([
    $('#edit-title').value.trim(), $('#edit-note').value.trim(),
    $('#edit-date').value, $('#edit-time').value, $('#edit-parent').value,
    editRec, editPinned, editProgress, editPrio, $('#edit-list').value,
  ]);
}

function editDirty() {
  return editId !== null && editState() !== editSnapshot;
}

// switchEdit switches the open sheet to a different task (subtask, link) —
// with a confirmation prompt instead of silently discarding input.
async function switchEdit(id) {
  if (editDirty() && !(await confirmDlg(t('edit.discard_confirm'), t('edit.discard')))) return;
  openEdit(id);
}

function openEdit(id) {
  editId = id;
  const t = byId(id);
  if (!t) return;
  fillEditForm(t);
  fillParentSelect(t);
  refreshEditLists();
  editSnapshot = editState();
  if (!editDlg.open) editDlg.showModal();
}

$('#edit-cancel').addEventListener('click', () => editDlg.close());
// Deliberately NO closing via a backdrop click: an accidental tap nearby
// would otherwise discard unsaved input. Only "Save" or "Cancel" close
// the sheet.
editDlg.addEventListener('close', () => { editId = null; });

$('#edit-save').addEventListener('click', async () => {
  const task = byId(editId);
  if (!task) return;
  const parentVal = $('#edit-parent').value;
  const body = {
    title: $('#edit-title').value.trim(),
    note: $('#edit-note').value.trim(),
    due_date: $('#edit-date').value,
    due_time: $('#edit-time').value,
    recurrence: parentVal ? '' : editRec,
    pinned: parentVal ? false : editPinned,
    in_progress: parentVal ? false : editProgress,
    priority: editPrio,
    // Subtasks follow their parent task — so don't send a list in that case.
    list_id: parentVal ? undefined : Number($('#edit-list').value),
    parent_id: parentVal ? Number(parentVal) : null,
  };
  try {
    const res = await api(`/api/tasks/${task.id}`, { method: 'PUT', body: JSON.stringify(body) });
    if (await failed(res, t('task.save_failed'))) return;
  } catch {
    return;
  }
  editDlg.close();
  toast(t('task.saved'));
  loadTasks();
});

$('#edit-delete').addEventListener('click', () => {
  const t = byId(editId);
  if (t) deleteTask(t);
});
$('#snooze-1').addEventListener('click', () => { const t = byId(editId); if (t) snooze(t, 1); });
$('#snooze-7').addEventListener('click', () => { const t = byId(editId); if (t) snooze(t, 7); });

// busy locks a button while its request is in flight: a double tap would
// otherwise create everything twice (or overwrite the running passkey ceremony).
async function busy(btn, fn) {
  if (btn.disabled) return;
  btn.disabled = true;
  try {
    return await fn();
  } finally {
    btn.disabled = false;
  }
}

$('#add-sub').addEventListener('click', () => busy($('#add-sub'), async () => {
  const title = $('#new-sub').value.trim();
  const t = byId(editId);
  if (!title || !t) return;
  try {
    const res = await api('/api/tasks', { method: 'POST', body: JSON.stringify({ title, parent_id: t.id }) });
    if (await failed(res)) return;
  } catch {
    return;
  }
  $('#new-sub').value = '';
  loadTasks();
}));
$('#new-sub').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { e.preventDefault(); $('#add-sub').click(); }
});

$('#add-link').addEventListener('click', () => busy($('#add-link'), async () => {
  const other = $('#link-select').value;
  const t = byId(editId);
  if (!other || !t) return;
  try {
    const res = await api(`/api/tasks/${t.id}/links`, { method: 'POST', body: JSON.stringify({ other: Number(other) }) });
    if (await failed(res)) return;
  } catch {
    return;
  }
  $('#link-select').value = '';
  loadTasks();
}));

// --- Search ---

$('#toggle-search').addEventListener('click', () => {
  const row = $('#search-row');
  row.hidden = !row.hidden;
  if (!row.hidden) {
    $('#search').focus();
  } else {
    $('#search').value = '';
    searchTerm = '';
    rerender();
  }
});
$('#search').addEventListener('input', () => {
  searchTerm = $('#search').value.trim();
  render(); // no transition while typing, otherwise every keystroke stutters
});

// --- Lists ---

function renderChips() {
  const bar = $('#lists');
  const mk = (value, text, cls = '') => {
    const b = el('button', 'listchip' + cls, text);
    b.type = 'button';
    b.dataset.list = String(value);
    return b;
  };
  const chips = [mk('all', t('lists.all'), currentList === 'all' ? ' active' : '')];
  for (const l of LISTS) {
    const b = mk(l.id, listLabel(l), currentList === l.id ? ' active' : '');
    if (sharedList(l)) {
      b.classList.add('shared');
      b.prepend(icon('people'));
      b.setAttribute('aria-label', `${listLabel(l)} (${t('lists.shared')})`);
    }
    attachListMenu(b, l);
    chips.push(b);
  }
  const add = mk('add', '＋', ' add');
  add.setAttribute('aria-label', t('lists.new'));
  chips.push(add);
  bar.replaceChildren(...chips);
  for (const b of chips) b.setAttribute('aria-pressed', String(b.classList.contains('active')));
}

// A click right after a long press doesn't also open the list;
// each new touch (pointerdown) resets the flag.
let chipLongPress = false;
$('#lists').addEventListener('pointerdown', () => { chipLongPress = false; }, true);
$('#lists').addEventListener('click', (e) => {
  const b = e.target.closest('.listchip');
  if (!b || chipLongPress) return;
  if (b.dataset.list === 'add') { openListDialog(null); return; }
  setCurrentList(b.dataset.list === 'all' ? 'all' : Number(b.dataset.list));
});

// A long press (touch) or right-click opens rename/delete.
function attachListMenu(b, l) {
  let timer = null;
  b.addEventListener('contextmenu', (e) => { e.preventDefault(); openListDialog(l); });
  b.addEventListener('pointerdown', (e) => {
    if (e.pointerType === 'mouse') return;
    timer = setTimeout(() => { chipLongPress = true; openListDialog(l); }, 500);
  });
  for (const ev of ['pointerup', 'pointerleave', 'pointercancel']) b.addEventListener(ev, () => clearTimeout(timer));
}

let listEditing = null; // null = new list
function openListDialog(l) {
  // Android also fires contextmenu after a long press — the dialog is
  // already open by then (showModal would throw).
  if ($('#list-dialog').open) return;
  listEditing = l;
  // Another person's list: only view who's on it, and leave.
  const foreign = !!(l && l.owner_name);
  $('#list-dialog-title').textContent = foreign ? t('lists.owner', { name: l.owner_name }) : t(l ? 'lists.edit' : 'lists.new');
  $('#list-name').value = l ? listLabel(l) : '';
  $('.list-dialog-body').hidden = foreign;
  $('#list-save').hidden = foreign;
  $('#list-delete').hidden = !l || l.is_default || foreign;
  $('#list-leave').hidden = !foreign;
  fillShare(l);
  $('#list-dialog').showModal();
  if (!foreign) $('#list-name').focus();
}

// --- Sharing ---
const sharedList = (l) => !!l.owner_name || (l.members || []).length > 0;

// "Shared with" section: members (removable by the owner) and the picker
// for adding more people. The default list and new lists don't have it.
async function fillShare(l) {
  const box = $('#list-share');
  box.hidden = !l || l.is_default;
  if (box.hidden) return;
  const own = !l.owner_name;
  $('#list-members').replaceChildren(...(l.members || []).map((p) => {
    const li = el('li', 'minirow');
    const body = el('div', 'body');
    body.append(el('div', 'title', p.name));
    li.append(body);
    if (own) {
      const del = el('button', 'rowbtn del');
      del.type = 'button';
      del.append(icon('xmark'));
      del.setAttribute('aria-label', t('lists.remove_member', { name: p.name }));
      del.addEventListener('click', () => busy(del, async () => {
        const r = await api(`/api/lists/${l.id}/members/${p.id}`, { method: 'DELETE' });
        if (await failed(r, t('error.delete_failed'))) return;
        await refreshListDialog(l.id);
      }).catch(() => {}));
      li.append(del);
    }
    return li;
  }));
  $('#share-line').hidden = true;
  $('#share-none').hidden = true;
  if (!own) return;
  let people = [];
  try {
    const r = await api('/api/people');
    if (r.ok) people = await r.json();
  } catch {}
  if (listEditing !== l) return; // the dialog is meanwhile open for a different list
  const members = new Set((l.members || []).map((p) => p.id));
  const free = people.filter((p) => !members.has(p.id));
  const pick = el('option', '', t('lists.share_pick'));
  pick.value = '';
  $('#share-person').replaceChildren(pick, ...free.map((p) => {
    const o = el('option', '', p.name);
    o.value = String(p.id);
    return o;
  }));
  $('#share-line').hidden = free.length === 0;
  $('#share-none').hidden = people.length > 0;
}

async function refreshListDialog(id) {
  lastRendered = '';
  await loadTasks();
  const l = listById(id);
  if (!l) { $('#list-dialog').close(); return; }
  listEditing = l;
  await fillShare(l);
}

$('#share-add').addEventListener('click', () => busy($('#share-add'), async () => {
  const l = listEditing;
  const uid = $('#share-person').value;
  if (!l || !uid) return;
  const r = await api(`/api/lists/${l.id}/members/${uid}`, { method: 'PUT' });
  if (await failed(r, t('task.save_failed'))) return;
  await refreshListDialog(l.id);
}).catch(() => {}));

// The current account's own ID (for leaving a list) comes from /api/settings.
async function myId() {
  if (!ME) {
    const res = await api('/api/settings');
    if (!res.ok) throw new Error(t('error.internal'));
    ME = (await res.json()).me;
  }
  return ME.id;
}

$('#list-leave').addEventListener('click', async () => {
  const l = listEditing;
  if (!l) return;
  $('#list-dialog').close();
  if (!(await confirmDlg(t('lists.leave_confirm', { name: listLabel(l) }), t('lists.leave')))) return;
  try {
    const r = await api(`/api/lists/${l.id}/members/${await myId()}`, { method: 'DELETE' });
    if (await failed(r, t('error.delete_failed'))) return;
    if (currentList === l.id) setCurrentList('all');
    lastRendered = '';
    await loadTasks();
  } catch {}
});
$('#list-cancel').addEventListener('click', () => $('#list-dialog').close());
$('#list-name').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { e.preventDefault(); $('#list-save').click(); }
});

$('#list-save').addEventListener('click', () => busy($('#list-save'), async () => {
  const name = $('#list-name').value.trim();
  const l = listEditing;
  try {
    const res = await api(l ? `/api/lists/${l.id}` : '/api/lists', { method: l ? 'PUT' : 'POST', body: JSON.stringify({ name }) });
    if (await failed(res, t('task.save_failed'))) return;
    const saved = await res.json();
    $('#list-dialog').close();
    lastRendered = '';
    await loadTasks();
    if (!l) setCurrentList(saved.id);
  } catch {}
}));

// choiceDlg: action sheet with multiple options; returns the key of the
// chosen one, or null (cancel, Esc, tapped outside).
function choiceDlg(message, options, cancelLabel = t('common.cancel')) {
  return new Promise((resolve) => {
    const d = $('#choice-dialog');
    $('#choice-text').textContent = message;
    let result = null;
    const btns = options.map((o) => {
      const b = el('button', o.danger ? 'danger-text' : '', o.label);
      b.type = 'button';
      b.addEventListener('click', () => { result = o.key; d.close(); });
      return b;
    });
    const cancel = el('button', '', cancelLabel);
    cancel.type = 'button';
    cancel.addEventListener('click', () => d.close());
    $('#choice-btns').replaceChildren(...btns, cancel);
    d.addEventListener('close', () => resolve(result), { once: true });
    d.showModal();
  });
}
$('#choice-dialog').addEventListener('click', (e) => {
  if (e.target === $('#choice-dialog')) $('#choice-dialog').close();
});

$('#list-delete').addEventListener('click', async () => {
  const l = listEditing;
  if (!l) return;
  $('#list-dialog').close();
  const n = TASKS.filter((x) => x.list_id === l.id).length;
  let mode = '';
  if (n === 0) {
    if (!(await confirmDlg(t('lists.delete_confirm', { name: listLabel(l) })))) return;
  } else {
    mode = await choiceDlg(t('lists.delete_tasks', { n, name: listLabel(l) }), [
      { key: 'move', label: t('lists.move_to', { name: listLabel(defaultList()) }) },
      { key: 'delete', label: t('lists.delete_with'), danger: true },
    ]);
    if (!mode) return;
  }
  try {
    const res = await api(`/api/lists/${l.id}${mode ? '?tasks=' + mode : ''}`, { method: 'DELETE' });
    if (await failed(res, t('error.delete_failed'))) return;
  } catch { return; }
  if (currentList === l.id) setCurrentList('all');
  lastRendered = '';
  loadTasks();
});

// --- Quick add ---

$('#toggle-details').addEventListener('click', () => {
  const details = $('#add-details');
  details.hidden = !details.hidden;
  $('#toggle-details').setAttribute('aria-expanded', String(!details.hidden));
});

let submitting = false;
$('#add-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (submitting) return;
  const title = $('#new-title').value.trim();
  if (!title) return;
  // #Name picks the list; if no title were left afterward, it doesn't count.
  const tag = parseListTag(title, LISTS.map((l) => ({ id: l.id, label: listLabel(l) })));
  const useTag = tag.listId !== null && tag.title !== '';
  const text = useTag ? tag.title : title;
  // Recognize natural-language details in the text; explicitly set fields win
  const parsed = parseNaturalDate(text, LANG);
  const erkannt = parsed.title && (parsed.date || parsed.time || parsed.rec || parsed.prio);
  const body = { title: erkannt ? parsed.title : text };
  body.due_date = $('#new-date').value || (erkannt && parsed.date) || undefined;
  body.due_time = $('#new-time').value || (erkannt && parsed.time) || undefined;
  body.recurrence = $('#new-rec').value || (erkannt && parsed.rec) || undefined;
  if (erkannt && parsed.prio) body.priority = parsed.prio;
  // Without #Name: the currently open list, or in "All" the default list (server default)
  if (useTag) body.list_id = tag.listId;
  else if (currentList !== 'all') body.list_id = currentList;
  // Time/recurrence without a date: default it to today for convenience —
  // computed fresh, since TODAY from the server can be stale across midnight
  if ((body.due_time || body.recurrence) && !body.due_date) body.due_date = localDateStr(new Date());
  submitting = true;
  try {
    const res = await api('/api/tasks', { method: 'POST', body: JSON.stringify(body) });
    if (!res.ok) {
      const data = await res.json().catch(() => ({}));
      toast(data.error || t('task.save_failed'));
      return;
    }
  } catch {
    return;
  } finally {
    submitting = false;
  }
  $('#new-title').value = '';
  $('#new-date').value = '';
  $('#new-time').value = '';
  $('#new-rec').value = '';
  syncPickers();
  $('#add-details').hidden = true;
  $('#toggle-details').setAttribute('aria-expanded', 'false');
  $('#new-title').focus();
  if (erkannt || useTag) {
    const parts = [];
    if (body.due_date) parts.push(fmtDate(body.due_date));
    if (body.due_time) parts.push(fmtTime(body.due_time));
    if (body.recurrence) parts.push(t(REC_KEYS[body.recurrence]));
    if (body.priority) parts.push(t(PRIO_KEYS[body.priority]));
    if (useTag) parts.push(listLabel(listById(tag.listId)));
    toast(t('add.recognized', { what: parts.join(' · ') }));
  }
  loadTasks();
});

// --- Settings ---

const dlg = $('#settings');

// fmtDateNum: a timestamp's date, numeric, in the formatting locale.
function fmtDateNum(iso) {
  const d = new Date(iso);
  return isNaN(d) ? '' : new Intl.DateTimeFormat(FMT_LOCALE, { day: '2-digit', month: '2-digit', year: 'numeric' }).format(d);
}

// Managing passkeys requires a fresh confirmation (requireFreshAuth on the
// server): if the last sign-in was too long ago, it responds with
// {"reauth": true}. Then sign in once more via passkey and retry — a
// hijacked session cookie alone isn't enough to swap passkeys this way.
async function reauth() {
  toast(t('passkeys.reauth'));
  await passkeyLogin();
}

async function loadPasskeys() {
  const res = await api('/api/passkeys');
  if (!res.ok) { toast(t('passkeys.load_failed')); return; }
  const keys = await res.json();
  const ul = $('#passkey-list');
  ul.replaceChildren();
  for (const k of keys) {
    const li = el('li', 'minirow');
    const body = el('div', 'body');
    body.append(el('div', 'title', k.name || t('passkeys.unnamed')));
    body.append(el('div', 'meta', t('passkeys.created', { date: fmtDateNum(k.created_at) })));
    const delBtn = el('button', 'rowbtn del');
    delBtn.append(icon('xmark'));
    delBtn.setAttribute('aria-label', t('passkeys.delete', { name: k.name }));
    delBtn.addEventListener('click', () => busy(delBtn, async () => {
      if (keys.length <= 1) { toast(t('error.last_passkey')); return; }
      if (!(await confirmDlg(t('passkeys.delete_confirm', { name: k.name })))) return;
      const del = () => api(`/api/passkeys/${k.id}`, { method: 'DELETE' });
      let r = await del();
      if (r.status === 403 && (await r.clone().json().catch(() => ({}))).reauth) {
        try {
          await reauth();
        } catch (ex) {
          if (ex.name !== 'NotAllowedError') toast(ex.message || t('passkeys.confirm_failed'));
          return;
        }
        r = await del();
      }
      if (!r.ok) { const d = await r.json().catch(() => ({})); toast(d.error || t('error.delete_failed')); return; }
      loadPasskeys();
    }).catch(() => {}));
    li.append(body, delBtn);
    ul.append(li);
  }
}

// --- Users (admin only) ---
let ME = null;

// Fresh sign-in, same as when deleting a passkey: 403 {"reauth": true} →
// confirm once via passkey and retry.
async function withReauth(call) {
  let r = await call();
  if (r.status === 403 && (await r.clone().json().catch(() => ({}))).reauth) {
    try {
      await reauth();
    } catch (ex) {
      if (ex.name !== 'NotAllowedError') toast(ex.message || t('passkeys.confirm_failed'));
      return null;
    }
    r = await call();
  }
  return r;
}

async function showCode(name, code) {
  const pick = await choiceDlg(t('users.code', { name, code, url: location.origin + '/setup' }),
    [{ key: 'copy', label: t('users.copy') }], t('common.close'));
  if (pick !== 'copy') return;
  try {
    await navigator.clipboard.writeText(code);
    toast(t('users.copied'));
  } catch {}
}

async function loadUsers() {
  const res = await api('/api/users');
  if (!res.ok) return;
  const users = await res.json();
  const ul = $('#user-list');
  ul.replaceChildren();
  for (const u of users) {
    const li = el('li', 'minirow');
    const body = el('div', 'body');
    body.append(el('div', 'title', u.name));
    // Without a passkey, the account is waiting to be set up — even if the
    // code expired after 7 days; it just needs a new one then.
    const setupOpen = !u.is_admin && !u.has_passkey;
    let meta = '';
    if (u.is_admin) meta = t('users.admin');
    else if (setupOpen) meta = u.code_pending ? t('users.pending') : t('users.expired');
    if (meta) body.append(el('div', 'meta', meta));
    li.append(body);
    if (setupOpen) {
      const codeBtn = el('button', 'textbtn', t('users.new_code'));
      codeBtn.type = 'button';
      codeBtn.addEventListener('click', () => busy(codeBtn, async () => {
        const r = await api(`/api/users/${u.id}/code`, { method: 'POST' });
        if (await failed(r, t('error.internal'))) return;
        await showCode(u.name, (await r.json()).code);
      }).catch(() => {}));
      li.append(codeBtn);
    }
    if (u.id !== ME?.id) {
      const delBtn = el('button', 'rowbtn del');
      delBtn.type = 'button';
      delBtn.append(icon('xmark'));
      delBtn.setAttribute('aria-label', t('users.remove', { name: u.name }));
      delBtn.addEventListener('click', () => busy(delBtn, async () => {
        if (!(await confirmDlg(t('users.remove_confirm', { name: u.name }), t('users.remove_ok')))) return;
        const r = await withReauth(() => api(`/api/users/${u.id}`, { method: 'DELETE' }));
        if (!r) return;
        if (await failed(r, t('error.delete_failed'))) return;
        loadUsers();
      }).catch(() => {}));
      li.append(delBtn);
    }
    ul.append(li);
  }
}

$('#add-user').addEventListener('click', () => busy($('#add-user'), async () => {
  const name = $('#user-name').value.trim();
  if (!name) return;
  const r = await api('/api/users', { method: 'POST', body: JSON.stringify({ name }) });
  if (await failed(r, t('error.internal'))) return;
  const d = await r.json();
  $('#user-name').value = '';
  await loadUsers();
  await showCode(d.user.name, d.code);
}).catch(() => {}));
$('#user-name').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { e.preventDefault(); $('#add-user').click(); }
});

async function loadStats() {
  try {
    const res = await api('/api/stats');
    if (!res.ok) return;
    const s = await res.json();
    const box = $('#stats-box');
    box.replaceChildren();
    const items = [
      [t('stats.today'), s.today],
      [t('stats.week'), s.week],
      [t('stats.streak'), t('stats.days', { n: s.streak })],
      [t('section.in_progress'), s.in_progress],
      [t('stats.open'), s.open],
    ];
    for (const [label, val] of items) {
      const cell = el('div', 'stat');
      cell.append(el('div', 'statval', String(val)), el('div', 'statlabel', label));
      box.append(cell);
    }
  } catch {}
}

function syncSoundSwitch() {
  const on = localStorage.getItem('soundOff') !== '1';
  $('#set-sound').classList.toggle('on', on);
  $('#set-sound').setAttribute('aria-checked', String(on));
}
$('#set-sound').addEventListener('click', () => {
  const off = localStorage.getItem('soundOff') === '1';
  localStorage.setItem('soundOff', off ? '0' : '1');
  syncSoundSwitch();
  if (off) playPop(); // let them hear it right away when turning it on
});

$('#open-settings').addEventListener('click', async () => {
  syncSoundSwitch();
  try {
    const res = await api('/api/settings');
    if (!res.ok) { toast(t('settings.load_failed')); return; }
    const s = await res.json();
    $('#set-webhook').value = s.webhook_url;
    $('#set-tz').value = s.tz;
    $('#set-digest').value = s.digest_time;
    $('#set-archive').value = s.archive_days;
    ME = s.me;
    $('#users-wrap').hidden = !ME.is_admin;
    $('#archive-row').hidden = !ME.is_admin;
    fillLangSelect(s.language);
    syncPickers();
    await loadPasskeys();
    if (ME.is_admin) await loadUsers();
    loadStats();
    await syncPushUI().catch(() => {});
  } catch {
    return;
  }
  dlg.showModal();
});
$('#close-settings').addEventListener('click', () => dlg.close());
// No backdrop click — closes only via "Done" (see edit sheet)

$('#settings-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const body = {
    webhook_url: $('#set-webhook').value.trim(),
    tz: $('#set-tz').value.trim(),
    digest_time: $('#set-digest').value,
    archive_days: Number($('#set-archive').value),
  };
  try {
    const res = await api('/api/settings', { method: 'PUT', body: JSON.stringify(body) });
    if (await failed(res, t('task.save_failed'))) return;
    toast(t('task.saved'));
    loadTasks(); // time zone can change sorting/today
  } catch {}
});

// Language: its own route, takes effect immediately — save, mirror, reload.
function fillLangSelect(current) {
  const sel = $('#set-lang');
  sel.replaceChildren();
  const auto = el('option', '', t('settings.language_auto', { lang: LANG_NAMES[browserLang()] }));
  auto.value = 'auto';
  sel.append(auto);
  for (const l of SUPPORTED) {
    const o = el('option', '', LANG_NAMES[l] || l);
    o.value = l;
    sel.append(o);
  }
  sel.value = current || 'auto';
}
$('#set-lang').addEventListener('change', async () => {
  const language = $('#set-lang').value;
  try {
    const res = await api('/api/settings/language', { method: 'PUT', body: JSON.stringify({ language }) });
    if (await failed(res, t('task.save_failed'))) return;
  } catch {
    return;
  }
  setStoredLang(language);
  location.reload();
});

// Sync the mirrored language setting: a new device only learns it after
// login (login and setup ran in the browser's language). If the language
// then differs, reload once. Without storage, detectLang stays on the
// browser language — so no reload, and hence no loop.
async function syncLangMirror() {
  try {
    const res = await api('/api/settings');
    if (!res.ok) return;
    const { language } = await res.json();
    setStoredLang(language);
    if (detectLang() !== LANG) location.reload();
  } catch {}
}

$('#test-webhook').addEventListener('click', async () => {
  try {
    const res = await api('/api/settings/test', { method: 'POST' });
    if (res.ok) { toast(t('settings.test_sent')); return; }
    const d = await res.json().catch(() => ({}));
    toast(d.error || t('settings.test_failed'));
  } catch {}
});

// --- Web push ---
// The browser holds the subscription with the push service; the server only
// knows its address and keys (push.go). Push is on or off per device.
const pushSupported = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
const isIOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
const standalone = matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;

function b64urlBytes(s) {
  const bin = atob(s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (s.length % 4)) % 4));
  return Uint8Array.from(bin, (c) => c.charCodeAt(0));
}
function sameKey(buf, bytes) {
  if (!buf) return false;
  const a = new Uint8Array(buf);
  return a.length === bytes.length && a.every((v, i) => v === bytes[i]);
}

let pushKey = null;
async function serverPushKey() {
  if (!pushKey) {
    const res = await api('/api/push/key');
    if (!res.ok) throw new Error('push key');
    pushKey = b64urlBytes((await res.json()).public_key);
  }
  return pushKey;
}

async function currentSubscription() {
  const reg = await navigator.serviceWorker.getRegistration();
  return reg ? reg.pushManager.getSubscription() : null;
}

// subscribeFresh subscribes the device with the server's current key —
// a subscription with an old key (e.g. after moving via backup) gets replaced.
async function subscribeFresh() {
  const reg = await navigator.serviceWorker.ready;
  const key = await serverPushKey();
  let sub = await reg.pushManager.getSubscription();
  if (sub && !sameKey(sub.options.applicationServerKey, key)) {
    await sub.unsubscribe();
    sub = null;
  }
  return sub || reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
}

async function sendSubscription(sub) {
  const res = await api('/api/push/subscription', { method: 'PUT', body: JSON.stringify(sub.toJSON()) });
  return res.ok ? null : res;
}

async function enablePush() {
  if ((await Notification.requestPermission()) !== 'granted') return; // syncPushUI shows "blocked"
  const sub = await subscribeFresh();
  const bad = await sendSubscription(sub);
  if (bad) {
    // The server rejects the device (e.g. an unsupported push service):
    // otherwise the toggle would stay "on" even though nothing is subscribed.
    await sub.unsubscribe().catch(() => {});
    await failed(bad, t('push.failed'));
  }
}

async function disablePush() {
  if (!pushSupported) return;
  const sub = await currentSubscription();
  if (!sub) return;
  await api('/api/push/subscription', { method: 'DELETE', body: JSON.stringify({ endpoint: sub.endpoint }) }).catch(() => {});
  await sub.unsubscribe();
}

async function syncPushUI() {
  let on = false;
  let msg = '';
  if (!pushSupported) {
    msg = isIOS && !standalone
      ? t('push.ios_hint')
      : t('push.unsupported');
  } else if (Notification.permission === 'denied') {
    msg = t('push.blocked');
  } else {
    on = Notification.permission === 'granted' && !!(await currentSubscription());
  }
  const toggle = $('#push-toggle');
  toggle.disabled = !!msg;
  toggle.classList.toggle('on', on);
  toggle.setAttribute('aria-checked', String(on));
  $('#push-hint').textContent = msg;
  $('#push-hint').hidden = !msg;
  $('#push-test').hidden = !on;
}

// syncPushUI only after busy(): busy re-enables the toggle at the end, which
// would otherwise overwrite a "blocked" state (permission just got denied).
$('#push-toggle').addEventListener('click', async () => {
  await busy($('#push-toggle'), async () => {
    try {
      if ($('#push-toggle').getAttribute('aria-checked') === 'true') await disablePush();
      else await enablePush();
    } catch {
      toast(t('push.toggle_failed'));
    }
  });
  await syncPushUI().catch(() => {});
});

$('#push-test').addEventListener('click', async () => {
  try {
    const sub = await currentSubscription();
    if (!sub) return;
    const res = await api('/api/push/test', { method: 'POST', body: JSON.stringify({ endpoint: sub.endpoint }) });
    if (await failed(res, t('settings.test_failed'))) return;
    toast(t('push.test_sent'));
  } catch {}
});

// Self-healing on load: after "sign out everywhere", a move via backup, or a
// new server key, the device quietly re-subscribes as long as permission
// is still granted.
async function healPush() {
  if (!pushSupported || Notification.permission !== 'granted') return;
  if (!(await currentSubscription())) return;
  await sendSubscription(await subscribeFresh());
}
healPush().catch(() => {});

// From a notification: /?task=<id>, or a message from the service worker,
// opens the task in the sheet once the list has loaded.
let pendingTask = Number(new URLSearchParams(location.search).get('task')) || null;
if (pendingTask) history.replaceState(null, '', '/');
function openPendingTask() {
  if (!pendingTask) return;
  const id = pendingTask;
  pendingTask = null;
  if (!byId(id)) { toast(t('task.gone')); return; }
  if (editDlg.open) switchEdit(id);
  else openEdit(id);
}
if ('serviceWorker' in navigator) {
  navigator.serviceWorker.addEventListener('message', (e) => {
    if (e.data?.type === 'open-task') {
      pendingTask = e.data.id;
      // The app may have been suspended (so the task was created on another
      // device) — TASKS is then stale. Resetting lastRendered forces a real
      // reload+redraw instead of checking against the old list (byId(id) would
      // fail -> "no longer exists" even though it succeeded).
      lastRendered = '';
      loadTasks();
    }
  });
}

$('#add-passkey').addEventListener('click', () => busy($('#add-passkey'), async () => {
  const register = () => passkeyRegister('/api/passkeys/begin', '/api/passkeys/finish', $('#passkey-name').value.trim());
  let confirmed = false;
  try {
    try {
      await register();
    } catch (ex) {
      if (!ex.reauth) throw ex;
      await reauth();
      confirmed = true;
      await register();
    }
    $('#passkey-name').value = '';
    toast(t('passkeys.added'));
    loadPasskeys();
  } catch (ex) {
    if (ex.name !== 'NotAllowedError') toast(ex.message || t('error.registration_failed'));
    // Safari only allows one passkey prompt per tap: registering right after
    // the confirmation fails there. But the confirmation is still valid
    // (10 minutes) — tapping once more is enough.
    else if (confirmed) toast(t('passkeys.tap_again'));
  }
}));

// Backup: export as a file, import replaces the entire dataset
$('#export-btn').addEventListener('click', async () => {
  try {
    const res = await api('/api/export');
    if (!res.ok) { toast(t('backup.export_failed')); return; }
    const blob = await res.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `todo-backup-${TODAY}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
  } catch {}
});

$('#import-btn').addEventListener('click', () => $('#import-file').click());
$('#import-file').addEventListener('change', async () => {
  const file = $('#import-file').files[0];
  $('#import-file').value = '';
  if (!file) return;
  if (!(await confirmDlg(t('backup.import_confirm'), t('backup.import_ok')))) return;
  let text;
  try {
    text = await file.text();
    JSON.parse(text); // fail early on a broken file
  } catch {
    toast(t('backup.invalid_file'));
    return;
  }
  try {
    const res = await api('/api/import', { method: 'POST', body: text });
    if (await failed(res, t('error.import_failed'))) return;
    toast(t('backup.imported'));
    loadTasks();
  } catch {
    toast(t('backup.import_offline')); // not "no backup": the file was readable after all
  }
});

$('#logout').addEventListener('click', async () => {
  await disablePush().catch(() => {}); // signed out means this device gets nothing more
  try {
    await api('/api/logout', { method: 'POST' });
  } catch {
    return;
  }
  location.href = '/login';
});

$('#logout-all').addEventListener('click', async () => {
  if (!(await confirmDlg(t('session.logout_all_confirm'), t('session.logout')))) return;
  try {
    await api('/api/logout-all', { method: 'POST' });
  } catch {
    return;
  }
  location.href = '/login';
});

// Pull to refresh (touch): pulling down at the top of the list reloads
const ptr = $('#ptr');
let ptrStart = null;
let ptrDist = 0;
let ptrActive = false;
let ptrLoading = false;
window.addEventListener('touchstart', (e) => {
  if (ptrLoading || dragActive || window.scrollY > 0 || document.querySelector('dialog[open]')) { ptrStart = null; return; }
  ptrStart = e.touches[0].clientY;
  ptrDist = 0;
  ptrActive = false;
}, { passive: true });
window.addEventListener('touchmove', (e) => {
  if (ptrStart === null || ptrLoading || dragActive) return;
  const d = e.touches[0].clientY - ptrStart;
  if (d <= 0 && !ptrActive) { ptrStart = null; return; }
  if (window.scrollY > 0 && !ptrActive) { ptrStart = null; return; }
  ptrActive = true;
  ptrDist = Math.min(110, d * 0.45); // rubber-band effect
  ptr.classList.add('pulling');
  ptr.style.setProperty('--pull', ptrDist + 'px');
  ptr.style.setProperty('--pullp', String(ptrDist));
  ptr.classList.toggle('ready', ptrDist > 62);
  if (d > 8 && e.cancelable) e.preventDefault();
}, { passive: false });
window.addEventListener('touchend', async () => {
  if (ptrStart === null) return;
  const trigger = ptrDist > 62;
  ptrStart = null;
  if (!ptrActive) return;
  if (trigger) {
    ptrLoading = true;
    ptr.classList.remove('pulling');
    ptr.classList.add('loading');
    navigator.vibrate?.(8);
    await loadTasks();
    await delay(400);
    ptrLoading = false;
  }
  ptr.classList.remove('pulling', 'ready', 'loading');
  ptr.style.removeProperty('--pull');
  ptr.style.removeProperty('--pullp');
  ptrDist = 0;
  ptrActive = false;
});

if ('serviceWorker' in navigator) {
  navigator.serviceWorker.register('/sw.js');
}

// Live sync: the server pushes an event (SSE) on every change,
// all open devices reload immediately. EventSource reconnects on its own.
let esRetry = null;
function connectEvents() {
  const es = new EventSource('/api/events');
  const reload = () => {
    clearTimeout(connectEvents._debounce);
    connectEvents._debounce = setTimeout(loadTasks, 150);
  };
  es.onmessage = reload;
  // Also after every (re)connect: whatever happened during the interruption
  // (a deploy, a dead connection) never arrived as an event.
  es.onopen = reload;
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED) {
      // The browser gives up — typically a 401 because the session is gone.
      // loadTasks then redirects to login via api(), instead of endlessly
      // rebuilding a dead stream here every 5 s.
      loadTasks();
      clearTimeout(esRetry);
      esRetry = setTimeout(connectEvents, 5000);
    }
  };
}
// Only render once the catalog is ready (i18nReady) — otherwise the first
// list would show raw keys. The reload triggers also only attach from here on.
function start() {
  // The catalog arrived after the deadline: redraw whatever was already drawn with keys.
  document.addEventListener('i18n-late', () => { lastRendered = ''; loadTasks(); });
  connectEvents();
  renderHeader(null);
  loadTasks();
  syncLangMirror();
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
      // see pagehide above. The undo option must not be offered anymore after
      // this — it would run into nothing, the task is already gone.
      if (pendingDelete) { hideToast(); commitPendingDelete(); }
    } else {
      loadTasks(); // reload fresh when switching back into the app (PWA)
    }
  });
  // A tab left open should catch up on "today" and overdue items across
  // midnight — without a change there'd otherwise be no trigger to reload.
  setInterval(() => { if (!document.hidden) loadTasks(); }, 10 * 60 * 1000);
  if (matchMedia('(pointer: fine)').matches) $('#new-title').focus();
}
i18nReady.then(start);
