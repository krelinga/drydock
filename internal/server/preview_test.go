package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/preview"
)

const (
	previewDomain = "drydock-preview.test"
	previewSlug   = "myapp-5173-p2mq"
	previewHost   = previewSlug + "." + previewDomain
)

// sendsNoReferrer reports whether h's response says Referrer-Policy:
// no-referrer.
func sendsNoReferrer(h http.HandlerFunc) bool {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", "https://"+previewHost+"/.drydock/session?t=token", nil))
	return w.Result().Header.Get("Referrer-Policy") == "no-referrer"
}

// TestPreviewSessionSendsNoReferrer is the note beside previewHandlers, made
// a test so it cannot be forgotten. /.drydock/session carries the single-use
// preview token in its URL, and the preview server has no SecurityHeaders, so
// its handler must send Referrer-Policy: no-referrer itself (security review
// F4, PF §7) — on its own, not only because the front door adds it. The
// controls show the check tells a response with the header from one without.
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
	h, written := previewHandlers(nil)["preview.session"]
	if !written {
		t.Fatal("preview.session has no handler: the handshake is unbuilt")
	}
	if !sendsNoReferrer(h) {
		t.Fatal("the preview.session handler's refusal does not send Referrer-Policy: no-referrer; the token in its URL would leak in the next Referer")
	}

	// And on its success path, called directly with a consumed grant — not
	// through the front door, which adds the header again.
	r := start(t)
	seedPreview(t, r)
	sum := sha256.Sum256([]byte(r.signIn(t)))
	g := preview.Grant{AuthSessionID: hex.EncodeToString(sum[:]), Host: previewHost, PortID: "pprev", Path: "/landing"}
	req := httptest.NewRequest("GET", "https://"+previewHost+"/.drydock/session?t=token", nil)
	w := httptest.NewRecorder()
	previewHandlers(r.srv.Previews)["preview.session"](w, req.WithContext(preview.WithGrant(req.Context(), g)))
	if w.Code != http.StatusFound || w.Header().Get("Location") != "https://"+previewHost+"/landing" || w.Header().Get("Set-Cookie") == "" {
		t.Fatalf("control: the session handler with a grant = %d %v; want the cookie and the landing", w.Code, w.Header())
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("the session handler's success sends Referrer-Policy %q, Cache-Control %q; want no-referrer, no-store",
			w.Header().Get("Referrer-Policy"), w.Header().Get("Cache-Control"))
	}
}

