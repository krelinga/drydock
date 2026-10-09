package container_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
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

// devcontainerInit is PID 1 as the devcontainer CLI runs it in a container
// whose image has no entrypoint of its own: exit 0 on SIGTERM, otherwise
// wait for ever. Here it also starts $0, the stand-in session server, as its
// child — not PID 1, as the real server is not.
const devcontainerInit = `trap "exit 0" 15
sh -c "$0" &
while sleep 1 & wait $!; do :; done`

// standIn is a stand-in session server: its pid in the pid file,
// remote-control in its cmdline (what the signal script checks), and a TERM
// handler that says so on the container's log, as a real server's
// deregistration would happen in its own.
const standIn = `echo $$ > ` + container.RemoteControlPidFile + `; exec sh -c 'trap "echo got-TERM; exit 0" TERM; while :; do sleep 0.1; done' remote-control`

// TestUnpauseLetsTheSessionServerHaveItsSIGTERM: against real Docker, a
// paused workspace container that container.Unpause has unpaused takes the
// session server's SIGTERM — the stand-in's TERM handler runs — which is why
// a workspace stop, rebuild or delete unpauses first (internal/provision).
// The control is the same paused container ended as it was before, by
// `docker stop` alone: Docker thaws the container to deliver its SIGTERM to
// PID 1 (measured, Docker 29.8.2: the stop is quick, exit 0), PID 1 exits,
// and the server is SIGKILLed with the rest of the container — its TERM
// handler never runs, so a real one never deregisters. Unpause of a
// workspace with nothing paused unpauses nothing and is no error.
func TestUnpauseLetsTheSessionServerHaveItsSIGTERM(t *testing.T) {
	needDocker(t)
	ctx := context.Background()
	p := prefix(t)
	m := manager(p)
	start := func() (string, string) {
		ws, err := workspace.NewID(time.Now(), sys.CryptoRandom{})
		if err != nil {
			t.Fatal(err)
		}
		id := labelled(t, p, ws, "sh", "-c", devcontainerInit, standIn)
		deadline := time.Now().Add(20 * time.Second)
		for {
			if found, err := m.SignalSession(ctx, ws, container.SessionAlive, ""); err == nil && found {
				return ws, id
			}
			if time.Now().After(deadline) {
				t.Fatal("the stand-in server never came up")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	ws, id := start()
	if n, err := m.Unpause(ctx, ws); n != 0 || err != nil {
		t.Errorf("control: unpausing a running workspace: %d, %v; want nothing and no error", n, err)
	}
	docker(t, "pause", id)
	t.Cleanup(func() { exec.Command("docker", "unpause", id).Run() })
	if n, err := m.Unpause(ctx, ws); n != 1 || err != nil {
		t.Fatalf("unpausing the paused workspace: %d, %v", n, err)
	}
	if st := docker(t, "inspect", "-f", "{{.State.Status}}", id); st != "running" {
		t.Fatalf("after Unpause the container is %q", st)
	}
	if found, err := m.SignalSession(ctx, ws, container.SessionTerm, ""); err != nil || !found {
		t.Fatalf("SIGTERM after unpausing: found %v, err %v", found, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(docker(t, "logs", id), "got-TERM") {
		if time.Now().After(deadline) {
			t.Fatal("the server's TERM handler did not run after the unpause")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The control: paused, and ended by docker stop alone.
	_, id2 := start()
	docker(t, "pause", id2)
	t.Cleanup(func() { exec.Command("docker", "unpause", id2).Run() })
	began := time.Now()
	docker(t, "stop", "-t", "5", id2)
	took := time.Since(began)
	st := docker(t, "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", id2)
	t.Logf("docker stop of the paused container took %v and left it %q", took, st)
	if st != "exited 0" {
		t.Errorf("control: a paused container after docker stop is %q, want exited 0 (PID 1 had its SIGTERM)", st)
	}
	if logs := docker(t, "logs", id2); strings.Contains(logs, "got-TERM") {
		t.Errorf("control: the server's TERM handler ran without an unpause; logs %q", logs)
	}
}
