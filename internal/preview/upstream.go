package preview

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// MaxReturn is the longest preview URL the handshake carries. A longer one
// is refused by ParseReturn, so the preview mux sends it to its own denied
// page before the round trip rather than to authorize's 400 on the UI origin.
const MaxReturn = 8 << 10

// ParseReturn validates /preview/authorize's `return` (PF §7 step 2): it must
// be an https URL on a preview host under domain — one label, not the
// reserved probe name — with no port (an empty one, `host:`, included), no
// user info, and nothing a browser
// would read differently from Go. It returns the host and the request URI to
// land on. Anything else is refused rather than repaired: authorize redirects
// only to the preview host it was asked about, so it is never an open
// redirect.
func ParseReturn(raw, domain string) (host, uri string, ok bool) {
	if raw == "" || len(raw) > MaxReturn {
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
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" || strings.Contains(u.Host, ":") {
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

// Upstream is what a request with a valid preview cookie is handed to: in
// production the Proxy, which re-resolves the workspace's container by label
// before every dial (PF §8.1). The request it receives has already had the
// preview cookie removed, and its response passes through a writer that drops
// any Set-Cookie claiming that name.
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

type recheckKey struct{}

// WithRecheck attaches the question "does this request's preview session
// still hold?" to its context. The gate attaches it; the proxy asks it again
// while an upgraded connection stays open, because a websocket has no next
// request for a revocation to refuse (PF §13.4).
func WithRecheck(ctx context.Context, f func(context.Context) bool) context.Context {
	return context.WithValue(ctx, recheckKey{}, f)
}

// RecheckFrom returns the gate's recheck, if it attached one.
func RecheckFrom(ctx context.Context) (func(context.Context) bool, bool) {
	f, ok := ctx.Value(recheckKey{}).(func(context.Context) bool)
	return f, ok && f != nil
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

// UICookieName is the UI's session cookie (internal/auth's CookieName, which
// a test holds this to). A browser never sends it to a preview host — it is
// host-only on another registrable domain — so stripping it too costs nothing
// and holds even if the two are ever misconfigured onto one domain (PF §10.3).
const UICookieName = "__Host-drydock"

// StripCookie returns a copy of r whose Cookie header no longer carries the
// preview cookie (PF §7's warning box, second hop) or the UI's session
// cookie. Every other cookie — the app's own — passes through in its order; a
// request left with no cookie at all carries no Cookie header.
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
			if isPreviewCookie(name) || strings.EqualFold(strings.TrimSpace(name), UICookieName) {
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
// Set-Cookie whose name is the preview cookie's before any header block is
// sent — so a previewed app can neither overwrite the device's preview session
// nor read one back by setting it. Every other Set-Cookie passes.
//
// It filters on every WriteHeader, informational ones included, and stops
// only once the final header block has gone (a status of 200 or more, or a
// 101, or the implicit 200 of a first Write or Flush). A 1xx is sent with the
// header map as it stands and the map is still writable afterwards, so a guard
// that latched on the first WriteHeader would pass a 103 Early Hints carrying
// the cookie — and, worse, let a Set-Cookie added after the 103 ride the
// final response. httputil.ReverseProxy forwards an upstream's 1xx by default
// (its Got1xxResponse copies the headers and calls WriteHeader), so that is
// the path the Proxy opens.
//
// A websocket upgrade's 101 is the one block this writer cannot see:
// ReverseProxy writes it through the hijacked connection. The Proxy filters
// it itself, in ModifyResponse, which runs for a 101 before the hijack.
type CookieGuard struct {
	http.ResponseWriter
	final bool
}

// filter strips the preview cookie from the header map unless the final
// header block has already gone; final marks that it is going now.
func (g *CookieGuard) filter(final bool) {
	if g.final {
		return
	}
	FilterSetCookie(g.ResponseWriter.Header())
	g.final = final
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

// WriteHeader filters, then writes — every time, 1xx included.
func (g *CookieGuard) WriteHeader(code int) {
	g.filter(code >= 200 || code == http.StatusSwitchingProtocols)
	g.ResponseWriter.WriteHeader(code)
}

// Write filters before an implicit 200.
func (g *CookieGuard) Write(b []byte) (int, error) {
	g.filter(true)
	return g.ResponseWriter.Write(b)
}

// Flush keeps streaming responses streaming (PF §8.4).
func (g *CookieGuard) Flush() {
	g.filter(true)
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack is passed through for the Proxy's websocket upgrade; see the type's
// note on what that obliges the proxy to do.
func (g *CookieGuard) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	g.filter(true)
	if h, ok := g.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (g *CookieGuard) Unwrap() http.ResponseWriter { return g.ResponseWriter }