// previewDo sends one request to the real preview socket.
func (r *running) previewDo(t *testing.T, method, rawURL string, hdr http.Header) (*http.Response, string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	hr, err := http.NewRequest(method, "http://socket"+u.RequestURI(), nil)
	if err != nil {
		t.Fatal(err)
	}
	hr.Host = u.Host
	for k, v := range hdr {
		hr.Header[k] = v
	}
	resp, err := unixClient(r.cfg.PreviewSocket).Do(hr)
	if err != nil {
		t.Fatalf("%s %s on the preview socket: %v", method, rawURL, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// seedPreview makes one running workspace with one enabled port, as the
// registry (PF §13 step 4) will.
func seedPreview(t *testing.T, r *running) {
	t.Helper()
	// After boot reconciliation, which would otherwise mark a running row
	// with no container stopped.
	<-r.srv.reconciled
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (7, 1, 'o/myapp', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('wprev', 7, '/x', 'main', 'running')`,
		`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled) VALUES ('pprev', 'wprev', 5173, '` + previewSlug + `', 1)`,
	} {
		if _, err := r.srv.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// recordingUpstream stands in for the proxy (preview.Proxy): it records what it was
// handed and tries to set the preview cookie itself.
type recordingUpstream struct {
	mu   sync.Mutex
	seen []http.Header
}

func (u *recordingUpstream) ServePreview(w http.ResponseWriter, r *http.Request, t preview.Target) {
	u.mu.Lock()
	u.seen = append(u.seen, r.Header.Clone())
	u.mu.Unlock()
	w.Header().Add("Set-Cookie", "app-own=from-upstream; Path=/")
	w.Header().Add("Set-Cookie", preview.CookieName+"=from-upstream; Path=/; Secure; HttpOnly")
	fmt.Fprintf(w, "upstream for port %d", t.ContainerPort)
}

func cookieFrom(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestPreviewHandshakeOverTheSockets is PF §13 step 2's done-when against the
// real server over its real sockets: a signed-in device runs the handshake —
// preview socket, API socket, preview socket — and reaches the upstream with
// the preview cookie stripped; a device that is not signed in is sent to
// sign-in carrying the authorize URL, and back; and revoke-all closes it on
// the next request. Then the canary sweep (testing §4.2): the token and the
// preview cookie are in no file under the temp root, no byte of the database
// (the cookie's SHA-256 is — the control), no process's argv, and no answer
// the server gave except the two that carry them by design.
func TestPreviewHandshakeOverTheSockets(t *testing.T) {
	dir := t.TempDir()
	r := startIn(t, dir)
	up := &recordingUpstream{}
	r.srv.PreviewUpstream = up
	seedPreview(t, r)
	cookie := r.signIn(t)
	ui := http.Header{"Cookie": {"__Host-drydock=" + cookie}}
	start := "https://" + previewHost + "/app/page?x=1"

	// 1 → 2: the preview socket, no preview cookie: to authorize.
	resp, _ := r.previewDo(t, "GET", start, nil)
	authorize := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || authorize != uiOrigin+"/preview/authorize?return="+url.QueryEscape(start) {
		t.Fatalf("step 2 = %d %q", resp.StatusCode, authorize)
	}

	// Not signed in: authorize sends the device to sign-in, carrying itself.
	au, _ := url.Parse(authorize)
	resp = r.do(t, req{method: "GET", path: au.RequestURI()})
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/signin?return="+url.QueryEscape(au.RequestURI()) {
		t.Fatalf("authorize with no session = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// …and once signed in, the same URL (the one sign-in returns to) goes on.
	resp = r.do(t, req{method: "GET", path: au.RequestURI(), cookie: cookie})
	sessionURL := resp.Header.Get("Location")
	su, err := url.Parse(sessionURL)
	if resp.StatusCode != http.StatusFound || err != nil || su.Host != previewHost || su.Path != preview.SessionPath {
		t.Fatalf("step 5 = %d %q", resp.StatusCode, sessionURL)
	}
	token := su.Query().Get("t")

	// 6: the preview socket consumes it, sets the cookie, lands on the
	// clean URL.
	resp, body := r.previewDo(t, "GET", sessionURL, nil)
	pc := cookieFrom(resp, preview.CookieName)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != start || pc == nil {
		t.Fatalf("step 6 = %d %q %v", resp.StatusCode, resp.Header.Get("Location"), resp.Header.Values("Set-Cookie"))
	}
	if resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("step 6's headers: %v", resp.Header)
	}
	heard := body
	// Spent: the same URL again is the denied page's redirect.
	if resp, _ := r.previewDo(t, "GET", sessionURL, nil); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != preview.DeniedPath {
		t.Errorf("a spent token = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Landing: the upstream, with the app's cookie and without the preview
	// cookie — and the upstream's attempt to set the preview cookie dropped.
	withCookie := http.Header{"Cookie": {"app-own=browser; " + preview.CookieName + "=" + pc.Value}}
	resp, body = r.previewDo(t, "GET", start, withCookie)
	if resp.StatusCode != 200 || body != "upstream for port 5173" {
		t.Fatalf("landing = %d %q", resp.StatusCode, body)
	}
	heard += body
	up.mu.Lock()
	got := up.seen[len(up.seen)-1].Get("Cookie")
	up.mu.Unlock()
	if got != "app-own=browser" {
		t.Errorf("the upstream saw Cookie %q; want the app's own and nothing else", got)
	}
	if set := resp.Header.Values("Set-Cookie"); len(set) != 1 || !strings.HasPrefix(set[0], "app-own=") {
		t.Errorf("Set-Cookie through the proxy = %q; want the app's own only", set)
	}

	// The UI session cookie on the preview socket is not a preview session,
	// and reaches no API route.
	if resp, _ := r.previewDo(t, "GET", "https://"+previewHost+"/api/auth/session", ui); resp.StatusCode != http.StatusFound ||
		!strings.HasPrefix(resp.Header.Get("Location"), uiOrigin+"/preview/authorize?") {
		t.Errorf("the UI cookie on the preview socket = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Revoke-all closes it on the next request.
	if resp := r.do(t, req{method: "DELETE", path: "/api/auth/session?all=true", origin: uiOrigin, cookie: cookie}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("sign out everywhere = %d", resp.StatusCode)
	}
	resp, _ = r.previewDo(t, "GET", start, withCookie)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != authorize {
		t.Errorf("after revoke-all = %d %q; want the handshake again", resp.StatusCode, resp.Header.Get("Location"))
	}
	var rows int
	r.srv.DB.QueryRow(`SELECT count(*) FROM preview_session`).Scan(&rows)
	if rows != 0 {
		t.Errorf("%d preview sessions survived revoke-all", rows)
	}

	// The sweep. Stop the server first so the WAL is checkpointed into the
	// file the sweep reads, and nothing is written after it.
	r.stop()
	where := func(needle string) []string {
		var found []string
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(needle)) {
					found = append(found, p)
				}
			}
			return nil
		})
		procs, _ := os.ReadDir("/proc")
		for _, d := range procs {
			if b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "cmdline")); err == nil && bytes.Contains(b, []byte(needle)) {
				found = append(found, "/proc/"+d.Name()+"/cmdline")
			}
		}
		if strings.Contains(heard, needle) {
			found = append(found, "a response body")
		}
		return found
	}
	for name, needle := range map[string]string{"the token": token, "the preview cookie": pc.Value} {
		if found := where(needle); len(found) > 0 {
			t.Errorf("%s was written to %v", name, found)
		}
	}
	// Control: the sweep finds what is really there. A planted token is
	// found where it was planted (TestPreviewCookieIsStoredAsItsHash finds
	// the cookie's hash in the database file while its row is live).
	if err := os.WriteFile(filepath.Join(dir, "planted"), []byte("x"+token+"x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found := where(token); len(found) != 1 || !strings.HasSuffix(found[0], "planted") {
		t.Errorf("control: the sweep did not find a planted token: %v", found)
	}
}

// TestPreviewCookieIsStoredAsItsHash is the sweep's positive half: while a
// preview session is live, its SHA-256 is in the database file and the cookie
// is not.
func TestPreviewCookieIsStoredAsItsHash(t *testing.T) {
	dir := t.TempDir()
	r := startIn(t, dir)
	seedPreview(t, r)
	cookie := r.signIn(t)
	resp := r.do(t, req{method: "GET", path: "/preview/authorize?return=" + url.QueryEscape("https://"+previewHost+"/"), cookie: cookie})
	resp, _ = r.previewDo(t, "GET", resp.Header.Get("Location"), nil)
	pc := cookieFrom(resp, preview.CookieName)
	if pc == nil {
		t.Fatal("no preview cookie")
	}
	r.stop()
	b, err := os.ReadFile(filepath.Join(dir, "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(pc.Value))
	if !bytes.Contains(b, []byte(hex.EncodeToString(sum[:]))) {
		t.Error("control: the cookie's SHA-256 is not in the database file")
	}
	if bytes.Contains(b, []byte(pc.Value)) {
		t.Error("the preview cookie is in the database file")
	}
}

// TestInstallerProbeHostIsDenied pins what deploy/install.sh's final check
// expects of https://drydock-check.<preview-domain>/: a 302 to the denied
// page, never the handshake — the probe name is reserved, so no slug can ever
// be it.
func TestInstallerProbeHostIsDenied(t *testing.T) {
	r := start(t)
	resp, _ := r.previewDo(t, "GET", "https://drydock-check."+previewDomain+"/", nil)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != preview.DeniedPath {
		t.Errorf("the probe host = %d %q; want 302 %s", resp.StatusCode, resp.Header.Get("Location"), preview.DeniedPath)
	}
	// Control: a well-formed slug goes to the handshake instead.
	resp, _ = r.previewDo(t, "GET", "https://a-b."+previewDomain+"/", nil)
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), uiOrigin+"/preview/authorize?") {
		t.Errorf("control: a slug = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestPreviewSocketReachesNoAPIRoute is step 1's done-when, kept: with a live
// session, every declared API route on the preview socket — under the preview
// host, the UI host and a foreign one — is no API answer: a preview host's is
// the handshake, anything else the denied page. The control is the same
// session reading the API on the API socket.
func TestPreviewSocketReachesNoAPIRoute(t *testing.T) {
	r := start(t)
	cookie := r.signIn(t)
	if resp := r.do(t, req{method: "GET", path: "/api/auth/session", cookie: cookie}); resp.StatusCode != 200 {
		t.Fatalf("control: GET /api/auth/session on the API socket = %d", resp.StatusCode)
	}
	hdr := http.Header{"Cookie": {"__Host-drydock=" + cookie + "; " + preview.CookieName + "=forged"}, "Origin": {uiOrigin}}
	for _, rt := range api.Table {
		p := strings.NewReplacer("{id}", "01JABCDEFGHJKMNPQRSTVWXYZ", "{name}", "TEST_X", "{lid}", "01JLOGIN").Replace(rt.Pattern)
		for _, host := range []string{"a-b." + previewDomain, uiHost, "evil.example"} {
			for _, m := range []string{"GET", "POST", "DELETE"} {
				resp, body := r.previewDo(t, m, "https://"+host+p, hdr)
				loc := resp.Header.Get("Location")
				if m == "GET" && p == preview.DeniedPath {
					// The dead end itself, on any host.
					if resp.StatusCode != http.StatusForbidden {
						t.Errorf("GET %s (Host %s) = %d; want the denied page", p, host, resp.StatusCode)
					}
					continue
				}
				if resp.StatusCode != http.StatusFound || strings.Contains(body, `"devices"`) {
					t.Errorf("%s %s (Host %s) on the preview socket = %d %q", m, p, host, resp.StatusCode, body)
				}
				wantAuthorize := host == "a-b."+previewDomain && !strings.HasPrefix(p, "/.drydock")
				if wantAuthorize != strings.HasPrefix(loc, uiOrigin+"/preview/authorize?") {
					t.Errorf("%s %s (Host %s) = Location %q", m, p, host, loc)
				}
				if resp.Header.Get("Referrer-Policy") != "no-referrer" || resp.Header.Get("Cache-Control") != "no-store" {
					t.Errorf("%s %s (Host %s): Referrer-Policy %q, Cache-Control %q", m, p, host,
						resp.Header.Get("Referrer-Policy"), resp.Header.Get("Cache-Control"))
				}
			}
		}
	}
}
