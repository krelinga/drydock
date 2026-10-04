package subproc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
