package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/preview"
)

// loopbackResolver is Docker for the component tier: the workspace's
// container "is" this host's loopback. (internal/container refuses a
// loopback address from a real inspect; this tier's subject is the proxy.)
type loopbackResolver struct{}

func (loopbackResolver) Resolve(context.Context, string) (preview.Endpoint, error) {
	return preview.Endpoint{ContainerID: "c", IP: netip.MustParseAddr("127.0.0.1")}, nil
}
func (loopbackResolver) Confirm(context.Context, string, preview.Endpoint) error { return nil }

// devServer is a dev server for the proxy to reach. It keeps every request it
// was sent, byte for byte as Go reads it, for the canary sweep; tries to set
// the preview cookie on every answer, a 101 included; and serves an upgrade
// (hello, then an echo), an event stream, and a request that waits.
type devServer struct {
	*httptest.Server
	mu      sync.Mutex
	dumps   bytes.Buffer
	last    *http.Request
	release chan struct{}
}

func newDevServer(t *testing.T) *devServer {
	t.Helper()
	d := &devServer{release: make(chan struct{})}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dump, _ := httputil.DumpRequest(r, false)
		d.mu.Lock()
		d.dumps.Write(dump)
		d.last = r.Clone(context.Background())
		d.mu.Unlock()
		switch r.URL.Path {
		case "/hmr":
			c, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
				"Set-Cookie: " + preview.CookieName + "=planted; Path=/; Secure; HttpOnly\r\nSet-Cookie: app-ws=kept\r\n\r\nhello\n")
			brw.Flush()
			for {
				line, err := brw.ReadString('\n')
				if err != nil {
					return
				}
				brw.WriteString("echo:" + line)
				brw.Flush()
			}
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: one\n\n")
			w.(http.Flusher).Flush()
			<-d.release
			fmt.Fprint(w, "data: two\n\n")
		case "/hold":
			<-d.release
		default:
			w.Header().Add("Set-Cookie", "app-own=from-app; Path=/")
			w.Header().Add("Set-Cookie", preview.CookieName+"=from-app; Path=/; Secure; HttpOnly")
			fmt.Fprint(w, "app at "+r.URL.RequestURI())
		}
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *devServer) port() int {
	u, _ := url.Parse(d.URL)
	p, _ := strconv.Atoi(u.Port())
	return p
}

