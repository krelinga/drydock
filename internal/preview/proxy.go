package preview

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/krelinga/drydock/internal/sys"
)

// The two `host_header` values (PF §5, §8.3).
const (
	// HostLocalhost, the default (owner, 8 October 2026), sends
	// `localhost:<port>` upstream: what a dev server's host allowlist —
	// Vite's server.allowedHosts, Django's ALLOWED_HOSTS — accepts unedited.
	HostLocalhost = "localhost"
	// HostPassthrough sends the preview host, for an app that builds
	// absolute URLs from Host and ignores X-Forwarded-Host.
	HostPassthrough = "passthrough"
)

// Defaults for the two bounds PF §10.7 requires of the preview mux.
const (
	// DefaultMaxConnections is how many requests the preview socket serves
	// at once, an upgraded connection counting for its whole life. A dev
	// server's first page load is hundreds of module requests that Caddy
	// fans out from one HTTP/2 connection, so the cap is set where a few
	// tabs loading at once fit under it and a flood does not.
	DefaultMaxConnections = 512
	// DefaultIdleTimeout closes an upgraded connection — an HMR websocket —
	// after this long with no byte in either direction. Long, because
	// Vite's client reloads the page when its socket drops: the timeout is
	// for the tab a phone abandoned, not for a quiet editor.
	DefaultIdleTimeout = 30 * time.Minute
	// DefaultDialTimeout bounds one TCP connect to the container.
	DefaultDialTimeout = 5 * time.Second
	// DefaultRecheckEvery is how often an open upgrade's preview session is
	// validated again: the longest a websocket outlives a revocation that
	// happened somewhere the proxy is not told of (`drydock passwd`, a
	// disabled port, an expired session).
	DefaultRecheckEvery = 30 * time.Second
)

// ErrNotRunning is a resolution that found no running container for the
// workspace now. The proxy answers it as a stopped workspace: the denied page,
// never a dial to an address remembered from before (PF §8.1).
var ErrNotRunning = errors.New("preview: the workspace has no running container to dial")

// Endpoint is one resolution of a workspace's container: which container, and
// its address on a Docker network. It is used for one dial and dropped.
type Endpoint struct {
	ContainerID string
	IP          netip.Addr
}

// Resolver finds a workspace's container by its label, from Docker, now
// (internal/container's Address and Confirm in production). An error that is
// ErrNotRunning means "treat the workspace as stopped"; any other error is
// Drydock failing to ask.
type Resolver interface {
	// Resolve is asked immediately before every dial.
	Resolve(ctx context.Context, workspaceID string) (Endpoint, error)
	// Confirm is asked immediately after a dial connects: the same
	// container, still running, still at that address. A connection made
	// to an address the container no longer holds is closed unused.
	Confirm(ctx context.Context, workspaceID string, e Endpoint) error
}

// Proxy is the preview proxy (PF §3.1, §8; §13 step 3): what a request with a
// valid preview cookie reaches. It holds no address: the upstream is derived,
// never supplied (§10.7) — the Target names a workspace and a port, and the
// container's address is resolved from Docker by label immediately before
// every dial and confirmed immediately after it.
//
// What it does to a request, and why:
//
//   - The path and query go upstream as the device sent them. Drydock makes
//     no decision on either past the front door's /.drydock/ check, so
//     cleaning would only change what the app's own router sees (an app's
//     `//`, a `%2F`, a `;` in the query are its business).
//   - Host is `localhost:<port>` unless the port is `passthrough` (§8.3).
//   - X-Forwarded-Proto is https, X-Forwarded-Host the preview host, and
//     X-Forwarded-For the one address Caddy put there; every other
//     Forwarded, X-Forwarded-* or X-Real-Ip is dropped, so nothing a client
//     or Caddy sent reaches the app unexamined.
//   - The preview cookie is already gone (the front door's StripCookie) and
//     a Set-Cookie claiming it is dropped from every response header block:
//     by the CookieGuard the front door wraps the writer in, and here, in
//     ModifyResponse, for the one block that bypasses the guard — a 101,
//     which httputil.ReverseProxy writes through the hijacked connection.
//   - Responses flush as they arrive (SSE, chunked streams, HMR), and an
//     upgrade is proxied both ways, closed after IdleTimeout with no traffic.
type Proxy struct {
	Resolver Resolver
	Clock    sys.Clock
	// IdleTimeout closes an upgraded connection idle this long in both
	// directions; zero is DefaultIdleTimeout.
	IdleTimeout time.Duration
	// RecheckEvery is how often an open upgrade asks again whether its
	// preview session still holds (the gate's WithRecheck), closing both
	// sides when it does not; zero is DefaultRecheckEvery. A websocket has
	// no next request for a revocation to refuse, and Vite's client pings
	// every 30 s, so without it a socket on a lost device would outlive the
	// sign-out that was meant to end it (PF §13.4).
	RecheckEvery time.Duration
	// DialTimeout bounds one TCP connect; zero is DefaultDialTimeout.
	DialTimeout time.Duration
	// Dial is the TCP dial, a test's seam; nil is a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Logf reports what an operator should see in the journal: Drydock
	// failing to ask Docker. Never a cookie, a header or a request URL.
	Logf func(string, ...any)

	once      sync.Once
	transport *http.Transport
	resolving chan struct{} // bounds concurrent Docker lookups

	mu       sync.Mutex
	upgrades map[*upgrade]struct{}
	closed   bool
}

