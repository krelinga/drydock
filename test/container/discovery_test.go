package container_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/life"
	"github.com/krelinga/drydock/internal/preview"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/sys"
)

// discoveryServers is a container's command: a server on 0.0.0.0:8000 — a
// port nothing declares — and a debugger-like one on 127.0.0.1:9229. Node,
// because the pinned image has no python3; the scan reads sockets, not
// programs, so `python3 -m http.server` would read the same.
const discoveryServers = `const h = require('http');
h.createServer((q, s) => s.end('ok')).listen(8000, '0.0.0.0');
h.createServer((q, s) => s.end('dbg')).listen(9229, '127.0.0.1');`

// TestDiscoveryReadsARealContainer is PF §13 step 5 against real Docker and
// a real kernel: the scan reads a running container's socket table from the
// host — as this unprivileged user, with nothing run inside the container —
// and the real scanner lists the undeclared server as a row, off, and the
// loopback one classified. Then `docker restart`: a new PID, which the next
// read resolves and follows. And a container on the host's network, whose
// namespace is the host's, is never read (the bridge one is the control).
func TestDiscoveryReadsARealContainer(t *testing.T) {
	needDocker(t)
	image := config.DefaultClaudeBaseImage
	if out, err := exec.Command("docker", "pull", "--quiet", image).CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v: %s", image, err, out)
	}
	p := prefix(t)
	ws, hostWS := "01JTESTD1SC0VERY0000000000", "01JTESTD1SC0VERYH0ST000000"
	id := docker(t, "run", "-d", "--label", p+".workspace="+ws, image, "node", "-e", discoveryServers)
	docker(t, "run", "-d", "--network", "host", "--label", p+".workspace="+hostWS, image, "sleep", "600")
	m := manager(p)
	ctx := context.Background()

	// The servers take a moment to bind: read until both are there.
	read := func(want int) []container.Listener {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			ls, err := m.Listeners(ctx, ws)
			if err == nil && len(ls) >= want {
				return ls
			}
			if time.Now().After(deadline) {
				t.Fatalf("Listeners = %v, %v; want %d listeners", ls, err, want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	got := map[int]container.Listener{}
	for _, l := range read(2) {
		got[l.Port] = l
	}
	if l := got[8000]; l.Addr.String() != "0.0.0.0" || l.Loopback() {
		t.Errorf("8000 = %+v; want 0.0.0.0, not loopback", l)
	}
	if l := got[9229]; !l.Loopback() {
		t.Errorf("9229 = %+v; want loopback", l)
	}
	if len(got) != 2 {
		t.Errorf("listeners = %v; want the container's two and nothing of the host's", got)
	}
	if _, err := m.Listeners(ctx, hostWS); !errors.Is(err, container.ErrNoAddress) {
		t.Errorf("a host-network container: %v; want ErrNoAddress, its table never read", err)
	}

	// The real scanner over it, writing a real registry.
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (1, 1, 'o/app', 'main')`,
		`INSERT INTO workspace (id, repository_id, host_path, branch, state) VALUES ('` + ws + `', 1, '/x', 'main', 'running')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	clock := sys.Production().Clock
	svc := &preview.Service{DB: db.DB, Clock: clock, Random: sys.Production().Random, Events: events.New(db.DB, clock)}
	sc := &preview.Scanner{Registry: svc, Clock: clock, Interval: 300 * time.Millisecond,
		Source: preview.ListenerFunc(func(ctx context.Context, id string) ([]preview.Listener, error) {
			ls, err := m.Listeners(ctx, id)
			switch {
			case errors.Is(err, container.ErrNotRunning):
				return nil, preview.ErrNotRunning
			case errors.Is(err, container.ErrMoved):
				return nil, preview.ErrScanRaced
			case err != nil:
				return nil, err
			}
			out := make([]preview.Listener, len(ls))
			for i, l := range ls {
				out[i] = preview.Listener{Port: l.Port, Addr: l.Addr}
			}
			return out, nil
		}),
		Logf: t.Logf}
	g := life.NewGroup(ctx)
	if err := sc.Start(g); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		g.Stop()
		d, cancel := sys.NewTimer(clock, 30*time.Second)
		defer cancel()
		g.Wait(d)
	})
	deadline := time.Now().Add(30 * time.Second)
	var rows map[int]preview.Port
	for {
		ps, err := svc.Ports(ctx, ws, true)
		if err != nil {
			t.Fatal(err)
		}
		rows = map[int]preview.Port{}
		for _, p := range ps {
			rows[p.ContainerPort] = p
		}
		if len(rows) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rows = %+v; want 8000 and 9229 listed", rows)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for port, loop := range map[int]bool{8000: false, 9229: true} {
		r := rows[port]
		if r.Enabled || !r.Observed || r.Declared || r.Manual || r.Loopback != loop || r.ObservedState == nil || *r.ObservedState != "listening" {
			t.Errorf("port %d = %+v; want listed, off, observed, loopback %v", port, r, loop)
		}
	}

	// docker restart: the same container, a new process. The PID is
	// resolved again, and the read follows it.
	before, err := m.Address(ctx, ws)
	if err != nil || before.Pid <= 0 {
		t.Fatalf("Address = %+v, %v", before, err)
	}
	docker(t, "restart", "--time", "1", id)
	after, err := m.Address(ctx, ws)
	if err != nil || after.Pid <= 0 || after.Pid == before.Pid {
		t.Fatalf("after the restart: %+v, %v; want a new PID (was %d)", after, err, before.Pid)
	}
	if ls := read(2); len(ls) != 2 {
		t.Errorf("after the restart: %v", ls)
	}
	t.Logf("PID %d, then %d after the restart", before.Pid, after.Pid)
	var enabled int
	db.QueryRow(`SELECT count(*) FROM forwarded_port WHERE enabled = 1`).Scan(&enabled)
	if enabled != 0 {
		t.Errorf("%d rows enabled by discovery", enabled)
	}
}
