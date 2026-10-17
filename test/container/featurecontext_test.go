package container_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/classify"
	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/provision"
	"github.com/krelinga/drydock/internal/workspace"
)

// TestFeaturesAreThisBuildsOwn is issue #182: a workspace whose up installed
// another workspace's version of a Feature. The CLI stages Features in
// $TMPDIR/…/container-features/<version>-<Date.now()> and hands that folder
// to buildx as a named build context; buildx keys BuildKit's copy of it by
// the folder's basename and its own node id (BUILDX_CONFIG/.buildNodeID),
// and the next build under that key re-sends only files whose path, size,
// mode or mtime changed. Creates started together share a millisecond —
// three of four did, measured — so they shared a key, and one whose build
// came after another's could keep the other's bytes. The marker Feature's
// versions are such files: the same names, the same sizes, mtime 0.
//
// The same millisecond is made certain here rather than hoped for: the CLI
// runs with Date.now pinned (NODE_OPTIONS), so six ups in turn, alternating
// 1.0.0 and 1.1.0, all stage in folders of one name, which the test checks.
// The first is the control that the marker tells versions apart. Each build
// loads that context twice, so which earlier copy a load takes is BuildKit's
// choice: of two ups in turn, the second kept the first's bytes half the
// time; of six, every run had one keep another's. Each up's own
// BUILDX_CONFIG is a new node id, so no key is shared.
func TestFeaturesAreThisBuildsOwn(t *testing.T) {
	needDevcontainer(t)
	pullImage(t, provision.DefaultImage)
	p := prefix(t)
	reg := newFeatureRegistry(t, "1.0.0", "1.1.0")
	m := manager(p)
	ctx := context.Background()

	root, err := os.MkdirTemp("", "ddf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	pin := filepath.Join(root, "now.js")
	// This run's moment, for every up in it: a later run's ups share no key
	// with this one's.
	now := fmt.Sprintf("Date.now = () => %d;\n", time.Now().UnixMilli())
	if err := os.WriteFile(pin, []byte(now), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require "+pin)

	versions := []string{"1.0.0", "1.1.0", "1.0.0", "1.1.0", "1.0.0", "1.1.0"}
	var staged []string
	for i, version := range versions {
		ws, _ := workspace.NewID(time.Now(), rand.Reader)
		folder := filepath.Join(root, ws, "repo")
		devc := filepath.Join(folder, ".devcontainer")
		if err := os.MkdirAll(devc, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := `{"image":"` + provision.DefaultImage + `","features":{"` + reg.Host + `/drydock-test/marker:` + version + `":{}}}`
		if err := os.WriteFile(filepath.Join(devc, "devcontainer.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// The container first, so its images are free to go.
			out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+p+".workspace="+ws).Output()
			if ids := strings.Fields(string(out)); len(ids) > 0 {
				exec.Command("docker", append([]string{"rm", "-f"}, ids...)...).Run()
			}
			m.RemoveBuiltImages(context.Background(), folder)
		})
		tmp := container.TempDirFor(folder)
		if err := os.MkdirAll(tmp, 0o700); err != nil {
			t.Fatal(err)
		}
		upCtx, cancel := context.WithTimeout(ctx, upLimit+2*time.Minute)
		res, stderr, err := m.Up(upCtx, container.UpSpec{
			WorkspaceID: ws, RepositoryID: 300 + int64(i), FullName: "krelinga/marker", Branch: "main",
			Folder: folder, TempDir: tmp, ConfigDir: devc,
		})
		cancel()
		if err != nil || res.Outcome != classify.ContainerRunning {
			t.Fatalf("up %d (marker %s): %+v %v\n%s", i, version, res, err, tail(stderr))
		}
		dirs, _ := filepath.Glob(filepath.Join(tmp, "devcontainercli-*", "container-features", "*"))
		for _, d := range dirs {
			staged = append(staged, filepath.Base(d))
		}
		if got := docker(t, "exec", res.ContainerID, "cat", markerPath); got != version {
			t.Errorf("up %d declared marker %s and installed %q", i, version, got)
		}
	}
	// What makes the later ups' assertions mean something: each staged in a
	// folder of the first's name, the key a shared buildx config shares.
	if len(staged) != len(versions) || strings.Count(strings.Join(staged, " "), staged[0]) != len(versions) {
		t.Errorf("the ups staged their Features in %q; want one name for all %d", staged, len(versions))
	}
}
