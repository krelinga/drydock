package container_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/usage"
	"github.com/krelinga/drydock/internal/workspace"
)

type usageRows []workspace.Workspace

func (r *usageRows) List(context.Context) ([]workspace.Workspace, error) { return *r, nil }

// TestUsageAgainstRealDocker is design §6's *Resources* against the real
// daemon: a workspace container holding ~40 MB in a shell variable reads as
// that much memory through `docker stats` — plausible, not zero and not the
// host's — and its 8 MB written outside any mount reads as its writable layer
// through `docker inspect --size`, beside the workspace directory's own walk.
// Stopped, it has no memory figure at all, while its disk stays. A second
// workspace's container is running beside it the whole time, and is asked
// for only while its row says running: the control.
func TestUsageAgainstRealDocker(t *testing.T) {
	needDocker(t)
	p := prefix(t)
	ctx := context.Background()
	const ws, other = "01JVSAGEWSAAAAAAAAAAAAAAAA", "01JVSAGE0THERAAAAAAAAAAAAA"
	id := labelled(t, p, ws, "sh", "-c",
		`x=$(head -c 40000000 /dev/zero | tr "\0" a); head -c 8000000 /dev/zero > /blob; touch /ready; sleep 300`)
	labelled(t, p, other, "sleep", "300")
	for i := 0; ; i++ {
		if exec.Command("docker", "exec", id, "test", "-e", "/ready").Run() == nil {
			break
		}
		if i > 100 {
			t.Fatal("the container never filled its memory")
		}
		time.Sleep(100 * time.Millisecond)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ws, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ws, "repo", "file"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := &usageRows{{ID: ws, State: workspace.Running}, {ID: other, State: workspace.Stopped}}
	clock := sys.NewFakeClock(time.Now())
	s := &usage.Sampler{Workspaces: rows, Containers: manager(p), Disk: sys.HostDisk{}, Clock: clock,
		Root: root, LimitPercent: 90, Logf: t.Logf}

	f := s.Round(ctx)
	r := f.Workspaces[ws]
	if r.Memory == nil || r.Memory.Stale || r.Memory.Bytes < 35_000_000 || r.Memory.Bytes > 1<<30 {
		t.Errorf("memory of a container holding ~40 MB: %+v", r.Memory)
	}
	if r.Disk == nil || r.Disk.ContainerBytes == nil || *r.Disk.ContainerBytes < 8_000_000 ||
		r.Disk.DirectoryBytes < 1<<20 || r.Disk.Bytes != r.Disk.DirectoryBytes+*r.Disk.ContainerBytes || r.Disk.Partial {
		t.Errorf("disk: %+v", r.Disk)
	}
	if m := f.Workspaces[other].Memory; m != nil {
		t.Errorf("a stopped row's running container was measured: %+v", m)
	}
	if f.Host == nil || f.Host.TotalBytes == 0 {
		t.Errorf("host %+v", f.Host)
	}

	// A container removed between the listing and the reading — a delete or
	// a rebuild landing mid-round — fails no one else's: real Docker answers
	// it with exit 1 and nothing for the others, and the reader asks again.
	gone := docker(t, "create", "--label", p+".workspace=01JVSAGEG0NEAAAAAAAAAAAAAA", image, "true")
	docker(t, "rm", gone)
	if mem, err := manager(p).Memory(ctx, []string{id, gone}); err != nil || mem[id] < 35_000_000 {
		t.Errorf("memory beside a vanished container: %v, %v", mem, err)
	}
	if sizes, err := manager(p).WritableSizes(ctx, []string{id, gone}); err != nil || sizes[id] < 8_000_000 {
		t.Errorf("sizes beside a vanished container: %v, %v", sizes, err)
	}

	docker(t, "stop", "-t", "1", id)
	(*rows)[0].State = workspace.Stopped
	clock.Advance(usage.DefaultMemoryInterval)
	f = s.Round(ctx)
	if m := f.Workspaces[ws].Memory; m != nil {
		t.Errorf("a stopped workspace has memory %+v; it has no reading", m)
	}
	if d := f.Workspaces[ws].Disk; d == nil || d.ContainerBytes == nil || *d.ContainerBytes < 8_000_000 {
		t.Errorf("a stopped workspace kept no disk: %+v", d)
	}

	// Running in the database, but its container exited: still no reading,
	// rather than docker's "0B / 0B" read as zero.
	(*rows)[0].State = workspace.Running
	clock.Advance(usage.DefaultMemoryInterval)
	if m := s.Round(ctx).Workspaces[ws].Memory; m != nil {
		t.Errorf("an exited container read as %+v", m)
	}
}
