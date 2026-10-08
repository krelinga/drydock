package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/web"
)

const (
	uiHost   = "drydock.test"
	uiOrigin = "https://drydock.test"
	password = "correct horse battery staple"
)

type running struct {
	cfg    config.Config
	srv    *Server
	client *http.Client
	// gh is the fake GitHub, for a test that changes the installation.
	gh *githubtest.Fake
	// stop ends Serve and waits for it (startIn only).
	stop func()
}

func testConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	// Not a test of the disk: the host's own fill must never refuse a create here.
	// The disk-full tests set their own limit and inject the figures.
	c.DiskLimitPercent = 100
	c.UIOrigin, c.UIHost, c.PreviewDomain = uiOrigin, uiHost, "drydock-preview.test"
	c.DatabasePath = filepath.Join(dir, "drydock.db")
	c.APISocket = filepath.Join(dir, "run", "http.sock")
	c.PreviewSocket = filepath.Join(dir, "run", "preview.sock")
	c.SocketGroup = g.Name
	// Short: <dir>/<id>/broker.sock must fit a Unix socket's 107 bytes, and
	// t.TempDir paths named after a long test do not leave room for it.
	short, err := os.MkdirTemp("", "dds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	c.BrokerDir = filepath.Join(short, "sock")
	c.LabelPrefix = "drydock.test.server"
	c.WorkspaceRoot = filepath.Join(dir, "ws") // never the real /srv/drydock
	// Never the real login: a volume nobody creates, so the watch reports
	// absent without building the Claude image.
	c.ClaudeVolume = "drydock-test-server-no-volume"
	return c
}

func start(t *testing.T) *running {
	t.Helper()
	return startIn(t, t.TempDir())
}

// startIn is start with its state under dir, for a test that sweeps it.
func startIn(t *testing.T, dir string) *running {
	t.Helper()
	cfg := testConfig(t, dir)
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket), stop: stop}
}

func unixClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
		// The redirect is an assertion, not something to follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type req struct {
	method, path, body string
	origin, host, ip   string
	cookie             string
	// ifNoneMatch, when set, is sent as If-None-Match: the secret create.
	ifNoneMatch string
}

func (r *running) do(t *testing.T, q req) *http.Response {
	t.Helper()
	var body io.Reader
	if q.body != "" {
		body = strings.NewReader(q.body)
	}
	hr, err := http.NewRequest(q.method, "http://socket"+q.path, body)
	if err != nil {
		t.Fatal(err)
	}
	hr.Host = uiHost
	if q.host != "" {
		hr.Host = q.host
	}
	if q.origin != "" {
		hr.Header.Set("Origin", q.origin)
	}
	if q.ifNoneMatch != "" {
		hr.Header.Set("If-None-Match", q.ifNoneMatch)
	}
	ip := q.ip
	if ip == "" {
		ip = "192.0.2.10"
	}
	hr.Header.Set("X-Forwarded-For", ip) // what Caddy sets; see clientIP
	if q.cookie != "" {
		hr.AddCookie(&http.Cookie{Name: "__Host-drydock", Value: q.cookie})
	}
	resp, err := r.client.Do(hr)
	if err != nil {
		t.Fatalf("%s %s: %v", q.method, q.path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (r *running) signIn(t *testing.T) string {
	t.Helper()
	if err := r.srv.Auth.SetPassword(context.Background(), password); err != nil {
		t.Fatal(err)
	}
	resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: uiOrigin,
		body: fmt.Sprintf(`{"password":%q}`, password)})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("sign-in = %d; want 204", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "__Host-drydock" {
			return c.Value
		}
	}
	t.Fatal("no session cookie on a successful sign-in")
	return ""
}

func code(t *testing.T, resp *http.Response) string {
	t.Helper()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&e)
	return e.Error.Code
}

// ---- §8.1, end to end --------------------------------------------------------

// tcpListenInodes returns the inodes of every TCP socket in LISTEN state on the
// host, from /proc/net/tcp and tcp6 (state 0A).
func tcpListenInodes(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) > 9 && fields[3] == "0A" {
				out[fields[9]] = true
			}
		}
		fh.Close()
	}
	return out
}

