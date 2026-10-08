package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/krelinga/drydock/internal/preview"
)

// PreviewFrontDoor is everything the preview socket serves (port forwarding
// §3, §7, §13 steps 1–2).
//
// Two kinds of request, and nothing else:
//
//   - **Drydock's own paths.** The preview routes that have a written handler
//     (PreviewRoutes() and nothing else, so no API route is reachable however
//     the map is filled), each behind its gate exactly as Build mounts it.
//     Every answer here — the handler's, the gate's refusal, ServeMux's
//     path-cleaning 307 — says Referrer-Policy: no-referrer and
//     Cache-Control: no-store, because the request may carry a one-time token
//     in its query (PF §7; §13.1's second trap).
//   - **Everything else, in this order** (previewFallback): a Host that is not
//     a preview host, or an unmounted path under the reserved /.drydock/, goes
//     to /.drydock/denied; a request with no valid preview cookie for its Host
//     goes to /preview/authorize on the UI origin, carrying its URL; and only
//     a request whose cookie validates reaches up, the upstream, with the
//     preview cookie stripped from it and from any Set-Cookie coming back.
//
// The answers are uniform where the design needs them to be. A forged,
// expired, revoked or other-host preview cookie gets exactly the redirect no
// cookie gets; a spent, expired, forged or other-host token gets exactly the
// denial no token gets; and whether a well-formed slug names a real port is
// not decided here at all, but by /preview/authorize after sign-in — so a
// caller with nothing learns nothing from the difference between a real slug
// and an invented one.
func PreviewFrontDoor(g Gate, handlers map[string]http.HandlerFunc, up preview.Upstream) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range PreviewRoutes() {
		h := handlers[rt.Name]
		if h == nil {
			continue
		}
		rt.Handler = h
		mux.Handle(rt.Method+" "+rt.Pattern, wrap(rt, g))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A pattern is returned only for a mounted route, or the redirect
		// that cleans a path into one. Either way the answer is Drydock's,
		// not the app's.
		if _, pattern := mux.Handler(r); pattern != "" {
			previewNoStore(w)
			mux.ServeHTTP(w, r)
			return
		}
		previewFallback(g, up, w, r)
	})
}

// previewFallback is the preview socket's gate for every path the app owns.
// It is outside the route table, so it has meta-tests of its own in
// preview_test.go (§13.1's third trap): the order below, under a stub gate.
func previewFallback(g Gate, up preview.Upstream, w http.ResponseWriter, r *http.Request) {
	// 1. Host, by syntax. Caddy sends only one label under the preview
	// domain here, but a LAN client can reach the socket's other side only
	// through Caddy, and the check costs nothing.
	if !g.PreviewHost(r) {
		previewDeny(w, r)
		return
	}
	// 2. The reserved prefix is never the app's (PF §6), mounted or not.
	if r.URL.Path == "/.drydock" || strings.HasPrefix(r.URL.Path, preview.ReservedPrefix) {
		previewDeny(w, r)
		return
	}
	// 3. The preview cookie. With none that validates, the handshake: the
	// UI origin decides whether this device is signed in and whether the
	// slug is previewable.
	authed, ok := g.PreviewSession(r)
	if !ok {
		previewNoStore(w)
		http.Redirect(w, r, g.PreviewAuthorizeURL(r), http.StatusFound)
		return
	}
	t, _ := preview.TargetFrom(authed.Context())
	if up == nil {
		previewDeny(w, r)
		return
	}
	// 4. The upstream, which never sees the preview cookie and cannot set
	// one (PF §7's warning box, §10.7).
	up.ServePreview(&preview.CookieGuard{ResponseWriter: w}, preview.StripCookie(authed), t)
}

// previewNoStore is the two headers every Drydock-made answer on a preview
// host carries: the request may have a token in its URL, so no Referer may
// carry that URL onward and no cache may keep it.
func previewNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// previewDeny is the one dead end: a redirect to /.drydock/denied on the same
// host, whatever was wrong.
func previewDeny(w http.ResponseWriter, r *http.Request) {
	previewNoStore(w)
	http.Redirect(w, r, preview.DeniedPath, http.StatusFound)
}

// PreviewHandshake is the handlers of the main-origin half of the handshake and the
// preview mux's two reserved paths (PF §6, §7).
type PreviewHandshake struct {
	// Previews is nil when previews are off: authorize then refuses every
	// return, since no host is a preview host.
	Previews *preview.Service
}

