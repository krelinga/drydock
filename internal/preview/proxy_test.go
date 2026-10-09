package preview_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/preview"
	"github.com/krelinga/drydock/internal/sys"
)

// fakeResolver stands in for Docker: each Resolve answers the next address
// in ips (the last repeats), and counts what it was asked.
type fakeResolver struct {
	mu         sync.Mutex
	ips        []string
	err        error
	confirmErr error
	resolves   int
	confirms   int
}

func (f *fakeResolver) Resolve(_ context.Context, ws string) (preview.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if f.err != nil {
		return preview.Endpoint{}, f.err
	}
	ip := f.ips[min(f.resolves-1, len(f.ips)-1)]
	return preview.Endpoint{ContainerID: "container-at-" + ip, IP: netip.MustParseAddr(ip)}, nil
}

func (f *fakeResolver) Confirm(_ context.Context, _ string, e preview.Endpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirms++
	return f.confirmErr
}

func (f *fakeResolver) set(ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ips, f.resolves, f.confirms = ips, 0, 0
}

func (f *fakeResolver) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves, f.confirms
}

// proxyTarget is an enabled port whose container port is rawURL's port.
func proxyTarget(t *testing.T, rawURL string) preview.Target {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	return preview.Target{PortID: "p1", WorkspaceID: "01JABCDEFGHJKMNPQRSTVWXYZ0", ContainerPort: port, Slug: slug,
		Host: host, UpstreamScheme: "http", HostHeader: preview.HostLocalhost}
}

// front serves the proxy as the front door hands it a request: stripped of
// the preview cookie, writing through the CookieGuard.
func front(t *testing.T, p *preview.Proxy, tg preview.Target) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServePreview(&preview.CookieGuard{ResponseWriter: w}, preview.StripCookie(r), tg)
	}))
	t.Cleanup(func() { p.Close(); s.Close() })
	return s
}

// seen is what an upstream received.
type seen struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (s *seen) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r.Clone(context.Background()))
}

func (s *seen) last(t *testing.T) *http.Request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		t.Fatal("the upstream received nothing")
	}
	return s.reqs[len(s.reqs)-1]
}

func (s *seen) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func recordingUpstream(t *testing.T) (*httptest.Server, *seen) {
	t.Helper()
	got := &seen{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.add(r)
		fmt.Fprint(w, "app")
	}))
	t.Cleanup(up.Close)
	return up, got
}

func get(t *testing.T, rawURL string, hdr ...string) (*http.Response, string) {
	t.Helper()
	// Bounded, so a proxy that holds a request fails the test rather than hanging it.
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestHostIsLocalhostUnlessPassthrough: PF §8.3 and the owner's decision —
// `localhost:<port>` by default, the preview host only for a passthrough port.
// Each mode is the other's control.
func TestHostIsLocalhostUnlessPassthrough(t *testing.T) {
	up, got := recordingUpstream(t)
	for _, c := range []struct{ mode, want string }{
		{preview.HostLocalhost, "localhost:"},
		{"", "localhost:"}, // anything that is not passthrough is the default
		{preview.HostPassthrough, host},
	} {
		tg := proxyTarget(t, up.URL)
		tg.HostHeader = c.mode
		want := c.want
		if want == "localhost:" {
			want += strconv.Itoa(tg.ContainerPort)
		}
		fr := front(t, &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}, tg)
		if resp, body := get(t, fr.URL+"/x"); resp.StatusCode != 200 || body != "app" {
			t.Fatalf("%q: %d %q", c.mode, resp.StatusCode, body)
		}
		if h := got.last(t).Host; h != want {
			t.Errorf("host_header %q: the upstream saw Host %q; want %q", c.mode, h, want)
		}
	}
}

