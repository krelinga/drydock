package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/auth"
)

// The scope meta-tests (vscode-extension §4.3, §10.0a): with API tokens in
// play, every route of the table is walked again, by scope. Mutation-checked:
// a session route marked lifecycle fails TestNeverGrantableRoutesAreSession
// by name; a route with its scope cleared fails TestEveryAPIRouteDeclaresAScope
// by name; /preview/authorize answering a bearer (the bearer refusal removed
// from wrap's AuthRedirect case) fails TestAuthRedirectRefusesABearer by name.

// tokenWith is an API token holding exactly scopes (and self, which every
// token holds).
func tokenWith(scopes ...auth.Scope) auth.Credential {
	return auth.TokenCredential(auth.Token{ID: "tok_test", Label: "test", Scopes: scopes})
}

// tokenWithout is a token holding every grantable scope but s.
func tokenWithout(s auth.Scope) auth.Credential {
	var keep []auth.Scope
	for _, g := range auth.GrantableScopes {
		if g != s {
			keep = append(keep, g)
		}
	}
	return tokenWith(keep...)
}

func gateAs(c auth.Credential) stubGate {
	g := allOpen
	g.cred = &c
	return g
}

// validScopes is every scope a route may declare.
var validScopes = []auth.Scope{auth.ScopeRead, auth.ScopeLifecycle, auth.ScopeApprove, auth.ScopeSelf, auth.ScopeSession}

// TestEveryAPIRouteDeclaresAScope: an unset scope is closed to every token
// (protected by default), and a route left unset fails here by name, so the
// choice is always written down. Preview routes declare none: no token
// authenticates on the preview mux.
func TestEveryAPIRouteDeclaresAScope(t *testing.T) {
	n := 0
	for _, rt := range Table {
		switch rt.Mux {
		case MuxAPI:
			n++
			if !slices.Contains(validScopes, rt.Scope) {
				t.Errorf("%s declares scope %q; every API route declares one of %v", rt.Name, rt.Scope, validScopes)
			}
		case MuxPreview:
			if rt.Scope != "" {
				t.Errorf("%s is a preview route with scope %q; no token authenticates there", rt.Name, rt.Scope)
			}
		}
	}
	if n == 0 {
		t.Fatal("no API routes: the check above passed vacuously")
	}
}

// TestNeverGrantableRoutesAreSession is §4.1's list of what no token may
// reach, by route name, so a session route marked anything else fails by
// name: secrets (reads included), the token routes, the session routes and
// sign-out-everywhere, the one open route, and every AuthRedirect route.
// self belongs to the token's own revocation alone.
func TestNeverGrantableRoutesAreSession(t *testing.T) {
	sessionOnly := func(rt Route) bool {
		for _, p := range []string{"secrets.", "tokens.", "auth.session.", "auth.sessions."} {
			if strings.HasPrefix(rt.Name, p) {
				return true
			}
		}
		return rt.Auth == AuthRedirect || rt.Auth == AuthNone
	}
	var session, self []string
	for _, rt := range APIRoutes() {
		if sessionOnly(rt) {
			session = append(session, rt.Name)
			if rt.Scope != auth.ScopeSession {
				t.Errorf("%s has scope %q; it must be session — no token may reach it (vscode-extension §4.1)", rt.Name, rt.Scope)
			}
		}
		if rt.Scope == auth.ScopeSelf {
			self = append(self, rt.Name)
		}
	}
	// Controls: the list is the one §4.1 names, not an empty match.
	for _, want := range []string{"secrets.list", "secrets.put", "tokens.create", "tokens.list", "tokens.revoke",
		"auth.session.read", "auth.session.delete", "auth.sessions.delete", "preview.authorize"} {
		if !slices.Contains(session, want) {
			t.Errorf("control: %s is not among the session-only routes found (%v)", want, session)
		}
	}
	if len(self) != 1 || self[0] != "auth.token.delete" {
		t.Errorf("self-scoped routes = %v; want exactly [auth.token.delete]", self)
	}
}

