// Package container_test is the container tier (testing §3): real Docker,
// here the devcontainer's docker-in-docker. It holds only what is about
// Docker's behaviour rather than Drydock's branches — the reconciliation
// table is unit-tested in internal/reconcile, and the two rows repeated here
// are the ones whose interest is what Docker actually reports (testing §2).
package container_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/reconcile"
	"github.com/krelinga/drydock/internal/store"
	"github.com/krelinga/drydock/internal/subproc"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

const image = "debian:bookworm-slim"

// needDocker skips without a daemon, unless DRYDOCK_REQUIRE_DOCKER is set —
// CI sets it, because there a skip would be a silent pass.
func needDocker(t *testing.T) {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("DRYDOCK_REQUIRE_DOCKER") != "" {
			t.Fatalf("docker is unavailable and DRYDOCK_REQUIRE_DOCKER is set: %v", err)
		}
		t.Skip("docker is unavailable")
	}
	// Pull up front, so no test's timing or output depends on whether the
	// image happened to be cached.
	if out, err := exec.Command("docker", "pull", "--quiet", image).CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v: %s", image, err, out)
	}
}

// prefix is unique per test, as testing §5.4 requires: a test Drydock on the
// devcontainer's daemon must never share a prefix with anything else on it.
func prefix(t *testing.T) string {
	b := make([]byte, 4)
	rand.Read(b)
	p := "drydock.test." + hex.EncodeToString(b)
	t.Cleanup(func() {
		// Workspace containers, and any cleanup helper or login container
		// a failed test left.
		for _, k := range []string{".workspace", ".cleanup", ".login"} {
			out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+p+k).Output()
			if ids := strings.Fields(string(out)); len(ids) > 0 {
				exec.Command("docker", append([]string{"rm", "-f"}, ids...)...).Run()
			}
		}
		// The shared credential volume step 4 made, found by the label it
		// carries — after the containers that mounted it are gone.
		out, _ := exec.Command("docker", "volume", "ls", "-q", "--filter", "label="+p+".claude-config").Output()
		if vs := strings.Fields(string(out)); len(vs) > 0 {
			exec.Command("docker", append([]string{"volume", "rm", "-f"}, vs...)...).Run()
		}
	})
	return p
}

// claudeVolume is the test Drydock's shared credential volume: its own, so
// no test touches another's login, or the devcontainer's.
func claudeVolume(p string) string { return p + ".claude" }

