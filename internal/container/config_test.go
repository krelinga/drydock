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

	"github.com/krelinga/drydock/internal/sys"
)

// TestParseConfigurationAgainstTheCorpus: the recorded read-configuration
// results, each with the verdict its .meta says it must yield. The
// unparseable one is the case that matters — the CLI exits 0 for it, so a
// parser that trusted the exit code would hand `up` a config with no image.
func TestParseConfigurationAgainstTheCorpus(t *testing.T) {
	c, err := ParseConfiguration([]byte(fixture(t, "read-configuration-ok.json")))
	if err != nil || c.WorkspaceFolder != "/workspaces/alpha" ||
		c.ConfigFile != "/srv/drydock/ws/FIXTURE/alpha/.devcontainer/devcontainer.json" {
		t.Errorf("ok: %+v %v", c, err)
	}
	c, err = ParseConfiguration([]byte(fixture(t, "read-configuration-override.json")))
	if err != nil || c.WorkspaceFolder != "/workspaces/plain2" {
		t.Errorf("override: %+v %v", c, err)
	}
	if _, err := ParseConfiguration([]byte(fixture(t, "read-configuration-unparseable.json"))); !errors.Is(err, ErrUnbuildable) {
		t.Errorf("unparseable: %v, want ErrUnbuildable", err)
	}
	if _, err := ParseConfiguration([]byte(fixture(t, "read-configuration-noconfig.stdout"))); err == nil {
		t.Error("an empty stdout parsed")
	}
	for _, in := range []string{
		`{"configuration":{"image":"x"}}`, // no workspace
		`{"configuration":{"image":"x"},"workspace":{"workspaceFolder":"relative"}}`,
		`{"configuration":{"image":"x"},"workspace":{"workspaceFolder":"/w"}} {}`,
		`[]`,
	} {
		if _, err := ParseConfiguration([]byte(in)); err == nil || errors.Is(err, ErrUnbuildable) {
			t.Errorf("%s: %v; want a contract error", in, err)
		}
	}
	for _, cfg := range []string{`"image":"x"`, `"dockerFile":"Dockerfile"`, `"build":{"dockerfile":"D"}`,
		`"dockerComposeFile":"c.yml"`, `"dockerComposeFile":["a.yml","b.yml"]`} {
		in := `{"configuration":{` + cfg + `},"workspace":{"workspaceFolder":"/w"}}`
		if _, err := ParseConfiguration([]byte(in)); err != nil {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	for _, cfg := range []string{`"image":""`, `"build":{}`, `"dockerComposeFile":[]`, `"name":"x"`} {
		in := `{"configuration":{` + cfg + `},"workspace":{"workspaceFolder":"/w"}}`
		if _, err := ParseConfiguration([]byte(in)); !errors.Is(err, ErrUnbuildable) {
			t.Errorf("%s: %v, want ErrUnbuildable", cfg, err)
		}
	}
}

// TestReadConfigurationArgvAndExitCodes: the override is passed when given
// and only then, and a non-zero exit is a ReadError rather than a verdict.
func TestReadConfigurationArgvAndExitCodes(t *testing.T) {
	run, dir := fakes(t, map[string]string{
		"devcontainer": "cat <<'EOF'\n" + fixture(t, "read-configuration-ok.json") + "\nEOF\n",
	})
	m := Manager{Run: run, LabelPrefix: "drydock.test"}
	if _, err := m.ReadConfiguration(context.Background(), "/srv/ws/alpha", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadConfiguration(context.Background(), "/srv/ws/alpha", "/srv/ws/.drydock/devcontainer.json"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv(t, dir, "devcontainer"), " ")
	merged := "--include-merged-configuration --id-label drydock.test.read-configuration=none "
	want := "read-configuration --workspace-folder /srv/ws/alpha " + merged + "-- " +
		"read-configuration --workspace-folder /srv/ws/alpha " + merged + "--override-config /srv/ws/.drydock/devcontainer.json "
	if got != want {
		t.Errorf("argv\n got %q\nwant %q", got, want)
	}
	if _, err := m.ReadConfiguration(context.Background(), "/srv/ws/alpha", "relative.json"); err == nil {
		t.Error("a relative override was accepted")
	}

	run, _ = fakes(t, map[string]string{"devcontainer": "echo banner >&2; exit 1"})
	m.Run = run
	var re *ReadError
	if _, err := m.ReadConfiguration(context.Background(), "/srv/ws/alpha", ""); !errors.As(err, &re) || re.ExitCode != 1 || re.Stderr != "banner" {
		t.Errorf("exit 1: %v", err)
	}
}

func TestUpAndExecPassTheOverrideConfig(t *testing.T) {
	s := spec()
	s.OverrideConfig = "/srv/drydock/ws/" + wsID + "/.drydock/devcontainer.json"
	args, err := Manager{LabelPrefix: "drydock"}.Args(s)
	if err != nil || !strings.Contains(strings.Join(args, " "), " --override-config "+s.OverrideConfig) {
		t.Errorf("up argv %v %v", args, err)
	}
	s.OverrideConfig = "relative.json"
	if _, err := (Manager{LabelPrefix: "drydock"}).Args(s); err == nil {
		t.Error("a relative override was accepted by up")
	}

	run, dir := fakes(t, map[string]string{"devcontainer": "exit 0"})
	m := Manager{Run: run, LabelPrefix: "drydock.test"}
	if _, err := m.ExecIn(context.Background(), ExecSpec{WorkspaceID: wsID, Folder: "/srv/ws/alpha",
		OverrideConfig: "/srv/ws/.drydock/devcontainer.json", Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv(t, dir, "devcontainer"), " ")
	want := "exec --workspace-folder /srv/ws/alpha --id-label drydock.test.workspace=" + wsID +
		" --override-config /srv/ws/.drydock/devcontainer.json -- true "
	if got != want {
		t.Errorf("exec argv\n got %q\nwant %q", got, want)
	}
}

// Each read-configuration runs with a TMPDIR of its own, replacing
// Drydock's, and removed after: the CLI downloads each Feature's blob to a
// folder under $TMPDIR named by the millisecond, and two reads started
// together corrupted each other's (TAR_BAD_ARCHIVE). The control is the
// inherited one, which no read sees, and two reads, which see two.
func TestReadConfigurationRunsWithItsOwnTempDir(t *testing.T) {
	inherited := t.TempDir()
	t.Setenv("TMPDIR", inherited)
	out := filepath.Join(t.TempDir(), "tmpdir")
	run, _ := fakes(t, map[string]string{"devcontainer": `{ echo "$TMPDIR"; [ -d "$TMPDIR" ] && echo exists; } >> ` + out +
		"\ncat <<'EOF'\n" + fixture(t, "read-configuration-ok.json") + "\nEOF\n"})
	m := Manager{Run: run, LabelPrefix: "drydock.test"}
	for range 2 {
		if _, err := m.ReadConfiguration(context.Background(), "/srv/ws/alpha", ""); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 || lines[1] != "exists" || lines[3] != "exists" {
		t.Fatalf("read-configuration saw:\n%s", b)
	}
	for _, dir := range []string{lines[0], lines[2]} {
		if dir == inherited {
			t.Errorf("read-configuration ran with Drydock's own TMPDIR %s", dir)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s outlived its read-configuration: %v", dir, err)
		}
	}
	if lines[0] == lines[2] {
		t.Errorf("two reads shared TMPDIR %s", lines[0])
	}
}

// TestReadConfigurationHasItsOwnBound: a read-configuration that never
// answers — the CLI waiting on a docker client the daemon never answers, as a
// wedged daemon was measured to leave it — ends at ReadConfigurationTimeout
// on the Manager's clock, with a ReadTimeoutError, and the docker client it
// started is stopped with it. Controls: until the clock reaches the bound
// the read is still running, and a read that answers before it is a result.
// The test bounds itself in real time, so a missing bound fails it by name
// rather than hanging the package.
func TestReadConfigurationHasItsOwnBound(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child")
	run, _ := fakes(t, map[string]string{
		// The "docker client": a child the CLI started and does not stop.
		"devcontainer": "echo 'a CLI log line' >&2; sleep 300 & echo $! > '" + pidFile + "'; wait",
	})
	clock := sys.NewFakeClock(time.Now())
	m := Manager{Run: run, LabelPrefix: "drydock.test", Clock: clock}
	done := make(chan error, 1)
	go func() {
		_, err := m.ReadConfiguration(context.Background(), "/srv/ws/alpha", "")
		done <- err
	}()
	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0 || clock.WaitingFor(ReadConfigurationTimeout) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the read never started, or set no bound on the clock (child %d)", pid)
		}
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	// Control: a moment short of the bound, the read is still running.
	clock.Advance(ReadConfigurationTimeout - time.Second)
	select {
	case err := <-done:
		t.Fatalf("control: the read ended before its bound: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	clock.Advance(time.Second)
	select {
	case err := <-done:
		var te *ReadTimeoutError
		if !errors.As(err, &te) || te.Limit != ReadConfigurationTimeout || te.Stderr != "a CLI log line" {
			t.Errorf("at the bound: %v, want a ReadTimeoutError of %s carrying the log", err, ReadConfigurationTimeout)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the read did not end at its bound")
	}
	for deadline := time.Now().Add(5 * time.Second); procAlive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Errorf("the CLI's child %d outlived the stopped read", pid)
			break
		}
	}

	// Control: a read that answers is a result, on the same clock, the
	// clock never moved.
	run, _ = fakes(t, map[string]string{
		"devcontainer": "cat <<'EOF'\n" + fixture(t, "read-configuration-ok.json") + "\nEOF\n",
	})
	m.Run = run
	if c, err := m.ReadConfiguration(context.Background(), "/srv/ws/alpha", ""); err != nil || c.WorkspaceFolder != "/workspaces/alpha" {
		t.Errorf("control: a read that answers: %+v %v", c, err)
	}
	// And ReadTimeout overrides the default.
	run, _ = fakes(t, map[string]string{"devcontainer": "exec sleep 300"})
	m.Run, m.ReadTimeout = run, time.Minute
	go func() {
		_, err := m.ReadConfiguration(context.Background(), "/srv/ws/alpha", "")
		done <- err
	}()
	for deadline := time.Now().Add(10 * time.Second); clock.WaitingFor(time.Minute) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("ReadTimeout set no bound of its own")
		}
	}
	clock.Advance(time.Minute)
	select {
	case err := <-done:
		var te *ReadTimeoutError
		if !errors.As(err, &te) || te.Limit != time.Minute {
			t.Errorf("ReadTimeout: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ReadTimeout did not bound the read")
	}
}

// TestACallersDeadlineIsNotTheReadsOwn: rctx is a child of the caller's
// context, so when a caller's sys.WithTimeout fires first (the run's
// ProvisionTimeout), sys.TimedOut sees it through rctx too. That is the
// caller's deadline, and saying "did not finish within 3 minutes" would be
// false: the error is not a ReadTimeoutError. The control, in the same
// function, is a caller's deadline later than the read's own bound, where
// the read's own bound fires and is one.
func TestACallersDeadlineIsNotTheReadsOwn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent time.Duration
		own    bool
	}{
		{"caller first", time.Minute, false},
		{"own first (control)", ReadConfigurationTimeout + time.Hour, true},
	} {
		run, _ := fakes(t, map[string]string{"devcontainer": "exec sleep 300"})
		clock := sys.NewFakeClock(time.Now())
		m := Manager{Run: run, LabelPrefix: "drydock.test", Clock: clock}
		parent, cancel := sys.WithTimeout(context.Background(), clock, tc.parent)
		done := make(chan error, 1)
		go func() {
			_, err := m.ReadConfiguration(parent, "/srv/ws/alpha", "")
			done <- err
		}()
		for deadline := time.Now().Add(10 * time.Second); clock.WaitingFor(ReadConfigurationTimeout) == 0 || clock.WaitingFor(tc.parent) == 0; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the read never set its bound", tc.name)
			}
		}
		clock.Advance(min(tc.parent, ReadConfigurationTimeout))
		select {
		case err := <-done:
			var te *ReadTimeoutError
			if errors.As(err, &te) != tc.own || err == nil {
				t.Errorf("%s: %v; a ReadTimeoutError: want %v", tc.name, err, tc.own)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("%s: the read did not end", tc.name)
		}
		cancel()
	}
}

// procAlive reports whether pid is a live process: not gone, not a zombie.
func procAlive(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	return i >= 0 && i+2 < len(b) && b[i+2] != 'Z'
}
