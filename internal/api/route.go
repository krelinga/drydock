// Package api declares Drydock's HTTP surface as data.
//
// The route table in this file is the whole surface of both muxes. It exists as
// a slice rather than a sequence of mux.HandleFunc calls for one reason, which
// is testing-plan §5.1: http.ServeMux cannot be enumerated, and every security
// property Drydock has at the front door is a property of *every* route rather
// than of any particular one. A table can be walked by a test; a pile of
// registration calls can only be read by a human who remembers to.
//
// The consequence worth stating plainly: a route added in a later phase is
// covered by the auth, Origin, CORS and mux-separation meta-tests without
// anyone editing a test. That is the testing form of design §13.5's "protected
// by forgetting to think about it, not by remembering".
package api

import "net/http"

// Mux names the listener a route is served on. The two are separate processes'
// worth of trust: MuxAPI is the operator's own origin, MuxPreview is an origin
// that serves code out of a repository and must never reach an API handler
// (port forwarding §10.2, and the meta-test in route_test.go).
type Mux uint8

const (
	// MuxAPI is /run/drydock/http.sock — the UI, the REST API, the SSE stream.
	MuxAPI Mux = iota
	// MuxPreview is /run/drydock/preview.sock — repo dev servers, proxied.
	MuxPreview
)

func (m Mux) String() string {
	switch m {
	case MuxAPI:
		return "api"
	case MuxPreview:
		return "preview"
	}
	return "unknown"
}

// Auth is what happens to a request with no valid session cookie.
//
// Three values, not two, and the third one is a finding rather than a
// convenience: design §13.5 says "no unauthenticated route except the sign-in
// POST", and testing §5.1 turns that into "every entry except POST
// /api/auth/session returns 401 with no cookie". But GET /preview/authorize
// (port forwarding §7 step 3) is a *top-level browser navigation* that requires
// a session and, when there is none, has to land on the sign-in page carrying
// ?return= — a 401 there would show the operator a bare error instead of a
// sign-in form. So the invariant is not "everything but one returns 401"; it is
// "everything but one is gated before its handler runs", and the gate has two
// legal shapes.
type Auth uint8

const (
	// AuthRequired: no cookie, no handler, 401 with the error envelope.
	// The default, and what every API route should be unless there is a
	// written reason here.
	AuthRequired Auth = iota
	// AuthNone: reachable with no credential at all. On MuxAPI exactly one
	// route may carry this — the sign-in POST — and a meta-test enforces
	// that count.
	AuthNone
	// AuthRedirect: no cookie, no handler, 302 to the sign-in page with the
	// requested URL in ?return=. For top-level navigations only, where the
	// caller is a browser following a link rather than a fetch().
	AuthRedirect
	// AuthPreviewToken: gated by the single-use token in ?t=, consumed with
	// an atomic compare-and-delete (PF §7). Not a session gate at all — the
	// session cookie is host-only and cannot reach a preview origin, which
	// is the entire reason the handshake exists. Kept distinct from AuthNone
	// so "ungated" and "gated by something that is not the session" are not
	// the same word.
	AuthPreviewToken
)

func (a Auth) String() string {
	switch a {
	case AuthRequired:
		return "required"
	case AuthNone:
		return "none"
	case AuthRedirect:
		return "redirect"
	case AuthPreviewToken:
		return "preview-token"
	}
	return "unknown"
}

// Route is one entry of the declared surface.
//
// Handler is nil for a route that a later phase will implement. That is
// deliberate: the table is the complete contract from the first commit, so it
// can be reviewed against design §5 as a list, and Build mounts a 501 for the
// gaps. Crucially the auth gate still wraps those, because otherwise an
// unauthenticated caller could tell an unimplemented route (501) from a
// nonexistent one (404) and read the API surface off a server it cannot use.
type Route struct {
	// Method is a single uppercase HTTP method.
	Method string
	// Pattern is a net/http ServeMux pattern. Note design §5 writes path
	// parameters as `:id` while Go's mux spells them `{id}`; the Go form is
	// authoritative here and the doc form appears in Doc below.
	Pattern string
	// Mux is which listener serves it.
	Mux Mux
	// Auth is the gate applied before Handler runs.
	Auth Auth
	// Mutating marks a state-changing route, which additionally gets the
	// exact-match, fail-closed Origin check from design §13.3. It is not
	// derived from Method: POST /api/repos/refresh mutates, and a future
	// POST that only reads would not.
	Mutating bool
	// Name is a stable identifier for tests and for the error envelope's
	// `code` field. It never changes once shipped, even if the path does.
	Name string
	// Doc is the §5 spelling of the path, kept so the table can be diffed
	// against the design document by eye.
	Doc string
	// Handler is nil in the declared Table; Build fills it from the handler
	// map, keyed by Name, and mounts a 501 where none is supplied.
	Handler http.HandlerFunc
}