// TestForwardingHeadersAreTheProxysOwn: X-Forwarded-Proto https,
// X-Forwarded-Host the preview host, X-Forwarded-For the one address Caddy
// put last — and nothing else that claims to say where a request came from,
// whatever the client (or Caddy) sent. An ordinary header passes: the control.
func TestForwardingHeadersAreTheProxysOwn(t *testing.T) {
	up, got := recordingUpstream(t)
	tg := proxyTarget(t, up.URL)
	fr := front(t, &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}, tg)
	get(t, fr.URL+"/x",
		"X-Forwarded-For", "6.6.6.6, 7.7.7.7",
		"X-Forwarded-For", "203.0.113.9",
		"X-Forwarded-Host", "evil.example",
		"X-Forwarded-Proto", "http",
		"X-Forwarded-Port", "8443",
		"X-Forwarded-Prefix", "/evil",
		"X-Forwarded-Server", "evil",
		"Forwarded", "for=6.6.6.6;host=evil.example;proto=http",
		"X-Real-Ip", "6.6.6.6",
		"X-Custom", "kept")
	h := got.last(t).Header
	want := map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": host, "X-Forwarded-For": "203.0.113.9"}
	for k, v := range want {
		if vals := h.Values(k); len(vals) != 1 || vals[0] != v {
			t.Errorf("the upstream saw %s %q; want exactly %q", k, vals, v)
		}
	}
	for k := range h {
		if (strings.HasPrefix(k, "X-Forwarded-") || k == "Forwarded" || k == "X-Real-Ip") && want[k] == "" {
			t.Errorf("the upstream saw %s %q, which the client sent", k, h.Values(k))
		}
	}
	if h.Get("X-Custom") != "kept" {
		t.Errorf("control: an ordinary header arrived as %q", h.Get("X-Custom"))
	}
	// An X-Forwarded-For that is not an address is dropped, not passed on.
	get(t, fr.URL+"/x", "X-Forwarded-For", "<script>")
	if v := got.last(t).Header.Values("X-Forwarded-For"); len(v) != 0 {
		t.Errorf("a malformed X-Forwarded-For reached the upstream as %q", v)
	}
}

// rawRequest writes one request's bytes as given and returns the response.
func rawRequest(t *testing.T, addr, head string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, head); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp, c, br
}

// TestPathAndQueryGoUpstreamAsSent is the path-cleaning decision (PF §13.4):
// the proxy cleans nothing. A `..`, an encoded slash, a double slash, a `;`
// in the path and a `;` in the query all reach the app's own router as the
// device sent them — ReverseProxy on its own would drop the query's
// unparsable `x=1;y=2`, which is the control the test can fail on.
func TestPathAndQueryGoUpstreamAsSent(t *testing.T) {
	up, got := recordingUpstream(t)
	fr := front(t, &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}, proxyTarget(t, up.URL))
	const uri = "/a/../b%2Fc//d;p?x=1;y=2&z=%41"
	resp, _, _ := rawRequest(t, strings.TrimPrefix(fr.URL, "http://"), "GET "+uri+" HTTP/1.1\r\nHost: "+host+"\r\nConnection: close\r\n\r\n")
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if r := got.last(t); r.RequestURI != uri {
		t.Errorf("the upstream saw %q; want %q as sent", r.RequestURI, uri)
	}
}

// listenPair listens on 127.0.0.1 and 127.0.0.2 on one port: two containers
// at two addresses, one container port.
func listenPair(t *testing.T) (net.Listener, net.Listener) {
	t.Helper()
	for i := 0; i < 20; i++ {
		a, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		b, err := net.Listen("tcp", "127.0.0.2:"+strconv.Itoa(a.Addr().(*net.TCPAddr).Port))
		if err == nil {
			return a, b
		}
		a.Close()
	}
	t.Fatal("no port free on both 127.0.0.1 and 127.0.0.2")
	return nil, nil
}

