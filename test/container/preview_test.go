package container_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/github/githubtest"
	"github.com/krelinga/drydock/internal/preview"
)

// viteVersion is the Vite the workspace installs: web/'s own pin, so the one
// dev server this tier proves is the one this repository already trusts.
const viteVersion = "8.3.2"

// The Vite project the test repository commits: a page, a module that
// accepts its own hot updates, and a config whose plugin adds what the test
// reads through the proxy — an event stream, the request's own headers, and
// an echo over Vite's HMR websocket (a custom event, answered to the client
// that sent it).
const (
	viteIndex  = `<!doctype html><html><head><meta charset="utf-8"><title>preview</title></head><body><p id="out">loading</p><script type="module" src="/main.js"></script></body></html>`
	viteMain   = "document.getElementById('out').textContent = 'version one'\nif (import.meta.hot) import.meta.hot.accept()\n"
	viteConfig = `export default {
  server: { host: '0.0.0.0', port: 5173, strictPort: true },
  plugins: [{
    name: 'drydock-tier',
    configureServer(server) {
      server.middlewares.use('/sse', (req, res) => {
        res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' })
        res.write('data: one\n\n')
        setTimeout(() => res.end('data: two\n\n'), 2000)
      })
      server.middlewares.use('/headers', (req, res) => {
        res.setHeader('Content-Type', 'application/json')
        res.end(JSON.stringify(req.headers))
      })
      server.ws.on('drydock:echo', (data, client) => client.send('drydock:echo', data))
    },
  }],
}
`
)

