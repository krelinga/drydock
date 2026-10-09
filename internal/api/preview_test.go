package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/auth"
	"github.com/krelinga/drydock/internal/preview"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

const (
	testPreviewDomain = "drydock-preview.test"
	testPreviewHost   = "myapp-5173-p2mq." + testPreviewDomain
	testUIOrigin      = "https://drydock.example.com"
)

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

func checkNoStore(t *testing.T, what string, h http.Header) {
	t.Helper()
	if h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
		t.Errorf("%s: Referrer-Policy %q, Cache-Control %q; want no-referrer, no-store", what,
			h.Get("Referrer-Policy"), h.Get("Cache-Control"))
	}
}

// --- the fallback, under a stub gate (§13.1's third trap) -----------------

// TestPreviewFallbackOrder is the meta-test the route table cannot give the
// fallback, since the fallback is not in the table: its order — preview host,
// then the reserved prefix, then the preview cookie, then the upstream — with
// every gate shut and opened one at a time. The upstream runs only with all
// three passed, and every refusal is Drydock's no-referrer, no-store redirect.
func TestPreviewFallbackOrder(t *testing.T) {
	ran := false
	up := preview.UpstreamFunc(func(w http.ResponseWriter, _ *http.Request, _ preview.Target) {
		ran = true
		w.Write([]byte("app"))
	})
	cases := []struct {
		name string
		g    stubGate
		path string
		want string // Location, or "" for the upstream
	}{
		{"nothing", stubGate{}, "/", preview.DeniedPath},
		{"a preview session on a host that is not a preview host", stubGate{previewSession: true}, "/", preview.DeniedPath},
		{"every API gate open, but not a preview host", stubGate{session: true, origin: true, host: true, token: true, previewSession: true}, "/", preview.DeniedPath},
		{"a preview host with no preview session", stubGate{previewHost: true, session: true, origin: true, host: true}, "/app", stubAuthorize + "/app"},
		{"the reserved prefix, signed in", allOpen, "/.drydock/nope", preview.DeniedPath},
		{"the reserved prefix's root, signed in", allOpen, "/.drydock", preview.DeniedPath},
		{"a reserved path under another method, signed in", allOpen, "/.drydock/session", preview.DeniedPath},
		{"control: all three", allOpen, "/app", ""},
	}
	for _, c := range cases {
		ran = false
		front := PreviewFrontDoor(c.g, nil, up)
		method := "GET"
		if c.path == "/.drydock/session" {
			method = "POST"
		}
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, httptest.NewRequest(method, "https://"+testPreviewHost+c.path, nil))
		if c.want == "" {
			if !ran || rec.Body.String() != "app" {
				t.Errorf("%s: the upstream did not run (%d %q)", c.name, rec.Code, rec.Body)
			}
			continue
		}
		if ran {
			t.Errorf("%s: the upstream ran", c.name)
		}
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != c.want {
			t.Errorf("%s: %d Location %q; want 302 %q", c.name, rec.Code, rec.Header().Get("Location"), c.want)
		}
		checkNoStore(t, c.name, rec.Header())
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
			fmt.Fprintf(w, "handler %s", name)
		}
	}
	front := PreviewFrontDoor(allOpen, handlers, preview.UpstreamFunc(func(w http.ResponseWriter, _ *http.Request, _ preview.Target) {
		w.Write([]byte("upstream"))
	}))
	for _, rt := range APIRoutes() {
		for _, host := range []string{testPreviewHost, "drydock.example.com"} {
			r := requestFor(rt)
			r.Host = host
			rec := httptest.NewRecorder()
			front.ServeHTTP(rec, r)
			if bytes.Contains(rec.Body.Bytes(), []byte("handler ")) {
				t.Errorf("%s %s on the preview socket (Host %s) = %d %q; an API handler answered", rt.Method, rt.Pattern, host, rec.Code, rec.Body)
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

	// Control: a written preview route is reached on the preview socket.
	rec := httptest.NewRecorder()
	front.ServeHTTP(rec, httptest.NewRequest("GET", "https://"+testPreviewHost+"/.drydock/denied", nil))
	if rec.Body.String() != "handler preview.denied" {
		t.Errorf("control: a written preview.denied answered %d %q", rec.Code, rec.Body)
	}
	// And it is still behind its gate: the token route, with the token
	// refused, never reaches its handler.
	hits["preview.session"] = false
	PreviewFrontDoor(stubGate{previewHost: true}, handlers, nil).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "https://"+testPreviewHost+"/.drydock/session?t=forged", nil))
	if hits["preview.session"] {
		t.Error("preview.session ran with its token refused: the front door mounted it without its gate")
	}
}