// ownListeners intersects this process's open sockets with the host's TCP
// listeners: the TCP ports *this process* is listening on.
func ownListeners(t *testing.T) int {
	t.Helper()
	listen := tcpListenInodes(t)
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	n := 0
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if listen[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] {
			n++
		}
	}
	return n
}

// TestServeEndsWhenAListenerFails: Serve ends when a listener fails, with its
// context still live — and then it must stop what it started as surely as a
// cancelled context does. The loops it runs beside serving (the identity watch, the catalog, the
// supervisor's Watch) end on that context, and Serve waits for each of them
// before closing the database, so a context nobody cancelled held Serve, and
// the process, forever.
//
// The control is the same server serving: while its listener is up, Serve
// has not returned and a request is answered.
func TestServeEndsWhenAListenerFails(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}
	if resp := r.do(t, req{method: "GET", path: "/api/auth/session"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("control: GET /api/auth/session while serving: %d, want 401", resp.StatusCode)
	}
	select {
	case err := <-done:
		t.Fatalf("control: Serve returned while serving: %v", err)
	default:
	}

	srv.apiLn.Close() // the listener fails under the server
	select {
	case err := <-done:
		if err == nil {
			t.Error("Serve returned nil after its listener failed; want the listener's error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return after its listener failed, with its context never cancelled")
	}
}

// TestNoTCPListener is the first non-negotiable (§13.5), asserted on the
// running process rather than by reading the code for net.Listen calls.
func TestNoTCPListener(t *testing.T) {
	r := start(t)
	if n := ownListeners(t); n != 0 {
		t.Fatalf("the server process is listening on %d TCP socket(s)", n)
	}
	// Positive control 1: the server is genuinely up and serving.
	cookie := r.signIn(t)
	if resp := r.do(t, req{method: "GET", path: "/api/auth/session", cookie: cookie}); resp.StatusCode != 200 {
		t.Fatalf("control: signed-in GET over the socket = %d", resp.StatusCode)
	}
	// Positive control 2: the detector can see a TCP listener at all —
	// otherwise "0" above could mean it reads nothing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot open a control TCP listener: %v", err)
	}
	defer ln.Close()
	if n := ownListeners(t); n != 1 {
		t.Fatalf("the detector found %d listeners after opening one: the assertion above proves nothing", n)
	}
}

func TestSocketModeAndGroup(t *testing.T) {
	r := start(t)
	g, _ := user.LookupGroup(r.cfg.SocketGroup)
	wantGID, _ := strconv.Atoi(g.Gid)
	for _, p := range []string{r.cfg.APISocket, r.cfg.PreviewSocket} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSocket == 0 {
			t.Errorf("%s is not a socket", p)
		}
		if perm := fi.Mode().Perm(); perm != 0o660 {
			t.Errorf("%s mode = %o; want 660 (group-owned, nobody else)", p, perm)
		}
		if gid := int(fi.Sys().(*syscall.Stat_t).Gid); gid != wantGID {
			t.Errorf("%s group = %d; want %d", p, gid, wantGID)
		}
	}
	if resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: uiOrigin, body: `{}`}); resp.StatusCode == 0 {
		t.Error("control: no response through the socket")
	}
}

