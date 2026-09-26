package offline_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	offline "github.com/Elagoht/collage-offline"
	"github.com/Elagoht/collage/pkg/collage"
)

type setup struct {
	dev    bool
	config map[string]json.RawMessage
	pages  []*collage.Page
}

func site(t *testing.T, p *offline.Plugin, s setup) (*collage.App, error) {
	t.Helper()
	app, err := collage.New(&collage.Config{
		DevMode: s.dev,
		Server:  collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html":       {Data: []byte(`<html><head>{{offlineScript}}</head><body>home</body></html>`)},
			"t/n.html":       {Data: []byte(`<html><head>{{offlineScript "abc\"d"}}</head><body>nonce</body></html>`)},
			"t/offline.html": {Data: []byte(`<html><body>You are offline.</body></html>`)},
		}, Root: "t"},
		Cache:        collage.CacheConfig{Enabled: true, Type: "memory", DefaultTTL: time.Hour},
		Plugins:      []collage.Plugin{p},
		PluginConfig: s.config,
	})
	if err != nil {
		return nil, err
	}
	pages := s.pages
	if pages == nil {
		pages = []*collage.Page{
			collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Static().Build(),
			collage.NewPage("nonce").WithContent(collage.NewFragment("nonce", "n.html").Build()).WithPath("en", "/nonce").Static().Build(),
			collage.NewPage("offline").WithContent(collage.NewFragment("offline", "offline.html").Build()).WithPath("en", "/offline").Static().Build(),
		}
	}
	for _, page := range pages {
		if err := app.RegisterPage(page); err != nil {
			t.Fatal(err)
		}
	}
	return app, nil
}

func mustSite(t *testing.T, p *offline.Plugin, s setup) *collage.App {
	t.Helper()
	app, err := site(t, p, s)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func get(app *collage.App, path string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, r)
	return rec
}

var configRE = regexp.MustCompile(`const CONFIG = (\{.*\});`)

type workerConfig struct {
	Version   string   `json:"version"`
	Precache  []string `json:"precache"`
	Fallback  string   `json:"fallback"`
	Assets    []string `json:"assets"`
	MaxPages  int      `json:"maxPages"`
	MaxAssets int      `json:"maxAssets"`
}

func readConfig(t *testing.T, body string) workerConfig {
	t.Helper()
	m := configRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no configuration in the worker:\n%s", body)
	}
	var c workerConfig
	if err := json.Unmarshal([]byte(m[1]), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestServesTheWorker(t *testing.T) {
	app := mustSite(t, offline.New(offline.Options{Precache: []string{"/", "/static/app.css"}, Fallback: "/offline", Version: "1"}), setup{})
	rec := get(app, "/sw.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("/sw.js = %d %s", rec.Code, rec.Body.String())
	}
	for name, want := range map[string]string{
		"Content-Type":           "text/javascript; charset=utf-8",
		"Cache-Control":          "no-cache",
		"Service-Worker-Allowed": "/",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	c := readConfig(t, rec.Body.String())
	if c.Fallback != "/offline" || strings.Join(c.Precache, ",") != "/,/static/app.css" ||
		strings.Join(c.Assets, ",") != "/static/" || c.MaxPages != 50 || c.MaxAssets != 200 || len(c.Version) != 16 {
		t.Errorf("configuration = %+v", c)
	}
	for _, want := range []string{`request.mode === "navigate"`, `"/_collage/"`, `request.method !== "GET"`, `url.origin !== self.location.origin`, `caches.delete`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the worker lacks %s", want)
		}
	}

	// A browser checking for a new worker revalidates, and an unchanged one is a
	// 304 that still carries the worker's headers.
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	again := get(app, "/sw.js", "If-None-Match", etag)
	if again.Code != http.StatusNotModified || again.Header().Get("Cache-Control") != "no-cache" || again.Header().Get("Service-Worker-Allowed") != "/" {
		t.Errorf("revalidation = %d %v", again.Code, again.Header())
	}

	// Nothing else is given the worker's headers.
	page := get(app, "/")
	if page.Header().Get("Service-Worker-Allowed") != "" || page.Header().Get("Cache-Control") == "no-cache" {
		t.Errorf("a page got the worker's headers: %v", page.Header())
	}
}

// The worker has to parse, or the browser installs nothing and says so only in its
// console.
func TestWorkerIsValidJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	for _, dev := range []bool{false, true} {
		app := mustSite(t, offline.New(offline.Options{Precache: []string{"/"}, Fallback: "/offline"}), setup{dev: dev})
		file := filepath.Join(t.TempDir(), "sw.js")
		if err := os.WriteFile(file, get(app, "/sw.js").Body.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", file).CombinedOutput(); err != nil {
			t.Errorf("dev=%v: node --check: %v\n%s", dev, err, out)
		}
	}
}

func TestCacheVersion(t *testing.T) {
	version := func(opts offline.Options) string {
		p := offline.New(opts)
		app := mustSite(t, p, setup{})
		v := readConfig(t, get(app, "/sw.js").Body.String()).Version
		if v != p.CacheVersion() {
			t.Errorf("CacheVersion() = %q, the worker says %q", p.CacheVersion(), v)
		}
		return v
	}
	base := version(offline.Options{Precache: []string{"/"}, Version: "build-1"})
	if base != version(offline.Options{Precache: []string{"/"}, Version: "build-1"}) {
		t.Error("the same options and build made two versions")
	}
	if base == version(offline.Options{Precache: []string{"/"}, Version: "build-2"}) {
		t.Error("a new build kept the version, so a deployment would keep the old caches")
	}
	if base == version(offline.Options{Precache: []string{"/", "/offline"}, Version: "build-1"}) {
		t.Error("new options kept the version")
	}
	// Without a Version, the build is named by the binary, and stays the same
	// across starts of the same one.
	derived := version(offline.Options{})
	if derived == "" || derived != version(offline.Options{}) {
		t.Errorf("derived version %q is empty or unstable", derived)
	}
}

