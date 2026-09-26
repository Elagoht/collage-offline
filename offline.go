// Package offline is a collage plugin that serves a service worker, so the pages
// a visitor has read open again without a network.
//
//	app, err := collage.New(&collage.Config{
//		Plugins: []collage.Plugin{offline.New(offline.Options{
//			Precache: []string{"/"},
//			Fallback: "/offline",
//		})},
//	})
//
// and the layout registers it:
//
//	<head>
//	  {{offlineScript}}
//	</head>
//
// The worker is served at /sw.js. It fetches pages network-first, keeping a copy
// of each one it may, and answers from that copy — or with the Fallback page —
// when the network is gone. Static files under /static/ and every content-hashed
// URL collage mints are served stale-while-revalidate. collage's own /_collage/
// endpoints, other origins and anything but GET are never touched.
//
// The worker's caches are named after a version made from the options and the
// application's build, so a deployment replaces them: the new worker installs,
// takes over, and deletes what the old one kept.
//
// In development {{offlineScript}} renders nothing, and /sw.js is a worker that
// removes itself: a cache in front of pages somebody is editing is a live reload
// that shows the page from before the edit.
package offline

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/Elagoht/collage/pkg/collage"
)

// Name is the plugin's name, and the key its configuration is found under.
const Name = "elagoht/offline"

// Path is where the service worker is served. At the root, because a worker
// controls only the URLs beneath the path it is served from.
const Path = "/sw.js"

//go:embed sw.js
var workerSource string

//go:embed retire.js
var retireSource string

// configMarker is the one place in sw.js the options are put.
const configMarker = "/*collage-offline:config*/{}"

// Options configures the plugin.
type Options struct {
	// Precache are paths fetched when the worker installs, so they open offline
	// before anyone has visited them: "/", "/about", "/static/app.css". Each is
	// best effort; one that fails is logged in the browser's console and the rest
	// are kept.
	Precache []string `json:"precache"`
	// Fallback is the path of the page shown for a page that is neither reachable
	// nor kept: "/offline". It is precached, and a worker that cannot fetch it
	// does not install. Empty shows the browser's own offline error.
	Fallback string `json:"fallback"`
	// Assets are the path prefixes of the static files served
	// stale-while-revalidate. Default ["/static/"]; an empty list keeps only the
	// content-hashed URLs, which are always cached.
	Assets []string `json:"assets"`
	// MaxPages and MaxAssets cap how many responses each cache keeps, dropping
	// the oldest. Default 50 and 200; negative keeps everything.
	MaxPages  int `json:"maxPages"`
	MaxAssets int `json:"maxAssets"`
	// Version names the application's build in the cache version. Empty uses
	// collage's Host.BuildID; see the README for when to set it yourself.
	Version string `json:"version"`
	// InDev serves the real worker, and renders {{offlineScript}}, in
	// development too.
	InDev bool `json:"inDev"`
}

// Plugin serves the service worker.
type Plugin struct {
	opts       Options
	configured bool
	active     bool // the real worker is served, not the one that retires it
	version    string
}

// New returns a plugin with opts as its starting point, which the application's
// own configuration is then decoded over.
func New(opts Options) *Plugin { return &Plugin{opts: opts} }

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.1.1" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

var (
	_ collage.Plugin     = (*Plugin)(nil)
	_ collage.Configurer = (*Plugin)(nil)
)

// Configure reads and checks the configuration and adds {{offlineScript}}. It is
// here rather than in Init because the template function has to exist before the
// templates are parsed, and what it renders depends on the configuration.
func (p *Plugin) Configure(_ context.Context, host collage.ConfigHost) error {
	if err := host.Config(&p.opts); err != nil {
		return err
	}
	o := &p.opts
	if o.Assets == nil {
		o.Assets = []string{"/static/"}
	}
	if o.MaxPages == 0 {
		o.MaxPages = 50
	}
	if o.MaxAssets == 0 {
		o.MaxAssets = 200
	}
	if o.Fallback != "" {
		if err := checkPath("fallback", o.Fallback); err != nil {
			return err
		}
	}
	for _, path := range o.Precache {
		if err := checkPath("precache", path); err != nil {
			return err
		}
	}
	for _, prefix := range o.Assets {
		if err := checkPath("asset prefix", prefix); err != nil {
			return err
		}
	}
	p.active = !host.DevMode() || o.InDev
	p.configured = true
	return host.AddTemplateFunc("offlineScript", p.script)
}

// checkPath refuses what the worker could not honour: a URL on another origin,
// which it never caches, and collage's own endpoints and the worker itself,
// which it never touches.
func checkPath(what, path string) error {
	switch {
	case !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//"):
		return fmt.Errorf("offline: %s %q must be a path on the site, beginning with one /", what, path)
	case strings.HasPrefix(path, "/_collage/"):
		return fmt.Errorf("offline: %s %q is one of collage's own endpoints, which the worker leaves alone", what, path)
	case path == Path:
		return fmt.Errorf("offline: %s %q is the worker itself", what, path)
	case strings.ContainsAny(path, "#\r\n"):
		return fmt.Errorf("offline: %s %q holds a fragment or a line break", what, path)
	}
	return nil
}

