package api

import "net/http"

// Unrouted sends a request the route table declares to apiMux, and gates one
// it does not.
//
// Left to net/http, an unknown /api path gets a plain-text 404 straight from
// the router, before any auth check: an unauthenticated caller could tell a
// declared route (401) from an undeclared one (404) and read the API surface
// off a server it cannot use — the same oracle Build's ordering closes for
// declared-but-unbuilt routes. So an undeclared path goes through the gate's
// own order: Host, then session, and only then a JSON 404 (or 405 when the
// path exists under another method). Never HTML, never the SPA.
func Unrouted(gate Gate, apiMux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := apiMux.Handler(r)
		if pattern != "" {
			apiMux.ServeHTTP(w, r)
			return
		}
		if !gate.HostAllowed(r) {
			WriteError(w, http.StatusForbidden, CodeForbiddenHost,
				"This request was not addressed to Drydock.", "")
			return
		}
		if _, ok := gate.Authenticate(r); !ok {
			WriteError(w, http.StatusUnauthorized, CodeUnauthenticated, "Sign in to continue.", "")
			return
		}
		// The mux's fallback for an unmatched request is either NotFound or,
		// when the path is declared under other methods, a 405 that sets
		// Allow. Run it into a probe to learn which, then answer in the
		// envelope.
		var probe statusProbe
		h.ServeHTTP(&probe, r)
		if probe.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", probe.header.Get("Allow"))
			WriteError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"This API route does not accept that method.", "")
			return
		}
		WriteError(w, http.StatusNotFound, CodeNotFound, "No such API route.", "")
	})
}

// statusProbe is a ResponseWriter that records the status and headers and
// discards the body.
type statusProbe struct {
	header http.Header
	status int
}

func (p *statusProbe) Header() http.Header {
	if p.header == nil {
		p.header = http.Header{}
	}
	return p.header
}
func (p *statusProbe) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}
func (p *statusProbe) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}
