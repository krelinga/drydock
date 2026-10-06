//go:build linux

package login_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/krelinga/drydock/internal/login"
	"github.com/krelinga/drydock/internal/subproc"
)

const (
	testImage = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	busybox   = "busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
	loginID   = "0123456789abcdef01234567"
)

func launcher() login.DockerLauncher {
	return login.DockerLauncher{PrepImage: busybox, Volume: "drydock-claude-config", LabelPrefix: "drydock", UID: 1000, GID: 1000}
}

// pairs returns flag → values for the "--flag value" options in args.
func flagValues(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// TestRunArgs: the login container mounts the shared volume read-write at
// the workspaces' own path and nothing else, runs as Drydock's uid with no
// capabilities on a read-only root, carries the login label and never the
// workspace label, is given its whole environment, and is on a terminal.
func TestRunArgs(t *testing.T) {
	args, err := launcher().RunArgs(testImage, loginID)
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "run" || !slices.Contains(args, "--interactive") || !slices.Contains(args, "--tty") || !slices.Contains(args, "--rm") {
		t.Errorf("not an interactive, terminal, self-removing run: %q", args)
	}
	tail := args[len(args)-6:]
	if strings.Join(tail, " ") != "--entrypoint claude "+testImage+" auth login --claudeai" {
		t.Errorf("tail %q", tail)
	}
	if got := flagValues(args, "--mount"); len(got) != 1 || got[0] != "type=volume,source=drydock-claude-config,target=/home/vscode/.claude" {
		t.Errorf("mounts %q; want the volume alone, read-write, at the workspaces' CLAUDE_CONFIG_DIR", got)
	}
	if got := flagValues(args, "--label"); len(got) != 1 || got[0] != "drydock.login="+loginID {
		t.Errorf("labels %q", got)
	}
	if got := flagValues(args, "--user"); len(got) != 1 || got[0] != "1000:1000" {
		t.Errorf("user %q", got)
	}
	if got := flagValues(args, "--cap-drop"); len(got) != 1 || got[0] != "ALL" || slices.Contains(args, "--cap-add") {
		t.Errorf("capabilities: %q", args)
	}
	if !slices.Contains(args, "--read-only") || slices.Contains(args, "--privileged") {
		t.Errorf("root filesystem: %q", args)
	}
	want := []string{"CLAUDE_CONFIG_DIR=/home/vscode/.claude", "HOME=/tmp", "DISABLE_AUTOUPDATER=1"}
	if got := flagValues(args, "--env"); !slices.Equal(got, want) {
		t.Errorf("env %q; want %q", got, want)
	}
	joined := strings.Join(args, " ")
	for _, bad := range []string{"docker.sock", ".workspace", "--network none", "-v ", "--volume", "--env-file", "ANTHROPIC", "DISABLE_TELEMETRY", "DO_NOT_TRACK"} {
		if strings.Contains(joined, bad) {
			t.Errorf("argv carries %q: %s", bad, joined)
		}
	}
}

func TestRunArgsRefuses(t *testing.T) {
	for name, mut := range map[string]func(*login.DockerLauncher, *string, *string){
		"root":          func(d *login.DockerLauncher, _, _ *string) { d.UID = 0 },
		"no prefix":     func(d *login.DockerLauncher, _, _ *string) { d.LabelPrefix = "" },
		"bad volume":    func(d *login.DockerLauncher, _, _ *string) { d.Volume = "a,target=/etc" },
		"tagged image":  func(_ *login.DockerLauncher, img, _ *string) { *img = "node:22" },
		"bad id":        func(_ *login.DockerLauncher, _, id *string) { *id = "../x" },
		"option for id": func(_ *login.DockerLauncher, _, id *string) { *id = "--privileged" },
	} {
		d, img, id := launcher(), testImage, loginID
		mut(&d, &img, &id)
		if args, err := d.RunArgs(img, id); err == nil {
			t.Errorf("%s: accepted: %q", name, args)
		}
	}
	// Control: the unmutated launcher is accepted.
	if _, err := launcher().RunArgs(testImage, loginID); err != nil {
		t.Error(err)
	}
}

// TestPrepArgs: the ownership helper is root with only CHOWN and FOWNER, no
// network, the volume alone, and a constant script.
func TestPrepArgs(t *testing.T) {
	args, err := launcher().PrepArgs(loginID)
	if err != nil {
		t.Fatal(err)
	}
	if got := flagValues(args, "--cap-add"); !slices.Equal(got, []string{"CHOWN", "FOWNER"}) {
		t.Errorf("caps %q", got)
	}
	if got := flagValues(args, "--network"); !slices.Equal(got, []string{"none"}) {
		t.Errorf("network %q", got)
	}
	if got := flagValues(args, "--mount"); len(got) != 1 || got[0] != "type=volume,source=drydock-claude-config,target=/claude" {
		t.Errorf("mounts %q", got)
	}
	if got := flagValues(args, "--env"); !slices.Equal(got, []string{"DRYDOCK_UID=1000", "DRYDOCK_GID=1000"}) {
		t.Errorf("env %q", got)
	}
	if got := flagValues(args, "--label"); !slices.Equal(got, []string{"drydock.login=" + loginID}) {
		t.Errorf("label %q", got)
	}
	d := launcher()
	d.PrepImage = "busybox:latest"
	if _, err := d.PrepArgs(loginID); err == nil {
		t.Error("an unpinned helper image was accepted")
	}
}

// fakeRunner answers docker by argv and records every call.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	ans   func(args []string) (string, int)
}