// Init makes the worker, registers it at /sw.js, and sets the headers a worker
// script is served with.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if !p.configured {
		return fmt.Errorf("offline: register the plugin in Config.Plugins, where Configure runs; {{offlineScript}} needs it")
	}
	if p.opts.Fallback != "" && !pageAnswers(host, p.opts.Fallback) {
		return fmt.Errorf("offline: fallback %q is not the path of any page: a worker that cannot fetch it never installs", p.opts.Fallback)
	}
	body := []byte(retireSource)
	if p.active {
		// collage names the build: Config.Cache.Version, or a fingerprint of the
		// executable, the same value its disk cache is namespaced by.
		build := p.opts.Version
		if build == "" {
			build = host.BuildID()
		}
		var err error
		if body, err = p.worker(build); err != nil {
			return err
		}
	}
	doc := collage.NewDocument(Name, "text/javascript; charset=utf-8").
		AtRoot(Path).
		WithBody(body).
		Build()
	if err := host.RegisterDocument(doc); err != nil {
		return fmt.Errorf("offline: %w", err)
	}
	return host.Use(p.headers)
}

// pageAnswers reports whether a page answers path, as far as can be told without
// running anything: a literal path, in any locale, is compared; a page whose path
// has a parameter might answer it, and is given the benefit of the doubt, since
// deciding would mean listing its StaticParams — work that may reach a database
// — at every start.
func pageAnswers(host collage.Host, path string) bool {
	for _, page := range host.Pages() {
		for _, locale := range page.Locales() {
			pattern, _ := page.PathFor(locale)
			if strings.Contains(pattern, "{") {
				return true
			}
			if url, err := host.URL(page.Name, locale, nil); err == nil && url == path {
				return true
			}
		}
	}
	return false
}

// worker returns sw.js with the configuration in it.
func (p *Plugin) worker(build string) ([]byte, error) {
	o := p.opts
	precache := o.Precache
	if precache == nil {
		precache = []string{}
	}
	maxPages, maxAssets := o.MaxPages, o.MaxAssets
	if maxPages < 0 {
		maxPages = 0
	}
	if maxAssets < 0 {
		maxAssets = 0
	}
	config := struct {
		Version   string   `json:"version"`
		Precache  []string `json:"precache"`
		Fallback  string   `json:"fallback"`
		Assets    []string `json:"assets"`
		MaxPages  int      `json:"maxPages"`
		MaxAssets int      `json:"maxAssets"`
	}{"", precache, o.Fallback, o.Assets, maxPages, maxAssets}
	options, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("offline: %w", err)
	}
	// The version is a digest of everything the worker's behaviour depends on:
	// the options, this plugin's own script, and the build. Change any of them
	// and the caches the old worker filled are dropped on activate.
	sum := sha256.New()
	for _, part := range []string{string(options), workerSource, p.Version(), build} {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	config.Version = hex.EncodeToString(sum.Sum(nil))[:16]
	// Marshalled again with the version in it. encoding/json escapes <, > and &,
	// and U+2028 and U+2029, so what it writes is safe inside a script.
	final, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("offline: %w", err)
	}
	if !strings.Contains(workerSource, configMarker) {
		return nil, fmt.Errorf("offline: sw.js has lost its configuration marker")
	}
	p.version = config.Version
	return []byte(strings.Replace(workerSource, configMarker, string(final), 1)), nil
}

// CacheVersion is the version the worker names its caches after, or empty in
// development without InDev, where no caches are made.
func (p *Plugin) CacheVersion() string { return p.version }

// script is {{offlineScript}}: the registration, or nothing in development. A
// page with a Content-Security-Policy passes a nonce: {{offlineScript cspNonce}}.
func (p *Plugin) script(nonce ...string) template.HTML {
	if !p.active {
		return ""
	}
	var b bytes.Buffer
	b.WriteString("<script")
	if len(nonce) > 0 && nonce[0] != "" {
		b.WriteString(` nonce="` + template.HTMLEscapeString(nonce[0]) + `"`)
	}
	b.WriteString(`>if("serviceWorker"in navigator)addEventListener("load",function(){` +
		`navigator.serviceWorker.register("` + Path + `",{scope:"/"}).catch(function(e){console.warn("collage-offline:",e)})})</script>`)
	return template.HTML(b.String()) // assembled here from constants and an escaped nonce
}

// headers sets what a worker script is served with. collage writes a document's
// Cache-Control itself, after middleware has run, so the header is set as the
// response is committed rather than before it is handed on.
func (p *Plugin) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != Path {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&workerWriter{ResponseWriter: w}, r)
	})
}

// workerWriter puts the worker's headers on its response.
type workerWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *workerWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		if status == http.StatusOK || status == http.StatusNotModified {
			h := w.Header()
			// no-cache, not a lifetime: a browser checks for a new worker on
			// every navigation, and a worker kept for an hour is a deployment
			// that reaches nobody for an hour. The ETag makes the check a 304.
			h.Set("Cache-Control", "no-cache")
			h.Set("Service-Worker-Allowed", "/")
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *workerWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection beneath.
func (w *workerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
