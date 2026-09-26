// The service worker collage-offline serves at /sw.js in development.
//
// A browser that once installed the real worker on this origin — a production
// build run locally, a development server with InDev on — keeps it, and keeps
// answering pages from it, until something replaces it. This replaces it with
// nothing: it deletes what collage-offline cached and unregisters itself, so the
// live-reload script is not fighting a cache it cannot see.
"use strict";

self.addEventListener("install", () => self.skipWaiting());

self.addEventListener("activate", (event) => {
  event.waitUntil((async () => {
    const names = await caches.keys();
    await Promise.all(names
      .filter((name) => name.startsWith("collage-offline-"))
      .map((name) => caches.delete(name)));
    await self.registration.unregister();
  })());
});