// TestPreviewFrontDoorHidesUnwrittenRoutes: an unwritten preview route answers
// exactly like any other reserved path, and a written route under another
// method does too. A 501 or a 405 there would be a route list for the LAN.
func TestPreviewFrontDoorHidesUnwrittenRoutes(t *testing.T) {
	front := PreviewFrontDoor(stubGate{previewHost: true}, map[string]http.HandlerFunc{
		"preview.denied": func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "denied page") },
	}, nil)
	answerFor := func(m, p string) answer {
		rec := httptest.NewRecorder()
		front.ServeHTTP(rec, httptest.NewRequest(m, "https://"+testPreviewHost+p, nil))
		return answerOf(rec)
	}
	never := answerFor("GET", "/.drydock/never-declared")
	if never.status != http.StatusFound || never.header.Get("Location") != preview.DeniedPath {
		t.Fatalf("an undeclared reserved path = %s; want 302 to the denied page", never)
	}
	for _, c := range [][2]string{{"GET", "/.drydock/session?t=x"}, {"POST", "/.drydock/denied"}, {"DELETE", "/.drydock/denied"}} {
		// Against an undeclared path under the same method: net/http writes
		// a redirect's little body for GET and HEAD only.
		if got, want := answerFor(c[0], c[1]), answerFor(c[0], "/.drydock/never-declared"); !sameAnswer(got, want) {
			t.Errorf("%s %s = %s; want the same as an undeclared path: %s", c[0], c[1], got, want)
		}
	}
	if got := answerFor("GET", "/.drydock/denied"); got.body != "denied page" {
		t.Errorf("control: GET /.drydock/denied = %s", got)
	}
}

// --- the real gate and the real service ------------------------------------

type handshakeFixture struct {
	clock    *sys.FakeClock
	db       *store.DB
	sessions *auth.Sessions
	svc      *preview.Service
	gate     SessionGate
	cookie   string // the UI session cookie
	api      http.Handler
	front    http.Handler
	seen     []*http.Request // what the upstream received
	setOnUp  []string        // Set-Cookie values the upstream sends
}

