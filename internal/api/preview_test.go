package api

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testPreviewHost = "myapp-5173-p2mq.drydock-preview.test"

// answer is everything about a response a caller could tell apart.
type answer struct {
	status int
	header http.Header
	body   string
}

func (a answer) String() string {
	return fmt.Sprintf("%d %v %q", a.status, a.header, a.body)
}

func answerOf(rec *httptest.ResponseRecorder) answer {
	return answer{rec.Code, rec.Result().Header, rec.Body.String()}
}

func sameAnswer(a, b answer) bool {
	if a.status != b.status || a.body != b.body || len(a.header) != len(b.header) {
		return false
	}
	for k, v := range a.header {
		if strings.Join(v, "\x00") != strings.Join(b.header[k], "\x00") {
			return false
		}
	}
	return true
}

// previewProbes is every request shape the front door must answer alike: each
// route of both muxes under its own method and others, paths of every kind,
// and every credential a caller might hold — none, the UI's session cookie, a
// forged preview cookie, a token in ?t=, and the lot together.
func previewProbes() []*http.Request {
	paths := []string{"/", "/index.html", "/favicon.ico", "/api", "/api/", "/api/typo",
		"/.drydock", "/.drydock/", "/.drydock/session", "/.drydock/session?t=forged-token",
		"/.drydock/denied", "/.drydock/nope", "/preview/authorize?return=/", "/signin",
		"//api/auth/session", "/a/../api/auth/session", "/%2e%2e/api/repos", "/assets/app.js"}
	for _, rt := range Table {
		paths = append(paths, requestFor(rt).URL.RequestURI())
	}
	creds := []func(*http.Request){
		func(*http.Request) {},
		func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "__Host-drydock", Value: "a-real-looking-session"})
		},
		func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "drydock-preview", Value: "forged"}) },
		func(r *http.Request) {
			r.Header.Set("Origin", "https://drydock.test")
			r.Header.Set("Authorization", "Bearer x")
			r.AddCookie(&http.Cookie{Name: "__Host-drydock", Value: "a-real-looking-session"})
			q := r.URL.Query()
			q.Set("t", "spent-token")
			r.URL.RawQuery = q.Encode()
		},
	}
	var out []*http.Request
	for _, p := range paths {
		for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
			for _, c := range creds {
				r := httptest.NewRequest(m, "https://"+testPreviewHost+p, strings.NewReader(`{"password":"x"}`))
				r.Host = testPreviewHost
				c(r)
				out = append(out, r)
			}
		}
	}
	// A request addressed to another host entirely: the preview socket
	// answers it alike too (Caddy never sends one; a LAN client might).
	for _, h := range []string{"drydock.test", "evil.example", ""} {
		r := httptest.NewRequest("GET", "/api/auth/session", nil)
		r.Host = h
		out = append(out, r)
	}
	return out
}

// TestPreviewFrontDoorAnswersEverythingAlike is PF §13 step 1's done-when, at
// the mux: with no preview handler written, every request to the preview
// socket — whatever it asks for and whatever it carries — gets one 401, byte
// for byte. It runs under a gate that grants everything, so the 401 is not a
// gate refusing: it is the absence of anything to reach.
func TestPreviewFrontDoorAnswersEverythingAlike(t *testing.T) {
	for name, g := range map[string]Gate{"every gate open": allOpen, "every gate shut": stubGate{}} {
		h := PreviewFrontDoor(g, nil)
		var first answer
		n := 0
		for _, r := range previewProbes() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			got := answerOf(rec)
			if n == 0 {
				first = got
			}
			n++
			if r.Method == "HEAD" {
				got.body = first.body // net/http drops a HEAD body on the wire
			}
			if !sameAnswer(got, first) {
				t.Errorf("%s: %s %s answered\n  %s\nwant the same as every other request:\n  %s", name, r.Method, r.URL, got, first)
			}
		}
		if first.status != http.StatusUnauthorized {
			t.Errorf("%s: the uniform answer is %d; want 401", name, first.status)
		}
		if !strings.Contains(first.body, `"code":"unauthenticated"`) {
			t.Errorf("%s: the uniform answer is not the unauthenticated envelope: %s", name, first.body)
		}
		for k, want := range map[string]string{"Referrer-Policy": "no-referrer", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
			if first.header.Get(k) != want {
				t.Errorf("%s: %s = %q; want %q", name, k, first.header.Get(k), want)
			}
		}
		for _, k := range []string{"Content-Security-Policy", "Set-Cookie", "Allow", "Location", "Access-Control-Allow-Origin"} {
			if v := first.header.Get(k); v != "" {
				t.Errorf("%s: the uniform answer carries %s: %q", name, k, v)
			}
		}
		if n < 1000 {
			t.Fatalf("only %d probes: the sweep is not covering what it claims", n)
		}
	}
}

