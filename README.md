# elagoht/offline

A collage plugin that serves a service worker, so the pages a visitor has read
open again without a network. Pages are fetched network-first and kept; static
files are served stale-while-revalidate; a page that is neither reachable nor
kept is answered with a fallback page of your own.

```go
app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{offline.New(offline.Options{
		Precache: []string{"/"},
		Fallback: "/offline",
	})},
})
```

and the layout registers the worker:

```html
<head>
  {{offlineScript}}
</head>
```

Requires collage v0.24.0 or later. Register it in `Config.Plugins`: it adds a
template function, which only a plugin registered there can. Registering the plugin
serves `/sw.js`; nothing is installed in a browser until a page renders
`{{offlineScript}}`.

## What the worker does

| Request | What happens |
| --- | --- |
| A page (a navigation) | Network first. A successful response is kept; offline, the kept copy is served, or the `Fallback` page, or the browser's own error |
| A static file — under one of `Assets`, `/static/` by default | Stale-while-revalidate: the kept copy at once, and a fresh one fetched behind it for next time |
| A content-hashed URL (`/static/app.0d5f2b53aebf6c72.css`, what `{{asset}}` mints) | From the cache alone once kept: a name made from the bytes cannot describe any others |
| `/_collage/…`, another origin, anything but `GET`, a `Range` request, `/sw.js` | Left to the browser, untouched |
| Any other same-origin `GET` — an API, a JSON document | Left to the browser, untouched |

A response is kept only when it is a complete `200` from the site's own origin,
not reached through a redirect, and not marked `no-store`. collage marks a page
holding one reader's data that way — a form's forgery token, a preview, a page
whose middleware called `collage.SkipCache` — and a copy of such a page has no
business being shown later, perhaps to somebody else at the same device. Offline,
such a page shows the fallback.

When the worker installs it fetches the `Fallback` page and every `Precache` path.
The fallback must arrive, or the worker does not install and the previous one stays
in charge — an offline worker with nothing to show offline is worse than none. The
precache is best effort: a path that fails is logged in the browser's console and
the rest are kept, rather than one removed page leaving the site with no worker at
all.

Each cache keeps its most recent `MaxPages` or `MaxAssets` responses and drops the
oldest.

## The cache version, and deployments

The worker's caches are named after a version: a digest of the options, the
plugin's own script, and the application's build. A new deployment changes the
version, which changes `/sw.js`, which the browser notices on the next page it
opens (`/sw.js` is served `Cache-Control: no-cache` with an `ETag`, so the check is
usually a `304`). The new worker installs, takes over at once, and deletes every
cache the old one filled.

The build is `Options.Version` when you set it — a release tag, a commit hash from
CI — and otherwise collage's own `Host.BuildID`: the application's
`Config.Cache.Version` when it has one, or a fingerprint of the executable, the
same value collage namespaces its disk cache by. A new binary is a new build.

Set `Version`, or `Config.Cache.Version` for collage as a whole, when anything that
changes what visitors should see is **not** in the binary: templates or static files read from disk rather than embedded. Pages are
fetched network-first, so they stay fresh whatever the version; what a stale
version keeps is precached pages and stale-while-revalidate files one visit
longer, and old caches that are never dropped.

## Development

In development `{{offlineScript}}` renders nothing, and `/sw.js` is a worker that
deletes what this plugin cached and unregisters itself. A worker answering pages
from a cache is a live reload that shows the page from before the edit, and a
browser that once installed the real worker on `localhost` — a production build run
locally — would otherwise keep it. `InDev` serves the real worker in development
too, for working on the offline experience itself.

## Content-Security-Policy

`{{offlineScript}}` renders an inline script. A policy that allows inline scripts
by nonce passes one:

```html
{{offlineScript cspNonce}}
```

which works with `elagoht/secure`'s `{{cspNonce}}`. A policy that also restricts
`worker-src` (or, without it, `script-src` and `default-src`) must allow `'self'`.

## Options

| Option | Default | |
| --- | --- | --- |
| `Precache` | none | Paths fetched when the worker installs, so they open offline before anyone visited them |
| `Fallback` | none | The page shown offline for a page that is not kept. Precached, and required to install |
| `Assets` | `["/static/"]` | Path prefixes served stale-while-revalidate. An empty list keeps only content-hashed URLs |
| `MaxPages` | `50` | How many pages the worker keeps. Negative keeps every one |
| `MaxAssets` | `200` | How many static files the worker keeps. Negative keeps every one |
| `Version` | collage's build ID | Names the application's build in the cache version; see above |
| `InDev` | `false` | Serve the real worker, and render `{{offlineScript}}`, in development |

Every path must begin with a single `/`. A path on another origin, one of collage's
`/_collage/` endpoints, and `/sw.js` itself are refused, and the application does
not start; so is a `Fallback` that no page answers. A page whose path has a
parameter (`/{slug}`) might answer it, and is given the benefit of the doubt:
deciding would mean running its `StaticParams` at every start.

## Configuration

```json
{
  "elagoht/offline": {
    "precache": ["/", "/about", "/static/app.css"],
    "fallback": "/offline",
    "assets": ["/static/", "/_bundle/"],
    "maxPages": 100,
    "maxAssets": 300,
    "version": "2026.09.26",
    "inDev": false
  }
}
```

## Static builds

`/sw.js` is a static document, so a static build writes it to `sw.js` at the root
of the output, beside the pages that register it. A static host serves it with its
own headers, not the plugin's. Browsers fetch a worker script past their own HTTP
cache when they check for an update, but a CDN in front of the host does not: make
sure it does not keep `sw.js` for long, or a deployment reaches returning visitors
only when that copy expires.

## Limitations

- **One fallback for every locale.** A multilingual site shows the same fallback
  page to every reader.
- **The Fallback check sees pages only.** A fallback served by a document, a
  handler of your own, or a page another plugin registers after this plugin's
  `Init` is refused; register this plugin after the one that adds the page.
- **Precache paths are not checked against routes.** They may be static files a
  mount serves, which a plugin cannot enumerate; a wrong one is only a warning in
  the browser's console.
- **No network timeout.** Network-first waits for the network for as long as the
  browser does. On a connection that is up but barely moving, the kept copy is shown
  only once the request fails.
- **Query strings are part of the key.** `/search?q=a` and `/search?q=b` are two
  kept pages; `/?utm_source=x` is not `/`.
- **Scope is the whole origin.** The worker is served at `/sw.js` with scope `/`; a
  site under a path prefix, sharing its origin with other applications, would put
  them under this worker too.
- **The static-build headers are yours.** `Service-Worker-Allowed` and
  `Cache-Control: no-cache` are set by the server; a static host sends its own.

## Changes

### v0.1.2

- `collage.json`: the plugin described to editors — its template functions,
  snippets and configuration schema — for the Collage Snippets & Highlighter
  extension and any tool reading it.

### v0.1.1

- The build in the cache version is collage v0.24.0's `Host.BuildID` —
  `Config.Cache.Version`, or a fingerprint of the executable — unless
  `Options.Version` is set. The plugin's own chain of VCS revision, module
  version, executable hash and start time is gone.
- Requires collage v0.24.0.