func newHandshake(t *testing.T) *handshakeFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &handshakeFixture{clock: sys.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)), db: db}
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (1, 1, 'o/myapp', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('w1', 1, '/x', 'main', 'running')`,
		`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled) VALUES ('p1', 'w1', 5173, 'myapp-5173-p2mq', 1)`,
		`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled) VALUES ('p2', 'w1', 8080, 'myapp-8080-zzzz', 1)`,
		`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled) VALUES ('p3', 'w1', 9000, 'myapp-9000-off0', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f.sessions = &auth.Sessions{DB: db.DB, Clock: f.clock, Random: rand.Reader}
	f.cookie, _, err = f.sessions.Create(ctx, "test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	f.svc = &preview.Service{DB: db.DB, Clock: f.clock, Random: rand.Reader, Domain: testPreviewDomain, AuthIdle: auth.IdleLifetime}
	f.gate = SessionGate{Sessions: f.sessions, UIOrigin: testUIOrigin, UIHost: "drydock.example.com", Previews: f.svc}
	h := PreviewHandshake{Previews: f.svc}.Handlers()
	f.api = Build(MuxAPI, f.gate, h)
	f.front = PreviewFrontDoor(f.gate, h, preview.UpstreamFunc(func(w http.ResponseWriter, r *http.Request, tg preview.Target) {
		f.seen = append(f.seen, r)
		for _, v := range f.setOnUp {
			w.Header().Add("Set-Cookie", v)
		}
		fmt.Fprintf(w, "app on port %d", tg.ContainerPort)
	}))
	return f
}

// get sends one request to the API mux (UI host) or the front door (any
// other host), with the given cookies.
func (f *handshakeFixture) get(t *testing.T, rawURL string, cookies ...*http.Cookie) answer {
	t.Helper()
	r := httptest.NewRequest("GET", rawURL, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	if r.Host == "drydock.example.com" {
		f.api.ServeHTTP(rec, r)
	} else {
		f.front.ServeHTTP(rec, r)
	}
	return answerOf(rec)
}

func (f *handshakeFixture) uiCookie() *http.Cookie {
	return &http.Cookie{Name: auth.CookieName, Value: f.cookie}
}

// authorize runs /preview/authorize for a return URL and returns its token.
func (f *handshakeFixture) authorize(t *testing.T, back string) string {
	t.Helper()
	a := f.get(t, testUIOrigin+"/preview/authorize?return="+url.QueryEscape(back), f.uiCookie())
	loc, err := url.Parse(a.header.Get("Location"))
	if a.status != http.StatusFound || err != nil || loc.Path != preview.SessionPath {
		t.Fatalf("authorize(%s) = %s; want a redirect to the session path", back, a)
	}
	return loc.Query().Get("t")
}

// previewCookie runs the whole handshake for the default host and returns the
// cookie it set.
func (f *handshakeFixture) previewCookie(t *testing.T, host string) *http.Cookie {
	t.Helper()
	tok := f.authorize(t, "https://"+host+"/")
	a := f.get(t, "https://"+host+preview.SessionPath+"?t="+tok)
	for _, c := range (&http.Response{Header: a.header}).Cookies() {
		if c.Name == preview.CookieName {
			return &http.Cookie{Name: c.Name, Value: c.Value}
		}
	}
	t.Fatalf("the session path set no preview cookie: %s", a)
	return nil
}

// TestHandshakeRedirectChain is PF §7 at the muxes: the four requests and
// their three redirects, the token bound to its host, the cookie's attributes,
// and the landing URL clean.
func TestHandshakeRedirectChain(t *testing.T) {
	f := newHandshake(t)
	start := "https://" + testPreviewHost + "/app/page?x=1&y=%2F"

	// 1 → 2: no preview cookie, to authorize on the UI origin.
	a := f.get(t, start)
	want := testUIOrigin + "/preview/authorize?return=" + url.QueryEscape(start)
	if a.status != http.StatusFound || a.header.Get("Location") != want {
		t.Fatalf("step 2: %s; want 302 %s", a, want)
	}
	checkNoStore(t, "step 2", a.header)

	// 3 → 5: authorize, signed in, to the preview host's session path.
	a = f.get(t, want, f.uiCookie())
	loc, _ := url.Parse(a.header.Get("Location"))
	if a.status != http.StatusFound || loc.Scheme != "https" || loc.Host != testPreviewHost || loc.Path != preview.SessionPath || len(loc.Query()["t"]) != 1 || len(loc.Query()) != 1 {
		t.Fatalf("step 5: %s", a)
	}
	if a.header.Get("Cache-Control") != "no-store" {
		t.Errorf("authorize's redirect carries a token and Cache-Control %q", a.header.Get("Cache-Control"))
	}
	if a.header.Get("Set-Cookie") != "" {
		t.Error("authorize set a cookie")
	}

	// 6: the session path consumes, sets the cookie, lands on the clean path.
	a = f.get(t, loc.String())
	if a.status != http.StatusFound || a.header.Get("Location") != start {
		t.Fatalf("step 6: %s; want 302 %s", a, start)
	}
	checkNoStore(t, "step 6", a.header)
	sc := (&http.Response{Header: a.header}).Cookies()
	if len(sc) != 1 {
		t.Fatalf("step 6 set %d cookies", len(sc))
	}
	c := sc[0]
	if c.Name != preview.CookieName || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 0 {
		t.Errorf("the preview cookie is %+v; want __Host-, Secure, HttpOnly, Lax, Path=/, host-only", c)
	}

	// The landing: with the cookie, the upstream, on the clean URL.
	a = f.get(t, start, &http.Cookie{Name: c.Name, Value: c.Value})
	if a.status != 200 || a.body != "app on port 5173" {
		t.Fatalf("landing: %s", a)
	}
	if got := f.seen[len(f.seen)-1].URL.RequestURI(); got != "/app/page?x=1&y=%2F" {
		t.Errorf("the upstream saw %q", got)
	}
}

// TestTokenRefusalsAreUniform: at /.drydock/session, no token, a forged one, a
// spent one, an expired one and one minted for another host all get the same
// bytes — the denied redirect with no-referrer and no-store, and no cookie.
func TestTokenRefusalsAreUniform(t *testing.T) {
	f := newHandshake(t)
	at := func(tok string) answer {
		return f.get(t, "https://"+testPreviewHost+preview.SessionPath+"?t="+url.QueryEscape(tok))
	}
	none := f.get(t, "https://"+testPreviewHost+preview.SessionPath)
	if none.status != http.StatusFound || none.header.Get("Location") != preview.DeniedPath {
		t.Fatalf("no token = %s; want the denied redirect", none)
	}
	checkNoStore(t, "no token", none.header)
	if none.header.Get("Set-Cookie") != "" {
		t.Error("a refusal set a cookie")
	}

	spent := f.authorize(t, "https://"+testPreviewHost+"/")
	if a := at(spent); a.status != http.StatusFound || a.header.Get("Set-Cookie") == "" {
		t.Fatalf("control: a fresh token = %s; want the cookie and the landing", a)
	}
	expired := f.authorize(t, "https://"+testPreviewHost+"/")
	f.clock.Advance(preview.TokenTTL)
	cases := map[string]string{
		"empty": "", "forged": "Zm9yZ2VkLXRva2Vu", "spent": spent, "expired": expired,
	}
	for name, tok := range cases {
		if got := at(tok); !sameAnswer(got, none) {
			t.Errorf("%s token = %s; want the same as no token: %s", name, got, none)
		}
	}
	other2 := f.authorize(t, "https://myapp-8080-zzzz."+testPreviewDomain+"/")
	if got := at(other2); !sameAnswer(got, none) {
		t.Errorf("another host's token = %s; want the same as no token", got)
	}
	// Two t parameters are not one token.
	twice := f.authorize(t, "https://"+testPreviewHost+"/")
	if got := f.get(t, "https://"+testPreviewHost+preview.SessionPath+"?t="+twice+"&t="+twice); !sameAnswer(got, none) {
		t.Errorf("a doubled t = %s; want the same as no token", got)
	}
}

// TestPathCleaningSaysNoReferrer is §13.1's second trap: ServeMux's 307 for an
// unclean path to a mounted route keeps the query — the token — in its
// Location, so it must carry no-referrer and no-store like every other answer.
func TestPathCleaningSaysNoReferrer(t *testing.T) {
	f := newHandshake(t)
	for _, p := range []string{"/.drydock//session?t=abc", "/.drydock/./session?t=abc", "/x/../.drydock/denied"} {
		a := f.get(t, "https://"+testPreviewHost+p)
		if a.status != http.StatusTemporaryRedirect && a.status != http.StatusMovedPermanently {
			t.Errorf("%s = %s; expected ServeMux's cleaning redirect", p, a)
			continue
		}
		checkNoStore(t, p, a.header)
	}
}

// TestPreviewCookieRefusalsAreUniform: on a preview host, no cookie, a forged
// one, one for another preview host, a revoked session's, an idle one and a
// stopped workspace's get the same bytes — the handshake's redirect. The
// control is the live cookie reaching the upstream.
func TestPreviewCookieRefusalsAreUniform(t *testing.T) {
	f := newHandshake(t)
	page := "https://" + testPreviewHost + "/page"
	none := f.get(t, page)
	live := f.previewCookie(t, testPreviewHost)
	if a := f.get(t, page, live); a.status != 200 {
		t.Fatalf("control: the live cookie = %s", a)
	}
	otherHost := f.previewCookie(t, "myapp-8080-zzzz."+testPreviewDomain)
	refusals := map[string]*http.Cookie{
		"forged":             {Name: preview.CookieName, Value: "forged"},
		"other preview host": otherHost,
		"the UI's cookie":    f.uiCookie(),
	}
	for name, c := range refusals {
		if got := f.get(t, page, c); !sameAnswer(got, none) {
			t.Errorf("%s = %s; want the same as none: %s", name, got, none)
		}
	}
	// Revoked: sign out everywhere.
	if err := f.sessions.RevokeAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, page, live); !sameAnswer(got, none) {
		t.Errorf("after revoke-all = %s; want the same as none", got)
	}
}

// TestPreviewCookieIdleAndStopped: the other two refusals, with their own
// controls.
func TestPreviewCookieIdleAndStopped(t *testing.T) {
	f := newHandshake(t)
	page := "https://" + testPreviewHost + "/page"
	none := f.get(t, page)
	c := f.previewCookie(t, testPreviewHost)
	if _, err := f.db.Exec(`UPDATE workspace SET state = 'stopped'`); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, page, c); !sameAnswer(got, none) {
		t.Errorf("stopped workspace = %s; want the same as none", got)
	}
	f.db.Exec(`UPDATE workspace SET state = 'running'`)
	if a := f.get(t, page, c); a.status != 200 {
		t.Fatalf("control: running again = %s", a)
	}
	f.clock.Advance(preview.IdleLifetime)
	if got := f.get(t, page, c); !sameAnswer(got, none) {
		t.Errorf("idle cookie = %s; want the same as none", got)
	}
}