// docker runs a docker command and returns its stdout. Stdout only: on a
// cold cache `docker run` reports the image pull on stderr, and mixing the two
// glued "Unable to find image…" onto a container id in CI.
func docker(t *testing.T, args ...string) string {
	t.Helper()
	var stderr strings.Builder
	cmd := exec.Command("docker", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// labelled starts a container carrying the labels `devcontainer up` would put
// on a workspace's container.
func labelled(t *testing.T, p, ws string, cmd ...string) string {
	t.Helper()
	args := []string{"run", "-d",
		"--label", p + ".workspace=" + ws, "--label", p + ".repository-id=77",
		"--label", p + ".repo=krelinga/adopted", "--label", p + ".branch=feature",
		image}
	return docker(t, append(args, cmd...)...)
}

func setup(t *testing.T, p string) (*reconcile.Reconciler, *workspace.Store) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "drydock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	env := sys.Env{Clock: sys.RealClock{}, Random: sys.CryptoRandom{}}
	log := events.New(db.DB, env.Clock)
	ws := &workspace.Store{DB: db.DB, Events: log, Env: env, Root: "/srv/drydock/ws", Cap: 10}
	return &reconcile.Reconciler{Workspaces: ws, Events: log,
		Containers: container.Manager{Run: subproc.Exec{}, LabelPrefix: p}}, ws
}

// A lost database: a running container with this instance's labels and no
// row. It is adopted from its labels — not removed, and not ignored.
func TestAdoptAnOrphan(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	p := prefix(t)
	id, _ := workspace.NewID(time.Now(), rand.Reader)
	cid := labelled(t, p, id, "sleep", "300")

	// A container under another prefix — another Drydock on this daemon,
	// which is what a test run is — must be invisible to this one.
	other := prefix(t)
	foreignID, _ := workspace.NewID(time.Now(), rand.Reader)
	labelled(t, other, foreignID, "sleep", "300")

	r, ws := setup(t, p)
	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Get(ctx, id)
	if err != nil {
		t.Fatalf("the orphan was not adopted: %v", err)
	}
	if w.State != workspace.Running || w.ContainerID != cid || w.RepositoryID != 77 || w.Branch != "feature" {
		t.Errorf("adopted as %+v", w)
	}
	if state := docker(t, "inspect", "-f", "{{.State.Running}}", cid); state != "true" {
		t.Errorf("the orphan's container is no longer running: %s", state)
	}
	if _, err := ws.Get(ctx, foreignID); err == nil {
		t.Error("a container under another instance's prefix was adopted")
	}
}

// Died unobserved: the row says running, and the container exited while
// Drydock was down. Marked stopped — and not restarted.
func TestDiedUnobserved(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	p := prefix(t)
	r, ws := setup(t, p)

	if _, err := ws.DB.ExecContext(ctx,
		`INSERT INTO repository (id, installation_id, full_name, default_branch) VALUES (77, 1, 'krelinga/adopted', 'main')`); err != nil {
		t.Fatal(err)
	}
	w, err := ws.Create(ctx, 77, "feature")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []workspace.State{workspace.Cloning, workspace.Building, workspace.Running} {
		if _, err := ws.Move(ctx, w.ID, s, ""); err != nil {
			t.Fatal(err)
		}
	}
	cid := labelled(t, p, w.ID, "sh", "-c", "exit 3")
	ws.SetContainer(ctx, w.ID, cid)
	deadline := time.Now().Add(30 * time.Second)
	for docker(t, "inspect", "-f", "{{.State.Status}}", cid) != "exited" {
		if time.Now().After(deadline) {
			t.Fatal("the container never exited")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if _, err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := ws.Get(ctx, w.ID)
	if got.State != workspace.Stopped || got.ContainerID != cid {
		t.Errorf("after reconcile: %s %q; want stopped, keeping its container", got.State, got.ContainerID)
	}
	if status := docker(t, "inspect", "-f", "{{.State.Status}}", cid); status != "exited" {
		t.Errorf("reconcile started the container (%s): nothing starts itself", status)
	}
}

// TestLegacyBrokerMountAsDockerReportsIt: a container made the way a Drydock
// before the directory mount made it — the socket file bind-mounted at
// /run/drydock/broker.sock — is reported legacy from what docker inspect
// really says, stopped as well as running; one with the directory mounted at
// /run/drydock, the control, is not.
func TestLegacyBrokerMountAsDockerReportsIt(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	p := prefix(t)
	dir, err := os.MkdirTemp("", "ddm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "broker.sock")
	os.WriteFile(sock, nil, 0o600) // a file mount's source must exist; inspect needs no listener
	m := container.Manager{Run: subproc.Exec{}, LabelPrefix: p}
	for _, c := range []struct {
		mount  string
		legacy bool
	}{
		{"type=bind,source=" + sock + ",target=" + container.LegacyBrokerMountPoint, true},
		{"type=bind,source=" + dir + ",target=" + container.BrokerMountPoint, false},
	} {
		id, _ := workspace.NewID(time.Now(), rand.Reader)
		cid := docker(t, "run", "-d", "--label", p+".workspace="+id, "--mount", c.mount, image, "sleep", "300")
		for _, phase := range []string{"running", "stopped"} {
			if phase == "stopped" {
				docker(t, "stop", "-t", "1", cid)
			}
			got, err := m.LegacyBrokerMount(ctx, id)
			if err != nil || got != c.legacy {
				t.Errorf("%s, %s: LegacyBrokerMount = %v, %v; want %v", c.mount, phase, got, err, c.legacy)
			}
		}
	}
}