// Handlers returns the map Build and PreviewFrontDoor consume. Each mounts
// only the routes of its own mux, so one map serves both.
func (p PreviewHandshake) Handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"preview.authorize": p.authorize,
		"preview.session":   p.session,
		"preview.denied":    previewDenied,
	}
}

// authorize is PF §7 steps 3–5, on the UI origin behind the session gate: the
// session is valid by the time this runs (AuthRedirect sent anyone else to
// sign in, carrying this URL). It validates `return` — an https URL on exactly
// one preview host, nothing else — checks that host's slug names an enabled
// port on a running workspace, and sends the browser to that host's
// /.drydock/session with a one-time token bound to this session and that host.
func (p PreviewHandshake) authorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	sess, ok := SessionFrom(r.Context())
	vals := r.URL.Query()["return"]
	if !ok || p.Previews == nil || len(vals) != 1 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "That is not a preview address Drydock serves.", "")
		return
	}
	host, uri, ok := preview.ParseReturn(vals[0], p.Previews.Domain)
	if !ok {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "That is not a preview address Drydock serves.", "")
		return
	}
	slug, _ := p.Previews.Slug(host)
	t, err := p.Previews.Resolve(r.Context(), slug)
	if errors.Is(err, preview.ErrNotPreviewable) {
		// The host is a well-formed preview host, so its own dead end is
		// the right place to land — and says nothing about which of "no
		// such port", "disabled" or "workspace stopped" it was.
		http.Redirect(w, r, "https://"+host+preview.DeniedPath, http.StatusFound)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "drydock: preview authorize: %v\n", err)
		WriteError(w, http.StatusInternalServerError, CodeInternal, "Could not open that preview.", "")
		return
	}
	tok, err := p.Previews.Mint(preview.Grant{AuthSessionID: sess.ID, Host: host, PortID: t.PortID, Path: uri})
	if err != nil {
		WriteError(w, http.StatusServiceUnavailable, CodeUnavailable, "Too many previews are being opened at once. Try again in a minute.", "")
		return
	}
	http.Redirect(w, r, "https://"+host+preview.SessionPath+"?t="+tok, http.StatusFound)
}

// session is PF §7 step 6, behind the token gate, which has already spent the
// token and attached its grant. It writes the preview session, sets the
// host-only cookie and sends the browser to the clean path — so the token URL
// is never the app's address, its history entry, or a bookmark. Its answer
// carries no-referrer and no-store from the front door; set here again so the
// handler holds the obligation itself (TestPreviewSessionSendsNoReferrer).
func (p PreviewHandshake) session(w http.ResponseWriter, r *http.Request) {
	previewNoStore(w)
	g, ok := preview.GrantFrom(r.Context())
	if !ok || p.Previews == nil {
		previewDeny(w, r)
		return
	}
	cookie, _, err := p.Previews.StartSession(r.Context(), g)
	if err != nil {
		if !errors.Is(err, preview.ErrNotPreviewable) && !errors.Is(err, preview.ErrSessionGone) {
			fmt.Fprintf(os.Stderr, "drydock: preview session: %v\n", err)
		}
		previewDeny(w, r)
		return
	}
	// Host-only (no Domain), Secure, HttpOnly, SameSite=Lax, Path=/ — and
	// __Host- prefixed, so a browser enforces the first, second and last.
	// No Max-Age: the server's idle window and the auth session decide its
	// life, and a browser-session cookie leaves less behind.
	http.SetCookie(w, &http.Cookie{
		Name: preview.CookieName, Value: cookie, Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "https://"+g.Host+g.Path, http.StatusFound)
}

// deniedPage is the one dead end's whole text. It is constant on purpose: it
// names no workspace, port, state or reason, so it tells a caller nothing it
// did not already know (PF §6). "Try again" restarts the handshake.
const deniedPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Preview unavailable</title>` +
	`<h1>This preview is not available</h1>` +
	`<p>Its port may be switched off, its workspace may be stopped, or this device may need to sign in to Drydock again.</p>` +
	`<p><a href="/">Try again</a></p>`

// previewDenied answers /.drydock/denied: 403 and the constant page. No CSP
// and no framing rule — a preview origin's headers are the previewed app's,
// and this page has nothing a frame could misuse.
func previewDenied(w http.ResponseWriter, r *http.Request) {
	previewNoStore(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(deniedPage))
	}
}