// Table is the complete declared HTTP surface of both muxes.
//
// Ordering follows design §5 so the two can be read side by side. Everything
// here is AuthRequired unless a comment says why not.
var Table = []Route{
	// ---- session auth (design §5, §13.2) --------------------------------
	{
		Method: "POST", Pattern: "/api/auth/session", Mux: MuxAPI,
		// The only unauthenticated route in the system. It is also the
		// rate-limited and lockout-guarded one (§13.2), which is not a
		// coincidence: being reachable without a credential is exactly
		// what makes those necessary.
		Auth: AuthNone, Mutating: true,
		Name: "auth.session.create", Doc: "POST /api/auth/session",
	},
	{
		Method: "GET", Pattern: "/api/auth/session", Mux: MuxAPI,
		// Authenticated, despite §5's prose saying the three auth routes
		// are exempt — see the note in route_test.go. This returns the
		// device list, so an unauthenticated 200 here would hand out the
		// operator's signed-in devices; and the frontend depends on it
		// answering 401 when signed out (frontend §2.2, §4.4).
		Auth: AuthRequired,
		Name: "auth.session.read", Doc: "GET /api/auth/session",
	},
	{
		Method: "DELETE", Pattern: "/api/auth/session", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "auth.session.delete", Doc: "DELETE /api/auth/session",
	},

	// ---- repositories ----------------------------------------------------
	{
		Method: "GET", Pattern: "/api/repos", Mux: MuxAPI,
		Auth: AuthRequired,
		Name: "repos.list", Doc: "GET /api/repos",
	},
	{
		Method: "POST", Pattern: "/api/repos/refresh", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "repos.refresh", Doc: "POST /api/repos/refresh",
	},

	// ---- workspaces ------------------------------------------------------
	{
		Method: "GET", Pattern: "/api/workspaces", Mux: MuxAPI,
		// Every workspace with a row, newest first. Not in §5's first
		// table: the home list was to come from GET /api/repos' join, but
		// a workspace whose repository the installation dropped (§12), or
		// a second workspace for one repository, has nowhere to appear in
		// that join.
		Auth: AuthRequired,
		Name: "workspaces.list", Doc: "GET /api/workspaces",
	},
	{
		Method: "POST", Pattern: "/api/workspaces", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "workspaces.create", Doc: "POST /api/workspaces",
	},
	{
		Method: "GET", Pattern: "/api/workspaces/{id}", Mux: MuxAPI,
		Auth: AuthRequired,
		Name: "workspaces.read", Doc: "GET /api/workspaces/:id",
	},
	{
		Method: "DELETE", Pattern: "/api/workspaces/{id}", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "workspaces.delete", Doc: "DELETE /api/workspaces/:id",
	},
	{
		Method: "POST", Pattern: "/api/workspaces/{id}/start", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "workspaces.start", Doc: "POST /api/workspaces/:id/start",
	},
	{
		Method: "POST", Pattern: "/api/workspaces/{id}/stop", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "workspaces.stop", Doc: "POST /api/workspaces/:id/stop",
	},
	{
		Method: "POST", Pattern: "/api/workspaces/{id}/rebuild", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "workspaces.rebuild", Doc: "POST /api/workspaces/:id/rebuild",
	},
	{
		Method: "POST", Pattern: "/api/workspaces/{id}/config-approval", Mux: MuxAPI,
		// Design §6: approve the host-access request a stopped workspace is
		// waiting on, by the hash the operator was shown, and continue its
		// run. Settled by config.approved.
		Auth: AuthRequired, Mutating: true,
		Name: "workspaces.approve", Doc: "POST /api/workspaces/:id/config-approval",
	},
	{
		Method: "DELETE", Pattern: "/api/workspaces/{id}/config-approval", Mux: MuxAPI,
		// Decline it: the request is dropped, the workspace stays stopped.
		// Settled by the workspace.state event that carries no request.
		Auth: AuthRequired, Mutating: true,
		Name: "workspaces.decline", Doc: "DELETE /api/workspaces/:id/config-approval",
	},
	{
		Method: "POST", Pattern: "/api/workspaces/{id}/supervisor", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "workspaces.supervisor", Doc: "POST /api/workspaces/:id/supervisor",
	},
	{
		Method: "GET", Pattern: "/api/workspaces/{id}/logs", Mux: MuxAPI,
		// Frontend §4.5 #4: the supervisor's ring buffer is in process
		// memory by design (§8), so the UI has no other way in. Redacted,
		// never persisted.
		Auth: AuthRequired,
		Name: "workspaces.logs", Doc: "GET /api/workspaces/:id/logs?tail=n",
	},
	{
		Method: "GET", Pattern: "/api/workspaces/{id}/build-log", Mux: MuxAPI,
		// Design §12, *Image build fails*: the last 50 lines of the latest
		// failed `devcontainer up`, held in memory, redacted, never
		// persisted — the detail view's, beside the step's sentence.
		Auth: AuthRequired,
		Name: "workspaces.build_log", Doc: "GET /api/workspaces/:id/build-log",
	},

	// ---- secrets ---------------------------------------------------------
	{
		Method: "GET", Pattern: "/api/secrets", Mux: MuxAPI,
		// Names, reach, grants, last access. Never values — there is no
		// route shape that returns one, and no column behind it (§4).
		Auth: AuthRequired,
		Name: "secrets.list", Doc: "GET /api/secrets",
	},
	{
		Method: "PUT", Pattern: "/api/secrets/{name}", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		// Create-or-replace; with If-None-Match: * a create that refuses a
		// stored name (412 secret_exists). One route either way, so every
		// meta-test covers both.
		Name: "secrets.put", Doc: "PUT /api/secrets/:name",
	},
	{
		Method: "DELETE", Pattern: "/api/secrets/{name}", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "secrets.delete", Doc: "DELETE /api/secrets/:name",
	},
	{
		Method: "PUT", Pattern: "/api/secrets/{name}/grants", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "secrets.grants.put", Doc: "PUT /api/secrets/:name/grants",
	},

	// ---- Claude identity and the login handshake (design §7.2, §7.3) ----
	{
		Method: "GET", Pattern: "/api/auth/claude", Mux: MuxAPI,
		Auth: AuthRequired,
		Name: "claude.identity.read", Doc: "GET /api/auth/claude",
	},
	{
		Method: "POST", Pattern: "/api/auth/claude/check", Mux: MuxAPI,
		// Check the login now rather than at the next six-hourly poll
		// (§7.3): after fixing something from a shell, or to see a login
		// made in a workspace's terminal. Joins a check already running.
		Auth: AuthRequired, Mutating: true,
		Name: "claude.identity.check", Doc: "POST /api/auth/claude/check",
	},
	{
		Method: "POST", Pattern: "/api/auth/claude/login", Mux: MuxAPI,
		Auth: AuthRequired, Mutating: true,
		Name: "claude.login.begin", Doc: "POST /api/auth/claude/login",
	},
	{
		Method: "POST", Pattern: "/api/auth/claude/login/{lid}/code", Mux: MuxAPI,
		// Carries the one-time login code. Redact-by-default applies to
		// this request's body everywhere it could be written down: the
		// event log, an error string, a panic dump (Spike 01, §13.5).
		Auth: AuthRequired, Mutating: true,
		Name: "claude.login.code", Doc: "POST /api/auth/claude/login/:lid/code",
	},
	{
		Method: "DELETE", Pattern: "/api/auth/claude/login/{lid}", Mux: MuxAPI,
		// §7.2's cancel: a wedged or abandoned login must never need a
		// restart of Drydock. Kills the process and removes the container.
		Auth: AuthRequired, Mutating: true,
		Name: "claude.login.cancel", Doc: "DELETE /api/auth/claude/login/:lid",
	},

	// ---- the event stream ------------------------------------------------
	{
		Method: "GET", Pattern: "/api/events", Mux: MuxAPI,
		// Needs id: on every event, Last-Event-ID replay from a bounded
		// window, a resync event, and a ~20s heartbeat (frontend §4.5 #1).
		Auth: AuthRequired,
		Name: "events.stream", Doc: "GET /api/events",
	},

	// ---- the preview handshake's main-origin half (PF §6, §7) -----------
	{
		Method: "GET", Pattern: "/preview/authorize", Mux: MuxAPI,
		// AuthRedirect, not AuthRequired: this is step 3 of the handshake,
		// a cross-site top-level navigation carrying the session cookie
		// because it is SameSite=Lax. With no session the operator must
		// land on the sign-in page with ?return= rather than on a 401
		// (PF §7). Not Mutating despite minting a token: it is a GET
		// reached by browser navigation, so an Origin check would refuse
		// the very request it exists to serve — the token is single-use,
		// 60-second, and bound to both the session and the one preview
		// host, which is what carries the safety here instead.
		Auth: AuthRedirect,
		Name: "preview.authorize", Doc: "GET /preview/authorize",
	},

	// ---- the preview mux: exactly two routes, and no API ----------------
	{
		Method: "GET", Pattern: "/.drydock/session", Mux: MuxPreview,
		// Consumes the one-time token, sets the host-only preview cookie,
		// redirects to the originally requested path. Also the one place
		// that must answer Referrer-Policy: no-referrer, because the
		// token is in the query string (PF §7). The preview server has no
		// SecurityHeaders, so the handler must set it itself;
		// server.TestPreviewSessionSendsNoReferrer fails the day a handler
		// is written without it (security review F4).
		Auth: AuthPreviewToken,
		Name: "preview.session", Doc: "GET /.drydock/session",
	},
	{
		Method: "GET", Pattern: "/.drydock/denied", Mux: MuxPreview,
		// The human-readable dead end, and genuinely ungated: it has to
		// be reachable precisely when nothing else is. What it *says*
		// still varies by what the caller holds — "not signed in" rather
		// than naming a workspace state — so the gate being open is not a
		// licence for the page to be chatty.
		Auth: AuthNone,
		Name: "preview.denied", Doc: "GET /.drydock/denied",
	},
}

// APIRoutes and PreviewRoutes partition Table by listener.
func APIRoutes() []Route     { return routesFor(MuxAPI) }
func PreviewRoutes() []Route { return routesFor(MuxPreview) }

func routesFor(m Mux) []Route {
	var out []Route
	for _, r := range Table {
		if r.Mux == m {
			out = append(out, r)
		}
	}
	return out
}
