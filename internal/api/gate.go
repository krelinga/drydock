package api

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/preview"
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
	// Previews is the preview handshake's state (PF §7). Nil means no
	// preview gate passes: no token is valid, no cookie, no host.
	Previews *preview.Service
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

// PreviewTokenValid consumes the request's ?t= token on the Host it arrived
// on (PF §7 step 6). Exactly one t is required; the consume is atomic and
// spends the token whatever the outcome. A missing, unknown, spent, expired or
// other-host token is false, alike.
func (g SessionGate) PreviewTokenValid(r *http.Request) (*http.Request, bool) {
	if g.Previews == nil {
		return r, false
	}
	ts := r.URL.Query()["t"]
	if len(ts) != 1 {
		return r, false
	}
	grant, ok := g.Previews.Consume(ts[0], r.Host)
	if !ok {
		return r, false
	}
	return r.WithContext(preview.WithGrant(r.Context(), grant)), true
}

// PreviewHost reports whether the request's Host is a preview host at all:
// one well-formed label under the preview domain, never the reserved probe
// name. Syntax only — whether the slug names an enabled port is decided after
// sign-in, by /preview/authorize, so an unauthenticated caller cannot tell a
// real slug from an invented one.
func (g SessionGate) PreviewHost(r *http.Request) bool {
	if g.Previews == nil {
		return false
	}
	_, ok := g.Previews.Slug(r.Host)
	return ok
}

// PreviewSession validates the preview cookie for the request's Host and
// attaches what it may reach. Every cookie of that name is tried, since a
// browser may send more than one; any failure is "no session", with no
// distinction the caller can see.
func (g SessionGate) PreviewSession(r *http.Request) (*http.Request, bool) {
	if g.Previews == nil {
		return r, false
	}
	for _, c := range r.Cookies() {
		if c.Name != preview.CookieName {
			continue
		}
		if t, ok := g.Previews.Session(r.Context(), c.Value, r.Host); ok {
			return r.WithContext(preview.WithTarget(r.Context(), t)), true
		}
	}
	return r, false
}

// PreviewAuthorizeURL is where a preview request with no valid preview cookie
// goes (PF §7 step 2): the UI's /preview/authorize, carrying the URL it asked
// for, rebuilt from the canonical (lowercase, port-less) preview host so the
// value authorize validates is one this gate produced.
func (g SessionGate) PreviewAuthorizeURL(r *http.Request) string {
	host := r.Host
	if g.Previews != nil {
		if slug, ok := g.Previews.Slug(r.Host); ok {
			host = g.Previews.HostFor(slug)
		}
	}
	back := "https://" + host + r.URL.RequestURI()
	return g.UIOrigin + "/preview/authorize?return=" + url.QueryEscape(back)
}

// SignInRedirect sends a navigation with no session to the sign-in page,
// carrying where it was going. The value is a relative path, so it can only
// ever bring the browser back to this origin.
func (g SessionGate) SignInRedirect(r *http.Request) string {
	return "/signin?return=" + url.QueryEscape(r.URL.RequestURI())
}
