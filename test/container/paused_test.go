package container_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/container"
	"github.com/krelinga/drydock/internal/sys"
	"github.com/krelinga/drydock/internal/workspace"
)

// TestAPausedContainersSessionServerIsNotGone: against real Docker, a paused
// workspace container is never "no session server". `docker ps --filter
// status=running` does not list a paused container and `docker exec` refuses
// one, so before, SignalSession found nothing to signal and a stop called a
// frozen server stopped. Now it says the container is paused. The controls
// are the same container running, where the server is found, and `docker
// stop` of the paused container, which ends it — what a workspace stop or
// delete carries on to (internal/provision).
func TestAPausedContainersSessionServerIsNotGone(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	p := prefix(t)
	ws, err := workspace.NewID(time.Now(), sys.CryptoRandom{})
	if err != nil {
		t.Fatal(err)
	}
	// A stand-in session server: the pid in the pid file, and remote-control
	// in its cmdline, which is what the signal script checks.
	id := labelled(t, p, ws, "sh", "-c",
		`echo $$ > `+container.RemoteControlPidFile+`; exec sh -c 'while :; do sleep 1; done' remote-control`)
	m := manager(p)

	deadline := time.Now().Add(20 * time.Second)
	for {
		found, err := m.SignalSession(ctx, ws, container.SessionAlive, "")
		if err == nil && found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control: the running container's server was not found: %v %v", found, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	docker(t, "pause", id)
	t.Cleanup(func() { exec.Command("docker", "unpause", id).Run() })
	for _, sig := range []container.SessionSignal{container.SessionAlive, container.SessionTerm, container.SessionKill} {
		if _, err := m.SignalSession(ctx, ws, sig, ""); !errors.Is(err, container.ErrSessionContainerPaused) {
			t.Errorf("%s to a paused container's server: %v, want ErrSessionContainerPaused", sig, err)
		}
	}

	// Unpaused, the server is found again: nothing was signalled meanwhile.
	docker(t, "unpause", id)
	if found, err := m.SignalSession(ctx, ws, container.SessionAlive, ""); err != nil || !found {
		t.Errorf("control: after unpausing, found %v, err %v", found, err)
	}

	// docker stop ends a paused container.
	docker(t, "pause", id)
	docker(t, "stop", "-t", "1", id)
	if st := docker(t, "inspect", "-f", "{{.State.Status}}", id); st != "exited" {
		t.Errorf("a paused container after docker stop is %q, want exited", st)
	}
	if found, err := m.SignalSession(ctx, ws, container.SessionAlive, ""); err != nil || found {
		t.Errorf("a stopped container: found %v, err %v; want no server and no error", found, err)
	}
}
