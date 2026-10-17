package subproc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fake writes an executable shell script and resolves name to it.
func fake(t *testing.T, name, script string) FixedResolver {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return FixedResolver{name: p}
}

func TestRunPassesArgvExactlyAndReportsExitAsData(t *testing.T) {
	r := Exec{Resolver: fake(t, "devcontainer", `printf '%s\n' "$@"; echo err >&2; exit 3`)}
	var out, errOut bytes.Buffer
	res := r.Run(context.Background(), Cmd{Name: "devcontainer",
		Args:   []string{"up", "a b", "x;rm -rf /", "$(id)"},
		Stdout: &out, Stderr: &errOut})
	if res.Err != nil || res.ExitCode != 3 {
		t.Fatalf("result %+v; want exit 3 as data, no Err", res)
	}
	// No shell: each argument arrives whole and unexpanded.
	want := "up\na b\nx;rm -rf /\n$(id)\n"
	if out.String() != want || errOut.String() != "err\n" {
		t.Errorf("stdout %q stderr %q; want %q", out.String(), errOut.String(), want)
	}
}

// Env replaces the environment rather than adding to it, so a key in the
// parent's environment cannot reach a child nobody thought about (§13.5).
func TestEnvReplacesRatherThanInherits(t *testing.T) {
	t.Setenv("DRYDOCK_PARENT_ONLY", "leaked")
	r := Exec{Resolver: fake(t, "show", `env`)}
	var out bytes.Buffer
	r.Run(context.Background(), Cmd{Name: "show", Env: []string{"ONLY=this"}, Stdout: &out})
	if strings.Contains(out.String(), "leaked") {
		t.Error("the parent's environment reached the child")
	}
	if !strings.Contains(out.String(), "ONLY=this") {
		t.Errorf("control: the given environment is missing: %q", out.String())
	}
}

func TestRunUnknownProgramIsAnError(t *testing.T) {
	res := Exec{Resolver: FixedResolver{}}.Run(context.Background(), Cmd{Name: "nope"})
	if !errors.Is(res.Err, exec.ErrNotFound) {
		t.Errorf("%+v; want ErrNotFound", res)
	}
}

// Cancelling sends SIGTERM, so a CLI gets to clean up: the script traps it
// and exits 42, which a SIGKILL would never let it do.
func TestCancelSendsTerm(t *testing.T) {
	r := Exec{Resolver: fake(t, "slow", `trap 'echo term; exit 42' TERM; echo ready; while :; do sleep 0.05; done`)}
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan Result)
	go func() { done <- r.Run(ctx, Cmd{Name: "slow", Stdout: &out}) }()
	for !strings.Contains(out.String(), "ready") {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case res := <-done:
		if res.ExitCode != 42 || !strings.Contains(out.String(), "term") {
			t.Errorf("result %+v, output %q; want the TERM trap to run", res, out.String())
		}
		if res.Err == nil {
			t.Error("a cancelled run reported no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not stop the child")
	}
}

// A cancelled Group run stops the program's children too: the devcontainer
// CLI leaves the docker clients it started running when it is stopped. The
// script starts a child that ignores SIGTERM, so it is the SIGKILL to the
// group once the program has exited that ends it; the control is the same
// run without Group, whose child survives.
func TestCancelOfAGroupStopsItsChildren(t *testing.T) {
	for _, group := range []bool{false, true} {
		pidFile := filepath.Join(t.TempDir(), "pid")
		r := Exec{Resolver: fake(t, "parent", `(trap '' TERM; exec sleep 300) & echo $! > '`+pidFile+`'; wait`), WaitDelay: time.Second}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan Result)
		go func() { done <- r.Run(ctx, Cmd{Name: "parent", Group: group}) }()
		var pid int
		for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the child never started")
			}
			b, _ := os.ReadFile(pidFile)
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("cancel did not stop the program")
		}
		alive := false
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if alive = running(pid); !alive {
				break
			}
		}
		if group && alive {
			t.Errorf("Group: the child %d outlived the cancelled run", pid)
		}
		if !group && !alive {
			t.Errorf("control: without Group the child %d did not survive, so the test shows nothing", pid)
		}
	}
}

// The group's SIGTERM reaches the children, not only the program: a child
// that handles TERM (as a docker client cleans up on it) gets to, and to
// finish: the group's SIGKILL waits out the grace rather than landing the
// moment the program exits. The child's TERM trap takes 0.3 s, then writes a
// mark; under Group the mark appears, and the
// control — the same run without Group — leaves none, so the mark is not the
// child ending on its own.
func TestCancelOfAGroupTermsItsChildren(t *testing.T) {
	for _, group := range []bool{false, true} {
		dir := t.TempDir()
		pidFile, mark, ready := filepath.Join(dir, "pid"), filepath.Join(dir, "termed"), filepath.Join(dir, "ready")
		child := `trap 'sleep 0.3; echo yes > ` + mark + `; exit 0' TERM; echo > ` + ready + `; while :; do sleep 0.05; done`
		r := Exec{Resolver: fake(t, "parent", `sh -c "`+child+`" & echo $! > '`+pidFile+`'; wait`), WaitDelay: time.Second}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan Result)
		go func() { done <- r.Run(ctx, Cmd{Name: "parent", Group: group}) }()
		var pid int
		for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the child never set its trap")
			}
			if _, err := os.Stat(ready); err != nil {
				continue
			}
			b, _ := os.ReadFile(pidFile)
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("cancel did not stop the program")
		}
		// Run has returned, so a SIGTERM was sent before it did and the
		// trap has had WaitDelay to run; a moment more for the write.
		_, err := os.Stat(mark)
		for deadline := time.Now().Add(2 * time.Second); group && err != nil && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			_, err = os.Stat(mark)
		}
		if !group {
			time.Sleep(300 * time.Millisecond)
			_, err = os.Stat(mark)
		}
		if group && err != nil {
			t.Errorf("Group: the child was never sent SIGTERM, or was killed before its trap finished: %v", err)
		}
		if !group && err == nil {
			t.Error("control: without Group the child was sent SIGTERM, so the test shows nothing")
		}
	}
}

// Start and StartPTY refuse Group rather than drop it: only Run signals the
// group. The control is the same Cmd without Group, which starts.
func TestStartRefusesGroup(t *testing.T) {
	r := Exec{Resolver: fake(t, "prog", "exit 0")}
	if _, err := r.Start(context.Background(), Cmd{Name: "prog", Group: true}); err == nil {
		t.Error("Start accepted Group")
	}
	if _, _, err := r.StartPTY(context.Background(), Cmd{Name: "prog", Group: true}, 80, 24); err == nil {
		t.Error("StartPTY accepted Group")
	}
	p, err := r.Start(context.Background(), Cmd{Name: "prog"})
	if err != nil {
		t.Fatalf("control: Start without Group: %v", err)
	}
	p.Wait()
	p, f, err := r.StartPTY(context.Background(), Cmd{Name: "prog"}, 80, 24)
	if err != nil {
		t.Fatalf("control: StartPTY without Group: %v", err)
	}
	p.Wait()
	f.Close()
}

// running reports whether pid is a live process — not gone, and not a
// zombie waiting for a reaper.
func running(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
