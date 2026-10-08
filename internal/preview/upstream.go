package preview

import (
	"bufio"
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// ParseReturn validates /preview/authorize's `return` (PF §7 step 2): it must
// be an https URL on a preview host under domain — one label, not the
// reserved probe name — with no port, no user info, and nothing a browser
// would read differently from Go. It returns the host and the request URI to
// land on. Anything else is refused rather than repaired: authorize redirects
// only to the preview host it was asked about, so it is never an open
// redirect.
func ParseReturn(raw, domain string) (host, uri string, ok bool) {
	if raw == "" || len(raw) > 8<<10 {
		return "", "", false
	}
	for _, c := range raw {
		// Control characters, space and backslash: a browser's URL
		// parser strips or rewrites them where Go's does not, which is how
		// a value that parses here to one host navigates there to another.
		if c <= 0x20 || c == 0x7f || c == '\\' {
			return "", "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" || u.Port() != "" {
		return "", "", false
	}
	if u.Host != strings.ToLower(u.Host) {
		return "", "", false
	}
	slug, ok := Slug(u.Host, domain)
	if !ok {
		return "", "", false
	}
	uri = u.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	if !strings.HasPrefix(uri, "/") {
		return "", "", false
	}
	if u.RawQuery != "" {
		uri += "?" + u.RawQuery
	}
	return slug + "." + domain, uri, true
}

// Upstream is what a request with a valid preview cookie is handed to. Step 2
// serves Placeholder; step 3 replaces it with the proxy, which re-resolves the
// workspace's container by label before every dial (PF §8.1). The request it
// receives has already had the preview cookie removed, and its response
// passes through a writer that drops any Set-Cookie claiming that name.
type Upstream interface {
	ServePreview(w http.ResponseWriter, r *http.Request, t Target)
}

// UpstreamFunc adapts a function to Upstream.
type UpstreamFunc func(http.ResponseWriter, *http.Request, Target)

// ServePreview calls f.
func (f UpstreamFunc) ServePreview(w http.ResponseWriter, r *http.Request, t Target) { f(w, r, t) }

type targetKey struct{}

// WithTarget attaches a resolved target to a request's context.
func WithTarget(ctx context.Context, t Target) context.Context {
	return context.WithValue(ctx, targetKey{}, t)
}

// TargetFrom returns the target the gate attached.
func TargetFrom(ctx context.Context) (Target, bool) {
	t, ok := ctx.Value(targetKey{}).(Target)
	return t, ok
}

type grantKey struct{}

// WithGrant attaches a consumed token's grant to a request's context.
func WithGrant(ctx context.Context, g Grant) context.Context {
	return context.WithValue(ctx, grantKey{}, g)
}

// GrantFrom returns the grant the token gate attached.
func GrantFrom(ctx context.Context) (Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(Grant)
	return g, ok
}

// isPreviewCookie compares names case-insensitively: a cookie name is
// case-sensitive to a browser, but the strip is a prohibition, and a
// near-miss that a lenient framework would read as the same name is not
// worth reasoning about.
func isPreviewCookie(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), CookieName)
}

// StripCookie returns a copy of r whose Cookie header no longer carries the
// preview cookie (PF §7's warning box, second hop). Every other cookie — the
// app's own — passes through in its order; a request left with no cookie at
// all carries no Cookie header.
func StripCookie(r *http.Request) *http.Request {
	out := r.Clone(r.Context())
	var kept []string
	for _, line := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			name, _, _ := strings.Cut(p, "=")
			if isPreviewCookie(name) {
				continue
			}
			kept = append(kept, p)
		}
	}
	out.Header.Del("Cookie")
	if len(kept) > 0 {
		out.Header.Set("Cookie", strings.Join(kept, "; "))
	}
	return out
}

// CookieGuard wraps the response writer an upstream writes to, and drops any
// Set-Cookie whose name is the preview cookie's before the header is sent —
// so a previewed app can neither overwrite the device's preview session nor
// read one back by setting it. Every other Set-Cookie passes.
//
// Step 3 must keep this true for a websocket upgrade too: a 101 written
// through a hijacked connection bypasses this writer, so the proxy has to
// filter the upstream's headers itself (httputil.ReverseProxy's
// ModifyResponse runs for a 101 as well).
type CookieGuard struct {
	http.ResponseWriter
	wrote bool
}

func (g *CookieGuard) filter() {
	if g.wrote {
		return
	}
	g.wrote = true
	h := g.ResponseWriter.Header()
	FilterSetCookie(h)
}

// FilterSetCookie removes every Set-Cookie in h that names the preview
// cookie.
func FilterSetCookie(h http.Header) {
	vals := h.Values("Set-Cookie")
	if len(vals) == 0 {
		return
	}
	var kept []string
	for _, v := range vals {
		name, _, _ := strings.Cut(v, "=")
		if !isPreviewCookie(name) {
			kept = append(kept, v)
		}
	}
	h.Del("Set-Cookie")
	for _, v := range kept {
		h.Add("Set-Cookie", v)
	}
}

// WriteHeader filters, then writes.
func (g *CookieGuard) WriteHeader(code int) {
	g.filter()
	g.ResponseWriter.WriteHeader(code)
}

// Write filters before an implicit 200.
func (g *CookieGuard) Write(b []byte) (int, error) {
	g.filter()
	return g.ResponseWriter.Write(b)
}

// Flush keeps streaming responses streaming (PF §8.4).
func (g *CookieGuard) Flush() {
	g.filter()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack is passed through for step 3's websocket upgrade; see the type's
// note on what that obliges the proxy to do.
func (g *CookieGuard) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	g.filter()
	if h, ok := g.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (g *CookieGuard) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// Placeholder is step 2's hardcoded upstream: a fixed page inside Drydock,
// standing where step 3's proxy to the workspace's container will. It says the
// device is signed in to this preview, and names the cookies the request
// carried for the app — names only, never values — so a reader (and the
// browser tier) can see the preview cookie is not among them.
var Placeholder Upstream = UpstreamFunc(func(w http.ResponseWriter, r *http.Request, t Target) {
	var names []string
	for _, c := range r.Cookies() {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	list := "none"
	if len(names) > 0 {
		list = html.EscapeString(strings.Join(names, ", "))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	if r.Method == http.MethodHead {
		return
	}
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Preview signed in</title>`+
		`<h1>Signed in to this preview</h1>`+
		`<p data-test="placeholder">This device may view port %d of this workspace. Drydock does not proxy to the container yet; that is the next step of previews.</p>`+
		`<p>Cookies this request carried for the app: <span data-test="app-cookies">%s</span>.</p>`,
		t.ContainerPort, list)
})
