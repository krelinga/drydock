package web

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// A bundle shaped like Vite's output, independent of whatever is in dist/.
func fakeBundle() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                  {Data: []byte(`<!doctype html><script type="module" src="/assets/index-AbC123xy.js"></script>`)},
		"assets/index-AbC123xy.js":    {Data: []byte(`console.log("app")`)},
		"assets/index-Zz9Yy8Xx.css":   {Data: []byte(`body{}`)},
		"assets/_plugin-vue-Q1w2.js":  {Data: []byte(`export{}`)},
		"favicon.svg":                 {Data: []byte(`<svg/>`)},
		"manifest.webmanifest":        {Data: []byte(`{}`)},
		"apple-touch-icon.png":        {Data: []byte("\x89PNG")},
		"nested/not-an-asset/file.js": {Data: []byte(`x`)},
	}
}

func get(t *testing.T, h http.Handler, method, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Result()
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func TestIndexIsNoStoreAndAssetsAreImmutable(t *testing.T) {
	h, err := New(fakeBundle())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, cache, ctype string }{
		{"/", CacheIndex, "text/html; charset=utf-8"},
		{"/index.html", CacheIndex, "text/html; charset=utf-8"},
		{"/assets/index-AbC123xy.js", CacheImmutable, "text/javascript; charset=utf-8"},
		{"/assets/index-Zz9Yy8Xx.css", CacheImmutable, "text/css; charset=utf-8"},
		{"/assets/_plugin-vue-Q1w2.js", CacheImmutable, "text/javascript; charset=utf-8"},
		// Unhashed public/ files: revalidated, never immutable.
		{"/favicon.svg", CacheRevalidate, "image/svg+xml"},
		{"/manifest.webmanifest", CacheRevalidate, "application/manifest+json"},
		{"/nested/not-an-asset/file.js", CacheRevalidate, "text/javascript; charset=utf-8"},
	} {
		r := get(t, h, "GET", c.path)
		if r.StatusCode != 200 || r.Header.Get("Cache-Control") != c.cache || r.Header.Get("Content-Type") != c.ctype {
			t.Errorf("%s = %d, Cache-Control %q, Content-Type %q; want 200, %q, %q",
				c.path, r.StatusCode, r.Header.Get("Cache-Control"), r.Header.Get("Content-Type"), c.cache, c.ctype)
		}
	}
	// index.html carries no validator: no-store means there is nothing to
	// revalidate. Control: a revalidated file does carry one, and honours it.
	if r := get(t, h, "GET", "/"); r.Header.Get("ETag") != "" {
		t.Errorf("index.html has an ETag %q", r.Header.Get("ETag"))
	}
	fav := get(t, h, "GET", "/favicon.svg")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/favicon.svg", nil)
	req.Header.Set("If-None-Match", fav.Header.Get("ETag"))
	h.ServeHTTP(rec, req)
	if fav.Header.Get("ETag") == "" || rec.Code != http.StatusNotModified {
		t.Errorf("favicon ETag %q, conditional GET = %d; want an ETag and 304", fav.Header.Get("ETag"), rec.Code)
	}
}

func TestSPAFallbackServesIndexButNeverForAPI(t *testing.T) {
	h, _ := New(fakeBundle())
	index := body(t, get(t, h, "GET", "/"))

	// Client routes, including deep and odd ones, get index.html.
	for _, p := range []string{"/settings", "/ws/01JABC/logs", "/signin?return=%2Fsettings", "/secrets/new", "/apiary", "/api-docs"} {
		r := get(t, h, "GET", p)
		if r.StatusCode != 200 || body(t, r) != index || r.Header.Get("Cache-Control") != CacheIndex {
			t.Errorf("%s did not fall back to a no-store index.html (%d)", p, r.StatusCode)
		}
	}
	// /api, at any depth, never does — even though the server routes it away
	// before it gets here.
	for _, p := range []string{"/api", "/api/", "/api/typo", "/api/auth/session/extra"} {
		r := get(t, h, "GET", p)
		b := body(t, r)
		var env struct{ Error struct{ Code string } }
		if r.StatusCode != 404 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") ||
			json.Unmarshal([]byte(b), &env) != nil || env.Error.Code != "not_found" || strings.Contains(b, "<") {
			t.Errorf("%s = %d %q %q; want a JSON 404 not_found", p, r.StatusCode, r.Header.Get("Content-Type"), b)
		}
	}
}