// TestResolvedBeforeEveryDial is PF §8.1's rule: no address outlives a dial.
// Two "containers" serve the same port at two addresses, and each answers
// with Connection: close, so every request dials. Between two requests the
// container moves; the second dials the new address — while the old one is
// still up and would have answered (the third request, the control, reaches
// it again). Each dial is resolved once before and confirmed once after.
func TestResolvedBeforeEveryDial(t *testing.T) {
	la, lb := listenPair(t)
	for name, ln := range map[string]net.Listener{"A": la, "B": lb} {
		name := name
		s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Connection", "close")
			fmt.Fprint(w, name)
		})}
		go s.Serve(ln)
		t.Cleanup(func() { s.Close() })
	}
	res := &fakeResolver{ips: []string{"127.0.0.1"}}
	fr := front(t, &preview.Proxy{Resolver: res}, proxyTarget(t, "http://x:"+strconv.Itoa(la.Addr().(*net.TCPAddr).Port)))
	if _, body := get(t, fr.URL+"/"); body != "A" {
		t.Fatalf("first request reached %q; want A", body)
	}
	res.set("127.0.0.2")
	if _, body := get(t, fr.URL+"/"); body != "B" {
		t.Errorf("after the container moved, the request reached %q; want B — the address was remembered", body)
	}
	res.set("127.0.0.1")
	if _, body := get(t, fr.URL+"/"); body != "A" {
		t.Errorf("control: the old address still answers, and was asked: got %q", body)
	}
	if r, c := res.counts(); r != 1 || c != 1 {
		t.Errorf("the last dial resolved %d times and confirmed %d; want once each", r, c)
	}
}

// TestNoRunningContainerIsDeniedWithoutADial: a label that resolves to no
// running container is a stopped workspace — the denied page, and no dial at
// all. A container that stopped or moved between the resolution and the
// connect (Confirm) is the same, and the app never sees the request. The
// control is the same port served once both say yes.
func TestNoRunningContainerIsDeniedWithoutADial(t *testing.T) {
	up, got := recordingUpstream(t)
	tg := proxyTarget(t, up.URL)
	var dials atomic.Int32
	dial := func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, n, a)
	}
	check := func(what string, res *fakeResolver, wantDials int32) {
		t.Helper()
		dials.Store(0)
		before := got.n()
		fr := front(t, &preview.Proxy{Resolver: res, Dial: dial}, tg)
		resp, _ := get(t, fr.URL+"/app")
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != preview.DeniedPath {
			t.Errorf("%s: %d %q; want the denied redirect", what, resp.StatusCode, resp.Header.Get("Location"))
		}
		if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", what, resp.Header)
		}
		if dials.Load() != wantDials || got.n() != before {
			t.Errorf("%s: %d dials and %d requests reached the app; want %d and none", what, dials.Load(), got.n()-before, wantDials)
		}
	}
	check("no running container", &fakeResolver{err: fmt.Errorf("%w: none", preview.ErrNotRunning)}, 0)
	check("moved after the resolution", &fakeResolver{ips: []string{"127.0.0.1"}, confirmErr: preview.ErrNotRunning}, 1)

	fr := front(t, &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Dial: dial}, tg)
	if resp, body := get(t, fr.URL+"/app"); resp.StatusCode != 200 || body != "app" {
		t.Errorf("control: %d %q", resp.StatusCode, body)
	}
}