// TestPreviewSessionTriesOneCookie: however many cookies of the preview
// cookie's name a request carries, the gate looks up one — the first — so a
// pile of forged ones costs one lookup, not one each. The live cookie behind
// a thousand forgeries is not found (the proof that only one was tried); the
// same live cookie first is (the control).
func TestPreviewSessionTriesOneCookie(t *testing.T) {
	f := newHandshake(t)
	live := f.previewCookie(t, testPreviewHost)
	var forged []*http.Cookie
	for i := 0; i < 1000; i++ {
		forged = append(forged, &http.Cookie{Name: preview.CookieName, Value: fmt.Sprintf("forged-%d", i)})
	}
	page := "https://" + testPreviewHost + "/page"
	none := f.get(t, page)
	if got := f.get(t, page, append(forged, live)...); !sameAnswer(got, none) {
		t.Errorf("the live cookie after 1,000 forged ones = %s; want the no-cookie answer: only the first may be tried", got)
	}
	if got := f.get(t, page, append([]*http.Cookie{live}, forged...)...); got.status != 200 {
		t.Errorf("control: the live cookie first = %s; want the upstream", got)
	}
}

// TestTooLongForTheHandshakeIsDenied: a preview URL longer than authorize
// will carry lands on the preview's own denied page, not on a 400 on the UI
// origin; one just short of the limit runs the handshake, and authorize mints
// for it (the control that the two limits agree).
func TestTooLongForTheHandshakeIsDenied(t *testing.T) {
	f := newHandshake(t)
	prefix := "https://" + testPreviewHost + "/p?q="
	fits := prefix + strings.Repeat("a", preview.MaxReturn-len(prefix))
	a := f.get(t, fits)
	if a.status != http.StatusFound || !strings.HasPrefix(a.header.Get("Location"), testUIOrigin+"/preview/authorize?") {
		t.Fatalf("a URL of exactly MaxReturn = %d %q; want the handshake", a.status, a.header.Get("Location"))
	}
	if b := f.get(t, a.header.Get("Location"), f.uiCookie()); b.status != http.StatusFound || !strings.Contains(b.header.Get("Location"), preview.SessionPath) {
		t.Fatalf("control: authorize refused the longest URL the front door sends it: %d", b.status)
	}
	a = f.get(t, fits+"a")
	if a.status != http.StatusFound || a.header.Get("Location") != preview.DeniedPath {
		t.Errorf("a URL one byte over = %d %q; want the denied page", a.status, a.header.Get("Location"))
	}
	checkNoStore(t, "too long", a.header)
}

