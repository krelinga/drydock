// Package web serves the built Vue app from the binary (frontend §3).
//
// The bundle is web/'s Vite output, written to dist/ here and committed, so
// that `go build` needs no Node: the constraint frontend §3 sets and leaves to
// packaging to honour. A Go-only machine builds the real UI, and the web/
// toolchain's `npm run check:dist` fails if the committed copy no longer
// matches its sources.
//
// Three serving rules, from frontend §3:
//
//  1. Hashed assets (everything under assets/) are immutable for a year.
//  2. index.html is no-store. It is the only file that names the hashed ones,
//     and a cached copy after an upgrade asks for assets that no longer exist.
//  3. The SPA fallback never swallows /api. An unknown /api path is the API's
//     business and gets a JSON 404 (the server routes it to the gate before
//     it can reach this handler; this package refuses it again regardless).
//
// Security headers (frontend §8) are applied by SecurityHeaders, around the
// whole API socket rather than here, so the API's JSON carries them too.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/krelinga/drydock/internal/api"
)

// The all: prefix matters: without it, embed silently drops files whose names
// begin with '_' or '.', and Vite names shared chunks like
// _plugin-vue_export-helper-<hash>.js.
//
//go:embed all:dist
var embedded embed.FS

// Dist is the embedded build output, rooted at dist/.
func Dist() fs.FS {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		panic(err) // a constant path into an embed.FS: cannot fail
	}
	return sub
}

// CSP is frontend §8's policy, verbatim. No 'unsafe-inline' and no
// 'unsafe-eval' anywhere: the build emits no inline script or style
// (web/scripts/budget.mjs checks), and Vue's runtime build compiles nothing at
// run time. frame-ancestors 'none' and frame-src 'none' are the two halves of
// "no iframes, in either direction".
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; " +
	"form-action 'self'; frame-src 'none'; frame-ancestors 'none'"

// Cache policies, by rule.
const (
	CacheImmutable = "public, max-age=31536000, immutable"
	CacheIndex     = "no-store"
	// CacheRevalidate is for the few unhashed files public/ contributes —
	// favicons, the manifest. They may change across an upgrade, so they are
	// revalidated by ETag rather than trusted for a year.
	CacheRevalidate = "no-cache"
)

// SetSecurityHeaders writes frontend §8's three headers.
func SetSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", CSP)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

// SecurityHeaders sets them before next runs, so they are present on every
// response next writes — success, error envelope, redirect, or 404 — and a
// handler cannot forget them.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetSecurityHeaders(w.Header())
		next.ServeHTTP(w, r)
	})
}

type file struct {
	data  []byte
	ctype string
	etag  string
	cache string
}

// Handler serves one build of the app. Every file is read into memory at
// construction: the whole bundle is tens of kilobytes, and a missing
// index.html is then a startup error rather than a 500 on first visit.
type Handler struct {
	files map[string]file // keyed by slash path relative to the root, no leading slash
	index file
}

// New loads fsys, which must hold index.html at its root.
func New(fsys fs.FS) (*Handler, error) {
	h := &Handler{files: map[string]file{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		f := file{
			data:  b,
			ctype: contentType(p),
			etag:  `"` + hex.EncodeToString(sum[:12]) + `"`,
			cache: CacheRevalidate,
		}
		if strings.HasPrefix(p, "assets/") {
			f.cache = CacheImmutable
		}
		h.files[p] = f
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("web: load bundle: %w", err)
	}
	idx, ok := h.files["index.html"]
	if !ok {
		return nil, errors.New("web: the bundle has no index.html; rebuild web/ with `npm run build`")
	}
	idx.cache = CacheIndex
	idx.etag = "" // no-store: there is nothing to revalidate against
	h.index = idx
	delete(h.files, "index.html")
	return h, nil
}

func contentType(p string) string {
	switch path.Ext(p) {
	case ".webmanifest":
		return "application/manifest+json"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// IsAPIPath reports whether a path belongs to the API rather than the app. It
// is the one definition both the server's routing and this handler's refusal
// use, so the two cannot disagree about where /api ends.
func IsAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if IsAPIPath(r.URL.Path) {
		// Rule 3, defended a second time: if routing ever sent an /api path
		// here, answer as the API would, never with the app.
		api.WriteError(w, http.StatusNotFound, api.CodeNotFound, "No such API route.", "")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	f, ok := h.files[name]
	switch {
	case ok:
	case strings.HasPrefix(name, "assets/"):
		// A hashed asset that does not exist is an old index.html asking
		// for a previous build. index.html in its place would be parsed as
		// JavaScript; a plain 404 says what happened.
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "not found", http.StatusNotFound)
		return
	default:
		// Rule 3's other half: every other unknown path is a client route.
		f = h.index
	}
	serve(w, r, f)
}

func serve(w http.ResponseWriter, r *http.Request, f file) {
	hd := w.Header()
	hd.Set("Content-Type", f.ctype)
	hd.Set("Cache-Control", f.cache)
	if f.etag != "" {
		hd.Set("ETag", f.etag)
	}
	// ServeContent handles HEAD, Range and If-None-Match. The zero modtime
	// suppresses Last-Modified, which an embedded file does not have.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(f.data))
}