// TestPreviewFrontDoorReachesNoAPIHandler hands the front door a handler for
// every route of both muxes. It mounts the preview routes and nothing else:
// no API handler runs, whatever the request. The control is the same handlers
// and the same requests on the API mux, where every one of them runs — so the
// refusals are about which socket, not a handler that never could.
func TestPreviewFrontDoorReachesNoAPIHandler(t *testing.T) {
	hits := map[string]bool{}
	handlers := map[string]http.HandlerFunc{}
	for _, rt := range Table {
		name := rt.Name
		handlers[name] = func(w http.ResponseWriter, _ *http.Request) {
			hits[name] = true
			w.Header().Set("Referrer-Policy", "no-referrer")
			fmt.Fprintf(w, "handler %s", name)
		}
	}
	front := PreviewFrontDoor(allOpen, handlers)
	for _, rt := range APIRoutes() {
		for _, host := range []string{testPreviewHost, "drydock.example.com"} {
			r := requestFor(rt)
			r.Host = host
			rec := httptest.NewRecorder()
			front.ServeHTTP(rec, r)
			if rec.Code != http.StatusUnauthorized || bytes.Contains(rec.Body.Bytes(), []byte("handler ")) {
				t.Errorf("%s %s on the preview socket (Host %s) = %d %q; want the uniform 401", rt.Method, rt.Pattern, host, rec.Code, rec.Body)
			}
		}
	}
	for name := range hits {
		if name != "preview.session" && name != "preview.denied" {
			t.Errorf("the API handler %s ran on the preview socket", name)
		}
	}

	// Control: the API mux, same handlers, same gate, runs each one.
	apiMux := Build(MuxAPI, allOpen, handlers)
	for _, rt := range APIRoutes() {
		hits[rt.Name] = false
		apiMux.ServeHTTP(httptest.NewRecorder(), requestFor(rt))
		if !hits[rt.Name] {
			t.Errorf("control: %s did not run on the API mux either", rt.Name)
		}
	}

	// Control: a written preview route is reached on the preview socket —
	// so the 401s above come from a door that opens, not one that cannot.
	r := httptest.NewRequest("GET", "https://"+testPreviewHost+"/.drydock/denied", nil)
	rec := httptest.NewRecorder()
	front.ServeHTTP(rec, r)
	if rec.Body.String() != "handler preview.denied" {
		t.Errorf("control: a written preview.denied answered %d %q", rec.Code, rec.Body)
	}
	// And it is still behind its gate: the token route, with the token
	// refused, never reaches its handler.
	hits["preview.session"] = false
	PreviewFrontDoor(stubGate{}, handlers).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "https://"+testPreviewHost+"/.drydock/session?t=forged", nil))
	if hits["preview.session"] {
		t.Error("preview.session ran with its token refused: the front door mounted it without its gate")
	}
}

// TestPreviewFrontDoorHidesUnwrittenRoutes: once one preview route is written,
// the other — unwritten — answers exactly like a path that was never declared,
// and a written route under another method does too. A 501 or a 405 there
// would be a route list for anyone on the LAN.
func TestPreviewFrontDoorHidesUnwrittenRoutes(t *testing.T) {
	front := PreviewFrontDoor(allOpen, map[string]http.HandlerFunc{
		"preview.denied": func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "denied page") },
	})
	answerFor := func(m, p string) answer {
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, httptest.NewRequest(m, "https://"+testPreviewHost+p, nil))
		return answerOf(rec)
	}
	never := answerFor("GET", "/never-declared")
	if never.status != http.StatusUnauthorized {
		t.Fatalf("an undeclared path = %d; want 401", never.status)
	}
	for _, c := range [][2]string{{"GET", "/.drydock/session?t=x"}, {"POST", "/.drydock/denied"}, {"DELETE", "/.drydock/denied"}} {
		if got := answerFor(c[0], c[1]); !sameAnswer(got, never) {
			t.Errorf("%s %s = %s; want the same as an undeclared path: %s", c[0], c[1], got, never)
		}
	}
	// Control: the written route itself is served.
	if got := answerFor("GET", "/.drydock/denied"); got.body != "denied page" {
		t.Errorf("control: GET /.drydock/denied = %s", got)
	}
}
