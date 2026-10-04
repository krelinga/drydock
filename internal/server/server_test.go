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
	"syscall"
	"testing"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
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
	c.UIOrigin, c.UIHost, c.PreviewDomain = uiOrigin, uiHost, "drydock-preview.test"
	c.DatabasePath = filepath.Join(dir, "drydock.db")
	c.APISocket = filepath.Join(dir, "run", "http.sock")
	c.PreviewSocket = filepath.Join(dir, "run", "preview.sock")
	c.SocketGroup = g.Name
	c.LabelPrefix = "drydock.test.server"
	return c
}

func start(t *testing.T) *running {
	t.Helper()
	cfg := testConfig(t, t.TempDir())
	srv, err := New(context.Background(), cfg, sys.Production())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return &running{cfg: cfg, srv: srv, client: unixClient(cfg.APISocket)}
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
