//go:build linux

package subproc

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/pty"
)

func drain(t *testing.T, m *os.File) string {
	t.Helper()
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := m.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if !pty.IsEOF(err) {
				t.Fatalf("read: %v", err)
			}
			return strings.ReplaceAll(out.String(), "\r", "")
		}
	}
}

// The package's rules hold on a terminal too: the Resolver picks the program,
// argv arrives whole with no shell, Env replaces, and the child has a terminal
// of the asked width as its stdio.
func TestStartPTYKeepsTheRules(t *testing.T) {
	t.Setenv("DRYDOCK_PARENT_ONLY", "leaked")
	r := Exec{Resolver: fake(t, "devcontainer",
		`printf '%s\n' "$@"; test -t 0 && test -t 1 && echo tty; stty size; env | grep -c DRYDOCK_PARENT_ONLY; echo "ONLY=$ONLY"`)}
	p, m, err := r.StartPTY(context.Background(), Cmd{Name: "devcontainer",
		Args: []string{"exec", "a b", "$(id)"}, Env: []string{"ONLY=this", "PATH=/usr/bin:/bin"}}, 120, 40)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	out := drain(t, m)
	if res := p.Wait(); res.Err != nil {
		t.Fatalf("%+v", res)
	}
	want := "exec\na b\n$(id)\ntty\n40 120\n0\nONLY=this\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// Cancelling the context sends SIGTERM — which a child can answer — never a
// SIGKILL it cannot: the trap's line and exit status are the proof.
func TestStartPTYCancelSendsSIGTERM(t *testing.T) {
	r := Exec{Resolver: fake(t, "child", `trap 'echo got-term; exit 7' TERM; echo ready; while :; do sleep 0.05; done`)}
	ctx, cancel := context.WithCancel(context.Background())
	p, m, err := r.StartPTY(ctx, Cmd{Name: "child"}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for !strings.Contains(out.String(), "ready") {
		n, err := m.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			t.Fatalf("read before ready: %v (%q)", err, out.String())
		}
	}
	cancel()
	rest := drain(t, m)
	res := p.Wait()
	if !strings.Contains(rest, "got-term") || res.ExitCode != 7 {
		t.Errorf("after cancel: %q, exit %d; want got-term and exit 7", rest, res.ExitCode)
	}
}

func TestStartPTYRefusesPresetStdio(t *testing.T) {
	r := Exec{Resolver: fake(t, "x", `true`)}
	if _, _, err := r.StartPTY(context.Background(), Cmd{Name: "x", Stdout: io.Discard}, 80, 24); err == nil {
		t.Fatal("accepted a preset stdout")
	}
	if _, _, err := r.StartPTY(context.Background(), Cmd{Name: "x"}, 0, 24); err == nil {
		t.Fatal("accepted a zero width")
	}
}
