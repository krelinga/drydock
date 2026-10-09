package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/preview"
)

// countingResolver is loopbackResolver that counts what it is asked, and can
// be told the container moved after the connect.
type countingResolver struct {
	mu                 sync.Mutex
	resolves, confirms int
	moved              bool
}

func (c *countingResolver) Resolve(context.Context, string) (preview.Endpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolves++
	return preview.Endpoint{ContainerID: "c", IP: netip.MustParseAddr("127.0.0.1")}, nil
}

func (c *countingResolver) Confirm(context.Context, string, preview.Endpoint) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.confirms++
	if c.moved {
		return preview.ErrNotRunning
	}
	return nil
}

func (c *countingResolver) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolves, c.confirms
}

// seedDisabledPort is one running workspace with one port listed by hand and
// not enabled: what POST …/ports leaves, with a known slug.
func seedDisabledPort(t *testing.T, r *running, port int) {
	t.Helper()
	<-r.srv.reconciled
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (7, 1, 'o/myapp', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('wprev', 7, '/x', 'main', 'running')`,
		`INSERT INTO forwarded_port (id, workspace_id, container_port, slug, manual) VALUES ('pprev', 'wprev', ?, '` + previewSlug + `', 1)`,
	} {
		if _, err := r.srv.DB.Exec(q, port); err != nil {
			t.Fatal(err)
		}
	}
}

func readJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%d %q: %v", resp.StatusCode, b, err)
	}
}

// eventKinds is every event kind the log holds for the workspace, in order.
func eventKinds(t *testing.T, r *running) []string {
	t.Helper()
	es, err := r.srv.Events.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if e.WorkspaceID == "wprev" && strings.HasPrefix(e.Kind, "port.") {
			out = append(out, e.Kind)
		}
	}
	return out
}

// TestPortRegistryOverTheSockets is PF §13 step 4's done-when against the real
// server — enable a port, open it, disable it, watch it close — over the real
// API and preview sockets with a real dev server behind the proxy:
//
//   - listed, the port is off and has no URL; a PATCH enables it (202, then
//     port.enabled), and the device's handshake and websocket work;
//   - the probe dials through the proxy's own resolver — counted — and says
//     what the proxy would; a container that moved after the connect is
//     not_running there too;
//   - a PATCH disables it and its open websocket closes at once, with the
//     recheck an hour away; the device's next request is the handshake, which
//     ends on its own host with Clear-Site-Data (PF §10.3);
//   - a DELETE retires it, and the port listed again has a new slug.
func TestPortRegistryOverTheSockets(t *testing.T) {
	app := newDevServer(t)
	res := &countingResolver{}
	r := startWith(t, t.TempDir(), nil, func(s *Server) {
		s.Proxy.Resolver = res
		s.Proxy.RecheckEvery = time.Hour
	})
	seedDisabledPort(t, r, app.port())
	ui := r.signIn(t)
	base := "/api/workspaces/wprev/ports"

	var list api.PortList
	readJSON(t, r.do(t, req{method: "GET", path: base, cookie: ui}), &list)
	if !list.Previews || len(list.Ports) != 1 || list.Ports[0].Enabled || list.Ports[0].URL != nil || list.Ports[0].Slug != previewSlug {
		t.Fatalf("the list before enabling = %+v", list)
	}

	// Enable.
	if resp := r.do(t, req{method: "PATCH", path: base + "/pprev", origin: uiOrigin, cookie: ui, body: `{"enabled":true}`}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enable = %d", resp.StatusCode)
	}
	if k := eventKinds(t, r); len(k) != 1 || k[0] != preview.KindPortEnabled {
		t.Fatalf("events after enabling = %v", k)
	}
	readJSON(t, r.do(t, req{method: "GET", path: base, cookie: ui}), &list)
	if p := list.Ports[0]; !p.Enabled || p.URL == nil || *p.URL != "https://"+previewHost+"/" {
		t.Fatalf("the enabled port = %+v", p)
	}

	// Open it: the handshake, then an HMR socket.
	pc := r.previewCookieFor(t, ui)
	resp, ws, br := r.previewUpgrade(t, "/hmr", preview.CookieName+"="+pc)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d", resp.StatusCode)
	}
	br.ReadString('\n')
	if !echoes(ws, br, 5*time.Second) {
		t.Fatal("control: the open websocket does not echo")
	}

	// Probe: the proxy's resolver, asked again.
	r0, c0 := res.counts()
	var probe preview.ProbeResult
	readJSON(t, r.do(t, req{method: "GET", path: base + "/pprev/probe", cookie: ui}), &probe)
	if r1, c1 := res.counts(); probe.Outcome != preview.ProbeAnswering || r1 != r0+1 || c1 != c0+1 {
		t.Errorf("probe = %+v with %d resolutions and %d confirmations; want answering through the proxy's one of each", probe, r1-r0, c1-c0)
	}
	res.mu.Lock()
	res.moved = true
	res.mu.Unlock()
	readJSON(t, r.do(t, req{method: "GET", path: base + "/pprev/probe", cookie: ui}), &probe)
	if probe.Outcome != preview.ProbeNotRunning {
		t.Errorf("probe of a container that moved after the connect = %+v", probe)
	}
	res.mu.Lock()
	res.moved = false
	res.mu.Unlock()

	// Disable: the websocket closes at once.
	if resp := r.do(t, req{method: "PATCH", path: base + "/pprev", origin: uiOrigin, cookie: ui, body: `{"enabled":false}`}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("disable = %d", resp.StatusCode)
	}
	if !closes(ws, br, time.Second) {
		t.Error("disabling the port left its websocket open")
	}
	if k := eventKinds(t, r); len(k) != 2 || k[1] != preview.KindPortDisabled {
		t.Errorf("events after disabling = %v", k)
	}
	presp, _ := r.previewDo(t, "GET", "https://"+previewHost+"/after", http.Header{"Cookie": {preview.CookieName + "=" + pc}})
	if presp.StatusCode != http.StatusFound || !strings.HasPrefix(presp.Header.Get("Location"), uiOrigin+"/preview/authorize?") {
		t.Errorf("after disabling, the preview = %d %q; want the handshake", presp.StatusCode, presp.Header.Get("Location"))
	}
	aresp := r.do(t, req{method: "GET", path: "/preview/authorize?return=" + url.QueryEscape("https://"+previewHost+"/after"), cookie: ui})
	su, err := url.Parse(aresp.Header.Get("Location"))
	if aresp.StatusCode != http.StatusFound || err != nil || su.Path != preview.SessionPath {
		t.Fatalf("authorize for a disabled port = %d %q", aresp.StatusCode, aresp.Header.Get("Location"))
	}
	cresp, _ := r.previewDo(t, "GET", su.String(), nil)
	if cresp.StatusCode != http.StatusForbidden || cresp.Header.Get("Clear-Site-Data") != api.ClearSiteData || cookieFrom(cresp, preview.CookieName) != nil {
		t.Errorf("the disabled port's landing = %d %v; want 403 with Clear-Site-Data and no cookie", cresp.StatusCode, cresp.Header)
	}

	// Retire, and list the port again: a new slug.
	if resp := r.do(t, req{method: "DELETE", path: base + "/pprev", origin: uiOrigin, cookie: ui}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("retire = %d", resp.StatusCode)
	}
	if resp := r.do(t, req{method: "GET", path: base + "/pprev/probe", cookie: ui}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("probe of a retired port = %d; want 404", resp.StatusCode)
	}
	if resp := r.do(t, req{method: "POST", path: base, origin: uiOrigin, cookie: ui,
		body: `{"container_port":` + itoa(app.port()) + `,"label":"again"}`}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("add = %d", resp.StatusCode)
	}
	readJSON(t, r.do(t, req{method: "GET", path: base, cookie: ui}), &list)
	if len(list.Ports) != 1 || list.Ports[0].Slug == previewSlug || list.Ports[0].Enabled || !list.Ports[0].Manual {
		t.Errorf("the port listed again = %+v; want a new, disabled row with a new slug", list.Ports)
	}
	if k := eventKinds(t, r); len(k) != 4 || k[2] != preview.KindPortRetired || k[3] != preview.KindPortAdded {
		t.Errorf("events = %v", k)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
