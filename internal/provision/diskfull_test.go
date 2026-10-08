package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// TestDiskPreflightRefusesBeforeAnything is design §12's *Disk full* row and
// testing §8.6's "disk pre-flight refuses": an injected disk at the limit
// refuses a create before a row, a clone or a token exists, and refuses a
// start and a rebuild before their runs begin — while the same requests just
// under the limit proceed. The boundary is exact: at the limit is over.
func TestDiskPreflightRefusesBeforeAnything(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.wire(t)
	disk := &sys.FakeDisk{}
	disk.Set(89, 100) // just under: the control
	e.p.Disk, e.p.DiskLimitPercent = disk, 90

	v := e.create(t, alpha, "")
	if v.State != workspace.Running {
		t.Fatalf("control: under the limit, state %s (%s)", v.State, deref(v.StateDetail))
	}
	if err := e.p.Stop(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.wg.Wait()

	disk.Set(90, 100) // exactly at the limit: refused
	mints, events := len(e.fake.TokenRequests), len(e.stepEvents(t, v.ID))
	_, err := e.p.Create(ctx, plain, "")
	var full *DiskFullError
	if !errors.Is(err, ErrDiskFull) || !errors.As(err, &full) || full.Percent() != 90 || full.LimitPercent != 90 {
		t.Fatalf("Create at the limit = %v, want a DiskFullError naming 90%% of 90%%", err)
	}
	if vs, _ := e.p.Workspaces.Views(ctx); len(vs) != 1 {
		t.Errorf("a refused create left a row: %d rows", len(vs))
	}
	for name, act := range map[string]func() error{
		"start":   func() error { return e.p.Start(ctx, v.ID) },
		"rebuild": func() error { return e.p.Rebuild(ctx, v.ID) },
	} {
		if err := act(); !errors.Is(err, ErrDiskFull) {
			t.Errorf("%s at the limit = %v", name, err)
		}
	}
	e.p.wg.Wait()
	if len(e.fake.TokenRequests) != mints || len(e.stepEvents(t, v.ID)) != events {
		t.Error("a refused request reached GitHub or wrote a step")
	}
	if got := e.view(t, v.ID).State; got != workspace.Stopped {
		t.Errorf("a refused start moved the workspace to %s", got)
	}

	// A disk that cannot be read refuses nothing: the check saves a doomed
	// build, and nothing else relies on it.
	disk.Err = errors.New("statfs: broken")
	if err := e.p.Start(ctx, v.ID); err != nil {
		t.Errorf("start with an unreadable disk = %v", err)
	}
	e.p.wg.Wait()
	disk.Err = nil

	// A limit of 100 is the check turned off, even on a full disk.
	disk.Set(100, 100)
	e.p.DiskLimitPercent = 100
	if _, err := e.p.Create(ctx, plain, ""); err != nil {
		t.Errorf("create with the check off = %v", err)
	}
	e.p.wg.Wait()
}

func TestOverLimitBoundary(t *testing.T) {
	for _, c := range []struct {
		used, total uint64
		limit       int
		want        bool
	}{
		{89, 100, 90, false}, {90, 100, 90, true}, {91, 100, 90, true},
		{899_999_999_999, 1_000_000_000_000, 90, false}, {900_000_000_000, 1_000_000_000_000, 90, true},
		{100, 100, 100, false}, {5, 0, 90, false}, {1, 100, 1, true}, {0, 100, 1, false},
	} {
		if got := workspace.OverLimit(c.used, c.total, c.limit); got != c.want {
			t.Errorf("OverLimit(%d, %d, %d) = %v", c.used, c.total, c.limit, got)
		}
	}
}