// TestEveryRouteIsGatedEndToEnd is Phase 1's "done when", against the real
// server and the real gate: every API route without a cookie is refused, and
// the same route with a cookie gets past the gate.
func TestEveryRouteIsGatedEndToEnd(t *testing.T) {
	r := start(t)
	cookie := r.signIn(t)
	path := func(rt api.Route) string {
		p := strings.NewReplacer("{id}", "01JABCDEFGHJKMNPQRSTVWXYZ", "{name}", "TEST_X", "{lid}", "01JL").Replace(rt.Pattern)
		return p
	}
	for _, rt := range api.APIRoutes() {
		rt := rt
		t.Run(rt.Name, func(t *testing.T) {
			anon := r.do(t, req{method: rt.Method, path: path(rt), origin: uiOrigin})
			switch rt.Auth {
			case api.AuthRequired:
				if anon.StatusCode != http.StatusUnauthorized {
					t.Errorf("no cookie = %d; want 401", anon.StatusCode)
				}
			case api.AuthRedirect:
				if anon.StatusCode != http.StatusFound || !strings.HasPrefix(anon.Header.Get("Location"), "/signin?return=") {
					t.Errorf("no cookie = %d to %q; want 302 to /signin", anon.StatusCode, anon.Header.Get("Location"))
				}
			case api.AuthNone:
				if anon.StatusCode == http.StatusUnauthorized {
					t.Errorf("the one open route answered 401")
				}
				return
			}
			// The control: with a cookie it gets past the gate. Unimplemented
			// routes answer 501; the session routes answer their own codes.
			if rt.Name == "auth.session.delete" {
				return // would sign the shared cookie out from under the other subtests
			}
			authed := r.do(t, req{method: rt.Method, path: path(rt), origin: uiOrigin, cookie: cookie})
			if authed.StatusCode == http.StatusUnauthorized || authed.StatusCode == http.StatusFound {
				t.Errorf("with a valid cookie = %d; the gate refused a signed-in request", authed.StatusCode)
			}
		})
	}
}

func TestSignInSetsTheDesignedCookie(t *testing.T) {
	r := start(t)
	r.srv.Auth.SetPassword(context.Background(), password)
	resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: uiOrigin,
		body: fmt.Sprintf(`{"password":%q}`, password)})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("sign-in = %d; want 204", resp.StatusCode)
	}
	// Exact, attribute for attribute (testing §8.1): every one of these is
	// load-bearing, and a looser match would let one quietly drop.
	want := regexp.MustCompile(`^__Host-drydock=[A-Za-z0-9_-]{43}; Path=/; Max-Age=2592000; HttpOnly; Secure; SameSite=Lax$`)
	got := resp.Header.Values("Set-Cookie")
	if len(got) != 1 || !want.MatchString(got[0]) {
		t.Errorf("Set-Cookie = %q; want one header matching %s", got, want)
	}
}

func TestSessionRoundTripAndSignOut(t *testing.T) {
	r := start(t)
	cookie := r.signIn(t)

	resp := r.do(t, req{method: "GET", path: "/api/auth/session", cookie: cookie})
	var body struct {
		Current string
		Devices []struct {
			ID        string
			IsCurrent bool `json:"is_current"`
		}
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 200 || len(body.Devices) != 1 || !body.Devices[0].IsCurrent || body.Devices[0].ID != body.Current {
		t.Fatalf("session read = %d %+v; want one device marked current", resp.StatusCode, body)
	}
	if strings.Contains(fmt.Sprint(body), cookie) {
		t.Error("the device list contains the cookie value")
	}

	out := r.do(t, req{method: "DELETE", path: "/api/auth/session", origin: uiOrigin, cookie: cookie})
	if out.StatusCode != http.StatusNoContent || !strings.Contains(out.Header.Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("sign-out = %d, Set-Cookie %q; want 204 clearing the cookie", out.StatusCode, out.Header.Get("Set-Cookie"))
	}
	if again := r.do(t, req{method: "GET", path: "/api/auth/session", cookie: cookie}); again.StatusCode != http.StatusUnauthorized {
		t.Errorf("after sign-out the old cookie = %d; want 401", again.StatusCode)
	}
}

// TestOriginAndHostAreRefusedIndependently drives §13.3's two defenses through
// the real gate on the one route that needs no cookie, where they are the
// only thing standing in front of the handler.
func TestOriginAndHostAreRefusedIndependently(t *testing.T) {
	r := start(t)
	r.srv.Auth.SetPassword(context.Background(), password)
	body := fmt.Sprintf(`{"password":%q}`, password)
	for _, c := range []struct {
		name, origin, host, code string
	}{
		{"absent origin", "", "", api.CodeForbiddenOrigin},
		{"foreign origin", "https://evil.example", "", api.CodeForbiddenOrigin},
		{"subdomain lookalike", "https://evil.drydock.test", "", api.CodeForbiddenOrigin},
		{"suffix lookalike", "https://drydock.test.evil.example", "", api.CodeForbiddenOrigin},
		{"http not https", "http://drydock.test", "", api.CodeForbiddenOrigin},
		{"trailing slash", "https://drydock.test/", "", api.CodeForbiddenOrigin},
		{"rebound host", uiOrigin, "evil.example", api.CodeForbiddenHost},
	} {
		resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: c.origin, host: c.host, body: body})
		if resp.StatusCode != http.StatusForbidden || code(t, resp) != c.code {
			t.Errorf("%s: %d; want 403 %s", c.name, resp.StatusCode, c.code)
		}
	}
	// Control: the exact origin on the right host signs in.
	if resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: uiOrigin, body: body}); resp.StatusCode != http.StatusNoContent {
		t.Errorf("control: the real origin = %d; want 204", resp.StatusCode)
	}
}

