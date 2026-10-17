package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/events"
	"github.com/krelinga/drydock/internal/workspace"
)

// TestARebuildCutOffInUpDoesNotRepause: a rebuild of a running workspace
// whose container is paused unpauses it to stop the session server, and its
// step 3 then stops it. Cut off by shutdown inside up, the job settles
// (container.UpSettle) — and so must not also spend RepauseTimeout looking
// for something to pause: step 3 stopped what was unpaused, and nothing in
// the run starts it again. That is what keeps one job's worst path inside
// internal/server's jobFits, which assumes the two bounds are alternatives.
// The positive control, in the same function, is the same rebuild cut short
// before step 3's stop succeeded (the stop fails): it does look, and pauses
// the container again.
func TestARebuildCutOffInUpDoesNotRepause(t *testing.T) {
	for _, stopFails := range []bool{false, true} {
		name := map[bool]string{false: "cut off in up", true: "control: step 3's stop failed"}[stopFails]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			e := lifecycleEnv(t)
			v := e.running(t, alpha)
			e.p.StopSupervisor = func(context.Context, workspace.Workspace) error {
				if stopFails {
					os.WriteFile(filepath.Join(e.cli.dir, "docker-fail-stop"), nil, 0o600)
				}
				return nil
			}
			e.setStatus(t, v.ID, "paused")
			inUp := filepath.Join(e.cli.dir, "in-up")
			e.cli.up = "touch '" + inUp + "'\nwhile :; do sleep 0.05; done"
			e.wire(t)
			if err := e.p.Rebuild(ctx, v.ID); err != nil {
				t.Fatal(err)
			}
			listings := func() int {
				n := 0
				for _, a := range e.cli.callsTo(t, "docker") {
					if len(a) > 1 && a[1] == "ps" && strings.Contains(strings.Join(a, " "), "status=running") {
						n++
					}
				}
				return n
			}
			before := 0
			if stopFails {
				e.p.idle()
			} else {
				for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
					if _, err := os.Stat(inUp); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("up never started")
					}
				}
				before = listings()
				if late := e.shutdown(20 * time.Second); len(late) > 0 {
					t.Fatalf("shutdown left %v running", late)
				}
			}
			pauses := 0
			for _, a := range e.cli.callsTo(t, "docker") {
				if len(a) > 1 && a[1] == "pause" {
					pauses++
				}
			}
			if stopFails {
				if listings() == 0 || pauses != 1 || e.kinds(t, v.ID, string(events.KindContainerRepaused)) == 0 {
					t.Errorf("control: %d running listings, %d pauses; a rebuild cut short before step 3 must pause again", listings(), pauses)
				}
				return
			}
			if got := e.view(t, v.ID); got.State != workspace.Failed || !strings.Contains(got.Steps[workspace.StepUp].Detail, "shut down") {
				t.Errorf("control: %s %+v, want failed at up for the shutdown", got.State, got.Steps[workspace.StepUp])
			}
			if n := listings() - before; n != 0 || pauses != 0 {
				t.Errorf("after the cut-off up: %d re-pause listings, %d pauses; step 3 had stopped the container", n, pauses)
			}
		})
	}
}