// TestFailuresAreDrydocksOwnPages: PF §11's answers when the app is not
// there, each Drydock's page — no-store, no-referrer — naming the port and
// nothing internal: no address, no container id, no error text.
func TestFailuresAreDrydocksOwnPages(t *testing.T) {
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	port := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	tg := proxyTarget(t, "http://x:"+strconv.Itoa(port))
	var dials atomic.Int32
	counting := func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, n, a)
	}
	// The resolved address is 127.0.0.3 (nothing listens there either); the
	// page's advice names 127.0.0.1, which is not a leak.
	leaks := []string{"127.0.0.3", "127.0.0.9", "container-at", "refused", "dial", "daemon"}
	page := func(what string, p *preview.Proxy, status int, words ...string) {
		t.Helper()
		resp, body := get(t, front(t, p, tg).URL+"/")
		if resp.StatusCode != status {
			t.Errorf("%s: %d; want %d (%q)", what, resp.StatusCode, status, body)
		}
		if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", what, resp.Header)
		}
		for _, w := range words {
			if !strings.Contains(body, w) {
				t.Errorf("%s: the page does not say %q: %q", what, w, body)
			}
		}
		for _, l := range leaks {
			if strings.Contains(body, l) {
				t.Errorf("%s: the page carries %q: %q", what, l, body)
			}
		}
	}

	// Nothing listening: one dial. Resolved again, the same answer — so
	// no second dial (§11's retry is for a container that moved).
	res := &fakeResolver{ips: []string{"127.0.0.3"}}
	page("refused", &preview.Proxy{Resolver: res, Dial: counting}, http.StatusBadGateway, "port "+strconv.Itoa(port), "0.0.0.0")
	if r, _ := res.counts(); dials.Load() != 1 || r != 2 {
		t.Errorf("refused: %d dials, %d resolutions; want 1 and 2", dials.Load(), r)
	}

	// Timed out.
	blocking := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	page("timed out", &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.3"}}, Dial: blocking, DialTimeout: 50 * time.Millisecond},
		http.StatusGatewayTimeout, "port "+strconv.Itoa(port), "in time")

	// Docker could not be asked: Drydock's fault, said as such, and the
	// reason in the journal only.
	var logged []string
	var mu sync.Mutex
	p := &preview.Proxy{Resolver: &fakeResolver{err: errors.New("docker ps: exit 1: daemon down at 127.0.0.9")},
		Logf: func(f string, a ...any) { mu.Lock(); logged = append(logged, fmt.Sprintf(f, a...)); mu.Unlock() }}
	page("docker failed", p, http.StatusServiceUnavailable, "service log")
	mu.Lock()
	if len(logged) != 1 || !strings.Contains(logged[0], "daemon down") {
		t.Errorf("the lookup failure was logged as %q", logged)
	}
	mu.Unlock()
}

// TestARestartedContainerIsRetriedOnce: §11's "container restarted, IP
// changed": the first address refuses, the resolution after it names a new
// one, and the request is served from that — two dials, invisible to the
// device.
func TestARestartedContainerIsRetriedOnce(t *testing.T) {
	la, lb := listenPair(t)
	port := la.Addr().(*net.TCPAddr).Port
	la.Close() // the old container: gone
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "new") })}
	go s.Serve(lb)
	t.Cleanup(func() { s.Close() })
	res := &fakeResolver{ips: []string{"127.0.0.1", "127.0.0.2"}}
	fr := front(t, &preview.Proxy{Resolver: res}, proxyTarget(t, "http://x:"+strconv.Itoa(port)))
	if resp, body := get(t, fr.URL+"/"); resp.StatusCode != 200 || body != "new" {
		t.Errorf("%d %q; want the new container's answer", resp.StatusCode, body)
	}
	if r, c := res.counts(); r != 2 || c != 1 {
		t.Errorf("%d resolutions, %d confirmations; want 2 and 1", r, c)
	}
}

// TestStreamsFlushAsTheyArrive: an SSE event and a chunk of a chunked
// response each reach the device before the app writes the next (PF §8.4).
func TestStreamsFlushAsTheyArrive(t *testing.T) {
	for _, ct := range []string{"text/event-stream", "text/plain"} {
		next := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			fmt.Fprint(w, "data: one\n\n")
			w.(http.Flusher).Flush()
			<-next
			fmt.Fprint(w, "data: two\n\n")
		}))
		fr := front(t, &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}, proxyTarget(t, up.URL))
		// The request and its first read in one goroutine: a proxy that
		// held the stream would hold its headers too, and a Get that waits
		// for them would never let the test say so.
		first := make(chan string, 1)
		body := make(chan io.ReadCloser, 1)
		go func() {
			resp, err := http.Get(fr.URL + "/events")
			if err != nil {
				first <- "error: " + err.Error()
				body <- io.NopCloser(strings.NewReader(""))
				return
			}
			body <- resp.Body
			b := make([]byte, 64)
			n, _ := io.ReadAtLeast(resp.Body, b, len("data: one\n\n"))
			first <- string(b[:n])
		}()
		select {
		case got := <-first:
			if got != "data: one\n\n" {
				t.Errorf("%s: first chunk %q", ct, got)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: the first event was held back until the response ended", ct)
		}
		close(next)
		b := <-body
		rest, _ := io.ReadAll(b)
		b.Close()
		if !strings.HasSuffix(string(rest), "data: two\n\n") {
			t.Errorf("%s: then %q", ct, rest)
		}
		up.Close()
	}
}

