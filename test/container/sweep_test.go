package container_test

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/config"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/server"
	"github.com/krelinga/drydock/internal/sys"
)

// TestBootSweepsLeftoverHelpers: a cleanup helper a delete left behind —
// Drydock killed while it ran, and the daemon never reaching --rm — is
// removed at boot, after reconciliation, by this instance's
// <prefix>.cleanup label, through the real server against the real daemon.
// Two are made: one stopped (created and never run, as a daemon restart
// leaves an --rm container) and one still running.
//
// Around them, what the sweep must never touch, each beside the helpers it
// does remove: a workspace's container (adopted by reconciliation, not
// removed); a container carrying the cleanup label *and* the workspace label,
// which no delete makes and which is a workspace container whatever else it
// carries; and another instance's helper, under a different prefix.
func TestBootSweepsLeftoverHelpers(t *testing.T) {
	needDocker(t)
	p, other := prefix(t), prefix(t)
	ctx := context.Background()
	const ws, ws2, gone = "01JSWEEPWSAAAAAAAAAAAAAAAA", "01JSWEEPWS2AAAAAAAAAAAAAAA", "01JSWEEPG0NEAAAAAAAAAAAAAA"

	stopped := docker(t, "create", "--label", p+".cleanup="+gone, image, "true")
	live := docker(t, "run", "-d", "--label", p+".cleanup="+gone, image, "sleep", "300")
	workspaceC := labelled(t, p, ws, "sleep", "300")
	both := docker(t, "run", "-d", "--label", p+".cleanup="+ws2, "--label", p+".workspace="+ws2, image, "sleep", "300")
	foreign := docker(t, "create", "--label", other+".cleanup="+gone, image, "true")
	exists := func(id string) bool {
		return docker(t, "ps", "-aq", "--no-trunc", "--filter", "id="+id) != ""
	}
	for _, id := range []string{stopped, live, workspaceC, both, foreign} {
		if !exists(id) {
			t.Fatalf("setup: %s is not there", id)
		}
	}

	dir, err := os.MkdirTemp("", "dds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	u, _ := user.Current()
	g, _ := user.LookupGroupId(u.Gid)
	cfg := config.Default()
	// Not a test of the disk: the runner's own fill must never refuse its creates (design §12).
	cfg.DiskLimitPercent = 100
	cfg.UIOrigin, cfg.UIHost = "https://drydock.test", "drydock.test"
	cfg.DatabasePath = filepath.Join(dir, "drydock.db")
	cfg.APISocket, cfg.PreviewSocket = filepath.Join(dir, "http.sock"), filepath.Join(dir, "preview.sock")
	cfg.BrokerDir, cfg.WorkspaceRoot = filepath.Join(dir, "sock"), filepath.Join(dir, "ws")
	cfg.SocketGroup, cfg.LabelPrefix = g.Name, p
	srv, err := server.New(ctx, cfg, sys.Production())
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// The sweep writes one system event when it has removed something.
	deadline := time.Now().Add(30 * time.Second)
	for {
		evs, err := srv.Events.Since(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		var swept string
		for _, ev := range evs {
			if ev.Kind == provision.KindHelpersSwept {
				swept = string(ev.Data)
			}
		}
		if swept != "" {
			if swept != `{"count":2}` {
				t.Errorf("swept %s; want the two helpers", swept)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the boot sweep never reported; events %+v", evs)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The positive control: both of this instance's helpers are gone.
	for name, id := range map[string]string{"the stopped helper": stopped, "the running helper": live} {
		if exists(id) {
			t.Errorf("%s survived the boot sweep", name)
		}
	}
	// And nothing else was touched.
	for name, id := range map[string]string{
		"the workspace's container":                          workspaceC,
		"a container with the workspace label and the other": both,
		"another instance's helper":                          foreign,
	} {
		if !exists(id) {
			t.Errorf("the boot sweep removed %s", name)
		}
	}
	if out := docker(t, "inspect", "--format", "{{.State.Running}}", workspaceC); strings.TrimSpace(out) != "true" {
		t.Errorf("the workspace's container is not running after boot: %s", out)
	}
	// Reconciliation saw the workspace's container and adopted it — the
	// sweep ran after it, beside it, not instead of it.
	if w, err := srv.Workspaces.Get(ctx, ws); err != nil || w.ContainerID != workspaceC {
		t.Errorf("the workspace container was not adopted: %+v %v", w, err)
	}
}