func TestLockoutOverHTTP(t *testing.T) {
	r := start(t)
	r.srv.Auth.SetPassword(context.Background(), password)
	wrong := req{method: "POST", path: "/api/auth/session", origin: uiOrigin, body: `{"password":"wrong"}`, ip: "192.0.2.66"}
	for i := 0; i < 5; i++ {
		if resp := r.do(t, wrong); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d; want 401", i+1, resp.StatusCode)
		}
	}
	resp := r.do(t, wrong)
	if resp.StatusCode != http.StatusTooManyRequests || code(t, resp) != api.CodeLockedOut {
		t.Fatalf("sixth attempt = %d; want 429 locked_out", resp.StatusCode)
	}
	if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil || ra < 1 {
		t.Errorf("Retry-After = %q; want a positive number of seconds", resp.Header.Get("Retry-After"))
	}
	// Control: another device is not locked out by this one.
	other := wrong
	other.ip = "192.0.2.77"
	if resp := r.do(t, other); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a different IP = %d; want 401 (wrong password, not locked out)", resp.StatusCode)
	}
}

// TestSecondInstanceLeavesTheFirstAlone: New opens the store before touching
// the socket paths, so a second instance fails without unlinking the socket
// the first is serving on.
func TestSecondInstanceLeavesTheFirstAlone(t *testing.T) {
	r := start(t)
	_, err := New(context.Background(), r.cfg, sys.Production())
	if !errors.Is(err, store.ErrLocked) {
		t.Fatalf("second instance = %v; want store.ErrLocked", err)
	}
	if resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: uiOrigin, body: `{}`}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("the first instance stopped answering after the second was refused: %d", resp.StatusCode)
	}
}

// TestRefusesAMixedCaseUIOriginAtStartup: a browser sends Origin with its host
// lowercased, and the Origin check is exact, so a server started with
// https://Drydock.test would refuse every sign-in as forbidden_origin while
// the case-insensitive Host check passed the same requests. It must refuse to
// start instead, naming the fix, before it opens the database or binds a
// socket. The control is the same configuration in lowercase, which starts and
// lets the browser's lowercase Origin through.
func TestRefusesAMixedCaseUIOriginAtStartup(t *testing.T) {
	for _, mixed := range []struct{ origin, host string }{
		{"https://Drydock.test", uiHost},
		{uiOrigin, "DRYDOCK.test"},
	} {
		cfg := testConfig(t, t.TempDir())
		cfg.UIOrigin, cfg.UIHost = mixed.origin, mixed.host
		_, err := New(context.Background(), cfg, sys.Production())
		if err == nil || !strings.Contains(err.Error(), "must be lowercase") {
			t.Fatalf("New(%q, %q) = %v; want a refusal saying it must be lowercase", mixed.origin, mixed.host, err)
		}
		if _, err := os.Stat(cfg.DatabasePath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused start created the database: %v", err)
		}
		if _, err := os.Stat(cfg.APISocket); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused start bound the socket: %v", err)
		}
	}

	r := start(t) // control: lowercase, as the installer now writes it
	resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: strings.ToLower(uiOrigin), body: `{}`})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("control: a lowercase Origin on a sign-in = %d; want 400 (past the Origin check, refused for its body)", resp.StatusCode)
	}
}

