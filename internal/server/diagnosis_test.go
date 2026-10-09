package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/preview"
)

// TestDiagnosisOverTheSockets is PF §13 step 6 against the real server, over
// the real API and preview sockets, with discovery fed a socket table and a
// real dev server behind the proxy:
//
//   - the port listed on 0.0.0.0 is enabled and its preview answers through
//     the proxy's resolver (the control: it is dialled);
//   - the dev server moves to 127.0.0.1: two rescans later the preview's next
//     request and the probe both say the --host 0.0.0.0 sentence, and the
//     resolver — every dial's first step — is asked nothing (the done-when);
//     an enable of it is 409 port_loopback;
//   - the detail view's activity list carries the operator's port.enabled and
//     none of discovery's events, which the log still holds;
//   - the workspace stopped, the device opening the preview is sent to the
//     workspace's page naming the port.
func TestDiagnosisOverTheSockets(t *testing.T) {
	app := newDevServer(t)
	res := &countingResolver{}
	var mu sync.Mutex
	bind := "0.0.0.0"
	r := startWith(t, t.TempDir(), nil, func(s *Server) {
		s.Proxy.Resolver = res
		s.Discovery.Interval = -1 // only the scans asked for, and boot's
		s.Discovery.Source = preview.ListenerFunc(func(_ context.Context, id string) ([]preview.Listener, error) {
			mu.Lock()
			defer mu.Unlock()
			if id != "wprev" {
				return nil, preview.ErrNotRunning
			}
			return []preview.Listener{{Port: app.port(), Addr: netip.MustParseAddr(bind)}}, nil
		})
	})
	seedDisabledPort(t, r, app.port())
	ui := r.signIn(t)
	base := "/api/workspaces/wprev/ports"
	scans := 0
	rescan := func() {
		t.Helper()
		if resp := r.do(t, req{method: "POST", path: base + "/rescan", origin: uiOrigin, cookie: ui}); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("rescan = %d", resp.StatusCode)
		}
		scans++
		deadline := time.Now().Add(10 * time.Second)
		for countKind(t, r, preview.KindPortScanned) < scans {
			if time.Now().After(deadline) {
				t.Fatal("no port.scanned")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	rescan()
	rescan()
	if resp := r.do(t, req{method: "PATCH", path: base + "/pprev", origin: uiOrigin, cookie: ui, body: `{"enabled":true}`}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("enable on 0.0.0.0 = %d", resp.StatusCode)
	}
	pc := r.previewCookieFor(t, ui)
	cookie := http.Header{"Cookie": {preview.CookieName + "=" + pc}}
	if resp, body := r.previewDo(t, "GET", "https://"+previewHost+"/", cookie); resp.StatusCode != http.StatusOK {
		t.Fatalf("control: the preview on 0.0.0.0 = %d %q", resp.StatusCode, body)
	}
	if n, _ := res.counts(); n == 0 {
		t.Fatal("control: the preview on 0.0.0.0 was never resolved")
	}

	// The dev server restarts on loopback.
	mu.Lock()
	bind = "127.0.0.1"
	mu.Unlock()
	rescan()
	rescan()
	r0, c0 := res.counts()
	resp, body := r.previewDo(t, "GET", "https://"+previewHost+"/", cookie)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "only reachable from inside the container. Start it with --host 0.0.0.0.") {
		t.Errorf("the preview of a loopback server = %d %q", resp.StatusCode, body)
	}
	var probe preview.ProbeResult
	readJSON(t, r.do(t, req{method: "GET", path: base + "/pprev/probe", cookie: ui}), &probe)
	if probe.Outcome != preview.ProbeLoopback {
		t.Errorf("probe = %+v", probe)
	}
	if r1, c1 := res.counts(); r1 != r0 || c1 != c0 {
		t.Errorf("a loopback port was resolved %d times and confirmed %d: a dial was attempted", r1-r0, c1-c0)
	}
	var e struct {
		Error api.Error `json:"error"`
	}
	resp = r.do(t, req{method: "PATCH", path: base + "/pprev", origin: uiOrigin, cookie: ui, body: `{"enabled":true}`})
	if readJSON(t, resp, &e); resp.StatusCode != http.StatusConflict || e.Error.Code != api.CodePortLoopback {
		t.Errorf("enable of a loopback port = %d %+v", resp.StatusCode, e)
	}
	var list api.PortList
	readJSON(t, r.do(t, req{method: "GET", path: base, cookie: ui}), &list)
	if list.Discovery == nil || *list.Discovery != preview.DiscoveryOK || len(list.Ports) != 1 || !list.Ports[0].Loopback || !list.Ports[0].Enabled {
		t.Errorf("the list = %+v", list)
	}

	// The activity list: the operator's, not discovery's.
	var detail struct {
		Events []events.Event `json:"events"`
	}
	readJSON(t, r.do(t, req{method: "GET", path: "/api/workspaces/wprev", cookie: ui}), &detail)
	var feed []string
	for _, ev := range detail.Events {
		var d struct{ Source string }
		json.Unmarshal(ev.Data, &d)
		if d.Source == preview.SourceDiscovery {
			t.Errorf("the activity list carries discovery's %s", ev.Kind)
		}
		feed = append(feed, ev.Kind)
	}
	if !strings.Contains(strings.Join(feed, " "), preview.KindPortEnabled) || countKind(t, r, preview.KindPortScanned) != 4 {
		t.Errorf("the activity list = %v; want port.enabled, with the log keeping discovery's", feed)
	}

	// Stopped: the device is sent to the workspace's page.
	r.srv.DB.Exec(`UPDATE workspace SET state = 'stopped' WHERE id = 'wprev'`)
	aresp := r.do(t, req{method: "GET", path: "/preview/authorize?return=https%3A%2F%2F" + previewHost + "%2F", cookie: ui})
	if aresp.StatusCode != http.StatusFound || aresp.Header.Get("Location") != "/ws/wprev?preview=pprev" {
		t.Errorf("authorize on a stopped workspace = %d %q", aresp.StatusCode, aresp.Header.Get("Location"))
	}
}

// countKind is how many events of a kind the log holds for wprev.
func countKind(t *testing.T, r *running, kind string) int {
	t.Helper()
	es, err := r.srv.Events.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range es {
		if e.WorkspaceID == "wprev" && e.Kind == kind {
			n++
		}
	}
	return n
}
