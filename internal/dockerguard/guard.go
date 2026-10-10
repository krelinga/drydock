package dockerguard

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/krelinga/drydock/internal/subproc"
)

// The guard's directory, /srv/drydock/ws/<id>/.drydock/guard: beside the
// clone, so no container sees it, and the workspace's alone.
//
//	docker       → the drydock binary: the --docker-path the CLI is given
//	real-docker  → the docker every passed command runs
//	policy.json  → what this run may ask for; present only during an up
//	refused.json → what the guard refused, for Drydock to read after the up
//
// The guard is the drydock binary run under the name "docker" (IsGuard): a
// separate process, so it learns its policy from a file Drydock writes, and
// it finds that file by the path it was run as. Not the environment, which
// the CLI hands to every process it starts, and not an argument, which
// --docker-path cannot carry.
const (
	LinkName    = "docker"
	RealName    = "real-docker"
	PolicyName  = "policy.json"
	RefusalName = "refused.json"
)

// ExitRefused is the guard's exit status for a refused command: EX_NOPERM.
const ExitRefused = 77

// Marker begins the one line the guard writes to stderr when it refuses.
const Marker = "drydock-docker-guard: refused"

// IsGuard reports whether a process run as argv0 is the guard.
func IsGuard(argv0 string) bool { return filepath.Base(argv0) == LinkName }

// Guard makes the guard's directory for a workspace.
type Guard struct {
	// Binary is the drydock binary, absolute: what the guard link names.
	Binary string
	// Resolver finds the real docker; nil is PATH.
	Resolver subproc.Resolver
}