// maxResolving bounds how many dials resolve at once. Each resolution is two
// docker invocations and each confirmation one more; a first page load of a
// few hundred modules would otherwise start them all together. A dial waits
// for a slot and then resolves — immediately before it dials, as ever.
const maxResolving = 8

func (p *Proxy) init() {
	p.once.Do(func() {
		p.resolving = make(chan struct{}, maxResolving)
		p.upgrades = map[*upgrade]struct{}{}
		p.transport = &http.Transport{
			// Never the environment's HTTP_PROXY: the only route to a
			// preview is this dial.
			Proxy:       nil,
			DialContext: p.dial,
			// The upstream is a dev server inside the workspace's own
			// container, found by its label; its certificate, when it
			// has one, is self-signed by construction (upstream_scheme
			// https, PF §5). Identity is the label resolution, not TLS.
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}, //nolint:gosec // see above
			TLSHandshakeTimeout: 10 * time.Second,
			// Accept-Encoding passes through as the device sent it, and
			// the body comes back as the app encoded it: the transport
			// adds no gzip of its own and decodes nothing.
			DisableCompression:     true,
			MaxIdleConnsPerHost:    16,
			IdleConnTimeout:        30 * time.Second,
			MaxResponseHeaderBytes: 1 << 20,
			ExpectContinueTimeout:  time.Second,
		}
	})
}

// ServePreview proxies one request to the target's container port.
func (p *Proxy) ServePreview(w http.ResponseWriter, r *http.Request, t Target) {
	p.init()
	if r.Method == http.MethodConnect {
		// Not a request to the app: a tunnel through the proxy, which is
		// TCP forwarding by another name (PF §1's non-goals).
		previewPage(w, http.StatusMethodNotAllowed, "Not a preview request", "Drydock forwards HTTP and websockets to a preview, nothing else.")
		return
	}
	if up := upgradeType(r.Header); up != "" && !strings.EqualFold(up, "websocket") {
		// HTTP and websockets only (PF §1): an upgrade to anything else is
		// a byte stream the app chose, which is the same tunnel.
		previewPage(w, http.StatusBadRequest, "Not a preview request", "Drydock forwards HTTP and websockets to a preview, nothing else.")
		return
	}
	u := &upgrade{p: p, done: make(chan struct{}), target: t}
	if f, ok := RecheckFrom(r.Context()); ok {
		u.recheck = f
	}
	defer u.close()
	rw := &proxyWriter{ResponseWriter: w, u: u}
	rp := &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { rewrite(pr, t) },
		Transport:     p.transport,
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			// Every response, the 101 above all: ReverseProxy writes a
			// 101's headers through the hijacked connection, where no
			// ResponseWriter — and so no CookieGuard — sees them.
			FilterSetCookie(res.Header)
			if res.StatusCode == http.StatusSwitchingProtocols {
				if rwc, ok := res.Body.(io.ReadWriteCloser); ok {
					res.Body = u.backend(rwc)
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) { p.fail(rw, r, t, err) },
		ErrorLog:     log.New(io.Discard, "", 0),
	}
	rp.ServeHTTP(rw, r)
}

