package dockerguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/krelinga/drydock/internal/subproc"
)

// TestMain makes this test binary the guard when it is run as "docker", as
// cmd/drydock does, so the tests below run the real Main through the link
// Prepare makes.
func TestMain(m *testing.M) {
	if IsGuard(os.Args[0]) {
		os.Exit(Main(os.Args[0], os.Args[1:], os.Stderr))
	}
	os.Exit(m.Run())
}

// fakeDocker is a "real docker" that records its argv one NUL-terminated
// argument at a time, copies stdin to stdout, writes a line to stderr, and
// exits 42 — every channel the guard must hand over unaltered.
const fakeDocker = `#!/bin/sh
for a; do printf '%s\0' "$a"; done > "$(dirname "$0")/ran"
cat
echo "docker's own stderr" >&2
exit 42
`

// guardDir prepares a guard directory with this binary as the guard and the
// fake as docker, and returns the --docker-path and the directory.
func guardDir(t *testing.T) (string, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	fake := filepath.Join(bin, "docker")
	if err := os.WriteFile(fake, []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), ".drydock", "guard")
	g := &Guard{Binary: self, Resolver: subproc.FixedResolver{"docker": fake}}
	dp, err := g.Prepare(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dp != filepath.Join(dir, LinkName) {
		t.Fatalf("docker path %q", dp)
	}
	return dp, dir
}

type ran struct {
	args           []string
	stdout, stderr string
	code           int
	ranDocker      bool
}