// websocketUpstream answers an upgrade with a hostile 101 — the preview
// cookie planted beside the app's — says hello, then echoes.
func websocketUpstream(t *testing.T, got *seen) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.add(r)
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "no upgrade", 400)
			return
		}
		c, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
			"Set-Cookie: " + preview.CookieName + "=planted; Path=/; Secure; HttpOnly\r\n" +
			"Set-Cookie: app=kept\r\n\r\nhello\n")
		brw.Flush()
		for {
			line, err := brw.ReadString('\n')
			if err != nil {
				return
			}
			brw.WriteString("echo:" + line)
			brw.Flush()
		}
	}))
	t.Cleanup(up.Close)
	return up
}

func upgradeVia(t *testing.T, fr *httptest.Server) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	return rawRequest(t, strings.TrimPrefix(fr.URL, "http://"), "GET /hmr HTTP/1.1\r\nHost: "+host+
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nCookie: "+preview.CookieName+"=device; app=1\r\n\r\n")
}

// TestUpgradeFiltersThe101AndCarriesBothWays: the 101 is written through the
// hijacked connection, past the CookieGuard, so the proxy filters it itself —
// the planted preview cookie is gone and the app's passes (the control). The
// request upgraded with the preview cookie stripped and Host rewritten, and
// bytes go both ways: the app's hello down, an echo of the device's line back.
func TestUpgradeFiltersThe101AndCarriesBothWays(t *testing.T) {
	got := &seen{}
	up := websocketUpstream(t, got)
	p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}
	tg := proxyTarget(t, up.URL)
	resp, c, br := upgradeVia(t, front(t, p, tg))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%d", resp.StatusCode)
	}
	set := resp.Header.Values("Set-Cookie")
	if len(set) != 1 || set[0] != "app=kept" {
		t.Errorf("the 101 carried Set-Cookie %q; want the app's alone", set)
	}
	r := got.last(t)
	if r.Header.Get("Cookie") != "app=1" || r.Host != "localhost:"+strconv.Itoa(tg.ContainerPort) {
		t.Errorf("the upgrade arrived with Cookie %q, Host %q", r.Header.Get("Cookie"), r.Host)
	}
	if line, _ := br.ReadString('\n'); line != "hello\n" {
		t.Errorf("app → device: %q", line)
	}
	io.WriteString(c, "ping\n")
	if line, _ := br.ReadString('\n'); line != "echo:ping\n" {
		t.Errorf("device → app → device: %q", line)
	}
	if p.Upgrades() != 1 {
		t.Errorf("%d upgrades tracked; want 1", p.Upgrades())
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestAnIdleUpgradeIsClosed: PF §10.7's idle timeout, on a fake clock. A byte
// either way restarts the wait: at 9 of 10 minutes the device speaks, so the
// first check at 10 finds one minute of quiet and waits again (the control);
// at 19, ten quiet minutes, both sides close.
func TestAnIdleUpgradeIsClosed(t *testing.T) {
	clock := sys.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Clock: clock, IdleTimeout: 10 * time.Minute}
	got := &seen{}
	resp, c, br := upgradeVia(t, front(t, p, proxyTarget(t, websocketUpstream(t, got).URL)))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%d", resp.StatusCode)
	}
	br.ReadString('\n') // hello
	waitFor(t, "the idle watch", func() bool { return clock.Waiting() == 1 })

	clock.Advance(9 * time.Minute)
	io.WriteString(c, "still here\n")
	if line, _ := br.ReadString('\n'); line != "echo:still here\n" {
		t.Fatalf("at 9 minutes: %q", line)
	}
	clock.Advance(time.Minute)
	// The check at 10 minutes found one quiet minute and waits again.
	waitFor(t, "the watch to wait again", func() bool { return clock.Waiting() == 1 })
	if p.Upgrades() != 1 {
		t.Fatal("closed at 10 minutes, one minute after the device spoke")
	}

	clock.Advance(9 * time.Minute)
	waitFor(t, "the upgrade to close", func() bool { return p.Upgrades() == 0 })
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadString('\n'); err == nil {
		t.Error("the device's side is still open after ten idle minutes")
	}
}

