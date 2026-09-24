// Bump this on every frontend change, otherwise offline usage gets stuck on the old version.
const CACHE = 'todo-v41';
const ASSETS = [
  '/style.css',
  '/i18n.js',
  '/dateparse.js',
  '/locales/de.json',
  '/locales/en.json',
  '/app.js',
  '/webauthn.js',
  '/manifest.webmanifest',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
];

self.addEventListener('install', (e) => {
  e.waitUntil(
    (async () => {
      const c = await caches.open(CACHE);
      await c.addAll(ASSETS);
      // Cache the app shell right on install (the first navigation ran without
      // SW control yet) — but only the real shell, never a login-redirect response
      try {
        const res = await fetch('/');
        if (res.ok && !res.redirected) await c.put('/', res);
      } catch {}
      await self.skipWaiting();
    })()
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

// Network-first with a cache fallback for our own static assets and navigations.
// /api/ is never touched: data always comes live from the server.
self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin || url.pathname.startsWith('/api/')) {
    return;
  }
  e.respondWith(
    fetch(e.request)
      .then((res) => {
        if (res.ok && !res.redirected) {
          const clone = res.clone();
          caches.open(CACHE).then((c) => c.put(e.request, clone));
        }
        return res;
      })
      .catch(async () => {
        const m = await caches.match(e.request);
        if (m) return m;
        if (e.request.mode === 'navigate') {
          const shell = await caches.match('/');
          if (shell) return shell;
          // The worker doesn't know the catalog — the device's language is good enough.
          const de = (self.navigator.language || '').toLowerCase().startsWith('de');
          const [lang, text] = de
            ? ['de', 'Offline — bitte einmal mit Internet öffnen.']
            : ['en', 'Offline — please open once with an internet connection.'];
          return new Response(
            `<!DOCTYPE html><html lang="${lang}"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Todo — offline</title><body style="font-family:system-ui;display:grid;place-items:center;min-height:100dvh;margin:0"><p>${text}</p></body></html>`,
            { status: 503, headers: { 'Content-Type': 'text/html; charset=utf-8' } }
          );
        }
        return Response.error();
      })
  );
});

// --- Web push (content and buttons: push.go / scheduler.go) ---
// Buttons only show where supported (not on iOS) — tapping always opens the task.
self.addEventListener('push', (e) => {
  let m = {};
  try { m = e.data ? e.data.json() : {}; } catch {}
  // The label comes from the server ({action, title}); plain strings are the
  // old format from before translation existed — then use the German labels.
  const labels = { done: 'Erledigt', snooze: '+1 Tag' };
  e.waitUntil(self.registration.showNotification(m.title || 'Todo', {
    body: m.body || '',
    tag: m.tag || undefined,
    // Without renotify, replacing a notification with the same tag (daily
    // recurrence, a second digest) stays silent on Chrome/Edge/Android — !!
    // important: renotify:true with an empty tag throws a TypeError.
    renotify: !!m.tag,
    icon: '/icons/icon-192.png',
    badge: '/icons/icon-192.png',
    data: { taskId: m.task_id || null, dueDate: m.due_date || null },
    actions: (m.actions || []).map((a) => (typeof a === 'string' ? { action: a, title: labels[a] || a } : a)),
  }));
});

async function openApp(taskId) {
  const wins = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  const win = wins.find((w) => new URL(w.url).origin === location.origin);
  if (win) {
    await win.focus();
    if (taskId) win.postMessage({ type: 'open-task', id: taskId });
    return;
  }
  await self.clients.openWindow(taskId ? `/?task=${taskId}` : '/');
}

self.addEventListener('notificationclick', (e) => {
  const { taskId, dueDate } = e.notification.data || {};
  e.notification.close();
  e.waitUntil((async () => {
    if (taskId && (e.action === 'done' || e.action === 'snooze')) {
      // expected_due_date on both actions: tasks.id gets reused after deletion,
      // so a day-old notification must not complete/snooze a different, newer
      // task (handleCompleteTask/handleSnoozeTask).
      const body = e.action === 'done'
        ? { expected_due_date: dueDate || '' }
        : { days: 1, expected_due_date: dueDate || '' };
      try {
        const res = await fetch(`/api/tasks/${taskId}/${e.action === 'done' ? 'complete' : 'snooze'}`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        if (res.ok) return;
      } catch {}
    }
    // A tap — or the action failed (signed out, offline, deleted)
    await openApp(taskId);
  })());
});

// The browser renews a subscription on its own: update the server right away
// instead of missing pings until the app is next opened.
self.addEventListener('pushsubscriptionchange', (e) => {
  e.waitUntil((async () => {
    const res = await fetch('/api/push/key');
    if (!res.ok) return;
    const s = (await res.json()).public_key;
    const bin = atob(s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (s.length % 4)) % 4));
    const key = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    const sub = await self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
    await fetch('/api/push/subscription', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(sub.toJSON()),
    });
  })());
});
