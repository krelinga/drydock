package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// upID is the container the fake daemon "creates" in these tests.
var upID = strings.Repeat("c", 64)

// TestACancelledUpStopsItsDockerClients: the devcontainer CLI, stopped,
// leaves the docker clients it started running (measured on 0.89.0: a
// buildx build and the build step under it, a docker run, a postCreate
// command's docker exec), so up runs in a process group of its own and a
// cancel stops the group. The positive control is the client running
// before the cancel; without Group it outlives the stopped up.
func TestACancelledUpStopsItsDockerClients(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "client")
	run, _ := fakes(t, map[string]string{
		// The "docker client": a child the CLI started and does not stop.
		"devcontainer": "sleep 300 & echo $! > '" + pidFile + "'; wait",
		"docker":       "exit 0",
	})
	m := guarded(t, run, "drydock.test")
	m.UpSettle = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := m.Up(ctx, upSpec(t))
		done <- err
	}()
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("up never started its client")
		}
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	// Control: the client is running while up is.
	if !procAlive(pid) {
		t.Fatalf("control: the client %d is not running before the cancel", pid)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled up: %v, want context.Canceled", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled up did not end")
	}
	for deadline := time.Now().Add(5 * time.Second); procAlive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the CLI's client %d outlived the cancelled up", pid)
		}
	}
}

// TestACancelledUpWaitsForTheContainerBeingCreated: a docker run killed
// mid-create leaves the daemon to finish the create, and the container
// lists only after the client's group has emptied (measured: 5–40 ms into
// the run, a created container listed half a second later and not at the
// group's end). So a cancelled Up keeps listing the workspace's label until
// one lists or UpSettle passes, and returns its id for the caller to record
// — removing nothing, since it is the workspace's container. A delete waits
// for the run it cancelled, so it then finds what landed.
//
// The fake CLI stands in for the client: on SIGTERM it hands the create to a
// process outside its group (the "daemon"), which lands it 200 ms later.
// Controls in the same function: an up with nothing landing returns no id
// once UpSettle has passed (the wait is bounded and invents nothing); a
// container that already lists is returned at once; an up that fails by
// itself lists nothing; and nothing is ever stopped or removed.
func TestACancelledUpWaitsForTheContainerBeingCreated(t *testing.T) {
	dir := t.TempDir()
	landed := filepath.Join(dir, "landed")
	listed := "[ -e '" + landed + "' ] && echo " + upID + "; exit 0"
	cli := func(land bool) string {
		hand := ""
		if land {
			// Its own session, and none of the CLI's pipes: the daemon
			// is neither in the group nor holding Run's output open.
			hand = "setsid sh -c 'sleep 0.2; touch \"" + landed + "\"' </dev/null >/dev/null 2>&1 & "
		}
		return "trap '" + strings.ReplaceAll(hand, "'", `'\''`) + "exit 143' TERM\ntouch '" + filepath.Join(dir, "running") + "'\nwhile :; do sleep 0.05; done"
	}
	cancelUp := func(t *testing.T, m Manager) (string, error, time.Duration) {
		t.Helper()
		os.Remove(filepath.Join(dir, "running"))
		ctx, cancel := context.WithCancel(context.Background())
		type out struct {
			id  string
			err error
		}
		done := make(chan out, 1)
		go func() {
			c, _, err := m.Up(ctx, upSpec(t))
			done <- out{c.ContainerID, err}
		}()
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, "running")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("up never started")
			}
		}
		start := time.Now()
		cancel()
		select {
		case o := <-done:
			return o.id, o.err, time.Since(start)
		case <-time.After(20 * time.Second):
			t.Fatal("the cancelled up did not end")
		}
		return "", nil, 0
	}
	noRemoval := func(t *testing.T, ddir string) {
		t.Helper()
		b, _ := os.ReadFile(filepath.Join(ddir, "docker.argv"))
		for _, call := range strings.Split(string(b), "--\n") {
			if f := strings.Fields(call); len(f) > 0 && f[0] != "ps" {
				t.Errorf("a cancelled up ran docker %s: it must remove or stop nothing", f[0])
			}
		}
	}

	t.Run("a create landing after the cancel", func(t *testing.T) {
		os.Remove(landed)
		run, ddir := fakes(t, map[string]string{"devcontainer": cli(true), "docker": listed})
		m := guarded(t, run, "drydock.test")
		m.Logf = t.Logf
		id, err, took := cancelUp(t, m)
		if id != upID || !errors.Is(err, context.Canceled) {
			t.Errorf("Up = %q, %v; want the container that landed, and context.Canceled", id, err)
		}
		if took > 2*time.Second {
			t.Errorf("Up took %s: it should stop looking once the container lists", took)
		}
		noRemoval(t, ddir)
	})

	t.Run("control: nothing landing", func(t *testing.T) {
		os.Remove(landed)
		run, ddir := fakes(t, map[string]string{"devcontainer": cli(false), "docker": listed})
		m := guarded(t, run, "drydock.test")
		m.UpSettle = 300 * time.Millisecond
		id, err, took := cancelUp(t, m)
		if id != "" || !errors.Is(err, context.Canceled) {
			t.Errorf("Up = %q, %v; want no id, and context.Canceled", id, err)
		}
		if took < 300*time.Millisecond || took > 5*time.Second {
			t.Errorf("Up took %s, want UpSettle (300ms) and a little more", took)
		}
		noRemoval(t, ddir)
	})

	t.Run("control: a container already there", func(t *testing.T) {
		if err := os.WriteFile(landed, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		run, _ := fakes(t, map[string]string{"devcontainer": cli(false), "docker": listed})
		m := guarded(t, run, "drydock.test")
		m.UpSettle = time.Hour
		id, _, took := cancelUp(t, m)
		if id != upID || took > 5*time.Second {
			t.Errorf("Up = %q after %s; want the listed container at once", id, took)
		}
	})

	t.Run("control: an up that failed by itself", func(t *testing.T) {
		run, ddir := fakes(t, map[string]string{"devcontainer": "exit 1", "docker": listed})
		m := guarded(t, run, "drydock.test")
		if _, _, err := m.Up(context.Background(), upSpec(t)); err == nil {
			t.Error("an up with no result was not an error")
		}
		if b, _ := os.ReadFile(filepath.Join(ddir, "docker.argv")); len(b) > 0 {
			t.Errorf("an up Drydock did not stop listed containers: %q", b)
		}
	})
}
