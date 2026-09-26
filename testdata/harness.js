// harness runs the served worker in node against fake caches and a fake network,
// and checks what it does with each kind of request. Usage: node harness.js sw.js
"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const ORIGIN = "https://example.com";

// A Response the way a service worker sees one from its own origin.
class BasicResponse extends Response {
  get type() { return "basic"; }
  clone() { const c = super.clone(); Object.setPrototypeOf(c, BasicResponse.prototype); return c; }
}

const routes = {
  "/": { body: "home" },
  "/offline": { body: "offline page" },
  "/about": { body: "about" },
  "/private": { body: "yours only", headers: { "Cache-Control": "private, no-store" } },
  "/static/app.css": { body: "css v1" },
  "/static/app.0123456789abcdef.css": { body: "hashed css" },
};
let online = true;
const hits = [];
async function network(req) {
  const url = new URL(typeof req === "string" ? req : req.url, ORIGIN);
  hits.push(url.pathname);
  if (!online) throw new TypeError("Failed to fetch");
  const route = routes[url.pathname];
  if (!route) return new BasicResponse("not found", { status: 404 });
  return new BasicResponse(route.body, { status: 200, headers: route.headers || {} });
}

const key = (r) => new URL(typeof r === "string" ? r : r.url, ORIGIN).href;
class MemCache {
  constructor() { this.m = new Map(); }
  async match(r) { const v = this.m.get(key(r)); return v ? v.clone() : undefined; }
  async put(r, res) { this.m.delete(key(r)); this.m.set(key(r), res.clone()); }
  async add(r) { const res = await network(r); if (!res.ok) throw new TypeError("status " + res.status); await this.put(r, res); }
  async keys() { return [...this.m.keys()].map((u) => ({ url: u })); }
  async delete(r) { return this.m.delete(key(r)); }
}
const store = new Map();
const caches = {
  async open(n) { if (!store.has(n)) store.set(n, new MemCache()); return store.get(n); },
  async keys() { return [...store.keys()]; },
  async delete(n) { return store.delete(n); },
  async match(r, o) {
    for (const n of o && o.cacheName ? [o.cacheName] : [...store.keys()]) {
      const c = store.get(n);
      const v = c && (await c.match(r));
      if (v) return v;
    }
  },
};

const listeners = {};
const self = {
  location: new URL(ORIGIN + "/sw.js"),
  addEventListener(type, fn) { listeners[type] = fn; },
  skipWaiting: async () => {},
  clients: { claim: async () => {} },
  registration: { unregister: async () => true },
};
function Req(url, init) {
  const r = new Request(new URL(url, ORIGIN), { method: (init && init.method) || "GET", headers: (init && init.headers) || {} });
  // A Request cannot be constructed in navigate mode outside a browser.
  return Object.defineProperty(r, "mode", { value: (init && init.mode) || "cors" });
}
// A browser resolves a worker's relative URLs against its own; node has no base.
class WorkerRequest extends Request {
  constructor(input, init) { super(typeof input === "string" ? new URL(input, ORIGIN) : input, init); }
}
vm.runInNewContext(fs.readFileSync(process.argv[2], "utf8"), {
  self, caches, fetch: network, Request: WorkerRequest, Response, Headers, URL, console, Set, Promise,
});

async function lifecycle(type) {
  const pending = [];
  listeners[type]({ waitUntil: (p) => pending.push(p) });
  await Promise.all(pending);
}
async function request(url, init) {
  const pending = [];
  let answer = null;
  listeners.fetch({ request: Req(url, init), respondWith: (p) => { answer = p; }, waitUntil: (p) => pending.push(p) });
  if (!answer) return null;
  const res = await answer;
  await Promise.all(pending);
  return res;
}
const text = async (res) => (res ? res.text() : null);

(async () => {
  store.set("collage-offline-pages-0000000000000000", new MemCache());
  store.set("someone-elses-cache", new MemCache());
  await lifecycle("install");
  await lifecycle("activate");
  const names = [...store.keys()];
  assert(!names.includes("collage-offline-pages-0000000000000000"), "an old version's cache survived activate");
  assert(names.includes("someone-elses-cache"), "activate deleted a cache that is not the worker's");
  assert(await caches.match("/offline"), "the fallback was not precached");
  assert(await caches.match("/"), "/ was not precached");
  assert(names.some((n) => n.startsWith("collage-offline-assets-")), "no asset cache");

  // Left to the browser.
  assert.strictEqual(await request("/about", { method: "POST", mode: "navigate" }), null, "a POST was answered");
  assert.strictEqual(await request("/_collage/reload"), null, "a /_collage/ request was answered");
  assert.strictEqual(await request("https://cdn.example.net/lib.js"), null, "a cross-origin request was answered");
  assert.strictEqual(await request("/api/data"), null, "a non-navigation, non-asset request was answered");
  assert.strictEqual(await request("/static/video.mp4", { headers: { Range: "bytes=0-9" } }), null, "a range request was answered");

  // Pages: network first, the kept copy offline, then the fallback.
  assert.strictEqual(await text(await request("/about", { mode: "navigate" })), "about");
  assert.strictEqual(await text(await request("/private", { mode: "navigate" })), "yours only");
  routes["/about"].body = "about v2";
  assert.strictEqual(await text(await request("/about", { mode: "navigate" })), "about v2", "online, the network was not asked first");
  await request("/static/app.0123456789abcdef.css");
  online = false;
  assert.strictEqual(await text(await request("/about", { mode: "navigate" })), "about v2", "offline, the kept page was not served");
  assert.strictEqual(await text(await request("/never-visited", { mode: "navigate" })), "offline page", "offline, the fallback was not served");
  assert.strictEqual(await text(await request("/private", { mode: "navigate" })), "offline page", "a no-store page was kept");

  // Assets: stale-while-revalidate, and a hashed name from the cache alone.
  assert.strictEqual(await text(await request("/static/app.css")), "css v1", "offline, a precached asset was not served");
  online = true;
  routes["/static/app.css"].body = "css v2";
  assert.strictEqual(await text(await request("/static/app.css")), "css v1", "the stale copy was not served first");
  assert.strictEqual(await text(await request("/static/app.css")), "css v2", "the copy was not revalidated");
  const before = hits.length;
  assert.strictEqual(await text(await request("/static/app.0123456789abcdef.css")), "hashed css");
  assert.strictEqual(hits.length, before, "a content-hashed asset was fetched again");
  console.log("ok");
})().catch((err) => { console.error(err.message); process.exit(1); });
