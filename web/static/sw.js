/* bevin service worker — the app shell (PWA installability + offline
 * fallback). Stage 1 of the PWA work: NO push handling (the web-push
 * transport is paused until the SSE event API lands — the design log).
 *
 * Intercept policy — static files ONLY:
 *   - GET, same-origin
 *   - the root document, or any path whose last segment has a static
 *     asset extension (.html/.css/.js/.svg/.png/.woff2/.webmanifest)
 *   - never /api/* (all chat + auth + SSE traffic lives there) and never
 *     any other path (redirects, /login, /admin, …)
 *
 * Strategy — NETWORK-FIRST with cache fallback + cache refresh on
 * success. The assets are embedded in the binary and served under the
 * SAME names across versions (no cache-busting query strings), so a
 * cache-first rule would keep an installed PWA on stale JS/CSS after a
 * server update. Network-first keeps updates instant; the cache exists
 * purely so the shell still opens when the network is down (the chat
 * itself is server-side and needs the network either way).
 */
"use strict";

const VERSION = "shell-v2";
const CACHE = "bevin-" + VERSION;

// The app shell, precached at install so first open after install works
// offline. (The font is big — everything else is small.)
const SHELL = [
  "/",
  "/index.html",
  "/brand.css",
  "/app.css",
  "/markdown.js",
  "/logo-mark.svg",
  "/favicon.svg",
  "/manifest.webmanifest",
  "/icon-192.png",
  "/icon-512.png",
  "/icon-maskable-512.png",
  "/apple-touch-icon.png",
  "/fonts/nunito-variable.woff2",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(CACHE)
      .then((cache) => cache.addAll(SHELL))
      .then(() => self.skipWaiting()) // a new binary takes over at once
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys()
      .then((keys) =>
        Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))
      )
      .then(() => self.clients.claim())
  );
});

const STATIC_EXT = /\.(html|css|js|svg|png|woff2|webmanifest)$/i;

function isStaticRequest(req) {
  if (req.method !== "GET") return false;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return false;
  if (url.pathname.startsWith("/api/")) return false; // never cache API/SSE
  if (url.pathname === "/" || url.pathname === "/index.html") return true;
  const last = url.pathname.split("/").pop();
  return STATIC_EXT.test(last);
}

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (!isStaticRequest(req)) return; // network, untouched

  // Network-first: serve the fresh copy, refresh the cache behind it.
  // On a network failure fall back to the cache (offline shell), and for
  // a document navigation that misses the cache too, serve the cached
  // index — the app's own signed-in gate takes it from there.
  event.respondWith(
    fetch(req)
      .then((res) => {
        if (res && res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then((cache) => cache.put(req, copy));
        }
        return res;
      })
      .catch(() =>
        caches.match(req).then(
          (hit) =>
            hit ||
            (req.mode === "navigate" ? caches.match("/index.html") : Promise.reject(new Error("offline")))
        )
      )
  );
});

// ---- Web push (the PWA stage 2) --------------------------------------
// The payload arrives encrypted by the server (VAPID, the standard
// aes128gcm content encoding). The BROWSER decrypts it with the
// device's subscription keys before delivering it (the spec's standard
// encodings are UA-decrypted), so the handler just parses the JSON:
// {conv_id, title, body, kind}.

// The visibility gate: if the app is open AND visible on this device,
// its /api/me/events channel is already updating the UI live — a push
// for the same turn would be a double notification. Suppress it.
async function anyClientVisible() {
  const clients = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  return clients.some((c) => c.visibilityState === "visible");
}

self.addEventListener("push", (e) => {
  let payload = null;
  if (e.data) {
    try {
      payload = e.data.json();
    } catch (err) {
      try {
        payload = { body: e.data.text() };
      } catch (err2) {}
    }
  }
  if (!payload) return;
  e.waitUntil(
    (async () => {
      if (await anyClientVisible()) return; // the app is looking at it
      await self.registration.showNotification(payload.title || "bevin", {
        body: payload.body || undefined,
        icon: "/icon-192.png",
        badge: "/icon-192.png",
        // Same tag = the newest turn replaces the previous one (no
        // stack of "turn complete" notifications per tool loop).
        tag: "bevin-" + (payload.conv_id || payload.kind || "system"),
        data: { url: payload.conv_id ? "/?conv=" + payload.conv_id : "/" },
      });
    })()
  );
});

// Click a notification: open (or focus) the app at the conversation.
self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || "/";
  e.waitUntil(
    (async () => {
      const all = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const c of all) {
        if (c.visibilityState === "visible") {
          try {
            await c.focus();
            return;
          } catch (err) {}
        }
      }
      await self.clients.openWindow(url);
    })()
  );
});