// TestAuthorizeRefusesOpenRedirects: return must be an https URL on exactly
// one preview host; everything else is a 400 with no Location and no token.
func TestAuthorizeRefusesOpenRedirects(t *testing.T) {
	f := newHandshake(t)
	for _, ret := range []string{
		"", "/", "//evil.example/", "https://evil.example/", "http://" + testPreviewHost + "/",
		"https://" + testPreviewHost + ".evil.example/", "https://" + testPreviewHost + "@evil.example/",
		"https://evil.example/" + testPreviewHost, "https://" + testPreviewHost + ":8443/",
		"https://drydock.example.com/", "https://drydock-check." + testPreviewDomain + "/",
		"https://a.b." + testPreviewDomain + "/", "https://" + testPreviewHost + "\\@evil.example/",
		"javascript:alert(document.cookie)", "https:/\\evil.example",
	} {
		a := f.get(t, testUIOrigin+"/preview/authorize?return="+url.QueryEscape(ret), f.uiCookie())
		if a.status != http.StatusBadRequest || a.header.Get("Location") != "" || f.svc.Pending() != 0 {
			t.Errorf("return=%q = %s (pending %d); want a 400 and no token", ret, a, f.svc.Pending())
		}
	}
	// Two return parameters are refused, not chosen between.
	a := f.get(t, testUIOrigin+"/preview/authorize?return="+url.QueryEscape("https://"+testPreviewHost+"/")+"&return=https://evil.example/", f.uiCookie())
	if a.status != http.StatusBadRequest {
		t.Errorf("two returns = %s; want 400", a)
	}
	// Control: the same request with a good return mints.
	f.authorize(t, "https://"+testPreviewHost+"/")
}

