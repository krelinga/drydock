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

// SessionGate is the production Gate: the session cookie or an API token,
// the exact-match Origin allowlist, and Drydock's own Host check (design
// §13.2, §13.3; vscode-extension §4.3).
type SessionGate struct {
	Sessions *auth.Sessions
	// Tokens resolves `Authorization: Bearer drydock_…`. Nil refuses every
	// bearer.
	Tokens *auth.Tokens
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
type credentialKey struct{}

// SessionFrom returns the cookie session the gate attached to a request's
// context. A request a token authenticated has none: a handler that acts on
// the browser session (the device list, sign-out, the preview handshake)
// finds nothing to act on, and its route is session-scoped besides.
func SessionFrom(ctx context.Context) (auth.Session, bool) {
	s, ok := ctx.Value(sessionKey{}).(auth.Session)
	return s, ok
}

// CredentialFrom returns the credential the gate attached: a cookie session
// or an API token, with its scopes.
func CredentialFrom(ctx context.Context) (auth.Credential, bool) {
	c, ok := ctx.Value(credentialKey{}).(auth.Credential)
	return c, ok
}

// WithCredential attaches c, as Authenticate does. For a Gate other than
// SessionGate (the meta-tests' stub), which must attach one too: wrap
// refuses an authenticated request that carries none.
func WithCredential(r *http.Request, c auth.Credential) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), credentialKey{}, c))
}

// Authenticate accepts one credential, and only one (vscode-extension §4.3):
// the session cookie, or `Authorization: Bearer drydock_…` looked up by its
// SHA-256. A request carrying the cookie and any Authorization header at all,
// whatever its scheme, is refused — an ambiguous request is not one to give
// the benefit of the doubt, as two Origin headers are not — and so are two
// Authorization headers. Any failure — none, unknown, expired, revoked, the
// store erroring — is "not signed in": the gate fails closed and never says
// which. A bearer lookup is not a sign-in attempt and never counts toward
// the lockout.
func (g SessionGate) Authenticate(r *http.Request) (*http.Request, bool) {
	authz := r.Header.Values("Authorization")
	c, cookieErr := r.Cookie(auth.CookieName)
	if len(authz) > 0 {
		if cookieErr == nil || len(authz) != 1 || g.Tokens == nil {
			return r, false
		}
		scheme, value, ok := strings.Cut(authz[0], " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return r, false
		}
		tok, err := g.Tokens.Lookup(r.Context(), value)
		if err != nil {
			return r, false
		}
		return WithCredential(r, auth.TokenCredential(tok)), true
	}
	if cookieErr != nil || c.Value == "" {
		return r, false
	}
	s, err := g.Sessions.Lookup(r.Context(), c.Value)
	if err != nil {
		return r, false
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey{}, s))
	return WithCredential(r, auth.SessionCredential(s)), true
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
// attaches what it may reach. Any failure is "no session", with no
// distinction the caller can see.
//
// Only the first cookie of that name is tried — one database lookup per
// request, however many a caller sends. A browser never sends two: the
// __Host- prefix forces Path=/ and no Domain, so a second cookie of the same
// name for the same host replaces the first rather than joining it. Trying
// each would let an unauthenticated request of ~3,000 forged cookies buy
// ~45 ms of database work (measured in review) — a cost the preview socket's
// connection cap (preview.Limit, PF §10.7) bounds in number but not in size.
func (g SessionGate) PreviewSession(r *http.Request) (*http.Request, bool) {
	if g.Previews == nil {
		return r, false
	}
	c, err := r.Cookie(preview.CookieName)
	if err != nil {
		return r, false
	}
	t, ok := g.Previews.Session(r.Context(), c.Value, r.Host)
	if !ok {
		return r, false
	}
	// The same question, asked again while an upgrade stays open: a
	// websocket has no next request for a revocation to refuse.
	svc, cookie, host := g.Previews, c.Value, r.Host
	ctx := preview.WithTarget(r.Context(), t)
	ctx = preview.WithRecheck(ctx, func(ctx context.Context) bool {
		_, ok := svc.Session(ctx, cookie, host)
		return ok
	})
	return r.WithContext(ctx), true
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
