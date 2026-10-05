//go:build linux

package pty

import (
	"bytes"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// readAll drains a master until the slave closes.
func readAll(t *testing.T, m io.Reader) []byte {
	t.Helper()
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := m.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if !IsEOF(err) {
				t.Fatalf("read master: %v", err)
			}
			return out.Bytes()
		}
	}
}

// The child sees a terminal, of the width asked for, as its controlling tty —
// and the two widths are different answers, so a helper that ignored the
// parameter fails one of them.
func TestStartGivesTheChildATerminalOfTheGivenWidth(t *testing.T) {
	for _, cols := range []int{80, 1000} {
		cmd := exec.Command("sh", "-c", `stty size; test -t 0 && test -t 1 && echo tty; tty </dev/tty >/dev/null && echo ctty`)
		m, err := Start(cmd, cols, 50)
		if err != nil {
			t.Fatal(err)
		}
		out := string(readAll(t, m))
		m.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("width %d: %v (%q)", cols, err, out)
		}
		lines := strings.Fields(strings.ReplaceAll(out, "\r", ""))
		want := []string{"50", strconv.Itoa(cols), "tty", "ctty"}
		if strings.Join(lines, " ") != strings.Join(want, " ") {
			t.Fatalf("width %d: got %q, want %q", cols, lines, want)
		}
	}
}

// Negative with its control: a pipe is not a terminal. The same probe over a
// pipe reports no tty, so the test above is not passing on a probe that
// always says yes.
func TestAPipeIsNotATerminal(t *testing.T) {
	out, _ := exec.Command("sh", "-c", `test -t 1 && echo tty || echo pipe`).Output()
	if strings.TrimSpace(string(out)) != "pipe" {
		t.Fatalf("probe over a pipe said %q", out)
	}
}

// The master reports the end once the child exits, rather than blocking.
func TestMasterEndsWhenTheChildExits(t *testing.T) {
	cmd := exec.Command("true")
	m, err := Start(cmd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	done := make(chan struct{})
	go func() { readAll(t, m); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("master read did not end after the child exited")
	}
	cmd.Wait()
}

func TestStartRefusesPresetStdio(t *testing.T) {
	cmd := exec.Command("true")
	cmd.Stdout = io.Discard
	if _, err := Start(cmd, 80, 24); err == nil {
		t.Fatal("Start accepted a command whose stdout was already set")
	}
	if _, err := Start(exec.Command("true"), 0, 24); err == nil {
		t.Fatal("Start accepted a zero width")
	}
}