// TestTokenReachesOnlyItsScopes walks every gated API route three ways: a
// token holding every scope but the route's is refused 403 forbidden_scope
// with the handler not run; a token holding only the route's scope reaches
// it — unless the scope is session, which no token can hold; and the
// operator's cookie session reaches every route but the one token-only one.
func TestTokenReachesOnlyItsScopes(t *testing.T) {
	for _, rt := range APIRoutes() {
		if rt.Auth != AuthRequired {
			continue // AuthNone has no credential; AuthRedirect: TestAuthRedirectRefusesABearer
		}
		rt := rt
		t.Run(rt.Name, func(t *testing.T) {
			// Every token holds self, so self's refusal is the operator's
			// (below).
			if rt.Scope != auth.ScopeSelf {
				rec, hit := serve(t, rt, gateAs(tokenWithout(rt.Scope)))
				if hit {
					t.Errorf("a token without %q reached the handler", rt.Scope)
				}
				if rec.Code != http.StatusForbidden || errCodeOf(rec) != CodeForbiddenScope {
					t.Errorf("a token without %q = %d %s; want 403 forbidden_scope", rt.Scope, rec.Code, errCodeOf(rec))
				}
			}
			switch rt.Scope {
			case auth.ScopeSession:
				if _, hit := serve(t, rt, gateAs(tokenWith(auth.GrantableScopes...))); hit {
					t.Errorf("a token with every grantable scope reached a session route")
				}
			default:
				if _, hit := serve(t, rt, gateAs(tokenWith(onlyGrantable(rt.Scope)...))); !hit {
					t.Errorf("a token holding %q did not reach the handler: the refusal above proves nothing", rt.Scope)
				}
			}
			rec, hit := serve(t, rt, gateAs(operator))
			if want := rt.Scope != auth.ScopeSelf; hit != want {
				t.Errorf("the operator's session reached the handler: %v; want %v (%d)", hit, want, rec.Code)
			}
			if rt.Scope == auth.ScopeSelf && errCodeOf(rec) != CodeForbiddenScope {
				t.Errorf("the operator's session on a token-only route = %d %s; want 403 forbidden_scope", rec.Code, errCodeOf(rec))
			}
		})
	}
}

// onlyGrantable is s as a token's scope list: self is every token's anyway.
func onlyGrantable(s auth.Scope) []auth.Scope {
	if s == auth.ScopeSelf {
		return nil
	}
	return []auth.Scope{s}
}

// TestUnsetScopeIsClosedToTokens: protected by default, as behaviour and not
// only as TestEveryAPIRouteDeclaresAScope's declaration — a route with its
// scope cleared refuses a token holding every scope, and the operator still
// reaches it (the control).
func TestUnsetScopeIsClosedToTokens(t *testing.T) {
	for _, rt := range APIRoutes() {
		if rt.Auth != AuthRequired {
			continue
		}
		rt.Scope = ""
		if _, hit := serve(t, rt, gateAs(fullToken)); hit {
			t.Errorf("%s with no scope reached the handler for a token", rt.Name)
		}
		if _, hit := serve(t, rt, gateAs(operator)); !hit {
			t.Errorf("control: %s with no scope did not reach the handler for the operator", rt.Name)
		}
	}
}

// TestAuthRedirectRefusesABearer: a top-level navigation route is the
// browser session's, and a bearer is refused at the auth step — the sign-in
// redirect, as for no credential at all — before scope is asked. The
// control: the operator's session reaches it.
func TestAuthRedirectRefusesABearer(t *testing.T) {
	found := false
	for _, rt := range APIRoutes() {
		if rt.Auth != AuthRedirect {
			continue
		}
		found = true
		for _, c := range []auth.Credential{fullToken, tokenWith(auth.ScopeRead)} {
			rec, hit := serve(t, rt, gateAs(c))
			if hit {
				t.Errorf("%s answered a bearer (%v)", rt.Name, c.Scopes)
			}
			if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/signin?return=") {
				t.Errorf("%s with a bearer = %d %q; want the sign-in redirect, before scope", rt.Name, rec.Code, rec.Header().Get("Location"))
			}
		}
		if _, hit := serve(t, rt, gateAs(operator)); !hit {
			t.Errorf("control: %s did not answer the operator's session", rt.Name)
		}
	}
	if !found {
		t.Fatal("no AuthRedirect route: /preview/authorize is one")
	}
}

