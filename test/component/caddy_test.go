// Package component holds tests that drive real binaries — here, Caddy — with
// real sockets and nothing mocked (testing plan §3).
//
// This file is testing §3.2: the Caddyfile is a tested artifact. Three of the
// design's defenses live in that one file — strict Host matching (§13.3),
// header_up replacing rather than appending X-Forwarded-For (§13.2), and no
// access log on the preview host (port forwarding §7) — so the file that ships,
// deploy/Caddyfile, is run here byte for byte under real Caddy, with
// recording Unix-socket backends standing where Drydock would.
package component

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	uiHost        = "drydock.test"
	previewDomain = "drydock-preview.test"
	previewHost   = "myapp-5173-p2mq.drydock-preview.test"
)

// recorder is a stand-in for Drydock on one socket. It records every request
// that reaches it, which is the only way to assert "this never got here".
type recorder struct {
	mu   sync.Mutex
	seen []*http.Request
	hold chan struct{} // released at teardown, for the held SSE response
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	rc.seen = append(rc.seen, r.Clone(context.Background()))
	rc.mu.Unlock()
	if r.URL.Path == "/api/events" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select { // hold the stream open: the frame must arrive anyway
		case <-rc.hold:
		case <-r.Context().Done():
		}
		return
	}
	fmt.Fprintf(w, "reached %s", r.Host)
}

func (rc *recorder) reset() { rc.mu.Lock(); rc.seen = nil; rc.mu.Unlock() }
func (rc *recorder) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.seen)
}
func (rc *recorder) last() *http.Request {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.seen) == 0 {
		return nil
	}
	return rc.seen[len(rc.seen)-1]
}

var env struct {
	root                 string // everything every Caddy writes lives under here
	pool                 *x509.CertPool
	apiSock, previewSock string
	sites                string // optional-sites dir holding a copy of deploy/preview.caddy
	api, preview         *recorder
	main                 *caddyProc // the shipped Caddyfile, unmodified
	skip                 string
	// Shorthands for the main instance.
	httpsPort, adminSock, caddyOut string
	client                         *http.Client
}

func TestMain(m *testing.M) {
	code := func() int {
		if _, err := exec.LookPath("caddy"); err != nil {
			env.skip = "caddy is not installed"
			return m.Run()
		}
		stop, err := setup()
		if err != nil {
			fmt.Fprintln(os.Stderr, "setup:", err)
			return 1
		}
		defer stop()
		return m.Run()
	}()
	os.Exit(code)
}

func need(t *testing.T) {
	t.Helper()
	if env.skip != "" {
		t.Skip(env.skip)
	}
	env.api.reset()
	env.preview.reset()
}

// shippedCaddyfile is the path of the file that ships.
func shippedCaddyfile() string {
	p, _ := filepath.Abs(filepath.Join("..", "..", "deploy", "Caddyfile"))
	return p
}

func setup() (func(), error) {
	root, err := os.MkdirTemp("", "drydock-caddy-")
	if err != nil {
		return nil, err
	}
	env.root = root
	if env.pool, err = writeCerts(root); err != nil {
		return nil, err
	}

	// Recording backends on Unix sockets, where Drydock's two muxes would be.
	// Every Caddy instance in this package proxies to these same two.
	env.api = &recorder{hold: make(chan struct{})}
	env.preview = &recorder{hold: make(chan struct{})}
	var servers []*http.Server
	sock := func(name string, h http.Handler) (string, error) {
		p := filepath.Join(root, name)
		ln, err := net.Listen("unix", p)
		if err != nil {
			return "", err
		}
		s := &http.Server{Handler: h}
		servers = append(servers, s)
		go s.Serve(ln)
		return p, nil
	}
	if env.apiSock, err = sock("http.sock", env.api); err != nil {
		return nil, err
	}
	if env.previewSock, err = sock("preview.sock", env.preview); err != nil {
		return nil, err
	}

	stopBackends := func() {
		close(env.api.hold)
		close(env.preview.hold)
		for _, s := range servers {
			s.Close()
		}
		os.RemoveAll(root)
	}
	// The optional-sites directory holds a byte-for-byte copy of the shipped
	// preview site, exactly as an operator installs it.
	if env.sites, err = sitesDir("with-preview", true); err != nil {
		stopBackends()
		return nil, err
	}
	env.main, err = startCaddy(shippedCaddyfile(), env.sites, "main")
	if err != nil {
		stopBackends()
		return nil, err
	}
	env.httpsPort, env.adminSock, env.caddyOut, env.client = env.main.port, env.main.adminSock, env.main.out, env.main.client
	return func() { env.main.stop(); stopBackends() }, nil
}