// Prepare makes dir, the workspace's guard directory, and its two links,
// replacing links already there, and returns the --docker-path to give the
// CLI. Every devcontainer invocation calls it, so a guard directory an older
// Drydock never made, or one naming a binary since moved, is right before it
// is used.
func (g *Guard) Prepare(dir string) (string, error) {
	if g == nil || !filepath.IsAbs(g.Binary) {
		return "", errors.New("dockerguard: no absolute path to the drydock binary")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("dockerguard: %q is not absolute", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("dockerguard: %s is not a directory", dir)
	}
	r := g.Resolver
	if r == nil {
		r = subproc.PathResolver{}
	}
	real, err := r.Resolve("docker")
	if err != nil {
		return "", fmt.Errorf("dockerguard: docker: %w", err)
	}
	if real, err = filepath.Abs(real); err != nil {
		return "", err
	}
	// The real docker must not be the guard: the guard would run itself.
	if a, err1 := os.Stat(real); err1 == nil {
		if b, err2 := os.Stat(g.Binary); err2 == nil && os.SameFile(a, b) {
			return "", fmt.Errorf("dockerguard: docker resolves to the drydock binary itself (%s)", real)
		}
	}
	if err := link(g.Binary, filepath.Join(dir, LinkName)); err != nil {
		return "", err
	}
	if err := link(real, filepath.Join(dir, RealName)); err != nil {
		return "", err
	}
	return filepath.Join(dir, LinkName), nil
}

// link points name at target, replacing whatever is at name in one rename.
func link(target, name string) error {
	if cur, err := os.Readlink(name); err == nil && cur == target {
		return nil
	}
	// A name of its own: two invocations for one workspace (a probe and the
	// session server, say) may prepare the directory at once.
	var b [8]byte
	rand.Read(b[:])
	tmp := name + "." + hex.EncodeToString(b[:]) + ".tmp"
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// WritePolicy writes the policy for the up about to run, replacing any.
func WritePolicy(dir string, p Policy) error {
	p.Version = PolicyVersion
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writeFile(dir, PolicyName, b)
}

// RemovePolicy removes the policy when the up is over, so a later command
// that would create a container finds none and is refused.
func RemovePolicy(dir string) error {
	err := os.Remove(filepath.Join(dir, PolicyName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func writeFile(dir, name string, b []byte) error {
	f, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// ReadPolicy reads the policy beside the guard: nil when there is none, an
// error when it is there but cannot be read as this version's — which the
// guard treats as none.
func ReadPolicy(dir string) (*Policy, error) {
	path := filepath.Join(dir, PolicyName)
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("dockerguard: %s is not a regular file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("dockerguard: %s: %w", path, err)
	}
	if p.Version != PolicyVersion || p.Clone == "" || p.TempDir == "" {
		return nil, fmt.Errorf("dockerguard: %s is not a version %d policy", path, PolicyVersion)
	}
	return &p, nil
}

// Refusal is what the guard refused during one up: settings from the
// closed set Check names, and the kinds of command.
type Refusal struct {
	Settings []string `json:"settings"`
	Commands []string `json:"commands"`
}

// ReadRefusal reads what the guard refused since ClearRefusal: nil when it
// refused nothing.
func ReadRefusal(dir string) (*Refusal, error) {
	b, err := os.ReadFile(filepath.Join(dir, RefusalName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Refusal
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ClearRefusal forgets an earlier run's refusal before an up.
func ClearRefusal(dir string) error {
	err := os.Remove(filepath.Join(dir, RefusalName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// recordRefusal adds d to the refusal file, keeping what an earlier command
// of the same up refused.
func recordRefusal(dir string, d Decision) error {
	r, _ := ReadRefusal(dir)
	if r == nil {
		r = &Refusal{}
	}
	r.Settings = union(r.Settings, d.Settings)
	r.Commands = union(r.Commands, []string{d.Command})
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeFile(dir, RefusalName, b)
}

// WithLabels is args with the policy's Labels added, as -l key=value in key
// order, right after `run` or `create` (or `container run`/`create`) — among
// the options, before the image. Any other command, or no policy, is args
// as given. They are labels and not id-labels: the CLI never sees them, so
// they change neither which container up matches nor ${devcontainerId}.
func WithLabels(p *Policy, args []string) []string {
	if p == nil || len(p.Labels) == 0 || len(args) == 0 {
		return args
	}
	at := 0
	switch {
	case args[0] == "run" || args[0] == "create":
		at = 1
	case len(args) > 1 && args[0] == "container" && (args[1] == "run" || args[1] == "create"):
		at = 2
	default:
		return args
	}
	keys := make([]string, 0, len(p.Labels))
	for k := range p.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := append([]string(nil), args[:at]...)
	for _, k := range keys {
		out = append(out, "-l", k+"="+p.Labels[k])
	}
	return append(out, args[at:]...)
}

func union(a, b []string) []string {
	m := map[string]bool{}
	for _, s := range append(append([]string(nil), a...), b...) {
		m[s] = true
	}
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Main is the guard: the drydock binary run as argv0, a path ending in
// /docker, with docker's arguments. A command Check refuses is never run —
// the refusal is recorded beside the guard, one line goes to stderr, and the
// exit status is ExitRefused. Any other command replaces this process with
// the real docker (execve), so its argv, stdin, stdout, stderr, signals and
// exit status are docker's own, exactly. Main returns only when it does not
// exec. The one change it makes to argv is WithLabels: the policy's Labels
// added to a run or create, which is then checked as docker will run it.
func Main(argv0 string, args []string, stderr io.Writer) int {
	if !filepath.IsAbs(argv0) {
		// Without its own path the guard can find neither its policy nor
		// the real docker: run nothing.
		fmt.Fprintf(stderr, "%s: the guard was not run by an absolute path, so it runs nothing\n", Marker)
		return ExitRefused
	}
	dir := filepath.Dir(argv0)
	p, err := ReadPolicy(dir)
	if err != nil {
		fmt.Fprintf(stderr, "drydock-docker-guard: %v; nothing that creates a container will run\n", err)
		p = nil
	}
	real := filepath.Join(dir, RealName)
	// The policy's labels are added to a run or create before it is
	// checked, so what is checked is what docker runs.
	args = WithLabels(p, args)
	d := Check(p, args)
	if !d.Refused && len(d.Start) > 0 {
		// The containers a start names are held to the policy as they were
		// created: read them with docker itself, argv as data, no shell.
		out, err := exec.Command(real, append([]string{"inspect", "--type", "container", "--"}, d.Start...)...).Output()
		if err != nil {
			d = Decision{Refused: true, Command: "start", Settings: []string{SettingStartUnread},
				Why: []string{fmt.Sprintf("docker inspect of the containers to start: %v", err)}}
		} else {
			d = CheckStarted(p, d.Start, out, func() (*LogConfig, error) { return DaemonLogConfig(real, p, stderr) })
		}
	}
	if d.Refused {
		if err := recordRefusal(dir, d); err != nil {
			fmt.Fprintf(stderr, "drydock-docker-guard: could not record the refusal: %v\n", err)
		}
		fmt.Fprintf(stderr, "%s: %s: %s\n", Marker, strings.Join(d.Settings, ", "), strings.Join(d.Why, "; "))
		return ExitRefused
	}
	err = unix.Exec(real, append([]string{real}, args...), os.Environ())
	fmt.Fprintf(stderr, "drydock-docker-guard: could not run docker: %v\n", err)
	return 126
}
