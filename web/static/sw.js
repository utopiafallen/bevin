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

const VERSION = "shell-v1";
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
