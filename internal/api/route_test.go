package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubGate lets a test choose, per request, which gates pass. Every field
// defaults to the hostile answer so a test that forgets to grant something
// fails closed rather than passing for the wrong reason.
type stubGate struct {
	session bool
	origin  bool
	host    bool
	token   bool
}

func (g stubGate) Authenticate(r *http.Request) (*http.Request, bool) { return r, g.session }
func (g stubGate) OriginAllowed(*http.Request) bool                   { return g.origin }
func (g stubGate) HostAllowed(*http.Request) bool                     { return g.host }
func (g stubGate) PreviewTokenValid(*http.Request) bool               { return g.token }
func (g stubGate) SignInRedirect(r *http.Request) string {
	return "/signin?return=" + r.URL.Path
}

// allOpen is every gate satisfied: a signed-in operator on the right host with
// the right Origin. It is the positive control for every refusal test below —
// testing-plan §4.1, which is the rule that keeps these from passing against a
// server that refuses everything.
var allOpen = stubGate{session: true, origin: true, host: true, token: true}

// reachedHandler marks a route's handler as having run, so a test can assert
// "no handler was reached" rather than inferring it from a status code.
func reachedHandler(hit *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*hit = true
		w.WriteHeader(http.StatusOK)
	}
}

// requestFor builds a request that matches a route's pattern, substituting a
// plausible value for every path parameter.
func requestFor(rt Route) *http.Request {
	p := rt.Pattern
	p = strings.ReplaceAll(p, "{id}", "01JABCDEFGHJKMNPQRSTVWXYZ")
	p = strings.ReplaceAll(p, "{name}", "TEST_DATABASE_URL")
	p = strings.ReplaceAll(p, "{lid}", "01JLOGINLOGINLOGINLOGIN")
	req := httptest.NewRequest(rt.Method, "https://drydock.example.com"+p, nil)
	req.Host = "drydock.example.com"
	return req
}

// serve drives one route through a mux built with the given gate, and reports
// the response plus whether the route's own handler ran.
func serve(t *testing.T, rt Route, g Gate) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	var hit bool
	probe := rt
	probe.Handler = reachedHandler(&hit)
	// Build a mux containing just this route, so one route's pattern cannot
	// shadow another's and quietly change what is under test.
	mux := http.NewServeMux()
	mux.Handle(probe.Method+" "+probe.Pattern, wrap(probe, g))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestFor(probe))
	return rec, hit
}

// --- the table itself -------------------------------------------------------

func TestTableIsWellFormed(t *testing.T) {
	if len(Table) == 0 {
		t.Fatal("route table is empty: every assertion below would pass vacuously")
	}
	seenName := map[string]bool{}
	seenRoute := map[string]bool{}
	for _, rt := range Table {
		if rt.Name == "" || rt.Doc == "" {
			t.Errorf("%s %s: Name and Doc are required", rt.Method, rt.Pattern)
		}
		if seenName[rt.Name] {
			t.Errorf("duplicate Name %q: codes must be unique, the UI switches on them", rt.Name)
		}
		seenName[rt.Name] = true

		key := rt.Mux.String() + " " + rt.Method + " " + rt.Pattern
		if seenRoute[key] {
			t.Errorf("duplicate route %s", key)
		}
		seenRoute[key] = true

		if rt.Method != strings.ToUpper(rt.Method) {
			t.Errorf("%s: method must be uppercase", key)
		}
		// Design §5: mutations are never GET. SameSite=Lax leans on this —
		// it sends the cookie on cross-site top-level GETs, so a mutating
		// GET would be reachable from a hostile page by link alone.
		if rt.Mutating && rt.Method == "GET" {
			t.Errorf("%s: a mutating route must not be GET (§5, §13.2)", key)
		}
	}
}

// TestOnlySignInIsUnauthenticated is design §13.5's "no unauthenticated route
// except the sign-in POST", asserted over the table rather than over a
// hand-kept list of paths.
func TestOnlySignInIsUnauthenticated(t *testing.T) {
	var open []string
	for _, rt := range APIRoutes() {
		if rt.Auth == AuthNone {
			open = append(open, rt.Method+" "+rt.Pattern)
		}
	}
	if len(open) != 1 || open[0] != "POST /api/auth/session" {
		t.Errorf("API routes reachable with no credential = %v; want exactly [POST /api/auth/session]", open)
	}
}

// TestNoRouteRunsWithoutItsGate is the assertion that actually protects a route
// added in a later phase: with every gate refused, no handler anywhere runs.
//
// It deliberately asserts on *whether the handler ran* rather than on a status
// code. A 403 with the handler already having had its side effect is the bug
// this shape catches and a status assertion does not.
func TestNoRouteRunsWithoutItsGate(t *testing.T) {
	for _, rt := range Table {
		rt := rt
		t.Run(rt.Name, func(t *testing.T) {
			if rt.Auth == AuthNone {
				t.Skip("deliberately ungated; covered by TestOnlySignInIsUnauthenticated")
			}
			_, hit := serve(t, rt, stubGate{}) // every gate refused
			if hit {
				t.Errorf("handler ran with no session, no Origin, no host, no token")
			}
			// The positive control: the same route, every gate satisfied,
			// must reach its handler. Without this the test above would
			// pass against a mux that refuses everything.
			_, hit = serve(t, rt, allOpen)
			if !hit {
				t.Errorf("handler did NOT run with every gate satisfied: the refusal above proves nothing")
			}
		})
	}
}