// caddyProc is one running Caddy.
type caddyProc struct {
	cmd                  *exec.Cmd
	port, adminSock, out string
	client               *http.Client
	outFile              *os.File
}

// startCaddy runs `caddy run` on the given Caddyfile with this package's
// certificates and backends, on fresh ports, with its own data directories so
// two instances never contend for Caddy's storage lock.
func startCaddy(caddyfile, sites, tag string) (*caddyProc, error) {
	dir := filepath.Join(env.root, tag)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	c := &caddyProc{port: freePort(), adminSock: filepath.Join(dir, "admin.sock"), out: filepath.Join(dir, "caddy.log")}
	f, err := os.Create(c.out)
	if err != nil {
		return nil, err
	}
	c.outFile = f
	c.cmd = exec.Command("caddy", "run", "--config", caddyfile, "--adapter", "caddyfile")
	c.cmd.Stdout, c.cmd.Stderr = f, f
	root := env.root
	c.cmd.Env = append(os.Environ(),
		"HOME="+dir, "XDG_DATA_HOME="+filepath.Join(dir, "data"), "XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"DRYDOCK_UI_HOST="+uiHost, "DRYDOCK_PREVIEW_DOMAIN="+previewDomain,
		"DRYDOCK_UI_CERT="+filepath.Join(root, "ui.crt"), "DRYDOCK_UI_KEY="+filepath.Join(root, "ui.key"),
		"DRYDOCK_PREVIEW_CERT="+filepath.Join(root, "preview.crt"), "DRYDOCK_PREVIEW_KEY="+filepath.Join(root, "preview.key"),
		"DRYDOCK_API_SOCKET="+env.apiSock, "DRYDOCK_PREVIEW_SOCKET="+env.previewSock,
		"DRYDOCK_HTTPS_PORT="+c.port, "DRYDOCK_HTTP_PORT="+freePort(),
		"DRYDOCK_CADDY_ADMIN=unix/"+c.adminSock+"|0600",
		"DRYDOCK_CADDY_SITES="+sites,
	)
	if err := c.cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	port := c.port
	c.client = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: env.pool},
			// Every name resolves to this Caddy, the way the browser tier's
			// host-resolver-rules do it (testing §10.1).
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, "127.0.0.1:"+port)
			},
		},
	}
	for i := 0; i < 100; i++ {
		if conn, err := tls.Dial("tcp", "127.0.0.1:"+port, &tls.Config{RootCAs: env.pool, ServerName: uiHost}); err == nil {
			conn.Close()
			return c, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.stop()
	b, _ := os.ReadFile(c.out)
	return nil, fmt.Errorf("caddy (%s) never served TLS:\n%s", tag, b)
}

func (c *caddyProc) stop() {
	c.cmd.Process.Signal(os.Interrupt)
	c.cmd.Wait()
	c.outFile.Close()
}

func get(t *testing.T, url, host string, header ...string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if host != "" {
		req.Host = host
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	resp, err := env.client.Do(req)
	if err == nil {
		t.Cleanup(func() { resp.Body.Close() })
	}
	return resp, err
}

// ---- testing §3.2, row by row ------------------------------------------------

func TestUIHostReachesOnlyTheAPISocket(t *testing.T) {
	need(t)
	resp, err := get(t, "https://"+uiHost+"/api/repos", "")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("UI host: %v %v", resp, err)
	}
	if env.api.count() != 1 || env.preview.count() != 0 {
		t.Errorf("api saw %d, preview saw %d; want 1 and 0", env.api.count(), env.preview.count())
	}
}

// TestPreviewHostReachesOnlyThePreviewSocket also covers "Host survives the
// hop to a Unix socket" — port forwarding §9 depends on it as the routing key.
func TestPreviewHostReachesOnlyThePreviewSocket(t *testing.T) {
	need(t)
	resp, err := get(t, "https://"+previewHost+"/", "")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("preview host: %v %v", resp, err)
	}
	if env.preview.count() != 1 || env.api.count() != 0 {
		t.Fatalf("preview saw %d, api saw %d; want 1 and 0", env.preview.count(), env.api.count())
	}
	if h := env.preview.last().Host; h != previewHost {
		t.Errorf("the preview socket saw Host %q; want %q intact", h, previewHost)
	}
}