// Close ends every upgraded connection and drops idle upstream connections.
// The server calls it at shutdown: http.Server.Shutdown does not track a
// hijacked connection.
func (p *Proxy) Close() {
	p.init()
	p.mu.Lock()
	p.closed = true
	ups := make([]*upgrade, 0, len(p.upgrades))
	for u := range p.upgrades {
		ups = append(ups, u)
	}
	p.mu.Unlock()
	for _, u := range ups {
		u.close()
	}
	p.transport.CloseIdleConnections()
}

// CloseWhere ends every open upgrade whose target matches, and reports how
// many. The server calls it when a sign-out or *Sign out everywhere* revokes
// the auth sessions those websockets were authorized by, so they close at
// once rather than at their next recheck.
func (p *Proxy) CloseWhere(match func(Target) bool) int {
	p.init()
	p.mu.Lock()
	var ups []*upgrade
	for u := range p.upgrades {
		if match(u.target) {
			ups = append(ups, u)
		}
	}
	p.mu.Unlock()
	for _, u := range ups {
		u.close()
	}
	return len(ups)
}

// Upgrades is how many upgraded connections are open, for tests.
func (p *Proxy) Upgrades() int {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.upgrades)
}

// rewrite is the outbound request (see Proxy for the reasons).
func rewrite(pr *httputil.ProxyRequest, t Target) {
	out := pr.Out
	out.URL.Scheme = "http"
	if t.UpstreamScheme == "https" {
		out.URL.Scheme = "https"
	}
	// The URL's host is the transport's pool key and what dial decodes:
	// the workspace and the port, never an address.
	out.URL.Host = dialKey(t.WorkspaceID, t.ContainerPort)
	// As sent: ReverseProxy drops query parameters it cannot parse, and
	// nothing past this point parses them.
	out.URL.RawQuery = pr.In.URL.RawQuery
	if t.HostHeader == HostPassthrough {
		out.Host = t.Host
	} else {
		out.Host = "localhost:" + strconv.Itoa(t.ContainerPort)
	}
	for k := range out.Header {
		if forwardingHeader(k) {
			delete(out.Header, k)
		}
	}
	out.Header.Set("X-Forwarded-Proto", "https")
	out.Header.Set("X-Forwarded-Host", t.Host)
	if ip, ok := ForwardedFor(pr.In); ok {
		out.Header.Set("X-Forwarded-For", ip)
	}
}

// upgradeType is the protocol a request asks to switch to, as ReverseProxy
// reads it: an Upgrade header that Connection names, or none.
func upgradeType(h http.Header) string {
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return h.Get("Upgrade")
			}
		}
	}
	return ""
}

// forwardingHeader is every header that claims to say where a request came
// from or how. Only the proxy's own reach the app.
func forwardingHeader(k string) bool {
	k = http.CanonicalHeaderKey(strings.TrimSpace(k))
	return k == "Forwarded" || k == "X-Real-Ip" || strings.HasPrefix(k, "X-Forwarded-")
}

// ForwardedFor is the client's address as Caddy put it in X-Forwarded-For
// (`header_up X-Forwarded-For {remote_host}`, which replaces whatever the
// client sent): the last entry, and only if it parses as an IP address. The
// preview socket admits Caddy and nothing else, so that entry is Caddy's;
// anything that is not an address is dropped rather than passed on.
func ForwardedFor(r *http.Request) (string, bool) {
	vals := r.Header.Values("X-Forwarded-For")
	if len(vals) == 0 {
		return "", false
	}
	parts := strings.Split(vals[len(vals)-1], ",")
	ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1]))
	if err != nil || ip.Zone() != "" {
		return "", false
	}
	return ip.Unmap().String(), true
}

// dialKey and parseDialKey encode a workspace and a port as a host the
// transport can pool on. Hex, so any workspace id is one lowercase label.
func dialKey(workspaceID string, port int) string {
	return net.JoinHostPort("ws-"+hex.EncodeToString([]byte(workspaceID))+".invalid", strconv.Itoa(port))
}

func parseDialKey(addr string) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	label, ok := strings.CutPrefix(host, "ws-")
	if ok {
		label, ok = strings.CutSuffix(label, ".invalid")
	}
	if !ok {
		return "", 0, fmt.Errorf("preview: %q is not a preview dial key", addr)
	}
	id, err := hex.DecodeString(label)
	if err != nil || len(id) == 0 {
		return "", 0, fmt.Errorf("preview: %q is not a preview dial key", addr)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("preview: %q has no port", addr)
	}
	return string(id), uint16(port), nil
}