// TestCloseEndsUpgrades: shutdown closes what http.Server.Shutdown cannot see.
func TestCloseEndsUpgrades(t *testing.T) {
	p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}
	_, c, br := upgradeVia(t, front(t, p, proxyTarget(t, websocketUpstream(t, &seen{}).URL)))
	br.ReadString('\n')
	p.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadString('\n'); err == nil {
		t.Error("an upgrade survived Close")
	}
}

// TestLimitRefusesPastTheCap: PF §10.7's cap. Two held requests fill a cap
// of two; the third is a plain 503 at once; one released, the next is served.
func TestLimitRefusesPastTheCap(t *testing.T) {
	release := make(chan struct{})
	var in atomic.Int32
	h := preview.Limit(2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			in.Add(1)
			<-release
		}
		fmt.Fprint(w, "served")
	}))
	s := httptest.NewServer(h)
	defer s.Close()
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free() // before Close, which waits for the held handlers
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(s.URL + "/hold"); err == nil {
				resp.Body.Close()
			}
		}()
	}
	waitFor(t, "two held requests", func() bool { return in.Load() == 2 })
	resp, body := get(t, s.URL+"/x")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "Try again") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("past the cap: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	release <- struct{}{}
	waitFor(t, "a slot", func() bool {
		resp, _ := get(t, s.URL+"/x")
		return resp.StatusCode == 200
	})
	free()
	wg.Wait()
}

// TestOnlyHTTPAndWebsockets: PF §1 — a CONNECT, or an upgrade to anything but
// a websocket, is a byte stream the device chose, and never reaches the app.
// The control is a websocket upgrade on the same proxy.
func TestOnlyHTTPAndWebsockets(t *testing.T) {
	got := &seen{}
	up := websocketUpstream(t, got)
	fr := front(t, &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}, proxyTarget(t, up.URL))
	addr := strings.TrimPrefix(fr.URL, "http://")
	for head, want := range map[string]int{
		"CONNECT " + host + ":22 HTTP/1.1\r\nHost: " + host + ":22\r\n\r\n":                               http.StatusMethodNotAllowed,
		"GET / HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: tcp\r\nConnection: Upgrade\r\n\r\n":             http.StatusBadRequest,
		"GET / HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: h2c\r\nConnection: keep-alive, Upgrade\r\n\r\n": http.StatusBadRequest,
	} {
		resp, _, _ := rawRequest(t, addr, head)
		if resp.StatusCode != want {
			t.Errorf("%q = %d; want %d", strings.SplitN(head, "\r\n", 2)[0], resp.StatusCode, want)
		}
	}
	if got.n() != 0 {
		t.Errorf("%d of them reached the app", got.n())
	}
	if resp, _, _ := upgradeVia(t, fr); resp.StatusCode != http.StatusSwitchingProtocols || got.n() != 1 {
		t.Errorf("control: a websocket upgrade = %d", resp.StatusCode)
	}
}