func TestScript(t *testing.T) {
	app := mustSite(t, offline.New(offline.Options{}), setup{})
	body := get(app, "/").Body.String()
	if !strings.Contains(body, `<script>if("serviceWorker"in navigator)`) || !strings.Contains(body, `register("/sw.js",{scope:"/"})`) {
		t.Errorf("no registration in the page:\n%s", body)
	}
	if body := get(app, "/nonce").Body.String(); !strings.Contains(body, `<script nonce="abc&#34;d">`) {
		t.Errorf("the nonce is missing or unescaped:\n%s", body)
	}
}

// In development the page registers nothing, and a browser that installed the
// worker earlier is handed one that removes it.
func TestDevelopment(t *testing.T) {
	app := mustSite(t, offline.New(offline.Options{Fallback: "/offline"}), setup{dev: true})
	if body := get(app, "/").Body.String(); strings.Contains(body, "serviceWorker") {
		t.Errorf("development page registers the worker:\n%s", body)
	}
	sw := get(app, "/sw.js")
	if sw.Code != http.StatusOK || !strings.Contains(sw.Body.String(), "registration.unregister()") || strings.Contains(sw.Body.String(), "respondWith") {
		t.Errorf("development /sw.js = %d\n%s", sw.Code, sw.Body.String())
	}
	if sw.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control = %q", sw.Header().Get("Cache-Control"))
	}

	app = mustSite(t, offline.New(offline.Options{InDev: true}), setup{dev: true})
	if body := get(app, "/").Body.String(); !strings.Contains(body, "serviceWorker") {
		t.Error("InDev does not register the worker")
	}
	if body := get(app, "/sw.js").Body.String(); !strings.Contains(body, "respondWith") {
		t.Error("InDev does not serve the real worker")
	}
}

func TestConfiguration(t *testing.T) {
	app := mustSite(t, offline.New(offline.Options{Precache: []string{"/"}}), setup{config: map[string]json.RawMessage{
		offline.Name: json.RawMessage(`{"fallback": "/offline", "assets": [], "maxPages": -1, "maxAssets": 10}`),
	}})
	c := readConfig(t, get(app, "/sw.js").Body.String())
	if c.Fallback != "/offline" || len(c.Assets) != 0 || c.MaxPages != 0 || c.MaxAssets != 10 || strings.Join(c.Precache, ",") != "/" {
		t.Errorf("configuration = %+v", c)
	}
}

// A path the worker could not honour stops the application: a worker that never
// installs is otherwise noticed by nobody.
func TestRefusals(t *testing.T) {
	for name, opts := range map[string]offline.Options{
		"relative fallback":   {Fallback: "offline"},
		"cross-origin":        {Precache: []string{"https://cdn.example.com/app.js"}},
		"protocol-relative":   {Precache: []string{"//cdn.example.com/app.js"}},
		"collage's endpoints": {Precache: []string{"/_collage/reload"}},
		"the worker":          {Precache: []string{"/sw.js"}},
		"asset prefix":        {Assets: []string{"static/"}},
	} {
		if _, err := site(t, offline.New(opts), setup{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A fallback no page answers is found at start, when the handler is built.
	app := mustSite(t, offline.New(offline.Options{Fallback: "/nowhere"}), setup{})
	if rec := get(app, "/"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("fallback without a page: / = %d", rec.Code)
	}
}

// A page whose path has a parameter may answer the fallback, and is not refused
// for it: deciding would mean running its StaticParams at every start.
func TestFallbackUnderAParameter(t *testing.T) {
	pages := []*collage.Page{
		collage.NewPage("home").WithContent(collage.NewFragment("home", "p.html").Build()).WithPath("en", "/").Static().Build(),
		collage.NewPage("any").WithContent(collage.NewFragment("any", "offline.html").Build()).WithPath("en", "/{slug}").Static().Build(),
	}
	app := mustSite(t, offline.New(offline.Options{Fallback: "/offline"}), setup{pages: pages})
	if rec := get(app, "/sw.js"); rec.Code != http.StatusOK {
		t.Errorf("/sw.js = %d", rec.Code)
	}
}

func TestStaticBuild(t *testing.T) {
	app := mustSite(t, offline.New(offline.Options{Fallback: "/offline", Version: "1"}), setup{})
	out := t.TempDir()
	b, err := collage.NewBuilder(app, collage.BuildOptions{OutDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	built, err := os.ReadFile(filepath.Join(out, "sw.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(built) != get(app, "/sw.js").Body.String() {
		t.Error("the built worker differs from the served one")
	}
	home, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(home), "serviceWorker") {
		t.Error("the built page does not register the worker")
	}
}

// The worker is run in node against a fake network and fake caches, and does
// with each kind of request what the README says it does.
func TestWorkerBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	app := mustSite(t, offline.New(offline.Options{Precache: []string{"/", "/static/app.css"}, Fallback: "/offline", Version: "1"}), setup{})
	file := filepath.Join(t.TempDir(), "sw.js")
	if err := os.WriteFile(file, get(app, "/sw.js").Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, filepath.Join("testdata", "harness.js"), file).CombinedOutput(); err != nil {
		t.Errorf("%v\n%s", err, out)
	}
}