// unreachableError is a dial that reached no dev server: refused, reset,
// unroutable, or timed out.
type unreachableError struct {
	err     error
	timeout bool
}

func (e *unreachableError) Error() string { return "preview: dialling the container: " + e.err.Error() }
func (e *unreachableError) Unwrap() error { return e.err }

// lookupError is Drydock failing to ask Docker.
type lookupError struct{ err error }

func (e *lookupError) Error() string { return "preview: resolving the container: " + e.err.Error() }
func (e *lookupError) Unwrap() error { return e.err }

// dial is the transport's only dial (PF §8.1): resolve the workspace's
// container by label now, connect to its address, confirm the container still
// holds that address, and hand the connection over. A refused or timed-out
// connect is resolved once more and retried only if the answer changed — a
// container restarted with a new address (§11) — so a dev server that is
// simply not listening costs one resolution, not two dials.
func (p *Proxy) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	ws, port, err := parseDialKey(addr)
	if err != nil {
		return nil, err
	}
	var tried *Endpoint
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		e, err := p.resolve(ctx, ws)
		if err != nil {
			return nil, err
		}
		if tried != nil && *tried == e {
			break
		}
		tried = &e
		conn, err := p.connect(ctx, netip.AddrPortFrom(e.IP, port).String())
		if err != nil {
			last = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if err := p.confirm(ctx, ws, e); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
	return nil, last
}

func (p *Proxy) resolve(ctx context.Context, ws string) (Endpoint, error) {
	select {
	case p.resolving <- struct{}{}:
	case <-ctx.Done():
		return Endpoint{}, ctx.Err()
	}
	defer func() { <-p.resolving }()
	e, err := p.Resolver.Resolve(ctx, ws)
	switch {
	case errors.Is(err, ErrNotRunning):
		return Endpoint{}, err
	case err != nil:
		if ctx.Err() != nil {
			return Endpoint{}, ctx.Err()
		}
		return Endpoint{}, &lookupError{err}
	}
	return e, nil
}

