package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/broker"
	"github.com/krelinga/drydock/internal/workspace"
)

// markLegacy rewrites the fake docker's world so the workspace's containers
// carry the file mount an earlier Drydock gave them.
func (e *env) markLegacy(t *testing.T, ws string) {
	t.Helper()
	path := filepath.Join(e.cli.dir, "containers")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if f := strings.Fields(l); len(f) == 3 && f[1] == ws {
			l += " legacy"
		}
		out = append(out, l)
	}
	os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// TestStartOfALegacyContainerAsksForARebuild: a start reuses its container,
// and one an earlier Drydock made has the broker socket mounted as a file
// that no longer exists. The start fails at up, before running it, with the
// sentence that names the fix; a rebuild — which replaces the container —
// then reaches running with the directory mount. Control first: the same
// stop and start with the current mount reaches running.
func TestStartOfALegacyContainerAsksForARebuild(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	v := e.running(t, alpha)
	stopStart := func() workspace.View {
		t.Helper()
		if err := e.p.Stop(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		e.p.idle()
		if s := e.view(t, v.ID).State; s != workspace.Stopped {
			t.Fatalf("setup: stop left %s", s)
		}
		if err := e.p.Start(ctx, v.ID); err != nil {
			t.Fatal(err)
		}
		e.p.idle()
		return e.view(t, v.ID)
	}
	if got := stopStart(); got.State != workspace.Running {
		t.Fatalf("control: a start with the current mount: %s (%s)", got.State, deref(got.StateDetail))
	}
	ups := len(e.cli.callsTo(t, "up"))

	e.markLegacy(t, v.ID)
	got := stopStart()
	if got.State != workspace.Failed || !strings.Contains(deref(got.StateDetail), "Rebuild it once") {
		t.Fatalf("a start of a legacy container: %s (%s)", got.State, deref(got.StateDetail))
	}
	if st := got.Steps[workspace.StepUp]; st.Status != "failed" || !strings.Contains(st.Detail, LegacyMountSentence) {
		t.Errorf("the up step reads %+v", st)
	}
	if n := len(e.cli.callsTo(t, "up")); n != ups {
		t.Errorf("devcontainer up ran against the legacy container (%d calls, was %d)", n, ups)
	}

	// The fix it names: a rebuild, here a start from failed, which replaces
	// the container.
	if err := e.p.Rebuild(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	if r := e.view(t, v.ID); r.State != workspace.Running {
		t.Fatalf("the rebuild: %s (%s)", r.State, deref(r.StateDetail))
	}
	if legacy, err := e.p.Containers.LegacyBrokerMount(ctx, v.ID); err != nil || legacy {
		t.Errorf("after the rebuild: legacy %v, %v", legacy, err)
	}
}

// TestResumeSupervisorsParksALegacyContainer: at boot, a running workspace
// whose container has the current mount has its session server started, as
// before (the control); one whose container has the legacy mount is parked
// with the sentence instead — its server would fail its prelude on every
// launch. Without a park seam it is started as it always was.
func TestResumeSupervisorsParksALegacyContainer(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	v := e.running(t, alpha)

	var started, parked []string
	var detail string
	e.p.StartSupervisor = func(_ context.Context, w workspace.Workspace) error {
		started = append(started, w.ID)
		return nil
	}
	e.p.ParkSupervisor = func(_ context.Context, w workspace.Workspace, reason, d string) error {
		if reason != ParkStaleBrokerMount {
			t.Errorf("parked for %q, want %q", reason, ParkStaleBrokerMount)
		}
		parked, detail = append(parked, w.ID), d
		return nil
	}
	resume := func() {
		t.Helper()
		started, parked, detail = nil, nil, ""
		if err := e.p.ResumeSupervisors(ctx, e.p.PausedAtBoot(ctx)); err != nil {
			t.Fatal(err)
		}
	}
	resume()
	if len(started) != 1 || started[0] != v.ID || len(parked) != 0 {
		t.Fatalf("control: started %v, parked %v", started, parked)
	}

	e.markLegacy(t, v.ID)
	resume()
	if len(parked) != 1 || parked[0] != v.ID || detail != LegacyMountSentence || len(started) != 0 {
		t.Errorf("legacy: parked %v with %q, started %v", parked, detail, started)
	}

	e.p.ParkSupervisor = nil
	resume()
	if len(started) != 1 {
		t.Errorf("without ParkSupervisor, started %v", started)
	}
}

// A delete whose broker directory still holds something the container's root
// made finishes anyway: the socket is gone, which is what ends access, and
// the step's note says what was left. Control: any other failure still
// stops it, resumable.
func TestDeleteFinishesPastALeftoverInTheBrokerDirectory(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		err      error
		finished bool
	}{
		{fmt.Errorf("%w: unlinkat: permission denied", broker.ErrLeftover), true},
		{errors.New("broker: something else"), false},
	} {
		e := lifecycleEnv(t)
		v := e.running(t, alpha)
		e.broker.removeErr = c.err
		if err := e.p.Delete(ctx, v.ID, "krelinga/alpha"); err != nil {
			t.Fatal(err)
		}
		e.p.idle()
		_, err := e.p.Workspaces.Get(ctx, v.ID)
		if gone := errors.Is(err, workspace.ErrNotFound); gone != c.finished {
			t.Errorf("%v: the delete finished = %v; actions %v", c.err, gone, e.actions(t, v.ID))
		}
	}
}