func TestMissingHashedAssetIs404NotIndex(t *testing.T) {
	h, _ := New(fakeBundle())
	r := get(t, h, "GET", "/assets/index-OLDBUILD.js")
	if r.StatusCode != 404 || strings.Contains(body(t, r), "<script") {
		t.Errorf("missing asset = %d; want a 404 that is not index.html", r.StatusCode)
	}
	// Control: the asset that does exist is served.
	if r := get(t, h, "GET", "/assets/index-AbC123xy.js"); r.StatusCode != 200 {
		t.Errorf("existing asset = %d", r.StatusCode)
	}
}

func TestOnlyGetAndHead(t *testing.T) {
	h, _ := New(fakeBundle())
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if r := get(t, h, m, "/signin"); r.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /signin = %d; want 405", m, r.StatusCode)
		}
	}
	if r := get(t, h, "HEAD", "/"); r.StatusCode != 200 || body(t, r) != "" {
		t.Errorf("HEAD / = %d", r.StatusCode)
	}
	if r := get(t, h, "GET", "/signin"); r.StatusCode != 200 {
		t.Errorf("control: GET /signin = %d", r.StatusCode)
	}
}

func TestNoIndexIsAStartupError(t *testing.T) {
	b := fakeBundle()
	delete(b, "index.html")
	if _, err := New(b); err == nil {
		t.Error("New accepted a bundle with no index.html")
	}
	if _, err := New(fakeBundle()); err != nil {
		t.Errorf("control: a complete bundle = %v", err)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/error":
			http.Error(w, "boom", 500)
		case "/redirect":
			http.Redirect(w, r, "/x", http.StatusFound)
		default:
			w.Write([]byte("ok"))
		}
	})
	h := SecurityHeaders(inner)
	for _, p := range []string{"/", "/error", "/redirect"} {
		r := get(t, h, "GET", p)
		if r.Header.Get("Content-Security-Policy") != CSP || r.Header.Get("Referrer-Policy") != "no-referrer" ||
			r.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s (%d) missing a security header: %v", p, r.StatusCode, r.Header)
		}
	}
	// Control: the unwrapped handler sets none, so the wrapper is what did it.
	if r := get(t, inner, "GET", "/"); r.Header.Get("Content-Security-Policy") != "" {
		t.Error("control: the inner handler set a CSP itself; the assertion above proves nothing")
	}
}

// TestCSPIsTheDesignedPolicy pins frontend §8's policy directive by directive,
// so a "temporary" loosening shows up as a failing test rather than a diff
// nobody reads.
func TestCSPIsTheDesignedPolicy(t *testing.T) {
	want := map[string]string{
		"default-src": "'self'", "script-src": "'self'", "style-src": "'self'",
		"img-src": "'self' data:", "connect-src": "'self'", "font-src": "'self'",
		"object-src": "'none'", "base-uri": "'none'", "form-action": "'self'",
		"frame-src": "'none'", "frame-ancestors": "'none'",
	}
	got := map[string]string{}
	for _, d := range strings.Split(CSP, ";") {
		f := strings.Fields(d)
		if len(f) > 0 {
			got[f[0]] = strings.Join(f[1:], " ")
		}
	}
	if len(got) != len(want) {
		t.Errorf("CSP has %d directives; want %d: %q", len(got), len(want), CSP)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("CSP %s = %q; want %q", k, got[k], v)
		}
	}
	if strings.Contains(CSP, "unsafe") {
		t.Errorf("CSP contains an unsafe- source: %q", CSP)
	}
}

// TestEmbeddedBundleIsComplete checks the committed dist/: it loads, and every
// asset index.html names is in it. A partial commit of a rebuild — new
// index.html, old assets — is the failure this catches before a browser does.
func TestEmbeddedBundleIsComplete(t *testing.T) {
	fsys := Dist()
	idx, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("no index.html in the embedded bundle: %v", err)
	}
	refs := regexp.MustCompile(`(?:src|href)="/(assets/[^"]+)"`).FindAllStringSubmatch(string(idx), -1)
	if len(refs) == 0 {
		t.Fatal("index.html references no assets/: the check below would be vacuous")
	}
	for _, m := range refs {
		if _, err := fs.Stat(fsys, m[1]); err != nil {
			t.Errorf("index.html references %s, which is not in the bundle", m[1])
		}
	}
	if strings.Contains(string(idx), "<script>") || strings.Contains(string(idx), "<style") {
		t.Error("index.html carries an inline script or style, which the CSP refuses")
	}
	if _, err := New(fsys); err != nil {
		t.Errorf("New(Dist()) = %v", err)
	}
}
