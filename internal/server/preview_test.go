package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krelinga/drydock/internal/api"
)

// sendsNoReferrer reports whether h's response says Referrer-Policy:
// no-referrer.
func sendsNoReferrer(h http.HandlerFunc) bool {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "https://a-b.drydock-preview.test/.drydock/session?t=token", nil))
	return w.Result().Header.Get("Referrer-Policy") == "no-referrer"
}

// TestPreviewSessionSendsNoReferrer is the note beside previewHandlers, made
// a test so it cannot be forgotten. /.drydock/session carries the single-use
// preview token in its URL, and the preview server has no SecurityHeaders, so
// its handler must send Referrer-Policy: no-referrer itself (security review
// F4, PF §7). Until that handler is written, the route must stay unreachable:
// the gate, which fails closed, sends every token to /.drydock/denied. The controls show the
// check tells a response with the header from one without.
func TestPreviewSessionSendsNoReferrer(t *testing.T) {
	if sendsNoReferrer(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusFound) }) {
		t.Fatal("control: a handler without the header passed the check")
	}
	if !sendsNoReferrer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(http.StatusFound)
	}) {
		t.Fatal("control: a handler with the header failed the check")
	}

	declared := false
	for _, rt := range api.PreviewRoutes() {
		declared = declared || rt.Name == "preview.session"
	}
	if !declared {
		t.Fatal("the route table no longer declares preview.session: move this obligation to wherever the token is consumed now")
	}

	h, written := previewHandlers()["preview.session"]
	if written {
		if !sendsNoReferrer(h) {
			t.Fatal("the preview.session handler does not send Referrer-Policy: no-referrer; the token in its URL would leak in the next Referer")
		}
		return
	}
	mux := api.Build(api.MuxPreview, api.SessionGate{UIOrigin: "https://drydock.test", UIHost: "drydock.test"}, previewHandlers())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "https://a-b.drydock-preview.test/.drydock/session?t=token", nil))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/.drydock/denied" {
		t.Fatalf("unwritten preview.session answered %d to %q; until its handler (and its Referrer-Policy) exists, the gate must send every token to the dead end", w.Code, w.Header().Get("Location"))
	}
}