func TestRefusesToReplaceANonSocketFile(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	os.MkdirAll(filepath.Dir(cfg.APISocket), 0o750)
	if err := os.WriteFile(cfg.APISocket, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), cfg, sys.Production()); err == nil {
		t.Fatal("New replaced a regular file at the socket path")
	}
	if b, _ := os.ReadFile(cfg.APISocket); string(b) != "not a socket" {
		t.Error("the regular file was modified or removed")
	}
}

// ---- The UI on the API socket (frontend §3, §8) -------------------------------

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// indexAndAsset fetches index.html and pulls out the entry script it names.
func indexAndAsset(t *testing.T, r *running) (index, asset string) {
	t.Helper()
	resp := r.do(t, req{method: "GET", path: "/"})
	index = readBody(t, resp)
	m := regexp.MustCompile(`<script[^>]+src="(/assets/[^"]+\.js)"`).FindStringSubmatch(index)
	if resp.StatusCode != 200 || m == nil {
		t.Fatalf("GET / = %d with no entry script; the embedded bundle is not being served:\n%s", resp.StatusCode, index)
	}
	return index, m[1]
}

// isEnvelope returns "" when resp is the JSON error envelope with this status
// and code, and otherwise says what it was instead.
func isEnvelope(t *testing.T, resp *http.Response, status int, wantCode string) string {
	t.Helper()
	b := readBody(t, resp)
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	switch {
	case resp.StatusCode != status:
		return fmt.Sprintf("status %d; want %d (body %q)", resp.StatusCode, status, b)
	case !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json"):
		return fmt.Sprintf("Content-Type %q; want JSON (body %q)", resp.Header.Get("Content-Type"), b)
	case strings.Contains(b, "<"):
		return fmt.Sprintf("body looks like HTML: %q", b)
	case json.Unmarshal([]byte(b), &e) != nil || e.Error.Code != wantCode:
		return fmt.Sprintf("code %q; want %q (body %q)", e.Error.Code, wantCode, b)
	}
	return ""
}

func TestUIIsServedFromTheEmbeddedBundle(t *testing.T) {
	r := start(t)
	index, asset := indexAndAsset(t, r)

	idx := r.do(t, req{method: "GET", path: "/"})
	if cc := idx.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("index.html Cache-Control = %q; want no-store", cc)
	}
	if ct := idx.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("index.html Content-Type = %q", ct)
	}
	if !strings.Contains(index, `<div id="app">`) {
		t.Error("index.html is not the app's")
	}

	a := r.do(t, req{method: "GET", path: asset})
	if a.StatusCode != 200 || a.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("%s = %d, Cache-Control %q; want 200 immutable", asset, a.StatusCode, a.Header.Get("Cache-Control"))
	}
	// Negative beside the positive: the hashed asset is not index.html, and
	// index.html is not immutable.
	if strings.Contains(readBody(t, a), `<div id="app">`) {
		t.Errorf("%s was answered with index.html", asset)
	}
	if strings.Contains(idx.Header.Get("Cache-Control"), "immutable") {
		t.Error("index.html is cached as immutable")
	}
}

func TestUnknownUIPathFallsBackToIndex(t *testing.T) {
	r := start(t)
	index, _ := indexAndAsset(t, r)
	for _, p := range []string{"/settings", "/ws/01JABCDEFGHJKMNPQRSTVWXYZ/logs", "/signin?return=%2Fsettings", "/apiary"} {
		resp := r.do(t, req{method: "GET", path: p})
		if b := readBody(t, resp); resp.StatusCode != 200 || b != index || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s = %d (no-store=%v, index=%v); want index.html", p, resp.StatusCode,
				resp.Header.Get("Cache-Control") == "no-store", b == index)
		}
	}
	// Control: a hashed asset that does not exist is a 404, not index.html —
	// an old index.html asking for a previous build must not get HTML as JS.
	resp := r.do(t, req{method: "GET", path: "/assets/index-NOTABUILD.js"})
	if b := readBody(t, resp); resp.StatusCode != 404 || b == index {
		t.Errorf("missing asset = %d; want a 404 that is not index.html", resp.StatusCode)
	}
}

