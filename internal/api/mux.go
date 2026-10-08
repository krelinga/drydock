package api

import (
	"fmt"
	"net/http"
)

// Gate is the policy a mux applies before any handler runs. It is an interface
// so the meta-tests can drive the whole route table with a stub, and so Phase 1
// can implement sessions and Origin checking without this file changing.
//
// Each method answers "may this request proceed?" and, if not, writes the
// refusal itself — because the refusal differs by policy (401 with an envelope
// for a fetch, 302 to sign-in for a navigation) and only the gate knows which.
type Gate interface {
	// Authenticate reports whether the request carries a valid session and,
	// if so, returns it with the session attached to its context, so a
	// handler that needs the session (the device list, sign-out) does not
	// look it up a second time.
	Authenticate(r *http.Request) (*http.Request, bool)
	// OriginAllowed reports whether a state-changing request's Origin is
	// the literal UI origin. Must fail closed on an absent Origin and must
	// never accept a suffix match (§13.3, §13.5).
	OriginAllowed(r *http.Request) bool
	// HostAllowed reports whether the Host header is the configured UI
	// hostname. Caddy refuses a foreign Host first; this is the second,
	// independent check so the defense does not live in one config file
	// (§13.3).
	HostAllowed(r *http.Request) bool
	// PreviewTokenValid consumes the single-use ?t= token, atomically, and
	// returns the request with the token's grant attached (PF §7).
	PreviewTokenValid(r *http.Request) (*http.Request, bool)
	// PreviewHost reports whether the Host is a preview host by syntax
	// alone: one label under the preview domain, not a reserved name.
	PreviewHost(r *http.Request) bool
	// PreviewSession validates the host-only preview cookie for this Host
	// and returns the request with its resolved target attached.
	PreviewSession(r *http.Request) (*http.Request, bool)
	// PreviewAuthorizeURL is where a preview request with no valid preview
	// cookie is sent: /preview/authorize on the UI origin, with ?return=.
	PreviewAuthorizeURL(r *http.Request) string
	// SignInRedirect is where an AuthRedirect route sends a caller with no
	// session, with the original URL carried in ?return=.
	SignInRedirect(r *http.Request) string
}

// Build mounts every route for one listener, wrapping each in the gate.
//
// The ordering inside the wrapper is the part that matters, and it is not
// arbitrary:
//
//  1. Host, because a rebound request should be refused before anything reads
//     its cookie.
//  2. Auth, because an unauthenticated caller must not be able to tell an
//     implemented route from an unimplemented one. If the 501 for a nil
//     Handler came first, the API would be a route oracle for anyone who can
//     reach the socket.
//  3. Origin, for mutating routes.
//  4. The handler, or 501.
func Build(m Mux, g Gate, handlers map[string]http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	for _, rt := range routesFor(m) {
		if h, ok := handlers[rt.Name]; ok {
			rt.Handler = h
		}
		mux.Handle(rt.Method+" "+rt.Pattern, wrap(rt, g))
	}
	return mux
}

func wrap(rt Route, g Gate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. Host. The preview mux is addressed by a wildcard hostname per
		// workspace-port, so its host check belongs to the preview proxy's
		// slug resolution rather than to a fixed allowlist.
		if rt.Mux == MuxAPI && !g.HostAllowed(r) {
			WriteError(w, http.StatusForbidden, CodeForbiddenHost,
				"This request was not addressed to Drydock.", "")
			return
		}

		// 2. Auth, before the not-implemented reply.
		switch rt.Auth {
		case AuthRequired:
			authed, ok := g.Authenticate(r)
			if !ok {
				WriteError(w, http.StatusUnauthorized, CodeUnauthenticated,
					"Sign in to continue.", "")
				return
			}
			r = authed
		case AuthRedirect:
			authed, ok := g.Authenticate(r)
			if !ok {
				http.Redirect(w, r, g.SignInRedirect(r), http.StatusFound)
				return
			}
			r = authed
		case AuthPreviewToken:
			authed, ok := g.PreviewTokenValid(r)
			if !ok {
				// A spent, expired, forged or other-host token is
				// indistinguishable from no token on purpose: all land on
				// the same dead end rather than telling the caller which.
				// The request carried a token in its URL, so the refusal
				// says no-referrer and no-store as every other answer to
				// a token URL does (PF §7, §13.1's second trap).
				previewDeny(w, r)
				return
			}
			r = authed
		case AuthNone:
			// Deliberately open. The meta-tests bound how many of these
			// may exist on the API mux.
		}

		// 3. Origin, exact match, fail closed.
		if rt.Mutating && !g.OriginAllowed(r) {
			WriteError(w, http.StatusForbidden, CodeForbiddenOrigin,
				"This request did not come from Drydock's own origin.", "")
			return
		}

		// 4. The handler, or an honest 501 for a route a later phase owns.
		if rt.Handler == nil {
			WriteError(w, http.StatusNotImplemented, CodeNotImplemented,
				"This route is not implemented yet.",
				fmt.Sprintf("route %s is declared but has no handler", rt.Name))
			return
		}
		rt.Handler(w, r)
	})
}
