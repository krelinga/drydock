package preview

import (
	"net/http"
)

// Limit bounds how many requests the preview socket serves at once (PF §10.7,
// §11): every request, whatever it is — the handshake's redirects, Drydock's
// own pages and the proxied app alike — and an upgraded connection for as
// long as it stays open, since the handler serving it does not return until
// it closes. An unauthenticated redirect flood is one of the things it
// bounds, so it sits outside the front door, not inside the proxy.
//
// Past the cap a request is refused at once with a plain 503 rather than
// queued: a queue would hold the flood's connections open for it.
func Limit(max int, h http.Handler) http.Handler {
	if max <= 0 {
		max = DefaultMaxConnections
	}
	slots := make(chan struct{}, max)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
		default:
			noStore(w)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Connection", "close")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("Drydock is serving as many preview connections as it allows. Try again in a moment.\n"))
			return
		}
		defer func() { <-slots }()
		h.ServeHTTP(w, r)
	})
}