// TestUnknownAPIPathIsGatedAndJSON is frontend §3's rule 3 and the route-oracle
// property together: an /api path nobody declared is refused by the session
// gate first, exactly like a real route, and is never the SPA.
func TestUnknownAPIPathIsGatedAndJSON(t *testing.T) {
	r := start(t)
	cookie := r.signIn(t)
	for _, p := range []string{"/api", "/api/", "/api/typo", "/api/auth/sessions", "/api/workspaces/x/y/z"} {
		for _, m := range []string{"GET", "POST"} {
			anon := r.do(t, req{method: m, path: p, origin: uiOrigin})
			if msg := isEnvelope(t, anon, http.StatusUnauthorized, api.CodeUnauthenticated); msg != "" {
				t.Errorf("%s %s without a cookie: %s", m, p, msg)
			}
			authed := r.do(t, req{method: m, path: p, origin: uiOrigin, cookie: cookie})
			if msg := isEnvelope(t, authed, http.StatusNotFound, api.CodeNotFound); msg != "" {
				t.Errorf("%s %s with a cookie: %s", m, p, msg)
			}
		}
	}

	// A declared path under an undeclared method: gated too, then a JSON 405.
	anon := r.do(t, req{method: "PUT", path: "/api/auth/session", origin: uiOrigin})
	if msg := isEnvelope(t, anon, http.StatusUnauthorized, api.CodeUnauthenticated); msg != "" {
		t.Errorf("PUT /api/auth/session without a cookie: %s", msg)
	}
	authed := r.do(t, req{method: "PUT", path: "/api/auth/session", origin: uiOrigin, cookie: cookie})
	allow := authed.Header.Get("Allow")
	if msg := isEnvelope(t, authed, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed); msg != "" || !strings.Contains(allow, "GET") {
		t.Errorf("PUT /api/auth/session with a cookie: %s, Allow %q", msg, allow)
	}

	// Control: the declared route, same cookie, answers 200 — the 404s above
	// are about the path, not a cookie that stopped working.
	if resp := r.do(t, req{method: "GET", path: "/api/auth/session", cookie: cookie}); resp.StatusCode != 200 {
		t.Errorf("control: GET /api/auth/session = %d", resp.StatusCode)
	}
}

func TestPreviewAuthorizeNeverReachesTheSPA(t *testing.T) {
	r := start(t)
	resp := r.do(t, req{method: "GET", path: "/preview/authorize?t=x"})
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/signin?return=") {
		t.Errorf("/preview/authorize without a session = %d to %q; want 302 to /signin", resp.StatusCode, resp.Header.Get("Location"))
	}
	if strings.Contains(readBody(t, resp), `<div id="app">`) {
		t.Error("/preview/authorize was answered with index.html")
	}
	// Control: a sibling path that is not the handler is an ordinary client route.
	if resp := r.do(t, req{method: "GET", path: "/preview/other"}); resp.StatusCode != 200 {
		t.Errorf("control: /preview/other = %d; want the SPA fallback", resp.StatusCode)
	}
}