// TestAnUpgradeClosesWhenItsSessionEnds: a websocket has no next request for
// a revocation to refuse, so the proxy asks the gate's question again every
// RecheckEvery while it is open (on a fake clock here). While the session
// holds, the socket stays open — the control — and at the first recheck after
// it stops holding, both sides close, long before the idle timeout.
func TestAnUpgradeClosesWhenItsSessionEnds(t *testing.T) {
	clock := sys.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Clock: clock,
		IdleTimeout: time.Hour, RecheckEvery: 30 * time.Second}
	var holds atomic.Bool
	holds.Store(true)
	var asked atomic.Int32
	tg := proxyTarget(t, websocketUpstream(t, &seen{}).URL)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(preview.WithRecheck(r.Context(), func(context.Context) bool {
			asked.Add(1)
			return holds.Load()
		}))
		p.ServePreview(&preview.CookieGuard{ResponseWriter: w}, preview.StripCookie(r), tg)
	}))
	t.Cleanup(func() { p.Close(); s.Close() })
	resp, c, br := upgradeVia(t, s)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%d", resp.StatusCode)
	}
	br.ReadString('\n')
	waitFor(t, "the watch", func() bool { return clock.Waiting() == 1 })

	clock.Advance(30 * time.Second)
	waitFor(t, "the first recheck", func() bool { return asked.Load() == 2 && clock.Waiting() == 1 })
	if p.Upgrades() != 1 {
		t.Fatal("control: closed while the session still held")
	}
	io.WriteString(c, "still\n")
	if line, _ := br.ReadString('\n'); line != "echo:still\n" {
		t.Fatalf("control: %q", line)
	}

	holds.Store(false)
	clock.Advance(30 * time.Second)
	waitFor(t, "the upgrade to close", func() bool { return p.Upgrades() == 0 })
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadString('\n'); err == nil {
		t.Error("the device's side is still open after its session ended")
	}
}

// TestAnUpgradeRevokedBeforeItRegisteredClosesAtOnce: a disable (or a
// sign-out) whose CloseWhere ran after the gate passed an upgrade but before
// the upgrade registered found nothing to close. The watch asks the session
// once as it starts, so that upgrade closes at once — with the clock never
// advanced, so not at a recheck. The control is TestAnUpgradeClosesWhenItsSessionEnds,
// whose session holds at registration and stays open.
func TestAnUpgradeRevokedBeforeItRegisteredClosesAtOnce(t *testing.T) {
	clock := sys.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Clock: clock,
		IdleTimeout: time.Hour, RecheckEvery: 30 * time.Second}
	var asked atomic.Int32
	tg := proxyTarget(t, websocketUpstream(t, &seen{}).URL)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(preview.WithRecheck(r.Context(), func(context.Context) bool {
			asked.Add(1)
			return false // the port was disabled while this request was in flight
		}))
		p.ServePreview(&preview.CookieGuard{ResponseWriter: w}, preview.StripCookie(r), tg)
	}))
	t.Cleanup(func() { p.Close(); s.Close() })
	resp, c, br := upgradeVia(t, s)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%d", resp.StatusCode)
	}
	waitFor(t, "the upgrade to close", func() bool { return asked.Load() >= 1 && p.Upgrades() == 0 })
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, err := br.ReadString('\n'); err != nil {
			break
		}
	}
	if clock.Waiting() != 0 {
		t.Errorf("the watch is still sleeping on the clock (%d)", clock.Waiting())
	}
}

// silentUpstream switches protocols and then sends nothing, ever: an app
// whose websocket waits for the device to speak first.
func silentUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		brw.Flush()
		io.Copy(io.Discard, brw) // until the proxy closes its side
	}))
	t.Cleanup(up.Close)
	return up
}

