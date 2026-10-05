// vops service worker: shows Web Push notifications and opens the dashboard on the page they point to. No caching.
"use strict";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

self.addEventListener("push", (e) => {
  let m;
  try { m = e.data ? e.data.json() : {}; } catch { m = { body: e.data.text() }; }
  e.waitUntil(self.registration.showNotification(m.title || "vops", {
    body: m.body || "", tag: m.tag || undefined, renotify: !!m.tag, icon: "icon-192.png", badge: "icon-192.png", data: { url: m.url || "/" },
  }));
});

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = new URL((e.notification.data && e.notification.data.url) || "/", self.registration.scope).href;
  e.waitUntil((async () => {
    const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    const win = wins.find((c) => new URL(c.url).origin === self.location.origin);
    if (!win) return self.clients.openWindow(url);
    await win.focus();
    win.postMessage({ open: url }); // the page moves to it (navigate() needs a controlled client)
  })());
});
