package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/api"
	"github.com/krelinga/drydock/internal/preview"
)

// TestDiscoveryOverTheSockets is PF §13 step 5's done-when against the real
// server, with the container's socket table played by a fake source: a
// workspace's panel open (its list read), a server starts on an undeclared
// port and another on loopback; two rescans over the API socket — each a 202
// settled by its port.scanned — and the list holds both, off, observed, the
// loopback one classified, announced by port.added and nothing else. No row
// is enabled, and the scan asked about the one running workspace only.
func TestDiscoveryOverTheSockets(t *testing.T) {
	var mu sync.Mutex
	listening := []preview.Listener{}
	asked := map[string]int{}
	r := startWith(t, t.TempDir(), nil, func(s *Server) {
		s.Discovery.Interval = -1 // only the scans asked for, and boot's
		s.Discovery.Source = preview.ListenerFunc(func(_ context.Context, id string) ([]preview.Listener, error) {
			mu.Lock()
			defer mu.Unlock()
			asked[id]++
			return append([]preview.Listener(nil), listening...), nil
		})
	})
	<-r.srv.reconciled
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (7, 1, 'o/myapp', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('wdisc', 7, '/x', 'main', 'running')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('wstop', 7, '/y', 'main', 'stopped')`,
	} {
		if _, err := r.srv.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ui := r.signIn(t)
	base := "/api/workspaces/wdisc/ports"
	var list api.PortList
	readJSON(t, r.do(t, req{method: "GET", path: base, cookie: ui}), &list)
	if len(list.Ports) != 0 {
		t.Fatalf("before: %+v", list)
	}

	mu.Lock()
	listening = []preview.Listener{
		{Port: 5173, Addr: netip.MustParseAddr("0.0.0.0")},
		{Port: 9229, Addr: netip.MustParseAddr("127.0.0.1")},
	}
	mu.Unlock()
	scanned := func() int {
		es, _ := r.srv.Events.Since(context.Background(), 0)
		n := 0
		for _, e := range es {
			if e.Kind == preview.KindPortScanned && e.WorkspaceID == "wdisc" {
				n++
			}
		}
		return n
	}
	for i := 1; i <= 2; i++ {
		resp := r.do(t, req{method: "POST", path: base + "/rescan", origin: uiOrigin, cookie: ui})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("rescan %d = %d %s", i, resp.StatusCode, code(t, resp))
		}
		deadline := time.Now().Add(10 * time.Second)
		for scanned() < i {
			if time.Now().After(deadline) {
				t.Fatalf("rescan %d: no port.scanned", i)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// A rescan without the Origin is refused like any mutation.
	if resp := r.do(t, req{method: "POST", path: base + "/rescan", cookie: ui}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("rescan with no Origin = %d", resp.StatusCode)
	}

	readJSON(t, r.do(t, req{method: "GET", path: base, cookie: ui}), &list)
	got := map[int]preview.Port{}
	for _, p := range list.Ports {
		got[p.ContainerPort] = p
	}
	for port, loop := range map[int]bool{5173: false, 9229: true} {
		p, ok := got[port]
		if !ok || p.Enabled || p.URL != nil || !p.Observed || p.Manual || p.Declared || p.Loopback != loop ||
			p.ObservedState == nil || *p.ObservedState != "listening" {
			b, _ := json.Marshal(p)
			t.Errorf("port %d = %s; want listed, off, observed, loopback %v", port, b, loop)
		}
	}
	var kinds []string
	es, _ := r.srv.Events.Since(context.Background(), 0)
	for _, e := range es {
		if e.WorkspaceID == "wdisc" {
			kinds = append(kinds, e.Kind)
		}
	}
	want := []string{"port.scanned", "port.added", "port.added", "port.scanned"}
	if len(kinds) != len(want) {
		t.Fatalf("events = %v; want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("events = %v; want %v", kinds, want)
			break
		}
	}
	var enabled int
	r.srv.DB.QueryRow(`SELECT count(*) FROM forwarded_port WHERE enabled = 1`).Scan(&enabled)
	if enabled != 0 {
		t.Errorf("%d rows enabled by discovery", enabled)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked["wstop"] != 0 || asked["wdisc"] < 2 {
		t.Errorf("asked %v; want the running workspace alone", asked)
	}
}
