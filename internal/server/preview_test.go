package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
// the preview front door answers a token URL with its uniform 401, which sends
// no-referrer too. The controls show the check tells a response with the
// header from one without.
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
	front := api.PreviewFrontDoor(api.SessionGate{UIOrigin: "https://drydock.test", UIHost: "drydock.test"}, previewHandlers())
	w := httptest.NewRecorder()
	front.ServeHTTP(w, httptest.NewRequest("GET", "https://a-b.drydock-preview.test/.drydock/session?t=token", nil))
	if w.Code != http.StatusUnauthorized || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("unwritten preview.session answered %d with Referrer-Policy %q; until its handler (and its own Referrer-Policy) exists, a token URL must get the front door's uniform 401, which sends no-referrer itself", w.Code, w.Header().Get("Referrer-Policy"))
	}
}

// previewDo sends one request to the real preview socket.
func (r *running) previewDo(t *testing.T, method, path, host string, hdr http.Header, body string) (*http.Response, string) {
	t.Helper()
	hr, err := http.NewRequest(method, "http://socket"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	hr.Host = host
	for k, v := range hdr {
		hr.Header[k] = v
	}
	resp, err := unixClient(r.cfg.PreviewSocket).Do(hr)
	if err != nil {
		t.Fatalf("%s %s on the preview socket: %v", method, path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// TestPreviewSocketAnswersOne401 is PF §13 step 1's done-when against the real
// server over its real sockets: every request to the preview socket is the
// same 401 — whatever path, method or Host, and whatever it carries, including
// a live session cookie, the right password, a forged preview cookie and a
// token — and no API route answers there. Each refusal has its control beside
// it: the same request on the API socket gets through.
func TestPreviewSocketAnswersOne401(t *testing.T) {
	r := start(t)
	cookie := r.signIn(t)
	const previewHost = "myapp-5173-p2mq.drydock-preview.test"
	session := http.Header{"Cookie": {"__Host-drydock=" + cookie}}
	everything := http.Header{
		"Cookie": {"__Host-drydock=" + cookie + "; drydock-preview=forged"},
		"Origin": {uiOrigin},
	}

	_, want := r.previewDo(t, "GET", "/", previewHost, nil, "")
	if !strings.Contains(want, `"code":"unauthenticated"`) {
		t.Fatalf("the preview socket's answer to GET / is %q; want the unauthenticated envelope", want)
	}
	check := func(what string, resp *http.Response, body string) {
		t.Helper()
		if resp.StatusCode != http.StatusUnauthorized || body != want {
			t.Errorf("%s on the preview socket = %d %q; want the uniform 401 %q", what, resp.StatusCode, body, want)
		}
		for _, h := range []string{"Set-Cookie", "Location", "Allow", "Content-Security-Policy"} {
			if v := resp.Header.Get(h); v != "" {
				t.Errorf("%s on the preview socket carries %s: %q", what, h, v)
			}
		}
		if resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Referrer-Policy %q, Cache-Control %q; want no-referrer, no-store", what,
				resp.Header.Get("Referrer-Policy"), resp.Header.Get("Cache-Control"))
		}
	}

	// The control, then the refusal: a live session reads the API on the API
	// socket, and the same cookie on the preview socket reads nothing — under
	// the preview host, and under the UI host too.
	if resp := r.do(t, req{method: "GET", path: "/api/auth/session", cookie: cookie}); resp.StatusCode != 200 {
		t.Fatalf("control: GET /api/auth/session on the API socket = %d", resp.StatusCode)
	}
	for _, host := range []string{previewHost, uiHost, "evil.example"} {
		resp, body := r.previewDo(t, "GET", "/api/auth/session", host, session, "")
		check("GET /api/auth/session with a live session, Host "+host, resp, body)
	}

	// The right password, the UI's Origin: a sign-in on the API socket, and
	// nothing at all on the preview socket.
	signin := fmt.Sprintf(`{"password":%q}`, password)
	resp, body := r.previewDo(t, "POST", "/api/auth/session", previewHost,
		http.Header{"Origin": {uiOrigin}, "Content-Type": {"application/json"}}, signin)
	check("a correct sign-in POST", resp, body)
	if resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: uiOrigin, body: signin}); resp.StatusCode != http.StatusNoContent {
		t.Errorf("control: the same sign-in on the API socket = %d; want 204", resp.StatusCode)
	}

	// Every declared route of both muxes, with everything a caller could
	// hold, and the paths a token or a slug would use.
	paths := []string{"/", "/signin", "/favicon.svg", "/api/typo", "/.drydock/session?t=forged",
		"/.drydock/session?t=", "/.drydock/denied", "/preview/authorize?return=%2F", "/.drydock/x"}
	for _, rt := range api.Table {
		paths = append(paths, strings.NewReplacer("{id}", "01JABCDEFGHJKMNPQRSTVWXYZ", "{name}", "TEST_X", "{lid}", "01JLOGIN").Replace(rt.Pattern))
	}
	for _, p := range paths {
		for _, m := range []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"} {
			for _, hdr := range []http.Header{nil, everything} {
				resp, body := r.previewDo(t, m, p, previewHost, hdr, "{}")
				check(m+" "+p, resp, body)
			}
		}
	}
	// HEAD has no body on the wire; the rest of it is the same.
	resp, body = r.previewDo(t, "HEAD", "/", previewHost, everything, "")
	if resp.StatusCode != http.StatusUnauthorized || body != "" {
		t.Errorf("HEAD / on the preview socket = %d %q", resp.StatusCode, body)
	}
}