// TestForeignHostMatchesNoSiteBlock is DNS rebinding's defense (§13.3): the
// TLS name is right — the browser thinks it is talking to Drydock — but the
// Host header carries the attacker's name.
func TestForeignHostMatchesNoSiteBlock(t *testing.T) {
	need(t)
	for _, h := range []string{"evil.example", "drydock.test.evil.example", "sibling." + uiHost, "127.0.0.1"} {
		get(t, "https://"+uiHost+"/api/repos", h)
		if n := env.api.count() + env.preview.count(); n != 0 {
			t.Errorf("Host %q reached a backend (%d request(s))", h, n)
		}
	}
	// Control: the same request with the real Host does get through.
	get(t, "https://"+uiHost+"/api/repos", "")
	if env.api.count() != 1 {
		t.Fatal("control: the legitimate request was not recorded either")
	}
}

func TestBareIPMatchesNoSiteBlock(t *testing.T) {
	need(t)
	// No SNI and an IP Host: there is no certificate and no site for it.
	get(t, "https://127.0.0.1:"+env.httpsPort+"/api/repos", "")
	if n := env.api.count() + env.preview.count(); n != 0 {
		t.Errorf("a bare-IP request reached a backend (%d request(s))", n)
	}
}

// TestForwardedForIsReplaced is what makes Drydock's lockout keyable on the
// header: a client cannot plant an address.
func TestForwardedForIsReplaced(t *testing.T) {
	need(t)
	if _, err := get(t, "https://"+uiHost+"/x", "", "X-Forwarded-For", "6.6.6.6", "X-Forwarded-For", "7.7.7.7"); err != nil {
		t.Fatal(err)
	}
	got := env.api.last().Header.Values("X-Forwarded-For")
	if len(got) != 1 || got[0] != "127.0.0.1" {
		t.Errorf("backend saw X-Forwarded-For %q; want exactly [127.0.0.1]", got)
	}
}

func TestSSEIsNotBuffered(t *testing.T) {
	need(t)
	resp, err := get(t, "https://"+uiHost+"/api/events", "")
	if err != nil {
		t.Fatal(err)
	}
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "data: first\n" {
			t.Errorf("first line = %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first SSE frame did not arrive while the stream was held open: Caddy is buffering")
	}
}

// TestPreviewTokenIsNotLogged sweeps everything Caddy wrote — its output, its
// data and config directories — for the one-time token in the query string.
func TestPreviewTokenIsNotLogged(t *testing.T) {
	need(t)
	canary := "tok-" + randHex(16)
	if _, err := get(t, "https://"+previewHost+"/.drydock/session?t="+canary, ""); err != nil {
		t.Fatal(err)
	}
	if q := env.preview.last().URL.RawQuery; !strings.Contains(q, canary) {
		t.Fatal("control: the request carrying the token never reached the backend")
	}
	time.Sleep(200 * time.Millisecond) // let any log writes land
	swept, found := 0, ""
	filepath.Walk(env.root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		b, _ := os.ReadFile(p)
		swept += len(b)
		if strings.Contains(string(b), canary) {
			found = p
		}
		return nil
	})
	if found != "" {
		t.Errorf("the one-time token was written to %s", found)
	}
	if swept == 0 {
		t.Fatal("the sweep read nothing: it cannot have found a leak")
	}
}

// TestAdminIsNotOnLoopback: the finding behind the global block. Caddy's
// default admin API on localhost:2019 lets any local process reconfigure the
// one process that can reach Drydock's socket.
func TestAdminIsNotOnLoopback(t *testing.T) {
	need(t)
	if c, err := net.DialTimeout("tcp", "127.0.0.1:2019", time.Second); err == nil {
		c.Close()
		t.Error("something is listening on localhost:2019 — Caddy's default admin endpoint")
	}
	fi, err := os.Stat(env.adminSock)
	if err != nil {
		t.Fatalf("the admin socket does not exist: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("admin socket mode = %o; want 600", fi.Mode().Perm())
	}
	// Control: Caddy is up, so the absence above is not just a dead process.
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+env.httpsPort, time.Second); err != nil {
		t.Fatalf("control: Caddy is not listening at all: %v", err)
	} else {
		c.Close()
	}
}