func TestSecurityHeadersOnAPIAndStaticAlike(t *testing.T) {
	r := start(t)
	_, asset := indexAndAsset(t, r)
	cookie := r.signIn(t)
	for _, c := range []struct {
		name string
		q    req
	}{
		{"index", req{method: "GET", path: "/"}},
		{"fallback", req{method: "GET", path: "/settings"}},
		{"asset", req{method: "GET", path: asset}},
		{"favicon", req{method: "GET", path: "/favicon.svg"}},
		{"api 200", req{method: "GET", path: "/api/auth/session", cookie: cookie}},
		{"api 401", req{method: "GET", path: "/api/auth/session"}},
		{"api 501", req{method: "GET", path: "/api/repos", cookie: cookie}},
		{"api 400", req{method: "POST", path: "/api/auth/session", origin: uiOrigin, body: `{}`}},
		{"api unknown 401", req{method: "GET", path: "/api/typo"}},
		{"api unknown 404", req{method: "GET", path: "/api/typo", cookie: cookie}},
		{"api 403 origin", req{method: "POST", path: "/api/auth/session", origin: "https://evil.example", body: `{}`}},
		{"static 403 host", req{method: "GET", path: "/", host: "evil.example"}},
		{"redirect", req{method: "GET", path: "/preview/authorize"}},
	} {
		resp := r.do(t, c.q)
		h := resp.Header
		if h.Get("Content-Security-Policy") != web.CSP || h.Get("Referrer-Policy") != "no-referrer" ||
			h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s (%d): CSP %q, Referrer-Policy %q, nosniff %q", c.name, resp.StatusCode,
				h.Get("Content-Security-Policy"), h.Get("Referrer-Policy"), h.Get("X-Content-Type-Options"))
		}
		if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			!strings.Contains(h.Get("Content-Security-Policy"), "frame-src 'none'") {
			t.Errorf("%s: the CSP does not forbid framing in both directions", c.name)
		}
	}
	// Control: the preview socket is a different origin serving repo code; the
	// UI's CSP is deliberately not imposed there — so the headers above come
	// from the UI socket's wrapper, not from something every response gets.
	prev := unixClient(r.cfg.PreviewSocket)
	hr, _ := http.NewRequest("GET", "http://socket/.drydock/denied", nil)
	hr.Host = "x.drydock-preview.test"
	resp, err := prev.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Security-Policy") != "" {
		t.Errorf("the preview socket carries the UI's CSP: %q", resp.Header.Get("Content-Security-Policy"))
	}
}

// TestForeignHostIsRefusedForStaticPaths: the app needs no session, but it
// still answers only to its own name — the gate's exact-Host rule (§13.3),
// applied to the bundle too, so a rebound hostname gets nothing.
func TestForeignHostIsRefusedForStaticPaths(t *testing.T) {
	r := start(t)
	_, asset := indexAndAsset(t, r)
	for _, p := range []string{"/", "/signin", "/settings", asset, "/favicon.svg", "/api/typo"} {
		for _, host := range []string{"evil.example", "evil.drydock.test", "drydock.test.evil.example"} {
			resp := r.do(t, req{method: "GET", path: p, host: host})
			if msg := isEnvelope(t, resp, http.StatusForbidden, api.CodeForbiddenHost); msg != "" {
				t.Errorf("GET %s on Host %s: %s", p, host, msg)
			}
		}
		// Control: the same path on the right Host (with a port, which Caddy
		// may pass through, and in another case) is not refused.
		for _, host := range []string{uiHost, uiHost + ":443", "DRYDOCK.TEST"} {
			resp := r.do(t, req{method: "GET", path: p, host: host})
			if resp.StatusCode == http.StatusForbidden {
				t.Errorf("control: GET %s on Host %s = 403", p, host)
			}
		}
	}
}

// TestEventStreamEndToEnd is the stream through the real gate and socket: a
// signed-in browser receives an event as it is written, and an open stream
// does not hold up shutdown.
func TestEventStreamEndToEnd(t *testing.T) {
	cfg := testConfig(t, t.TempDir())
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}
	cookie := r.signIn(t)

	resp := r.do(t, req{method: "GET", path: "/api/events", cookie: cookie})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("GET /api/events = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("the stream is missing the security headers every API response carries")
	}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	expect := func(prefix string) string {
		t.Helper()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the stream ended before %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no %q within 5s", prefix)
			}
		}
	}
	expect(": connected")
	e, err := srv.Events.Emit(context.Background(), "ws1", "info", "workspace.state", "Cloning.", map[string]string{"state": "cloning"})
	if err != nil {
		t.Fatal(err)
	}
	expect(fmt.Sprintf("id: %d", e.ID))
	if data := expect("data: "); !strings.Contains(data, `"state":"cloning"`) {
		t.Errorf("event data = %s", data)
	}

	// Shutdown with the stream still open: without closing the log first,
	// this waits out Shutdown's ten-second timeout.
	began := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s of cancel with a stream open")
	}
	if d := time.Since(began); d > 3*time.Second {
		t.Errorf("shutdown took %v with a stream open", d)
	}
}

