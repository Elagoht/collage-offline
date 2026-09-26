// The service worker collage-offline serves at /sw.js.
//
// Pages are fetched network-first: a reader online always sees the page as it is
// now, and a reader offline sees the copy kept the last time it was read, or the
// fallback page. Static files are served stale-while-revalidate, and a
// content-hashed one from the cache alone, since its name cannot describe any
// other bytes. Anything else — another origin, a method other than GET, collage's
// own /_collage/ endpoints, a range request — is left to the browser.
"use strict";

const CONFIG = /*collage-offline:config*/{};

const PREFIX = "collage-offline-";
const PAGES = PREFIX + "pages-" + CONFIG.version;
const ASSETS = PREFIX + "assets-" + CONFIG.version;

// A name collage's {{asset}} mints: sixteen hex characters before the extension.
const HASHED = /\.[0-9a-f]{16}\.[A-Za-z0-9]+$/;

function isHashed(url) {
  return HASHED.test(url.pathname);
}

function isAsset(url) {
  return isHashed(url) || CONFIG.assets.some((prefix) => url.pathname.startsWith(prefix));
}

// A response is kept only when it is a complete, same-origin success that its
// server allowed to be stored. collage marks a page holding one reader's data —
// a form's token, a preview — "no-store", and a copy of it has no business being
// shown later, perhaps to somebody else at the same device.
function storable(response) {
  if (!response || !response.ok || response.status !== 200 || response.type !== "basic" || response.redirected) {
    return false;
  }
  const control = response.headers.get("Cache-Control") || "";
  return !/no-store/i.test(control);
}

// trim keeps a cache to its most recent entries. Cache keys come back in
// insertion order, and a put replaces an entry in place, so the oldest are first.
async function trim(cache, max) {
  if (!max) {
    return;
  }
  const keys = await cache.keys();
  for (let i = 0; i < keys.length - max; i++) {
    await cache.delete(keys[i]);
  }
}

async function store(name, max, request, response) {
  const cache = await caches.open(name);
  await cache.put(request, response);
  await trim(cache, max);
}

self.addEventListener("install", (event) => {
  event.waitUntil((async () => {
    const pages = await caches.open(PAGES);
    const assets = await caches.open(ASSETS);
    // The fallback must be there, or offline has nothing to show: a failure to
    // fetch it fails the install, and the previous worker stays in charge.
    if (CONFIG.fallback) {
      await pages.add(new Request(CONFIG.fallback, { cache: "reload" }));
    }
    // The rest is best effort. cache.addAll would fail the whole install over one
    // path that has since gone away, which would leave the site with no worker.
    await Promise.all(CONFIG.precache.map(async (path) => {
      try {
        const url = new URL(path, self.location.origin);
        await (isAsset(url) ? assets : pages).add(new Request(url, { cache: "reload" }));
      } catch (err) {
        console.warn("collage-offline: could not precache " + path, err);
      }
    }));
    await self.skipWaiting();
  })());
});

self.addEventListener("activate", (event) => {
  event.waitUntil((async () => {
    // A new version means a new deployment or new options: what the old one kept
    // was made by something that no longer runs.
    const keep = new Set([PAGES, ASSETS]);
    const names = await caches.keys();
    await Promise.all(names
      .filter((name) => name.startsWith(PREFIX) && !keep.has(name))
      .map((name) => caches.delete(name)));
    await self.clients.claim();
  })());
});

async function networkFirst(event) {
  const request = event.request;
  try {
    const response = await fetch(request);
    if (storable(response)) {
      event.waitUntil(store(PAGES, CONFIG.maxPages, request, response.clone()));
    }
    return response;
  } catch (err) {
    const cached = await caches.match(request, { cacheName: PAGES });
    if (cached) {
      return cached;
    }
    if (CONFIG.fallback) {
      const fallback = await caches.match(CONFIG.fallback, { cacheName: PAGES });
      if (fallback) {
        return fallback;
      }
    }
    throw err;
  }
}

async function staleWhileRevalidate(event, url) {
  const request = event.request;
  const cache = await caches.open(ASSETS);
  const cached = await cache.match(request);
  if (cached && isHashed(url)) {
    // A content-hashed name is these bytes forever; asking again is waste.
    return cached;
  }
  const network = fetch(request).then((response) => {
    if (storable(response)) {
      event.waitUntil(store(ASSETS, CONFIG.maxAssets, request, response.clone()));
    }
    return response;
  });
  if (cached) {
    event.waitUntil(network.catch(() => undefined));
    return cached;
  }
  return network;
}

self.addEventListener("fetch", (event) => {
  const request = event.request;
  if (request.method !== "GET" || request.headers.has("Range")) {
    return;
  }
  const url = new URL(request.url);
  if (url.origin !== self.location.origin || url.pathname.startsWith("/_collage/") || url.pathname === "/sw.js") {
    return;
  }
  if (request.mode === "navigate") {
    event.respondWith(networkFirst(event));
    return;
  }
  if (isAsset(url)) {
    event.respondWith(staleWhileRevalidate(event, url));
  }
});
