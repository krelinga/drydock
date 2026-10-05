//go:build linux

package claudetest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/krelinga/drydock/internal/pty"
)

// Fake is one installed fakeclaude: a binary named `claude` in its own
// directory, the script beside it, and a state directory for its report.
type Fake struct {
	// Path is the absolute path of the `claude` binary. Hand it to
	// subproc.FixedResolver{"claude": f.Path}, or put Dir on PATH.
	Path string
	// Dir is the directory holding only that binary and its script.
	Dir string
	// Script is what was written; changing it after Install does nothing.
	Script Script
}

// CodeSHA256 is the hash Login.AcceptSHA256 wants: the script holds this, not
// the code, so neither the script nor the event log is a copy of a canary.
func CodeSHA256(code string) string {
	s := sha256.Sum256([]byte(code))
	return hex.EncodeToString(s[:])
}

// Install builds fakeclaude (once per test binary), links it into a fresh
// temp directory as `claude`, and writes s beside it. Corpus and StateDir
// default to the repository's test/fixtures and a fresh temp directory.
func Install(t testing.TB, s Script) *Fake {
	t.Helper()
	bin := build(t)
	if s.Corpus == "" {
		s.Corpus = CorpusRoot(t)
	}
	if s.StateDir == "" {
		s.StateDir = t.TempDir()
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	// A hard link, not a symlink: fakeclaude finds its script through
	// /proc/self/exe, which resolves a symlink to the shared build.
	if err := os.Link(bin, path); err != nil {
		b, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	js, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".json", js, 0o600); err != nil {
		t.Fatal(err)
	}
	return &Fake{Path: path, Dir: dir, Script: s}
}

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// build compiles ./fakeclaude into a directory that lives as long as the test
// binary. The go build cache makes every build after the first cheap.
func build(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeclaude-")
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", buildPath, "github.com/krelinga/drydock/internal/claudetest/fakeclaude")
		cmd.Dir = moduleRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build fakeclaude: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildPath
}

func moduleRoot() string {
	dir, _ := os.Getwd()
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

// CorpusRoot is the absolute path of test/fixtures.
func CorpusRoot(t testing.TB) string {
	t.Helper()
	p := filepath.Join(moduleRoot(), "test", "fixtures")
	if _, err := os.Stat(filepath.Join(p, "transcripts", "claude-"+Version)); err != nil {
		t.Fatalf("corpus for %s: %v", Version, err)
	}
	return p
}

// Transcript reads one corpus file for Version.
func Transcript(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(CorpusRoot(t), "transcripts", "claude-"+Version, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Command returns an exec.Cmd for the fake with argv[0] "claude", as the real
// one would be run.
func (f *Fake) Command(args ...string) *exec.Cmd {
	cmd := exec.Command(f.Path, args...)
	cmd.Args[0] = "claude"
	return cmd
}

// Start runs the fake on a fresh PTY cols wide. Width is a parameter because
// narrow and wide are separate cases (testing §6.4).
func (f *Fake) Start(t testing.TB, cols int, args ...string) *Term {
	t.Helper()
	return StartTerm(t, f.Command(args...), cols, 50)
}

// Events reads the fake's event log.
func (f *Fake) Events(t testing.TB) []Event {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.Script.StateDir, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("event log: %v", err)
		}
		evs = append(evs, e)
	}
	return evs
}

// Kind filters events.
func Kind(evs []Event, kind string) []Event {
	var out []Event
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Violations is every complaint the fake recorded about what it received.
func (f *Fake) Violations(t testing.TB) []Event { return Kind(f.Events(t), EventViolation) }

// NoViolations fails the test if the fake complained about anything.
func (f *Fake) NoViolations(t testing.TB) {
	t.Helper()
	for _, v := range f.Violations(t) {
		t.Errorf("fakeclaude invocation %d: %s", v.Invocation, v.What)
	}
}

// Term is a process on a PTY, with everything it has written kept.
type Term struct {
	cmd    *exec.Cmd
	master *os.File

	mu      sync.Mutex
	out     bytes.Buffer
	changed chan struct{} // closed and replaced on every read
	readEnd chan struct{}
	exited  chan struct{}
	state   *os.ProcessState
}

// StartTerm starts any command on a fresh PTY cols × rows. The process is
// killed and the PTY closed when the test ends.
func StartTerm(t testing.TB, cmd *exec.Cmd, cols, rows int) *Term {
	t.Helper()
	m, err := pty.Start(cmd, cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	tm := &Term{cmd: cmd, master: m, changed: make(chan struct{}), readEnd: make(chan struct{}), exited: make(chan struct{})}
	go tm.readLoop()
	go func() {
		cmd.Wait()
		tm.mu.Lock()
		tm.state = cmd.ProcessState
		tm.mu.Unlock()
		close(tm.exited)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-tm.exited
		m.Close()
		<-tm.readEnd
	})
	return tm
}

func (tm *Term) readLoop() {
	defer close(tm.readEnd)
	buf := make([]byte, 4096)
	for {
		n, err := tm.master.Read(buf)
		tm.mu.Lock()
		tm.out.Write(buf[:n])
		close(tm.changed)
		tm.changed = make(chan struct{})
		tm.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// Output is a copy of everything read from the terminal so far.
func (tm *Term) Output() []byte {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return bytes.Clone(tm.out.Bytes())
}

// Write types b at the terminal — what Drydock does with a login code.
func (tm *Term) Write(b []byte) error {
	_, err := tm.master.Write(b)
	return err
}

// Signal sends sig to the process.
func (tm *Term) Signal(sig syscall.Signal) error { return tm.cmd.Process.Signal(sig) }

// Pid is the process id.
func (tm *Term) Pid() int { return tm.cmd.Process.Pid }

// WaitOutput waits until the output satisfies ok, and returns it. On timeout
// it returns the output so far and an error.
func (tm *Term) WaitOutput(d time.Duration, ok func([]byte) bool) ([]byte, error) {
	deadline := time.After(d)
	for {
		tm.mu.Lock()
		out, ch := bytes.Clone(tm.out.Bytes()), tm.changed
		tm.mu.Unlock()
		if ok(out) {
			return out, nil
		}
		select {
		case <-ch:
		case <-tm.readEnd:
			out := tm.Output()
			if ok(out) {
				return out, nil
			}
			return out, errors.New("terminal closed first")
		case <-deadline:
			return tm.Output(), fmt.Errorf("not within %v", d)
		}
	}
}

// WaitFor waits until the output contains sub.
func (tm *Term) WaitFor(d time.Duration, sub []byte) ([]byte, error) {
	return tm.WaitOutput(d, func(b []byte) bool { return bytes.Contains(b, sub) })
}

// Wait waits up to d for the process to exit and for the terminal to drain.
// It reports false if the process is still running — which, for a gate that
// hangs, is the verdict (the timeout is the assertion; there is no message).
func (tm *Term) Wait(d time.Duration) (exited bool, code int, out []byte) {
	select {
	case <-tm.exited:
	case <-time.After(d):
		return false, 0, tm.Output()
	}
	select {
	case <-tm.readEnd:
	case <-time.After(5 * time.Second):
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return true, tm.state.ExitCode(), bytes.Clone(tm.out.Bytes())
}

// Running reports whether the process is still alive.
func (tm *Term) Running() bool {
	select {
	case <-tm.exited:
		return false
	default:
		return true
	}
}
