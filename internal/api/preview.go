package api

import "net/http"

// PreviewFrontDoor is everything the preview socket serves (port forwarding
// §3, §13 step 1).
//
// It mounts only the preview routes that have a written handler, each behind
// the gate exactly as Build mounts it, and answers every other request — any
// path, any method, any Host, any cookie, any ?t= — with PreviewUnauthorized.
// Three things follow, and each is the point rather than a side effect:
//
//   - **No API route is reachable**, because the routes it can mount are
//     PreviewRoutes() and nothing else: a handler map that names every API
//     route mounts none of them here. The separation is the socket, not a
//     branch in a handler (PF §3).
//   - **An unwritten preview route is not a 501.** On the API socket a
//     declared-but-unbuilt route answers 501 behind its gate, which is honest
//     to a signed-in operator. The preview origin has no signed-in operator
//     yet — the preview cookie is step 2's — so a 501 there would only tell
//     anyone on the LAN which /.drydock/ paths exist. Unwritten, a route is
//     indistinguishable from any other path.
//   - **The answer is uniform.** A spent, expired or forged token, a forged
//     preview cookie, the UI's own session cookie (which a browser never
//     sends here, but a LAN client can) and no credential at all get the same
//     status, headers and bytes, so a response says nothing about which was
//     tried (PF §7).
//
// What replaces the fallback later is fixed by PF §7: step 2 turns it into the
// redirect to /preview/authorize for a request with no valid preview cookie,
// and step 3 puts the proxy behind that cookie. Until /preview/authorize can
// mint a token, a redirect to it would be a redirect to a 501, so step 1
// answers 401 — the done-when of PF §13's first row.
func PreviewFrontDoor(g Gate, handlers map[string]http.HandlerFunc) http.Handler {
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
		// A pattern is returned only for a mounted route (or the redirect
		// that cleans a path into one). An unmatched path or method gets
		// the fallback, never ServeMux's own 404 or 405 — those would list
		// what is mounted.
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		PreviewUnauthorized(w)
	})
}

// PreviewUnauthorized is the preview socket's one refusal: 401 and the API's
// own unauthenticated envelope, byte for byte, so nothing on a preview origin
// distinguishes one refusal from another.
//
// Two headers beyond WriteError's, because the request may have carried a
// one-time token in its query string (PF §7): no-referrer, so a page loaded
// next cannot learn the URL, and no-store, so neither can a cache. Deliberately
// no Content-Security-Policy and no framing rule: the preview origin's headers
// are the previewed app's own (PF §9), and this answer stands where the app
// will.
func PreviewUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	WriteError(w, http.StatusUnauthorized, CodeUnauthenticated, "Sign in to continue.", "")
}