func runGuard(t *testing.T, dp string, stdin string, args ...string) ran {
	t.Helper()
	// The fake writes beside the path it was run as: the guard directory.
	ranFile := filepath.Join(filepath.Dir(dp), "ran")
	os.Remove(ranFile)
	cmd := exec.Command(dp, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := ran{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	if b, err := os.ReadFile(ranFile); err == nil {
		r.ranDocker = true
		r.args = strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	}
	return r
}

func mustReal(t *testing.T, link string) string {
	t.Helper()
	p, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A command the guard passes is docker's own: argv byte for byte (spaces,
// newlines, an empty argument, flags that would be refused in a run), stdin
// through to docker, docker's stdout and stderr, and docker's exit status.
func TestPassedCommandsAreDockersOwn(t *testing.T) {
	dp, _ := guardDir(t)
	for _, args := range [][]string{
		{"inspect", "--type", "container", "a b", "", "line\nbreak", "--privileged"},
		{"exec", "-i", "-u", "vscode", "-e", "X=a b", "abc", "/bin/sh", "-c", "echo $X"},
		{"ps", "-q", "-a", "--filter", "label=drydock.workspace=x"},
		{"-v"},
	} {
		r := runGuard(t, dp, "stdin bytes\x00\n", args...)
		if !r.ranDocker || !reflect.DeepEqual(r.args, args) {
			t.Errorf("%q: docker ran %v with %q", args, r.ranDocker, r.args)
		}
		if r.stdout != "stdin bytes\x00\n" || r.stderr != "docker's own stderr\n" || r.code != 42 {
			t.Errorf("%q: stdout %q stderr %q exit %d", args, r.stdout, r.stderr, r.code)
		}
	}
}

// Fail closed: with no policy — none written, one that does not parse, one
// of another version, one that is a symbolic link — a command that creates a
// container is refused, with the marker and ExitRefused, and docker never
// runs; the refusal is recorded beside the guard. The control is the same
// command with a policy that allows it, which runs.
func TestNoPolicyRefusesAndRunsNothing(t *testing.T) {
	dp, dir := guardDir(t)
	root := filepath.Dir(filepath.Dir(dir))
	run := []string{"run", "--sig-proxy=false", "--mount", "type=bind,source=" + filepath.Join(root, "repo") + ",target=/workspaces/repo", "img"}
	for name, setup := range map[string]func(){
		"none":        func() { os.Remove(filepath.Join(dir, PolicyName)) },
		"unparseable": func() { os.WriteFile(filepath.Join(dir, PolicyName), []byte(`{"version":`), 0o600) },
		"another version": func() {
			os.WriteFile(filepath.Join(dir, PolicyName), []byte(`{"version":2,"clone":"/x","temp_dir":"/y"}`), 0o600)
		},
		"a symlink": func() {
			p := FixturePolicy(root, nil)
			b, _ := json.Marshal(p)
			real := filepath.Join(root, "elsewhere.json")
			os.WriteFile(real, b, 0o600)
			os.Remove(filepath.Join(dir, PolicyName))
			os.Symlink(real, filepath.Join(dir, PolicyName))
		},
	} {
		setup()
		ClearRefusal(dir)
		r := runGuard(t, dp, "", run...)
		if r.ranDocker || r.code != ExitRefused || !strings.Contains(r.stderr, Marker+": "+SettingNoPolicy) {
			t.Errorf("%s: docker ran %v, exit %d, stderr %q", name, r.ranDocker, r.code, r.stderr)
		}
		ref, err := ReadRefusal(dir)
		if err != nil || ref == nil || !reflect.DeepEqual(ref.Settings, []string{SettingNoPolicy}) {
			t.Errorf("%s: refusal %+v %v", name, ref, err)
		}
	}
	os.Remove(filepath.Join(dir, PolicyName))
	if err := WritePolicy(dir, *FixturePolicy(root, nil)); err != nil {
		t.Fatal(err)
	}
	if r := runGuard(t, dp, "", run...); !r.ranDocker || r.code != 42 {
		t.Errorf("control: with a policy, docker ran %v, exit %d, stderr %q", r.ranDocker, r.code, r.stderr)
	}
	if err := RemovePolicy(dir); err != nil {
		t.Fatal(err)
	}
	if r := runGuard(t, dp, "", run...); r.ranDocker || r.code != ExitRefused {
		t.Errorf("after RemovePolicy: docker ran %v, exit %d", r.ranDocker, r.code)
	}
}

// A refused command names what it was refused for, on stderr and in the
// record, and records every refusal of one up.
func TestRefusalsAreRecorded(t *testing.T) {
	dp, dir := guardDir(t)
	root := filepath.Dir(filepath.Dir(dir))
	if err := WritePolicy(dir, *FixturePolicy(root, nil)); err != nil {
		t.Fatal(err)
	}
	r := runGuard(t, dp, "", "run", "--privileged", "img")
	if r.ranDocker || r.code != ExitRefused || !strings.Contains(r.stderr, Marker+": privileged") {
		t.Errorf("docker ran %v, exit %d, stderr %q", r.ranDocker, r.code, r.stderr)
	}
	runGuard(t, dp, "", "run", "--cap-add", "SYS_ADMIN", "img")
	ref, err := ReadRefusal(dir)
	if err != nil || ref == nil || !reflect.DeepEqual(ref.Settings, []string{SettingCapAdd, SettingPrivileged}) ||
		!reflect.DeepEqual(ref.Commands, []string{"run"}) {
		t.Errorf("refusal %+v %v", ref, err)
	}
	if err := ClearRefusal(dir); err != nil {
		t.Fatal(err)
	}
	if ref, _ := ReadRefusal(dir); ref != nil {
		t.Errorf("after ClearRefusal: %+v", ref)
	}
}

// Run by a relative path the guard cannot find its directory, so it runs
// nothing at all.
func TestARelativePathRunsNothing(t *testing.T) {
	dp, dir := guardDir(t)
	cmd := exec.Command("./"+LinkName, "ps")
	cmd.Dir = dir
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != ExitRefused || !strings.HasPrefix(errb.String(), Marker) {
		t.Errorf("relative: %v %q", err, errb.String())
	}
	if r := runGuard(t, dp, "", "ps"); !r.ranDocker {
		t.Error("control: by its absolute path, ps did not run")
	}
}

// Prepare refuses what would make the guard unsafe to run: no binary, a
// docker that is the guard itself (it would run itself), a relative
// directory — and replaces links an earlier Prepare left.
func TestPrepare(t *testing.T) {
	self, _ := os.Executable()
	dir := filepath.Join(t.TempDir(), "g")
	for name, g := range map[string]*Guard{
		"no binary":         {Resolver: subproc.FixedResolver{"docker": "/bin/true"}},
		"docker is drydock": {Binary: self, Resolver: subproc.FixedResolver{"docker": self}},
		"no docker":         {Binary: self, Resolver: subproc.FixedResolver{}},
	} {
		if _, err := g.Prepare(dir); err == nil {
			t.Errorf("%s: prepared", name)
		}
	}
	if _, err := (&Guard{Binary: self, Resolver: subproc.FixedResolver{"docker": "/bin/true"}}).Prepare("rel"); err == nil {
		t.Error("a relative directory was prepared")
	}
	g := &Guard{Binary: self, Resolver: subproc.FixedResolver{"docker": "/bin/true"}}
	if _, err := g.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	g.Resolver = subproc.FixedResolver{"docker": "/bin/false"}
	if _, err := g.Prepare(dir); err != nil {
		t.Fatal(err)
	}
	if got := mustReal(t, filepath.Join(dir, RealName)); got != "/bin/false" {
		t.Errorf("real-docker → %q after a second Prepare", got)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("guard directory %v %v", fi.Mode(), err)
	}
}
