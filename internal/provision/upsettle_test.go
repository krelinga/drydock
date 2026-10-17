package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/workspace"
)

// landingID is the container a killed docker run's create leaves.
var landingID = strings.Repeat("d", 64)

// landingUp is an up whose docker run is killed mid-create: on SIGTERM it
// hands the create to a process outside its group and none of its pipes —
// the daemon, which finishes a create whose client was killed (measured:
// the container lists only after the up's group has emptied) — and exits.
// The "daemon" lands the container 200 ms later and touches landed.
func landingUp(t *testing.T, e *env) {
	t.Helper()
	land := filepath.Join(e.cli.dir, "land.sh")
	script := "#!/bin/sh\nsleep 0.2\necho \"" + landingID + " $1 created\" >> '" + e.cli.dir + "/containers'\ntouch '" + e.cli.dir + "/landed'\n"
	if err := os.WriteFile(land, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.cli.up = `ws=; prev=
for a in "$@"; do
  if [ "$prev" = --id-label ]; then case "$a" in *.workspace=*) ws=${a#*.workspace=} ;; esac; fi
  prev=$a
done
trap 'setsid "` + land + `" "$ws" </dev/null >/dev/null 2>&1 & exit 143' TERM
touch '` + e.cli.dir + `/in-up'
while :; do sleep 0.05; done`
	e.wire(t)
}

func (e *env) awaitUp(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(e.cli.dir, "in-up")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("up never started")
		}
	}
}

func (e *env) awaitLanded(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(e.cli.dir, "landed")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("control: the create never landed")
		}
	}
}

// TestADeleteFindsTheContainerAStoppedUpWasCreating: a delete cancels the
// run in flight and waits for it, then removes the workspace's containers by
// label. When the run was inside up's docker run, the daemon can finish that
// create after the up's process group has emptied, so the run's end waits
// for it (container.UpSettle) — otherwise the delete's listing could run
// first, the row would go, and boot would adopt the deleted workspace's
// container as an orphan. The positive control is that the create did land,
// and was removed by the delete's own docker rm.
func TestADeleteFindsTheContainerAStoppedUpWasCreating(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	landingUp(t, e)
	w, err := e.p.Create(ctx, alpha, "")
	if err != nil {
		t.Fatal(err)
	}
	e.awaitUp(t)
	if err := e.p.Delete(ctx, w.ID, "krelinga/alpha"); err != nil {
		t.Fatal(err)
	}
	e.p.idle()
	// Control: the create landed — waited for here too, so a delete that
	// did not wait for it fails below, by the container it left.
	e.awaitLanded(t)
	if _, err := e.p.Workspaces.Get(ctx, w.ID); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("the row survived: %v; actions %v", err, e.actions(t, w.ID))
	}
	if cs := e.containers(t, w.ID); len(cs) != 0 {
		t.Errorf("the container the stopped up was creating survived the delete: %v", cs)
	}
	removed := false
	for _, a := range e.cli.callsTo(t, "docker") {
		if len(a) > 1 && a[1] == "rm" && strings.Contains(strings.Join(a, " "), landingID) {
			removed = true
		}
	}
	if !removed {
		t.Error("control: the delete's docker rm never named the landed container")
	}
}

// TestAStoppedUpRecordsTheContainerItWasCreating: a run cut off inside up —
// here by shutdown — records the container its killed docker run left, once
// it lands, rather than leaving the row without one until the next boot. It
// is recorded, never removed: the workspace's container, kept for the next
// start (which passes --remove-existing-container from failed) and for
// reconciliation. The control is that the run failed for the shutdown.
func TestAStoppedUpRecordsTheContainerItWasCreating(t *testing.T) {
	ctx := context.Background()
	e := lifecycleEnv(t)
	landingUp(t, e)
	w, err := e.p.Create(ctx, alpha, "")
	if err != nil {
		t.Fatal(err)
	}
	e.awaitUp(t)
	if late := e.shutdown(20 * time.Second); len(late) > 0 {
		t.Fatalf("shutdown left %v running", late)
	}
	e.awaitLanded(t)
	got, err := e.p.Workspaces.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != workspace.Failed {
		t.Errorf("control: state %s, want failed", got.State)
	}
	if got.ContainerID != landingID {
		t.Errorf("container %q, want the one the stopped up's create left, %q", got.ContainerID, landingID)
	}
	if cs := e.containers(t, w.ID); cs[landingID] != "created" {
		t.Errorf("the container: %v; a stopped up must remove nothing", cs)
	}
}