// TestViteHMRThroughThePreviewProxy is PF §13 step 3's done-when in the
// container tier: a real Vite dev server, with HMR, in a workspace container
// that Drydock made — through the real server, the real devcontainer CLI and
// the Feature from this checkout — reached through the real preview socket by
// a device that ran the handshake. Nothing in the container is published; the
// proxy dials the container's address on Docker's bridge, resolved by label.
//
//   - HTTP: the page, Vite's client and the module come back through the
//     proxy; Vite saw Host localhost:5173, the preview host as
//     X-Forwarded-Host, https, and no preview cookie.
//   - Host: switched to passthrough, Vite refuses the preview host ("Blocked
//     request") — which is why localhost is the default (PF §8.3); switched
//     back, it serves again.
//   - An event stream's first event arrives seconds before its last.
//   - The HMR websocket: Vite's `connected`, an echo of a custom event sent
//     up it, and — after the module is edited on the host, in the clone the
//     container bind-mounts — Vite's hot update for it, all through the
//     proxy.
//   - The container stopped behind Drydock's back (`docker stop`, the row
//     still running): the next request is the denied page, never a dial to
//     the address it had (PF §8.1).
//
// Vite is installed in the container from npm, as a developer would; the
// image is node 22 on bookworm-slim, the base Drydock already pins for its
// Claude image (config.DefaultClaudeBaseImage), with its own `node` user.
func TestViteHMRThroughThePreviewProxy(t *testing.T) {
	needDevcontainer(t)
	image := config.DefaultClaudeBaseImage
	pullImage(t, image)
	p := prefix(t)

	f := githubtest.New(t, 4242, time.Now)
	devcontainer := `{"image":"` + image + `","remoteUser":"node"}`
	f.Installations = []githubtest.Installation{{ID: 77, Account: "krelinga", Repos: []githubtest.Repo{
		{ID: 101, FullName: "krelinga/viteapp", DefaultBranch: "main", PushedAt: time.Now(),
			Files: []string{"README.md", ".devcontainer/devcontainer.json", "index.html", "main.js", "vite.config.js"},
			Contents: map[string]string{".devcontainer/devcontainer.json": devcontainer,
				"index.html": viteIndex, "main.js": viteMain, "vite.config.js": viteConfig}},
	}}}
	f.EnableGit(t)

	dir, err := os.MkdirTemp("", "ddv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	u, _ := user.Current()
	g, _ := user.LookupGroupId(u.Gid)
	key := filepath.Join(dir, "app.pem")
	os.WriteFile(key, githubtest.KeyPEM(t), 0o400)
	cfg := config.Default()
	cfg.DiskLimitPercent = 100
	cfg.UIOrigin, cfg.UIHost, cfg.PreviewDomain = "https://drydock.test", "drydock.test", "drydock-preview.test"
	cfg.DatabasePath = filepath.Join(dir, "drydock.db")
	cfg.APISocket, cfg.PreviewSocket = filepath.Join(dir, "http.sock"), filepath.Join(dir, "preview.sock")
	cfg.BrokerDir, cfg.WorkspaceRoot = filepath.Join(dir, "sock"), filepath.Join(dir, "ws")
	cfg.SocketGroup, cfg.LabelPrefix = g.Name, p
	cfg.GitHubAppID, cfg.GitHubAppKey, cfg.GitHubAPI = 4242, key, f.URL
	cfg.BotName, cfg.BotEmail = "krelinga-drydock-dev[bot]", botEmail
	cfg.Feature, cfg.ClaudeVolume = newFeatureRegistry(t).Drydock, claudeVolume(p)

	srv, err := newServer(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.Provisioner.Cloner.BaseURL = f.URL
	noLogin(t, srv.DB.DB, cfg.ClaudeVolume)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	c := &client{t: t, sock: cfg.APISocket}
	if err := srv.Auth.SetPassword(context.Background(), "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	c.cookie = c.signIn("correct horse battery staple")
	deadline := time.Now().Add(10 * time.Second)
	for {
		var repos struct{ Repos []struct{ ID int64 } }
		c.get("/api/repos", &repos)
		if len(repos.Repos) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the catalog never filled")
		}
		time.Sleep(50 * time.Millisecond)
	}

	status, body := c.post("/api/workspaces", `{"repository_id":101}`)
	var created struct{ ID string }
	if status != 202 || json.Unmarshal([]byte(body), &created) != nil || created.ID == "" {
		t.Fatalf("create: %d %s", status, body)
	}
	ws := created.ID
	// The images `up` built for this workspace's folder (vsc-repo-…-features
	// and -features-uid) are named by its folder, not labelled, so prefix's
	// cleanup never finds them: remove the container first, then them.
	t.Cleanup(func() {
		ctx := context.Background()
		if ids, _ := manager(p).Find(ctx, ws); len(ids) > 0 {
			exec.Command("docker", append([]string{"rm", "-f", "--"}, ids...)...).Run()
		}
		if _, err := manager(p).RemoveBuiltImages(ctx, filepath.Join(cfg.WorkspaceRoot, ws, "repo")); err != nil {
			t.Logf("removing the images up built: %v", err)
		}
	})
	deadline = time.Now().Add(15 * time.Minute)
	for {
		var v struct {
			State  string
			Events []runEvent
		}
		c.get("/api/workspaces/"+ws, &v)
		if v.State == "failed" {
			t.Fatalf("the workspace failed: %+v", v)
		}
		if v.State == "running" && settled(t, v.State, v.Events) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never running: %+v", v)
		}
		time.Sleep(2 * time.Second)
	}
	ids, err := manager(p).Find(context.Background(), ws)
	if err != nil || len(ids) != 1 {
		t.Fatalf("the workspace's container: %v %v", ids, err)
	}
	id := ids[0]
	if ports := docker(t, "inspect", "--format", "{{json .NetworkSettings.Ports}}", id); strings.Contains(ports, "HostPort") {
		t.Fatalf("the container publishes a port: %s", ports)
	}

	// Vite, from npm, as root; then served as the remote user from the clone.
	folder := docker(t, "inspect", "--format", `{{range .Mounts}}{{if eq .Source "`+filepath.Join(cfg.WorkspaceRoot, ws, "repo")+`"}}{{.Destination}}{{end}}{{end}}`, id)
	if folder == "" {
		t.Fatal("no mount of the clone in the container")
	}
	if out, err := exec.Command("docker", "exec", "-u", "0", id, "npm", "install", "--global", "--no-fund", "--no-audit", "vite@"+viteVersion).CombinedOutput(); err != nil {
		t.Fatalf("npm install vite: %v\n%s", err, out)
	}
	docker(t, "exec", "-d", "-u", "node", "-w", folder, id, "sh", "-c", "vite > /tmp/vite.log 2>&1")
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "exec", id, "cat", "/tmp/vite.log").CombinedOutput()
			t.Logf("vite's log:\n%s", out)
		}
	})

	// The port, enabled, as step 4's registry will write it.
	slug := "viteapp-5173-t3st"
	previewHost := slug + "." + cfg.PreviewDomain
	if _, err := srv.DB.Exec(`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, enabled) VALUES ('pvite', ?, 5173, ?, 1)`, ws, slug); err != nil {
		t.Fatal(err)
	}

	// The handshake.
	resp := c.authorize("https://" + previewHost + "/")
	su, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || su.Path != preview.SessionPath {
		t.Fatalf("authorize: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	pv := &previewClient{t: t, sock: cfg.PreviewSocket, host: previewHost}
	resp, _ = pv.get(su.RequestURI(), nil)
	for _, ck := range resp.Cookies() {
		if ck.Name == preview.CookieName {
			pv.cookie = ck.Value
		}
	}
	if pv.cookie == "" {
		t.Fatalf("no preview cookie: %d %v", resp.StatusCode, resp.Header)
	}

	// HTTP, once Vite is up: the 502 page until then.
	deadline = time.Now().Add(2 * time.Minute)
	var page string
	for {
		resp, page = pv.get("/", nil)
		if resp.StatusCode == 200 {
			break
		}
		if resp.StatusCode != http.StatusBadGateway || time.Now().After(deadline) {
			t.Fatalf("waiting for Vite: %d %q", resp.StatusCode, page)
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(page, "/@vite/client") || !strings.Contains(page, `src="/main.js"`) {
		t.Fatalf("the page, through the proxy: %q", page)
	}
	_, client := pv.get("/@vite/client", nil)
	m := regexp.MustCompile(`const wsToken = "([^"]+)"`).FindStringSubmatch(client)
	if m == nil {
		t.Fatalf("no websocket token in Vite's client (%d bytes)", len(client))
	}
	token := m[1]
	if resp, js := pv.get("/main.js", nil); resp.StatusCode != 200 || !strings.Contains(js, "version one") {
		t.Fatalf("the module: %d %q", resp.StatusCode, js)
	}
	_, hb := pv.get("/headers", http.Header{"X-Forwarded-Host": {"evil.example"}, "X-Forwarded-For": {"192.0.2.44"}})
	var seen map[string]string
	if err := json.Unmarshal([]byte(hb), &seen); err != nil {
		t.Fatalf("headers: %q", hb)
	}
	for k, want := range map[string]string{"host": "localhost:5173", "x-forwarded-host": previewHost,
		"x-forwarded-proto": "https", "x-forwarded-for": "192.0.2.44"} {
		if seen[k] != want {
			t.Errorf("Vite saw %s %q; want %q", k, seen[k], want)
		}
	}
	if strings.Contains(seen["cookie"], preview.CookieName) || strings.Contains(hb, pv.cookie) {
		t.Errorf("Vite saw the preview cookie: %q", seen["cookie"])
	}

	// Passthrough: Vite's own host check refuses the preview host.
	setHost := func(v string) {
		t.Helper()
		if _, err := srv.DB.Exec(`UPDATE forwarded_port SET host_header = ? WHERE id = 'pvite'`, v); err != nil {
			t.Fatal(err)
		}
	}
	setHost(preview.HostPassthrough)
	if resp, b := pv.get("/", nil); resp.StatusCode != http.StatusForbidden || !strings.Contains(b, "Blocked request") {
		t.Errorf("passthrough: %d %q; want Vite's own refusal of the preview host", resp.StatusCode, b)
	}
	setHost(preview.HostLocalhost)
	if resp, _ := pv.get("/", nil); resp.StatusCode != 200 {
		t.Errorf("localhost again: %d", resp.StatusCode)
	}

	// An event stream: the first event long before the last.
	start := time.Now()
	sresp := pv.stream("/sse")
	br := bufio.NewReader(sresp.Body)
	first, _ := br.ReadString('\n')
	firstAt := time.Since(start)
	rest, _ := io.ReadAll(br)
	sresp.Body.Close()
	endAt := time.Since(start)
	if first != "data: one\n" || !strings.Contains(string(rest), "data: two") || endAt-firstAt < time.Second {
		t.Errorf("the event stream: %q at %s, then %q at %s; want the first a second or more before the end", first, firstAt, rest, endAt)
	}

	// The HMR websocket, as Vite's client opens it.
	hmr := pv.websocket(token)
	defer hmr.Close()
	if msg := hmr.next(`"connected"`); msg == "" {
		t.Fatal("Vite never said connected")
	}
	nonce := fmt.Sprintf("n%d", time.Now().UnixNano())
	if err := websocket.Message.Send(hmr.ws, `{"type":"custom","event":"drydock:echo","data":{"n":"`+nonce+`"}}`); err != nil {
		t.Fatal(err)
	}
	if msg := hmr.next(nonce); !strings.Contains(msg, "drydock:echo") {
		t.Errorf("the echo up and back down the socket: %q", msg)
	}
	// Edit the module on the host; the container sees it through the bind
	// mount, and Vite pushes the update down the proxied socket.
	if err := os.WriteFile(filepath.Join(cfg.WorkspaceRoot, ws, "repo", "main.js"),
		[]byte(strings.Replace(viteMain, "version one", "version two", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := hmr.next(`/main.js`); !strings.Contains(msg, `"type":"update"`) {
		t.Errorf("the hot update: %q", msg)
	}
	if resp, js := pv.get("/main.js?t=1", nil); resp.StatusCode != 200 || !strings.Contains(js, "version two") {
		t.Errorf("the updated module: %d %q", resp.StatusCode, js)
	}
	hmr.Close()

	// Stopped behind Drydock's back: the row still says running, the label
	// resolves to nothing running, and the answer is the denied page.
	docker(t, "stop", "--time", "1", "--", id)
	deadline = time.Now().Add(30 * time.Second)
	for {
		resp, b := pv.get("/", nil)
		if resp.StatusCode == http.StatusFound && resp.Header.Get("Location") == preview.DeniedPath {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("with the container stopped: %d %q %q", resp.StatusCode, resp.Header.Get("Location"), b)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// previewClient is a device signed in to one preview, over the preview
// socket, with the Host and X-Forwarded-For Caddy would send.
type previewClient struct {
	t      *testing.T
	sock   string
	host   string
	cookie string
}

func (p *previewClient) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", p.sock)
}

func (p *previewClient) request(path string, hdr http.Header) *http.Request {
	req, err := http.NewRequest("GET", "http://"+p.host+path, nil)
	if err != nil {
		p.t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", "192.0.2.10")
	for k, v := range hdr {
		req.Header[k] = v
	}
	if p.cookie != "" {
		req.AddCookie(&http.Cookie{Name: preview.CookieName, Value: p.cookie})
		req.AddCookie(&http.Cookie{Name: "app-own", Value: "kept"})
	}
	return req
}

func (p *previewClient) get(path string, hdr http.Header) (*http.Response, string) {
	p.t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: p.dial, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Timeout: time.Minute}
	resp, err := hc.Do(p.request(path, hdr))
	if err != nil {
		p.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (p *previewClient) stream(path string) *http.Response {
	p.t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: p.dial, DisableKeepAlives: true}, Timeout: time.Minute}
	resp, err := hc.Do(p.request(path, nil))
	if err != nil || resp.StatusCode != 200 {
		p.t.Fatalf("GET %s: %v %v", path, resp, err)
	}
	return resp
}

type hmrSocket struct {
	t    *testing.T
	ws   *websocket.Conn
	conn net.Conn
}

// websocket opens Vite's HMR socket the way its client does: the page's own
// origin, the `vite-hmr` protocol and the token from /@vite/client.
func (p *previewClient) websocket(token string) *hmrSocket {
	p.t.Helper()
	conf, err := websocket.NewConfig("ws://"+p.host+"/?token="+url.QueryEscape(token), "https://"+p.host)
	if err != nil {
		p.t.Fatal(err)
	}
	conf.Protocol = []string{"vite-hmr"}
	conf.Header = http.Header{"Cookie": {preview.CookieName + "=" + p.cookie}, "X-Forwarded-For": {"192.0.2.10"}}
	conn, err := net.Dial("unix", p.sock)
	if err != nil {
		p.t.Fatal(err)
	}
	ws, err := websocket.NewClient(conf, conn)
	if err != nil {
		conn.Close()
		p.t.Fatalf("the HMR websocket through the proxy: %v", err)
	}
	return &hmrSocket{t: p.t, ws: ws, conn: conn}
}

// next reads messages until one contains want, for up to a minute.
func (h *hmrSocket) next(want string) string {
	h.t.Helper()
	h.conn.SetReadDeadline(time.Now().Add(time.Minute))
	var seen []string
	for {
		var msg string
		if err := websocket.Message.Receive(h.ws, &msg); err != nil {
			h.t.Errorf("waiting for %s: %v (saw %q)", want, err, seen)
			return ""
		}
		if strings.Contains(msg, want) {
			return msg
		}
		seen = append(seen, msg)
	}
}

func (h *hmrSocket) Close() { h.ws.Close() }

// authorize is GET /preview/authorize on the API socket, signed in, with the
// redirect read rather than followed.
func (c *client) authorize(ret string) *http.Response {
	c.t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", c.sock)
	}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", "http://drydock.test/preview/authorize?return="+url.QueryEscape(ret), nil)
	req.Header.Set("X-Forwarded-For", "192.0.2.10")
	req.AddCookie(&http.Cookie{Name: "__Host-drydock", Value: c.cookie})
	resp, err := hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}