// TestASilentUpgradeIsStillWatched: an upgrade on which no byte ever moves,
// either way, is still asked about and still idles out. The watch starts at
// the device side's first use, and with nothing to write that use is the
// device-to-app copier's Read, which ReverseProxy begins as soon as the 101 is
// flushed — what this test pins. Mutation-checked: without start() in
// activeConn.Read, the watch never starts and all three subtests fail.
func TestASilentUpgradeIsStillWatched(t *testing.T) {
	serve := func(t *testing.T, p *preview.Proxy, recheck func(context.Context) bool) (net.Conn, *bufio.Reader) {
		t.Helper()
		tg := proxyTarget(t, silentUpstream(t).URL)
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if recheck != nil {
				r = r.WithContext(preview.WithRecheck(r.Context(), recheck))
			}
			p.ServePreview(&preview.CookieGuard{ResponseWriter: w}, preview.StripCookie(r), tg)
		}))
		t.Cleanup(func() { p.Close(); s.Close() })
		resp, c, br := upgradeVia(t, s)
		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("%d", resp.StatusCode)
		}
		return c, br
	}
	closed := func(t *testing.T, c net.Conn, br *bufio.Reader) bool {
		t.Helper()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := br.ReadByte()
		return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
	}

	t.Run("a session that holds stays open", func(t *testing.T) {
		// The control for the next subtest: the watch asks at once, the
		// session holds, and the watch waits on the clock.
		clock := sys.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
		p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Clock: clock,
			IdleTimeout: time.Hour, RecheckEvery: 30 * time.Second}
		var asked atomic.Int32
		serve(t, p, func(context.Context) bool { asked.Add(1); return true })
		waitFor(t, "the watch's first ask", func() bool { return asked.Load() == 1 && clock.Waiting() == 1 })
		if p.Upgrades() != 1 {
			t.Error("closed although the session holds")
		}
	})

	t.Run("a revoked session closes it", func(t *testing.T) {
		clock := sys.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
		p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Clock: clock,
			IdleTimeout: time.Hour, RecheckEvery: 30 * time.Second}
		var asked atomic.Int32
		c, br := serve(t, p, func(context.Context) bool { asked.Add(1); return false })
		waitFor(t, "the upgrade to close", func() bool { return asked.Load() >= 1 && p.Upgrades() == 0 })
		if !closed(t, c, br) {
			t.Error("the device's side is still open after its session ended")
		}
	})

	t.Run("the idle timeout closes it", func(t *testing.T) {
		clock := sys.NewFakeClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
		p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}, Clock: clock,
			IdleTimeout: 10 * time.Minute}
		c, br := serve(t, p, nil)
		waitFor(t, "the idle watch", func() bool { return clock.Waiting() == 1 })
		clock.Advance(10*time.Minute - time.Second) // short of the timer: nothing fires
		if p.Upgrades() != 1 {
			t.Fatal("control: closed a second before the idle timeout")
		}
		clock.Advance(time.Second)
		waitFor(t, "the upgrade to close", func() bool { return p.Upgrades() == 0 })
		if !closed(t, c, br) {
			t.Error("the device's side is still open after ten idle minutes")
		}
	})
}

// TestCloseWhereClosesOnlyItsMatches: what a sign-out calls, with the auth
// session it revoked. The other session's websocket stays open and working.
func TestCloseWhereClosesOnlyItsMatches(t *testing.T) {
	p := &preview.Proxy{Resolver: &fakeResolver{ips: []string{"127.0.0.1"}}}
	up := websocketUpstream(t, &seen{})
	conns := map[string]*bufio.Reader{}
	writers := map[string]net.Conn{}
	for _, id := range []string{"revoked", "kept"} {
		tg := proxyTarget(t, up.URL)
		tg.AuthSessionID = id
		_, c, br := upgradeVia(t, front(t, p, tg))
		br.ReadString('\n')
		conns[id], writers[id] = br, c
	}
	if n := p.CloseWhere(func(t preview.Target) bool { return t.AuthSessionID == "revoked" }); n != 1 {
		t.Errorf("CloseWhere closed %d; want 1", n)
	}
	writers["revoked"].SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conns["revoked"].ReadString('\n'); err == nil {
		t.Error("the revoked session's websocket is still open")
	}
	io.WriteString(writers["kept"], "ping\n")
	writers["kept"].SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, _ := conns["kept"].ReadString('\n'); line != "echo:ping\n" {
		t.Errorf("control: the other session's websocket = %q", line)
	}
}