func (f *fakeRunner) Run(_ context.Context, c subproc.Cmd) subproc.Result {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{c.Name}, c.Args...))
	f.mu.Unlock()
	out, code := f.ans(c.Args)
	if c.Stdout != nil {
		io.WriteString(c.Stdout, out)
	}
	return subproc.Result{ExitCode: code}
}

func (f *fakeRunner) Start(context.Context, subproc.Cmd) (subproc.Process, error) {
	return nil, errors.New("unused")
}

func id64(c byte) string { return strings.Repeat(string(c), 64) }

// TestRemoveAndSweep: by label and full id; a sweep spares the login in
// progress.
func TestRemoveAndSweep(t *testing.T) {
	f := &fakeRunner{ans: func(a []string) (string, int) {
		if a[0] == "ps" {
			switch a[len(a)-1] {
			case "label=drydock.login":
				return id64('a') + "\n" + id64('b') + "\n" + id64('c') + "\n", 0
			case "label=drydock.login=" + loginID:
				return id64('b') + "\n", 0
			}
			return "", 0
		}
		return "", 0
	}}
	d := launcher()
	d.Run = f
	if err := d.Remove(context.Background(), loginID); err != nil {
		t.Fatal(err)
	}
	n, err := d.Sweep(context.Background(), loginID)
	if err != nil || n != 2 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	var rms []string
	for _, c := range f.calls {
		if c[1] == "rm" {
			rms = append(rms, strings.Join(c[1:], " "))
		}
	}
	want := []string{"rm --force -- " + id64('b'), "rm --force -- " + id64('a') + " " + id64('c')}
	if !slices.Equal(rms, want) {
		t.Errorf("rm calls %q; want %q", rms, want)
	}

	// Something that is not a container id is never handed to rm.
	f.ans = func(a []string) (string, int) {
		if a[0] == "ps" {
			return "--all\n", 0
		}
		return "", 0
	}
	f.calls = nil
	if _, err := d.Sweep(context.Background(), ""); err == nil {
		t.Error("a non-id from docker ps was accepted")
	}
	for _, c := range f.calls {
		if c[1] == "rm" {
			t.Errorf("rm ran: %q", c)
		}
	}
}

type volumes struct{ err error }

func (v volumes) EnsureClaudeVolume(context.Context, string) (bool, error) { return false, v.err }

type images struct{}

func (images) Ensure(context.Context) (string, error) { return testImage, nil }

// TestLaunchRefusesAForeignOwner: the helper's exit 4 is a refusal naming
// both uids, and nothing is started.
func TestLaunchRefusesAForeignOwner(t *testing.T) {
	f := &fakeRunner{ans: func(a []string) (string, int) { return "4242\n", 4 }}
	d := launcher()
	d.Run, d.Volumes, d.Image = f, volumes{}, images{}
	d.PTY = subproc.Exec{Resolver: subproc.FixedResolver{}} // no docker for the PTY: must never be reached
	_, err := d.Launch(context.Background(), loginID, 80, 24)
	var le *login.LaunchError
	if !errors.As(err, &le) || le.Problem != login.ProblemVolumeOwner || !strings.Contains(le.Message, "uid 4242") || !strings.Contains(le.Message, "uid 1000") {
		t.Fatalf("%v", err)
	}
	// The volume is ensured first: a failure there is the volume's problem,
	// and no helper runs against a volume that might not be labelled.
	f.calls = nil
	d.Volumes = volumes{err: errors.New("foreign")}
	_, err = d.Launch(context.Background(), loginID, 80, 24)
	if !errors.As(err, &le) || le.Problem != login.ProblemVolume || len(f.calls) != 0 {
		t.Errorf("%v, %d docker calls", err, len(f.calls))
	}
	// Control: a helper that exits 0 gets as far as finding docker.
	f.ans = func([]string) (string, int) { return "", 0 }
	d.Volumes = volumes{}
	_, err = d.Launch(context.Background(), loginID, 80, 24)
	if !errors.As(err, &le) || le.Problem != login.ProblemDocker {
		t.Errorf("control: %v", err)
	}
}
