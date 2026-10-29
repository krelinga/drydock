package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/krelinga/drydock/internal/workspace"
)

// allocate makes <root>/<id> only for a row whose host_path is a clone path
// under the root (workspace.CheckClonePath), and makes nothing otherwise.
// The control is the same call with the path ClonePath makes.
func TestAllocateRefusesAHostPathThatIsNotAClonePath(t *testing.T) {
	e := newEnv(t)
	r := &runState{p: e.p}
	const id = "01JAAAAAAAAAAAAAAAAAAAAAAA"
	for _, hp := range []string{
		filepath.Join(e.root, id, workspace.OwnDir), filepath.Join(e.root, id, "-x"),
		filepath.Join(e.root, id, "alpha", "deeper"), filepath.Join(e.root, "OTHER", "alpha"), filepath.Join(e.root, id),
	} {
		if err := r.allocate(context.Background(), workspace.Workspace{ID: id, HostPath: hp}); err == nil {
			t.Errorf("allocate with host_path %q succeeded", hp)
		}
		if _, err := os.Stat(filepath.Join(e.root, id)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("allocate with host_path %q made the workspace directory: %v", hp, err)
		}
	}
	hp, err := workspace.ClonePath(e.root, id, "krelinga/alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.allocate(context.Background(), workspace.Workspace{ID: id, HostPath: hp}); err != nil {
		t.Errorf("control: allocate with ClonePath's own %q: %v", hp, err)
	}
	if fi, err := os.Stat(filepath.Join(e.root, id)); err != nil || !fi.IsDir() {
		t.Errorf("control: no workspace directory: %v", err)
	}
}
