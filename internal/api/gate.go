package api

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/krelinga/drydock/internal/auth"
)

// SessionGate is the production Gate: the session cookie, the exact-match
// Origin allowlist, and Drydock's own Host check (design §13.2, §13.3).
type SessionGate struct {
	Sessions *auth.Sessions
	// UIOrigin is compared as an exact string — never a suffix, never a
	// registrable-domain match (§13.5).
	UIOrigin string
	// UIHost is the one hostname Drydock answers to.
	UIHost string
}

type sessionKey struct{}

// SessionFrom returns the session the gate attached to a request's context.
func SessionFrom(ctx context.Context) (auth.Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(auth.Session)
	return s, ok
}

// Authenticate looks the cookie up. Any failure — no cookie, unknown,
// expired, revoked, or the store erroring — is "not signed in": the gate fails
// closed, and the client is never told which.
func (g SessionGate) Authenticate(r *http.Request) (*http.Request, bool) {
	c, err := r.Cookie(auth.CookieName)
	if err != nil || c.Value == "" {
		return r, false
	}
	s, err := g.Sessions.Lookup(r.Context(), c.Value)
	if err != nil {
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), sessionKey{}, s)), true
}

// OriginAllowed is the belt behind SameSite=Lax (§13.3). Exactly one Origin
// header, exactly equal to the UI origin. Absent fails exactly as hard as
// wrong, and so does a request carrying two — a malformed request is not one
// to give the benefit of the doubt.
func (g SessionGate) OriginAllowed(r *http.Request) bool {
	v := r.Header.Values("Origin")
	return len(v) == 1 && v[0] != "" && v[0] == g.UIOrigin
}

// HostAllowed is the second of Fig 4's gates. Caddy refuses a foreign Host
// first; this refuses it again so the DNS-rebinding defense does not live in
// one config file. A port is tolerated (Caddy may pass one through); the
// hostname must match exactly, case-insensitively, as DNS names do.
func (g SessionGate) HostAllowed(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host != "" && strings.EqualFold(host, g.UIHost)
}

// PreviewTokenValid fails closed until previews are built: the preview mux's
// one-time token store is port-forwarding §7's work, and an always-false gate
// is the honest placeholder for it.
func (g SessionGate) PreviewTokenValid(*http.Request) bool { return false }

// SignInRedirect sends a navigation with no session to the sign-in page,
// carrying where it was going. The value is a relative path, so it can only
// ever bring the browser back to this origin.
func (g SessionGate) SignInRedirect(r *http.Request) string {
	return "/signin?return=" + url.QueryEscape(r.URL.RequestURI())
}
