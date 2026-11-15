/* Mail service worker. Served by the Go server at /sw.js (it has to live at
   the site root to control every page); the version and the precache list
   below are placeholders filled in at serve time from the built frontend
   assets.

   What it does:
   - Precaches the app's static CSS/JS and an offline page, so the shell opens
     (and says so) without a network.
   - Shows Web Push notifications for new mail.
   What it deliberately does NOT do: cache mail. Pages, message bodies and
   attachments always come from the network — they're private, and a cached
   copy would outlive a logout. */
"use strict";

const VERSION = "__VERSION__";
const CACHE = "mail-static-" + VERSION;
const PRECACHE = __PRECACHE__;
const OFFLINE_URL = "/offline";

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      // One missing file shouldn't stop the worker (and with it, push) from
      // installing; anything not cached now is fetched and cached on demand.
      .then((cache) => Promise.all(PRECACHE.map((u) => cache.add(u).catch(() => undefined))))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((names) =>
        Promise.all(names.filter((n) => n.startsWith("mail-static-") && n !== CACHE).map((n) => caches.delete(n))),
      )
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // Static assets: hashed filenames never change, so cache-first is safe.
  if (url.pathname.startsWith("/static/")) {
    event.respondWith(
      caches.match(req).then(
        (hit) =>
          hit ||
          fetch(req).then((res) => {
            if (res.ok) {
              const copy = res.clone();
              caches.open(CACHE).then((c) => c.put(req, copy));
            }
            return res;
          }),
      ),
    );
    return;
  }

  // Page loads: always the network; if it's unreachable, the offline page.
  if (req.mode === "navigate") {
    event.respondWith(
      fetch(req).catch(() => caches.match(OFFLINE_URL).then((r) => r || Response.error())),
    );
  }
  // Everything else (htmx partials, attachments, API calls) goes straight
  // to the network untouched.
});

self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch (_) {
    // Not JSON: fall through to the generic notification below.
  }
  const title = data.title || "New mail";
  event.waitUntil(
    self.registration.showNotification(title, {
      body: data.body || "",
      tag: data.tag || "new-mail",
      renotify: true,
      icon: "/static/icons/icon-192.png",
      badge: "/static/icons/badge-96.png",
      data: { url: data.url || "/mail/INBOX" },
    }),
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  // Payloads only ever come from our server, but never navigate off-site
  // regardless: anything that isn't a same-origin URL falls back to the inbox.
  const origin = self.location.origin;
  let url = new URL((event.notification.data && event.notification.data.url) || "/mail/INBOX", origin);
  if (url.origin !== origin) url = new URL("/mail/INBOX", origin);
  const target = url.href;
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(async (windows) => {
      // Reuse an open Mail window instead of piling up new ones.
      for (const w of windows) {
        if (new URL(w.url).origin === self.location.origin) {
          await w.focus();
          if (w.url !== target && "navigate" in w) {
            try {
              await w.navigate(target);
            } catch (_) {
              /* cross-origin or detached: leave it where it is */
            }
          }
          return;
        }
      }
      return self.clients.openWindow(target);
    }),
  );
});

// The browser rotated the push subscription (or the push service expired
// it): subscribe again with the same key and tell the server.
self.addEventListener("pushsubscriptionchange", (event) => {
  event.waitUntil(
    (async () => {
      const old = event.oldSubscription;
      const key = old && old.options && old.options.applicationServerKey;
      if (!key) return;
      const sub = await self.registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
      await fetch("/push/subscribe", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "same-origin",
        body: JSON.stringify(sub.toJSON()),
      });
    })(),
  );
});
