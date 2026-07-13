// Push-only service worker. Deliberately NO fetch handler: couchpilot deploys
// often and a cache layer would serve stale assets — the app always loads
// straight from the server. This worker exists solely so the browser can
// deliver push notifications (iOS requires an installed PWA + a registered
// service worker for that).

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

self.addEventListener('push', (e) => {
  let p = {};
  try { p = e.data.json(); } catch { p = { body: e.data && e.data.text() }; }
  e.waitUntil(self.registration.showNotification(p.title || 'couchpilot', {
    body: p.body || '',
    tag: p.tag || undefined,
    icon: '/static/icons/icon-192.png',
    data: { url: p.url || '/' },
  }));
});

// The push service (notably Chrome/FCM) can rotate or expire a subscription
// out from under us. When it does, the browser fires this event and the old
// endpoint stops delivering — but the server still has it and the push service
// keeps returning 201, so failures are invisible. Re-subscribe with the same
// VAPID key and hand the fresh subscription back so delivery resumes.
self.addEventListener('pushsubscriptionchange', (e) => {
  e.waitUntil((async () => {
    try {
      const { key } = await (await fetch('/api/push/key')).json();
      const sub = await self.registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlBase64ToUint8Array(key),
      });
      await fetch('/api/push/subscribe', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(sub.toJSON()),
      });
    } catch (err) {
      // Best-effort: nothing more the worker can do if the network or auth is down.
    }
  })());
});

function urlBase64ToUint8Array(base64) {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const b64 = (base64 + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64);
  return Uint8Array.from([...raw].map(c => c.charCodeAt(0)));
}

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || '/';
  e.waitUntil((async () => {
    const clients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
    for (const c of clients) {
      try {
        // Navigate-first: a tap must land on the review even when the open
        // client runs an older app.js with no message listener. The reload
        // also picks up fresh assets.
        if ('navigate' in c) await c.navigate(url);
        else c.postMessage({ type: 'open-url', url });
        await c.focus();
        return;
      } catch { /* try the next client or a fresh window */ }
    }
    await self.clients.openWindow(url);
  })());
});