func (p *Proxy) confirm(ctx context.Context, ws string, e Endpoint) error {
	select {
	case p.resolving <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-p.resolving }()
	err := p.Resolver.Confirm(ctx, ws, e)
	switch {
	case err == nil, errors.Is(err, ErrNotRunning):
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return &lookupError{err}
}

func (p *Proxy) connect(ctx context.Context, hostport string) (net.Conn, error) {
	d := p.DialTimeout
	if d <= 0 {
		d = DefaultDialTimeout
	}
	dctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(dctx, "tcp", hostport)
	if err != nil {
		timeout := errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() && ctx.Err() == nil {
			timeout = true
		}
		return nil, &unreachableError{err: err, timeout: timeout}
	}
	return conn, nil
}

// fail is every answer Drydock makes in the app's place. Each is Drydock's
// own page — no-store, no-referrer, constant text but the port — and none
// carries an address, a container id or an error string (PF §11).
func (p *Proxy) fail(w *proxyWriter, r *http.Request, t Target, err error) {
	if w.hijacked.Load() {
		return // the connection is the upgrade's now; nothing can be written
	}
	if r.Context().Err() != nil {
		// The device went away; there is no one to answer.
		return
	}
	outcome := p.outcomeOf(err, t.WorkspaceID)
	switch outcome {
	case ProbeNotRunning:
		// Treated as stopped (PF §8.1): the one dead end, as the front door
		// sends a stopped workspace's request.
		noStore(w)
		http.Redirect(w, r, DeniedPath, http.StatusFound)
	case ProbeLookupFailed:
		previewPage(w, http.StatusServiceUnavailable, "Preview unavailable", OutcomeSentence(outcome, t.ContainerPort))
	case ProbeTimedOut:
		previewPage(w, http.StatusGatewayTimeout, "Preview not answering", OutcomeSentence(outcome, t.ContainerPort))
	default:
		previewPage(w, http.StatusBadGateway, "Preview not answering", OutcomeSentence(outcome, t.ContainerPort))
	}
}

// What one dial came to — the proxy's failure pages and the probe endpoint
// tell these apart, and nothing else (PF §11, §13 step 4).
const (
	// ProbeAnswering: the container accepted a TCP connection on the port.
	ProbeAnswering = "answering"
	// ProbeNotRunning: no running container holds the workspace's label
	// now, or the one dialled moved — the proxy's denied page.
	ProbeNotRunning = "not_running"
	// ProbeRefused: the container is up and nothing accepted the
	// connection (refused, reset, unroutable) — the proxy's 502.
	ProbeRefused = "refused"
	// ProbeTimedOut: the connect did not finish in DialTimeout — 504.
	ProbeTimedOut = "timed_out"
	// ProbeLookupFailed: Drydock could not ask Docker — 503, and the
	// reason in the journal.
	ProbeLookupFailed = "lookup_failed"
)

// ProbeResult is GET …/ports/:port/probe: what a dial made now came to, and
// the sentence the proxy's own page would say for it.
type ProbeResult struct {
	Outcome string `json:"outcome"`
	Message string `json:"message"`
}

// Probe dials a workspace's container port now and reports what happened —
// through dial, the proxy's own and only dial: the container resolved by
// label (Resolver.Resolve), the connect, Resolver.Confirm, and the one retry
// a moved container gets. The connection is closed unused; nothing is sent
// on it. Its answers are the failure pages' (OutcomeSentence), so the panel
// and a preview tab never disagree about a port.
func (p *Proxy) Probe(ctx context.Context, workspaceID string, port int) ProbeResult {
	p.init()
	if port < 1 || port > 65535 {
		return ProbeResult{Outcome: ProbeRefused, Message: OutcomeSentence(ProbeRefused, port)}
	}
	conn, err := p.dial(ctx, "tcp", dialKey(workspaceID, port))
	if err == nil {
		conn.Close()
		return ProbeResult{Outcome: ProbeAnswering, Message: OutcomeSentence(ProbeAnswering, port)}
	}
	o := p.outcomeOf(err, workspaceID)
	return ProbeResult{Outcome: o, Message: OutcomeSentence(o, port)}
}

// outcomeOf is a failed dial's outcome; a lookup failure goes to the journal
// with its reason, which no page or probe answer carries.
func (p *Proxy) outcomeOf(err error, workspaceID string) string {
	var ue *unreachableError
	var le *lookupError
	switch {
	case errors.Is(err, ErrNotRunning):
		return ProbeNotRunning
	case errors.As(err, &le):
		if p.Logf != nil {
			p.Logf("drydock: preview: looking up workspace %s's container: %v", workspaceID, le.err)
		}
		return ProbeLookupFailed
	case errors.As(err, &ue) && ue.timeout:
		return ProbeTimedOut
	}
	return ProbeRefused
}

// OutcomeSentence is the one sentence for each outcome, naming the port and
// nothing internal — no address, container id or error text.
func OutcomeSentence(outcome string, port int) string {
	switch outcome {
	case ProbeAnswering:
		return fmt.Sprintf("Something is answering on port %d in this workspace's container.", port)
	case ProbeNotRunning:
		return "This workspace has no running container to reach."
	case ProbeLookupFailed:
		return "Drydock could not look up this workspace's container. The service log says why."
	case ProbeTimedOut:
		return fmt.Sprintf("Nothing answered on port %d in this workspace's container in time. Is the dev server running, and listening on 0.0.0.0 rather than 127.0.0.1?", port)
	}
	return fmt.Sprintf("Nothing is answering on port %d in this workspace's container. Is the dev server running, and listening on 0.0.0.0 rather than 127.0.0.1?", port)
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

// previewPage writes one of Drydock's own pages on a preview host.
func previewPage(w http.ResponseWriter, status int, title, sentence string) {
	h := w.Header()
	for k := range h {
		delete(h, k) // nothing of the app's (a 1xx's leftovers) rides Drydock's page
	}
	noStore(w)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>%s</title>`+
		`<h1>%s</h1><p data-test="preview-failure">%s</p><p><a href="">Try again</a></p>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(sentence))
}

// proxyWriter is the writer ReverseProxy sees: the front door's (a
// CookieGuard), with Hijack wrapped so an upgraded connection's client side
// is the upgrade's to watch and close.
type proxyWriter struct {
	http.ResponseWriter
	u        *upgrade
	hijacked atomic.Bool
}

func (w *proxyWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *proxyWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, brw, err := h.Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.hijacked.Store(true)
	return w.u.client(conn), brw, nil
}

func (w *proxyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// upgrade is one upgraded connection's two sides and the watch that closes
// both after IdleTimeout without a byte either way (PF §10.7).
type upgrade struct {
	p       *Proxy
	target  Target
	recheck func(context.Context) bool
	last    atomic.Int64 // UnixNano of the last byte, by p.Clock
	mu      sync.Mutex
	closers []io.Closer
	done    chan struct{}
	once    sync.Once
}

func (u *upgrade) touch() { u.last.Store(u.p.clock().Now().UnixNano()) }

func (u *upgrade) add(c io.Closer) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	select {
	case <-u.done:
		return false
	default:
	}
	u.closers = append(u.closers, c)
	return true
}

// backend wraps the upstream side (ModifyResponse, before the hijack).
func (u *upgrade) backend(rwc io.ReadWriteCloser) io.ReadWriteCloser {
	b := &activeRWC{rwc: rwc, u: u}
	if !u.add(rwc) {
		rwc.Close()
	}
	return b
}

// client wraps the device's side and starts the watch: both sides exist now.
func (u *upgrade) client(c net.Conn) net.Conn {
	if !u.add(c) {
		c.Close()
		return c
	}
	u.touch()
	u.p.mu.Lock()
	closed := u.p.closed
	if !closed {
		u.p.upgrades[u] = struct{}{}
	}
	u.p.mu.Unlock()
	if closed {
		u.close()
		return &activeConn{Conn: c, u: u}
	}
	go u.watch()
	return &activeConn{Conn: c, u: u}
}

func (u *upgrade) idle() time.Duration {
	if u.p.IdleTimeout > 0 {
		return u.p.IdleTimeout
	}
	return DefaultIdleTimeout
}

// watch sleeps until the connection could have been idle for the whole
// timeout — or, sooner, until its preview session is due to be asked about
// again — then looks: a session that no longer holds closes both sides, and so
// does a connection idle that long; otherwise it sleeps again. Sleeping on
// Clock.After is what lets a test drive it from a fake clock.
func (u *upgrade) watch() {
	limit := u.idle()
	every := u.p.RecheckEvery
	if every <= 0 {
		every = DefaultRecheckEvery
	}
	idleLeft := limit
	// Once at once, now that the upgrade is registered: a disable or a
	// sign-out whose CloseWhere ran after the gate passed this request but
	// before it registered found nothing to close, and would otherwise be
	// noticed only at the first RecheckEvery.
	if u.recheck != nil && !u.stillHolds() {
		u.close()
		return
	}
	for {
		wait := idleLeft
		if u.recheck != nil && every < wait {
			wait = every
		}
		select {
		case <-u.p.clock().After(wait):
		case <-u.done:
			return
		}
		if u.recheck != nil && !u.stillHolds() {
			u.close()
			return
		}
		quiet := u.p.clock().Now().Sub(time.Unix(0, u.last.Load()))
		if quiet >= limit {
			u.close()
			return
		}
		idleLeft = limit - quiet
	}
}

// stillHolds asks the gate's question again, bounded, and fails closed.
func (u *upgrade) stillHolds() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return u.recheck(ctx)
}