// TestScopeRunsAfterAuthAndBeforeOriginAndThe501: an unauthenticated caller
// learns nothing of a route's scope (401, as ever); a token without the scope
// is refused for scope even when its Origin is also wrong, and on a route
// with no handler (403, not 501); a token with the scope is still held to
// the Origin check on a mutating route (§4.3 step 3), and reaches it with the
// right one.
func TestScopeRunsAfterAuthAndBeforeOriginAndThe501(t *testing.T) {
	for _, rt := range APIRoutes() {
		if rt.Auth != AuthRequired || rt.Scope == auth.ScopeSession {
			continue
		}
		rt := rt
		t.Run(rt.Name, func(t *testing.T) {
			// No credential, scope irrelevant: 401.
			if rec, _ := serve(t, rt, stubGate{host: true, origin: true}); rec.Code != http.StatusUnauthorized {
				t.Errorf("no credential = %d; want 401", rec.Code)
			}
			// No scope, wrong origin: scope answers first.
			g := gateAs(tokenWithout(rt.Scope))
			g.origin = false
			if rt.Scope != auth.ScopeSelf {
				if rec, _ := serve(t, rt, g); errCodeOf(rec) != CodeForbiddenScope {
					t.Errorf("no scope and a bad Origin = %d %s; want forbidden_scope", rec.Code, errCodeOf(rec))
				}
				// No handler: still 403, never the 501.
				mux := http.NewServeMux()
				mux.Handle(rt.Method+" "+rt.Pattern, wrap(rt, gateAs(tokenWithout(rt.Scope))))
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, requestFor(rt))
				if rec.Code != http.StatusForbidden {
					t.Errorf("no scope on an unimplemented route = %d; want 403 before the 501", rec.Code)
				}
			}
			if !rt.Mutating {
				return
			}
			holder := gateAs(tokenWith(onlyGrantable(rt.Scope)...))
			holder.origin = false
			rec, hit := serve(t, rt, holder)
			if hit || errCodeOf(rec) != CodeForbiddenOrigin {
				t.Errorf("a token with the scope and a bad Origin = %d %s (ran %v); want 403 forbidden_origin", rec.Code, errCodeOf(rec), hit)
			}
			holder.origin = true
			if _, hit := serve(t, rt, holder); !hit {
				t.Errorf("control: a token with the scope and the right Origin did not reach the handler")
			}
		})
	}
}

// TestUnroutedRefusesATokenForScope: an undeclared path declares no scope,
// so a token is refused there for scope (403) — learning nothing of which
// paths exist under other methods — while the operator gets the JSON 404
// (the control), and no credential the 401.
func TestUnroutedRefusesATokenForScope(t *testing.T) {
	for name, c := range map[string]struct {
		g    stubGate
		code int
	}{
		"token":    {gateAs(fullToken), http.StatusForbidden},
		"operator": {gateAs(operator), http.StatusNotFound},
		"nobody":   {stubGate{host: true}, http.StatusUnauthorized},
	} {
		h := Unrouted(c.g, Build(MuxAPI, c.g, nil))
		for _, path := range []string{"/api/nope", "/api/workspaces/x/nope"} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "https://drydock.example.com"+path, nil)
			req.Host = "drydock.example.com"
			h.ServeHTTP(rec, req)
			if rec.Code != c.code {
				t.Errorf("%s: GET %s = %d; want %d", name, path, rec.Code, c.code)
			}
		}
	}
}

func errCodeOf(rec *httptest.ResponseRecorder) string {
	s := rec.Body.String()
	i := strings.Index(s, `"code":"`)
	if i < 0 {
		return ""
	}
	s = s[i+len(`"code":"`):]
	return s[:strings.IndexByte(s, '"')]
}