// TestAuthorizeSendsTheUnpreviewableToDenied: a well-formed preview host whose
// slug is unknown, or whose workspace is stopped, lands on its own denied page
// with no token minted; and the denied page says nothing about which. (A
// disabled or retired port's host is TestAuthorizeClearsASpentPreview's.)
func TestAuthorizeSendsTheUnpreviewableToDenied(t *testing.T) {
	f := newHandshake(t)
	var pages []string
	for _, h := range []string{"nosuch-1-abcd." + testPreviewDomain} {
		a := f.get(t, testUIOrigin+"/preview/authorize?return="+url.QueryEscape("https://"+h+"/x"), f.uiCookie())
		if a.status != http.StatusFound || a.header.Get("Location") != "https://"+h+preview.DeniedPath || f.svc.Pending() != 0 {
			t.Errorf("%s: %s; want its denied page and no token", h, a)
		}
		d := f.get(t, "https://"+h+preview.DeniedPath)
		pages = append(pages, d.String())
		if d.status != http.StatusForbidden {
			t.Errorf("%s's denied page = %d", h, d.status)
		}
		if d.header.Get("Clear-Site-Data") != "" {
			t.Errorf("%s's denied page clears the site", h)
		}
	}
	f.db.Exec(`UPDATE workspace SET state = 'stopped'`)
	a := f.get(t, testUIOrigin+"/preview/authorize?return="+url.QueryEscape("https://"+testPreviewHost+"/"), f.uiCookie())
	if a.header.Get("Location") != "https://"+testPreviewHost+preview.DeniedPath {
		t.Errorf("a stopped workspace's preview = %s; want denied", a)
	}
	pages = append(pages, f.get(t, "https://"+testPreviewHost+preview.DeniedPath).String())
	for _, p := range pages[1:] {
		if p != pages[0] {
			t.Errorf("the denied page differs between causes:\n%s\n%s", p, pages[0])
		}
	}
	for _, leak := range []string{"5173", "9000", "myapp", "nosuch", "w1", "drydock.example.com"} {
		if strings.Contains(pages[0], leak) {
			t.Errorf("the denied page says %q", leak)
		}
	}
}