// ---- helpers -------------------------------------------------------------------

func freePort() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return p
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// writeCerts issues a throwaway CA and two leaves on two registrable domains,
// exactly as the design requires — one for the UI host, one wildcard for
// previews — and returns a pool trusting the CA.
func writeCerts(dir string) (*x509.CertPool, error) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "drydock test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	ca, _ := x509.ParseCertificate(caDER)
	leaf := func(name string, serial int64, sans ...string) error {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: sans[0]}, DNSNames: sans,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
		if err != nil {
			return err
		}
		kb, _ := x509.MarshalECPrivateKey(k)
		os.WriteFile(filepath.Join(dir, name+".crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
		return os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	}
	if err := leaf("ui", 2, uiHost); err != nil {
		return nil, err
	}
	if err := leaf("preview", 3, "*."+previewDomain); err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool, nil
}

var _ = io.Discard

// TestForwardedForStaysReplacedWhenProxiesAreTrusted protects a line that the
// test above cannot. Caddy >=2.5 already discards a client's X-Forwarded-For
// when no proxy is trusted, so with today's global block, removing
// `header_up X-Forwarded-For {remote_host}` changes nothing observable — a
// mutation run confirmed it. The line earns its place the day someone adds
// `trusted_proxies`, as putting Tailscale in front of this Caddy (§13.1)
// plausibly would: Caddy then keeps a trusted peer's header, and only the
// explicit header_up stops a client planting an address in Drydock's lockout.
// So this runs the shipped site blocks, unmodified, under that future global.
func TestForwardedForStaysReplacedWhenProxiesAreTrusted(t *testing.T) {
	need(t)
	shipped, err := os.ReadFile(shippedCaddyfile())
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "\thttp_port {$DRYDOCK_HTTP_PORT:80}\n"
	if !strings.Contains(string(shipped), anchor) {
		t.Fatalf("the global block no longer contains %q; update this test's anchor", anchor)
	}
	derived := strings.Replace(string(shipped), anchor,
		anchor+"\tservers {\n\t\ttrusted_proxies static private_ranges\n\t}\n", 1)
	path := filepath.Join(env.root, "Caddyfile.trusted-proxies")
	if err := os.WriteFile(path, []byte(derived), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := startCaddy(path, env.sites, "trusted-proxies")
	if err != nil {
		t.Fatal(err)
	}
	defer c.stop()

	req, _ := http.NewRequest("GET", "https://"+uiHost+"/x", nil)
	req.Header.Add("X-Forwarded-For", "6.6.6.6")
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := env.api.last().Header.Values("X-Forwarded-For")
	if len(got) != 1 || got[0] != "127.0.0.1" {
		t.Errorf("with trusted_proxies set, the backend saw X-Forwarded-For %q; want exactly [127.0.0.1]", got)
	}
}

// sitesDir builds an optional-sites directory: empty, or holding a byte-for-byte
// copy of deploy/preview.caddy.
func sitesDir(name string, withPreview bool) (string, error) {
	dir := filepath.Join(env.root, "sites-"+name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if withPreview {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(shippedCaddyfile()), "preview.caddy"))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "preview.caddy"), b, 0o600); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// TestUIOnlyDeployment is the first real deployment: no preview wildcard
// certificate yet, so no preview.caddy installed. Caddy must still load the
// shipped file and serve the UI — and a preview hostname must reach nothing,
// rather than falling through to some other block.
func TestUIOnlyDeployment(t *testing.T) {
	need(t)
	empty, err := sitesDir("empty", false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := startCaddy(shippedCaddyfile(), empty, "ui-only")
	if err != nil {
		t.Fatalf("the shipped Caddyfile does not load without the preview site: %v", err)
	}
	defer c.stop()
	req, _ := http.NewRequest("GET", "https://"+uiHost+"/api/repos", nil)
	resp, err := c.client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("UI host on a UI-only deployment: %v %v", resp, err)
	}
	resp.Body.Close()
	if env.api.count() != 1 {
		t.Fatalf("control: the UI request did not reach the API socket")
	}
	req, _ = http.NewRequest("GET", "https://"+previewHost+"/", nil)
	if resp, err := c.client.Do(req); err == nil {
		resp.Body.Close()
	}
	if env.preview.count() != 0 {
		t.Error("a preview hostname reached the preview socket with no preview site installed")
	}
}