// TestUnauthenticatedCallerCannotProbeTheSurface is the ordering inside wrap():
// auth comes before the not-implemented reply, so a caller with no session
// cannot tell a declared-but-unimplemented route from a nonexistent one.
func TestUnauthenticatedCallerCannotProbeTheSurface(t *testing.T) {
	for _, rt := range APIRoutes() {
		if rt.Auth != AuthRequired {
			continue
		}
		rt := rt
		t.Run(rt.Name, func(t *testing.T) {
			// Note: no Handler is set, so this route is "unimplemented".
			mux := http.NewServeMux()
			mux.Handle(rt.Method+" "+rt.Pattern, wrap(rt, stubGate{host: true, origin: true}))
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, requestFor(rt))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d; want 401. A 501 here leaks which routes exist", rec.Code)
			}
			if strings.Contains(rec.Body.String(), rt.Name) {
				t.Errorf("body names the route (%q), which is the same leak by another path", rt.Name)
			}
		})
	}
}

// TestEveryMutatingRouteChecksOrigin is §13.3's belt behind SameSite.
func TestEveryMutatingRouteChecksOrigin(t *testing.T) {
	for _, rt := range Table {
		if !rt.Mutating {
			continue
		}
		rt := rt
		t.Run(rt.Name, func(t *testing.T) {
			// Signed in and on the right host, but the Origin is wrong,
			// a lookalike, or absent — the gate makes no distinction and
			// neither does this test, because fail-closed means all three
			// land identically.
			g := stubGate{session: true, host: true, token: true, origin: false}
			rec, hit := serve(t, rt, g)
			if hit {
				t.Errorf("handler ran with a refused Origin")
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d; want 403", rec.Code)
			}
			// Positive control.
			if _, hit := serve(t, rt, allOpen); !hit {
				t.Errorf("handler did not run with an allowed Origin")
			}
		})
	}
}

// TestNoRouteEmitsCORS is the other half of §13.5: the API emits no permissive
// or Origin-reflecting CORS, on success or on any refusal path.
func TestNoRouteEmitsCORS(t *testing.T) {
	gates := map[string]Gate{
		"all gates open":  allOpen,
		"all gates shut":  stubGate{},
		"origin refused":  stubGate{session: true, host: true, token: true},
		"host refused":    stubGate{session: true, origin: true, token: true},
		"session refused": stubGate{origin: true, host: true, token: true},
	}
	banned := []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Headers",
	}
	sawAResponse := false
	for name, g := range gates {
		for _, rt := range Table {
			rec, _ := serve(t, rt, g)
			sawAResponse = true
			for _, h := range banned {
				if v := rec.Header().Get(h); v != "" {
					t.Errorf("%s: %s %s emitted %s: %q", name, rt.Method, rt.Pattern, h, v)
				}
			}
		}
	}
	if !sawAResponse {
		t.Fatal("no responses were examined")
	}
}

// TestPreviewMuxServesNoAPI is port-forwarding §10.2 and testing §5.1: the
// preview origin serves repository code, so an API pattern reaching a handler
// there would put the control plane on an origin a repo's dev server controls.
func TestPreviewMuxServesNoAPI(t *testing.T) {
	previewMux := Build(MuxPreview, allOpen, nil)
	for _, rt := range APIRoutes() {
		rec := httptest.NewRecorder()
		previewMux.ServeHTTP(rec, requestFor(rt))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the preview mux = %d; want 404", rt.Method, rt.Pattern, rec.Code)
		}
	}
	// And the converse, so the assertion above is not just "the preview mux
	// 404s everything".
	var got []string
	for _, rt := range PreviewRoutes() {
		got = append(got, rt.Method+" "+rt.Pattern)
	}
	want := []string{"GET /.drydock/session", "GET /.drydock/denied"}
	if len(got) != len(want) {
		t.Fatalf("preview route set = %v; want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("preview route set = %v; want exactly %v", got, want)
			break
		}
	}
}

// TestAPIMuxServesNoPreviewRoute is the same separation from the other side.
func TestAPIMuxServesNoPreviewRoute(t *testing.T) {
	apiMux := Build(MuxAPI, allOpen, nil)
	for _, rt := range PreviewRoutes() {
		rec := httptest.NewRecorder()
		apiMux.ServeHTTP(rec, requestFor(rt))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s on the API mux = %d; want 404", rt.Method, rt.Pattern, rec.Code)
		}
	}
}

// TestForeignHostIsRefusedIndependently is the second of the three gates in
// Fig 4: Caddy turns away a foreign Host first, and Drydock turns it away
// again, so the defense does not live in one config file.
func TestForeignHostIsRefusedIndependently(t *testing.T) {
	for _, rt := range APIRoutes() {
		rt := rt
		t.Run(rt.Name, func(t *testing.T) {
			g := stubGate{session: true, origin: true, token: true, host: false}
			rec, hit := serve(t, rt, g)
			if hit {
				t.Errorf("handler ran for a foreign Host")
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d; want 403", rec.Code)
			}
		})
	}
}

// TestAuthRedirectLandsOnSignIn covers the third Auth shape, which exists for
// the preview handshake's step 3 (PF §7): a browser following a cross-site
// top-level navigation with no session must see a sign-in page carrying
// ?return=, not a 401 it cannot act on.
func TestAuthRedirectLandsOnSignIn(t *testing.T) {
	var found bool
	for _, rt := range Table {
		if rt.Auth != AuthRedirect {
			continue
		}
		found = true
		rec, hit := serve(t, rt, stubGate{host: true, origin: true})
		if hit {
			t.Errorf("%s: handler ran with no session", rt.Name)
		}
		if rec.Code != http.StatusFound {
			t.Errorf("%s: status = %d; want 302", rt.Name, rec.Code)
		}
		if loc := rec.Header().Get("Location"); !strings.Contains(loc, "return=") {
			t.Errorf("%s: Location = %q; want a ?return= so the operator comes back", rt.Name, loc)
		}
	}
	if !found {
		t.Skip("no AuthRedirect routes declared")
	}
}