// TestAuthorizeClearsASpentPreview is PF §10.3's countermeasure: a signed-in
// device sent to a switched-off or retired port's host lands on that host's
// /.drydock/session with a token that clears the site — Clear-Site-Data on the
// denied page itself, no preview cookie set — while an enabled port on a
// stopped workspace (not a revocation) gets the plain denied page. The
// clearing token is spent like any other: shown twice, the second is the
// uniform refusal, with no Clear-Site-Data.
func TestAuthorizeClearsASpentPreview(t *testing.T) {
	f := newHandshake(t)
	ctx := context.Background()
	// Control first: the enabled port starts a session, clearing nothing.
	tok := f.authorize(t, "https://"+testPreviewHost+"/")
	if a := f.get(t, "https://"+testPreviewHost+preview.SessionPath+"?t="+tok); a.status != http.StatusFound || a.header.Get("Clear-Site-Data") != "" {
		t.Fatalf("control: an enabled port's session = %s", a)
	}

	spent := map[string]func(){
		"myapp-9000-off0." + testPreviewDomain: func() {}, // born disabled
		"myapp-5173-p2mq." + testPreviewDomain: func() {
			if _, err := f.svc.SetEnabled(ctx, "w1", "p1", false); err != nil {
				t.Fatal(err)
			}
		},
		"myapp-8080-zzzz." + testPreviewDomain: func() {
			if err := f.svc.Retire(ctx, "w1", "p2"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for h, spend := range spent {
		spend()
		tok := f.authorize(t, "https://"+h+"/x")
		a := f.get(t, "https://"+h+preview.SessionPath+"?t="+tok)
		// Origin-scoped types only: "cookies" (or "*") would clear every
		// cookie of the registrable domain — every other preview's session.
		if a.status != http.StatusForbidden || a.header.Get("Clear-Site-Data") != `"cache", "storage"` {
			t.Errorf("%s: %s; want 403 with Clear-Site-Data: \"cache\", \"storage\"", h, a)
		}
		if a.body != deniedPage {
			t.Errorf("%s: the clearing answer is not the denied page: %q", h, a.body)
		}
		if len((&http.Response{Header: a.header}).Cookies()) != 0 {
			t.Errorf("%s: the clearing answer set a cookie: %v", h, a.header)
		}
		checkNoStore(t, h, a.header)
		again := f.get(t, "https://"+h+preview.SessionPath+"?t="+tok)
		if again.status != http.StatusFound || again.header.Get("Location") != preview.DeniedPath || again.header.Get("Clear-Site-Data") != "" {
			t.Errorf("%s: a spent clearing token = %s; want the uniform refusal", h, again)
		}
	}
	var n int
	f.db.QueryRow(`SELECT count(*) FROM preview_session`).Scan(&n)
	if n != 0 {
		t.Errorf("%d preview sessions remain after the disable and the retire", n)
	}

	// A stopped workspace's enabled port is not spent.
	if _, err := f.svc.SetEnabled(ctx, "w1", "p1", true); err != nil {
		t.Fatal(err)
	}
	f.db.Exec(`UPDATE workspace SET state = 'stopped'`)
	a := f.get(t, testUIOrigin+"/preview/authorize?return="+url.QueryEscape("https://"+testPreviewHost+"/"), f.uiCookie())
	if a.header.Get("Location") != "https://"+testPreviewHost+preview.DeniedPath || f.svc.Pending() != 0 {
		t.Errorf("a stopped workspace's enabled port = %s; want the plain denied page", a)
	}
}

// TestAuthorizeWithoutSessionGoesToSignIn: the AuthRedirect gate, carrying the
// whole authorize URL so sign-in comes back to it.
func TestAuthorizeWithoutSessionGoesToSignIn(t *testing.T) {
	f := newHandshake(t)
	u := "/preview/authorize?return=" + url.QueryEscape("https://"+testPreviewHost+"/a?b=c")
	a := f.get(t, testUIOrigin+u)
	if a.status != http.StatusFound || a.header.Get("Location") != "/signin?return="+url.QueryEscape(u) || f.svc.Pending() != 0 {
		t.Errorf("no session = %s; want sign-in carrying %s", a, u)
	}
}

// TestUpstreamNeverSeesThePreviewCookie is PF §7's warning box at the muxes:
// the preview cookie is gone from what the upstream receives, and a Set-Cookie
// claiming its name is dropped on the way back — while the app's own cookies
// pass both ways (the control).
func TestUpstreamNeverSeesThePreviewCookie(t *testing.T) {
	f := newHandshake(t)
	c := f.previewCookie(t, testPreviewHost)
	f.setOnUp = []string{"app-session=up; Path=/", preview.CookieName + "=hijacked; Path=/; Secure; HttpOnly", "theme=dark"}
	r := httptest.NewRequest("GET", "https://"+testPreviewHost+"/", nil)
	r.Header.Set("Cookie", "app-session=a1; "+preview.CookieName+"="+c.Value+"; theme=light")
	rec := httptest.NewRecorder()
	f.front.ServeHTTP(rec, r)
	if rec.Code != 200 || len(f.seen) == 0 {
		t.Fatalf("control: the request did not reach the upstream: %d", rec.Code)
	}
	got := f.seen[len(f.seen)-1].Header.Get("Cookie")
	if strings.Contains(got, c.Value) || strings.Contains(got, preview.CookieName) {
		t.Errorf("the upstream saw Cookie %q", got)
	}
	if got != "app-session=a1; theme=light" {
		t.Errorf("the upstream saw Cookie %q; want the app's two", got)
	}
	set := rec.Result().Header.Values("Set-Cookie")
	if len(set) != 2 || set[0] != "app-session=up; Path=/" || set[1] != "theme=dark" {
		t.Errorf("Set-Cookie to the browser = %q; want the app's two and no %s", set, preview.CookieName)
	}
	// And the device's session survived the attempt.
	if a := f.get(t, "https://"+testPreviewHost+"/", c); a.status != 200 {
		t.Errorf("the preview cookie stopped working after the upstream tried to replace it: %s", a)
	}
}