func (d *devServer) lastRequest() *http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// seedPreviewAt is seedPreview with the port the dev server listens on.
func seedPreviewAt(t *testing.T, r *running, port int, hostHeader string) {
	t.Helper()
	<-r.srv.reconciled
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (7, 1, 'o/myapp', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('wprev', 7, '/x', 'main', 'running')`,
		fmt.Sprintf(`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled, host_header) VALUES ('pprev', 'wprev', %d, '%s', 1, '%s')`,
			port, previewSlug, hostHeader),
	} {
		if _, err := r.srv.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

// previewCookie runs the handshake over both sockets and returns the preview
// cookie's value, with the token it spent.
func (r *running) previewCookie(t *testing.T) (cookie, token string) {
	t.Helper()
	ui := r.signIn(t)
	resp := r.do(t, req{method: "GET", path: "/preview/authorize?return=" + url.QueryEscape("https://"+previewHost+"/"), cookie: ui})
	su, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || su.Path != preview.SessionPath {
		t.Fatalf("authorize = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = r.previewDo(t, "GET", su.String(), nil)
	pc := cookieFrom(resp, preview.CookieName)
	if pc == nil {
		t.Fatalf("no preview cookie: %d %v", resp.StatusCode, resp.Header)
	}
	return pc.Value, su.Query().Get("t")
}

// previewUpgrade opens an upgrade on the preview socket and reads the 101.
func (r *running) previewUpgrade(t *testing.T, path, cookie string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("unix", r.cfg.PreviewSocket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nCookie: %s\r\n\r\n", path, previewHost, cookie)
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp, c, br
}

// TestPreviewProxyOverTheSockets is PF §13 step 3 against the real server
// over its real preview socket, with a real dev server behind the proxy:
//
//   - a request reaches the app with the preview cookie stripped, Host
//     `localhost:<port>`, and the proxy's own X-Forwarded-* — the client's
//     dropped — and the app's attempt to set the preview cookie is dropped
//     while its own cookie passes;
//   - a websocket upgrades, its 101 filtered the same way, and carries bytes
//     both ways;
//   - an event stream's first event arrives before its second is written;
//   - a passthrough port sends the preview host instead (its control);
//   - disabling the port closes the preview on its next request.
//
// Then the canary sweep (testing §4.2): the preview cookie and the token are
// in nothing the app received, nothing the proxy logged, no file under the
// temp root (the database and its events included), and no process's argv.
func TestPreviewProxyOverTheSockets(t *testing.T) {
	dir := t.TempDir()
	app := newDevServer(t)
	var logMu sync.Mutex
	var logs []string
	r := startWith(t, dir, nil, func(s *Server) {
		s.Proxy.Resolver = loopbackResolver{}
		s.Proxy.Logf = func(f string, a ...any) { logMu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); logMu.Unlock() }
	})
	seedPreviewAt(t, r, app.port(), preview.HostLocalhost)
	pc, token := r.previewCookie(t)
	withCookie := func(extra ...string) http.Header {
		h := http.Header{"Cookie": {"app-own=browser; " + preview.CookieName + "=" + pc}}
		for i := 0; i+1 < len(extra); i += 2 {
			h.Add(extra[i], extra[i+1])
		}
		return h
	}

	// HTTP.
	resp, body := r.previewDo(t, "GET", "https://"+previewHost+"/src/main.ts?v=1", withCookie(
		"X-Forwarded-For", "192.0.2.7", "X-Forwarded-Host", "evil.example", "X-Forwarded-Proto", "http", "X-Real-Ip", "6.6.6.6"))
	if resp.StatusCode != 200 || body != "app at /src/main.ts?v=1" {
		t.Fatalf("through the proxy = %d %q", resp.StatusCode, body)
	}
	got := app.lastRequest()
	for k, want := range map[string]string{"Cookie": "app-own=browser", "X-Forwarded-For": "192.0.2.7",
		"X-Forwarded-Host": previewHost, "X-Forwarded-Proto": "https", "X-Real-Ip": ""} {
		if v := strings.Join(got.Header.Values(k), ","); v != want {
			t.Errorf("the app saw %s %q; want %q", k, v, want)
		}
	}
	if got.Host != "localhost:"+strconv.Itoa(app.port()) {
		t.Errorf("the app saw Host %q", got.Host)
	}
	if set := resp.Header.Values("Set-Cookie"); len(set) != 1 || !strings.HasPrefix(set[0], "app-own=from-app") {
		t.Errorf("Set-Cookie through the proxy = %q; want the app's own only", set)
	}

	// A websocket.
	resp, c, br := r.previewUpgrade(t, "/hmr", "app-own=browser; "+preview.CookieName+"="+pc)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d", resp.StatusCode)
	}
	if set := resp.Header.Values("Set-Cookie"); len(set) != 1 || set[0] != "app-ws=kept" {
		t.Errorf("the 101 carried Set-Cookie %q; want the app's alone", set)
	}
	if line, _ := br.ReadString('\n'); line != "hello\n" {
		t.Errorf("app → device: %q", line)
	}
	fmt.Fprint(c, "save\n")
	if line, _ := br.ReadString('\n'); line != "echo:save\n" {
		t.Errorf("device → app → device: %q", line)
	}
	if v := app.lastRequest().Header.Get("Cookie"); v != "app-own=browser" {
		t.Errorf("the upgrade reached the app with Cookie %q", v)
	}
	c.Close()

	// An event stream, over a client that reads as it arrives — the request
	// and its first read together, since a proxy that held the stream would
	// hold its headers too.
	hr, _ := http.NewRequest("GET", "http://socket/events", nil)
	hr.Host = previewHost
	hr.Header = withCookie()
	first := make(chan string, 1)
	streamBody := make(chan io.ReadCloser, 1)
	go func() {
		sresp, err := unixClient(r.cfg.PreviewSocket).Do(hr)
		if err != nil {
			first <- "error: " + err.Error()
			streamBody <- io.NopCloser(strings.NewReader(""))
			return
		}
		streamBody <- sresp.Body
		b := make([]byte, 64)
		n, _ := io.ReadAtLeast(sresp.Body, b, len("data: one\n\n"))
		first <- string(b[:n])
	}()
	select {
	case got := <-first:
		if got != "data: one\n\n" {
			t.Errorf("first event %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Error("the first event was held until the stream ended")
	}
	close(app.release)
	sb := <-streamBody
	io.ReadAll(sb)
	sb.Close()

	// Passthrough: the control for the Host rewrite.
	if _, err := r.srv.DB.Exec(`UPDATE forwarded_port SET host_header = 'passthrough' WHERE id = 'pprev'`); err != nil {
		t.Fatal(err)
	}
	r.previewDo(t, "GET", "https://"+previewHost+"/x", withCookie())
	if h := app.lastRequest().Host; h != previewHost {
		t.Errorf("a passthrough port sent Host %q; want %q", h, previewHost)
	}

	// Disabled: the next request is the handshake again, and the app sees
	// nothing.
	if err := r.srv.Previews.SetEnabled(context.Background(), "pprev", false); err != nil {
		t.Fatal(err)
	}
	before := app.lastRequest()
	resp, _ = r.previewDo(t, "GET", "https://"+previewHost+"/after", withCookie())
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), uiOrigin+"/preview/authorize?") || app.lastRequest() != before {
		t.Errorf("after disabling = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// The sweep.
	r.stop()
	app.mu.Lock()
	received := app.dumps.String()
	app.mu.Unlock()
	if !strings.Contains(received, "app-own=browser") {
		t.Fatal("control: the dump of what the app received does not hold the app's cookie")
	}
	logMu.Lock()
	logged := strings.Join(logs, "\n")
	logMu.Unlock()
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
		if strings.Contains(received, needle) {
			found = append(found, "the app")
		}
		if strings.Contains(logged, needle) {
			found = append(found, "the proxy's log")
		}
		return found
	}
	for name, needle := range map[string]string{"the preview cookie": pc, "the token": token} {
		if found := where(needle); len(found) > 0 {
			t.Errorf("%s reached %v", name, found)
		}
	}
	if found := where("app-own=browser"); len(found) != 1 || found[0] != "the app" {
		t.Errorf("control: the sweep finds the app's cookie only where it went: %v", found)
	}
}

// TestPreviewCapCountsRedirectsAndUpgrades: PF §10.7's cap, over the real
// socket, with a cap of two. An open websocket and a request the app is still
// answering fill it; then an unauthenticated request — the handshake's
// redirect, which costs nothing but a slot — is refused with a plain 503. The
// websocket closed, the same request gets its redirect (the control).
func TestPreviewCapCountsRedirectsAndUpgrades(t *testing.T) {
	app := newDevServer(t)
	r := startWith(t, t.TempDir(), func(c *config.Config) { c.PreviewMaxConnections = 2 },
		func(s *Server) { s.Proxy.Resolver = loopbackResolver{} })
	seedPreviewAt(t, r, app.port(), preview.HostLocalhost)
	pc, _ := r.previewCookie(t)
	cookie := preview.CookieName + "=" + pc

	resp, ws, br := r.previewUpgrade(t, "/hmr", cookie)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d", resp.StatusCode)
	}
	br.ReadString('\n')
	held := make(chan struct{})
	go func() {
		defer close(held)
		hr, _ := http.NewRequest("GET", "http://socket/hold", nil)
		hr.Host = previewHost
		hr.Header.Set("Cookie", cookie)
		if resp, err := unixClient(r.cfg.PreviewSocket).Do(hr); err == nil {
			resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if req := app.lastRequest(); req != nil && req.URL.Path == "/hold" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the held request never reached the app")
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp, body := r.previewDo(t, "GET", "https://"+previewHost+"/no-cookie", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "Try again") {
		t.Errorf("past the cap, an unauthenticated request = %d %q; want a plain 503", resp.StatusCode, body)
	}

	ws.Close()
	deadline = time.Now().Add(10 * time.Second)
	for {
		resp, _ = r.previewDo(t, "GET", "https://"+previewHost+"/no-cookie", nil)
		if resp.StatusCode == http.StatusFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: with the websocket closed, still %d", resp.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(app.release)
	<-held
}

// previewCookieFor runs the handshake for a device already signed in to the
// UI with ui, and returns its preview cookie.
func (r *running) previewCookieFor(t *testing.T, ui string) string {
	t.Helper()
	resp := r.do(t, req{method: "GET", path: "/preview/authorize?return=" + url.QueryEscape("https://"+previewHost+"/"), cookie: ui})
	su, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || su.Path != preview.SessionPath {
		t.Fatalf("authorize = %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = r.previewDo(t, "GET", su.String(), nil)
	pc := cookieFrom(resp, preview.CookieName)
	if pc == nil {
		t.Fatalf("no preview cookie: %d", resp.StatusCode)
	}
	return pc.Value
}

// signInAgain is another device signing in, with the password signIn set —
// without setting it again, which would revoke every session.
func (r *running) signInAgain(t *testing.T) string {
	t.Helper()
	resp := r.do(t, req{method: "POST", path: "/api/auth/session", origin: uiOrigin,
		body: fmt.Sprintf(`{"password":%q}`, password)})
	for _, c := range resp.Cookies() {
		if c.Name == "__Host-drydock" && resp.StatusCode == http.StatusNoContent {
			return c.Value
		}
	}
	t.Fatalf("sign-in = %d", resp.StatusCode)
	return ""
}

// echoes reports whether an upgraded connection still echoes.
func echoes(c net.Conn, br *bufio.Reader, within time.Duration) bool {
	c.SetDeadline(time.Now().Add(within))
	if _, err := fmt.Fprint(c, "ping\n"); err != nil {
		return false
	}
	line, err := br.ReadString('\n')
	return err == nil && line == "echo:ping\n"
}

func closes(c net.Conn, br *bufio.Reader, within time.Duration) bool {
	c.SetReadDeadline(time.Now().Add(within))
	for {
		if _, err := br.ReadString('\n'); err != nil {
			var ne net.Error
			return !(errors.As(err, &ne) && ne.Timeout())
		}
	}
}

// TestRevocationClosesOpenWebsockets: a preview websocket has no next request
// for a revocation to refuse, and Vite's client pings it every 30 s, so it is
// closed when its session is revoked rather than left to idle out (PF §13.4):
//
//   - Sign out everywhere closes it at once, through the sign-out's hook;
//   - a password change — `drydock passwd`, another process, which no hook
//     reaches — closes it at the proxy's next recheck of the session.
//
// The control: another device signing itself out, past a recheck, leaves it
// open and echoing.
func TestRevocationClosesOpenWebsockets(t *testing.T) {
	app := newDevServer(t)
	r := startWith(t, t.TempDir(), nil, func(s *Server) {
		s.Proxy.Resolver = loopbackResolver{}
		s.Proxy.RecheckEvery = 2 * time.Second
	})
	seedPreviewAt(t, r, app.port(), preview.HostLocalhost)

	ui := r.signIn(t)
	resp, ws, br := r.previewUpgrade(t, "/hmr", preview.CookieName+"="+r.previewCookieFor(t, ui))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d", resp.StatusCode)
	}
	br.ReadString('\n')

	// Control: another device signs itself out; a recheck passes.
	other := r.signInAgain(t)
	if resp := r.do(t, req{method: "DELETE", path: "/api/auth/session", origin: uiOrigin, cookie: other}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("the other device's sign-out = %d", resp.StatusCode)
	}
	time.Sleep(2500 * time.Millisecond)
	if !echoes(ws, br, 5*time.Second) {
		t.Fatal("control: another device's sign-out closed this device's websocket")
	}

	// Sign out everywhere: closed at once — well inside one recheck.
	if resp := r.do(t, req{method: "DELETE", path: "/api/auth/session?all=true", origin: uiOrigin, cookie: ui}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("sign out everywhere = %d", resp.StatusCode)
	}
	if !closes(ws, br, time.Second) {
		t.Error("Sign out everywhere left the websocket open")
	}

	// A password change, made where no hook reaches: closed at the next
	// recheck.
	ui = r.signInAgain(t)
	resp, ws, br = r.previewUpgrade(t, "/hmr", preview.CookieName+"="+r.previewCookieFor(t, ui))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("second upgrade = %d", resp.StatusCode)
	}
	br.ReadString('\n')
	if err := r.srv.Auth.SetPassword(context.Background(), password); err != nil {
		t.Fatal(err)
	}
	if !closes(ws, br, 6*time.Second) {
		t.Error("a password change left the websocket open")
	}
}