func (u *upgrade) close() {
	u.once.Do(func() {
		u.mu.Lock()
		close(u.done)
		cs := u.closers
		u.closers = nil
		u.mu.Unlock()
		for _, c := range cs {
			c.Close()
		}
		u.p.mu.Lock()
		delete(u.p.upgrades, u)
		u.p.mu.Unlock()
	})
}

func (p *Proxy) clock() sys.Clock {
	if p.Clock == nil {
		return sys.RealClock{}
	}
	return p.Clock
}

type activeConn struct {
	net.Conn
	u *upgrade
}

func (c *activeConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.u.touch()
	}
	return n, err
}

func (c *activeConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.u.touch()
	}
	return n, err
}

// CloseWrite keeps ReverseProxy's half-close: when the app ends its side, the
// device's side is told so rather than left waiting.
func (c *activeConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

type activeRWC struct {
	rwc io.ReadWriteCloser
	u   *upgrade
}

func (c *activeRWC) Read(b []byte) (int, error) {
	n, err := c.rwc.Read(b)
	if n > 0 {
		c.u.touch()
	}
	return n, err
}

func (c *activeRWC) Write(b []byte) (int, error) {
	n, err := c.rwc.Write(b)
	if n > 0 {
		c.u.touch()
	}
	return n, err
}

func (c *activeRWC) Close() error { return c.rwc.Close() }