// A database belongs to the label prefix it was created under: starting it
// under another would orphan its containers and could adopt someone else's.
func TestRefusesAChangedLabelPrefix(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	srv.apiLn.Close()
	srv.prevLn.Close()
	srv.DB.Close()

	cfg.LabelPrefix = "drydock.test.other"
	if _, err := New(context.Background(), cfg, sys.Production()); err == nil || !strings.Contains(err.Error(), "label prefix") {
		t.Errorf("New under a changed prefix: %v; want a refusal naming the prefix", err)
	}
	// Control: the original prefix still starts.
	cfg.LabelPrefix = testConfig(t, dir).LabelPrefix
	again, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatalf("control: the original prefix was refused: %v", err)
	}
	again.apiLn.Close()
	again.prevLn.Close()
	again.DB.Close()
}

// TestRepoListEndToEnd: the App configured, the catalog refreshed at boot
// from a fake GitHub, and GET /api/repos through the real gate and socket.
// Without an App the same route says so, rather than answering an empty list.
func TestRepoListEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir)

	// Control first: no App, no list — and a code that says why.
	plain := start(t)
	cookie := plain.signIn(t)
	if resp := plain.do(t, req{method: "GET", path: "/api/repos", cookie: cookie}); resp.StatusCode != 503 || code(t, resp) != api.CodeAppNotConfigured {
		t.Errorf("without an App: %d", resp.StatusCode)
	}

	f := githubtest.New(t, 5189455, time.Now)
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 1, FullName: "krelinga/drydock", DefaultBranch: "main", PushedAt: time.Now(),
			Files: []string{".devcontainer/devcontainer.json"}},
	}}}
	keyPath := filepath.Join(dir, "app.pem")
	os.WriteFile(keyPath, githubtest.KeyPEM(t), 0o400)
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI = 5189455, keyPath, f.URL

	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	r := &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}
	cookie = r.signIn(t)

	var body struct {
		Repos []struct {
			FullName        string `json:"full_name"`
			HasDevcontainer *bool  `json:"has_devcontainer"`
		}
		Installations []struct {
			SettingsURL string `json:"settings_url"`
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(body.Repos) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the boot refresh never filled the list")
		}
		time.Sleep(20 * time.Millisecond)
		resp := r.do(t, req{method: "GET", path: "/api/repos", cookie: cookie})
		if resp.StatusCode != 200 {
			t.Fatalf("GET /api/repos = %d", resp.StatusCode)
		}
		json.NewDecoder(resp.Body).Decode(&body)
	}
	if body.Repos[0].FullName != "krelinga/drydock" || body.Repos[0].HasDevcontainer == nil || !*body.Repos[0].HasDevcontainer ||
		len(body.Installations) != 1 || body.Installations[0].SettingsURL == "" {
		t.Errorf("body %+v", body)
	}
	before := f.Count("GET /app/installations")
	if resp := r.do(t, req{method: "POST", path: "/api/repos/refresh", origin: uiOrigin, cookie: cookie}); resp.StatusCode != 202 {
		t.Errorf("POST /api/repos/refresh = %d, want 202", resp.StatusCode)
	}
	for f.Count("GET /app/installations") == before {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("the manual refresh never reached GitHub")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A configured App key that others can read stops the server at startup
// (§13.5), rather than serving without the list it was configured for.
func TestRefusesAnOpenAppKey(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, dir)
	keyPath := filepath.Join(dir, "app.pem")
	os.WriteFile(keyPath, githubtest.KeyPEM(t), 0o644)
	cfg.GitHubAppID, cfg.GitHubAppKey = 5189455, keyPath
	if _, err := New(context.Background(), cfg, sys.Production()); err == nil || !strings.Contains(err.Error(), "0400") {
		t.Errorf("New with a 0644 key: %v", err)
	}
}
