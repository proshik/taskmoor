// taskmoor's service worker (stage 10): the board's own files for when the
// network is gone — never the API, never a sign-in — and the notifications
// the service pushes to a paired device.
"use strict";

const SHELL = "taskmoor-shell-v2";
const FILES = ["/", "/app.js", "/app.css", "/tokens.css", "/base.css", "/manifest.webmanifest", "/icon-192.png", "/icon-512.png", "/apple-touch-icon.png",
  "/fonts/redhattext-latin.woff2", "/fonts/redhattext-latin-ext.woff2", "/fonts/redhatmono-latin.woff2", "/fonts/redhatmono-latin-ext.woff2"];

const CORE = ["/", "/app.js", "/app.css", "/tokens.css", "/base.css"]; // without these no offline start

self.addEventListener("install", (e) => {
  // the core must be cached, or the install fails and is tried again on
  // the next load; the rest is best effort, one by one
  e.waitUntil(caches.open(SHELL)
    .then((c) => c.addAll(CORE).then(() => Promise.all(FILES.filter((f) => !CORE.includes(f)).map((f) => c.add(f).catch(() => {})))))
    .then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(caches.keys()
    .then((ks) => Promise.all(ks.filter((k) => k !== SHELL).map((k) => caches.delete(k))))
    .then(() => self.clients.claim()));
});

// network first: a new version shows at once; the copy only when offline
self.addEventListener("fetch", (e) => {
  const u = new URL(e.request.url);
  if (e.request.method !== "GET" || u.origin !== self.location.origin) return;
  if (!FILES.includes(u.pathname)) return; // the API and the sign-in: the network's alone
  e.respondWith(fetch(e.request).then((r) => {
    if (r.ok && r.type === "basic") { const copy = r.clone(); caches.open(SHELL).then((c) => c.put(u.pathname, copy)); return r; }
    // the VPS answers but the Mac does not (a gateway error): the copy
    if (r.status >= 500) return caches.match(u.pathname).then((c) => c || r);
    return r; // a sign-in's redirect and the like pass as they are
  }).catch(() => caches.match(u.pathname).then((r) => r || Response.error())));
});

self.addEventListener("push", (e) => {
  let m = {};
  try { m = e.data ? e.data.json() : {}; } catch { /* not ours */ }
  e.waitUntil(self.registration.showNotification(m.title || "taskmoor", {
    body: m.body || "Something waits for you", tag: m.tag, renotify: !!m.tag,
    data: { url: m.url || "/" }, icon: "/icon-192.png", badge: "/icon-192.png",
  }));
});

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = new URL((e.notification.data && e.notification.data.url) || "/", self.location.origin).href;
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((cs) => {
    for (const c of cs) {
      if (new URL(c.url).origin === self.location.origin) {
        return c.focus().then((w) => (w || c).navigate ? (w || c).navigate(url) : null).catch(() => self.clients.openWindow(url));
      }
    }
    return self.clients.openWindow(url);
  }));
});
